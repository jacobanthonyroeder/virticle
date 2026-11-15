package main

// How "virticle node add|list|remove", "virticle status" and "virticle setup" reach the controller.
//
// While the controller runs, it serves a few API routes (api.Server.LocalHandler) on a Unix socket in its data
// directory: control.sock, mode 0600, in a directory of mode 0700, and the listener accepts only root and the
// controller's own account (SO_PEERCRED). The commands send their requests there, so a node added or removed takes effect
// at once, and the web UI hears of it through the event stream. Nothing new listens on the network.
//
// While the controller is stopped, the commands take the data directory's lock (the one a starting controller takes),
// open the database themselves and run the same handlers in their own process. The next start reads the result.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"virticle/internal/api"
	"virticle/internal/cluster"
	"virticle/internal/ctlbackup"
	"virticle/internal/store"
)

const socketName = "control.sock"

func socketPath(data string) string { return filepath.Join(data, socketName) }

// serveLocalSocket starts the command line's socket. A failure is logged and the controller goes on: the commands then
// say that they cannot reach it.
func serveLocalSocket(srv *api.Server, data string) {
	path := socketPath(data)
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		c.Close()
		log.Printf("WARNING: %s is answered by another process; the command line cannot reach this controller", path)
		return
	}
	os.Remove(path) // left over from a controller that ended
	old := syscall.Umask(0o177)
	l, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		log.Printf("WARNING: the command line cannot reach this controller (%v): virticle node add and virticle status need it", err)
		return
	}
	if err := os.Chmod(path, 0o600); err != nil {
		log.Printf("WARNING: %s: %v", path, err)
	}
	hs := &http.Server{Handler: srv.LocalHandler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := hs.Serve(peerListener{l}); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("local socket: %v", err)
		}
	}()
}

// peerListener only hands out connections from root and from this process's own account.
type peerListener struct{ net.Listener }

func (l peerListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if uid, ok := peerUID(c); ok && (uid == 0 || uid == os.Geteuid()) {
			return c, nil
		}
		c.Close()
	}
}

func peerUID(c net.Conn) (int, bool) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, false
	}
	var cred *unix.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) { cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil || cerr != nil {
		return 0, false
	}
	return int(cred.Uid), true
}

// ctl is the command line's connection to the controller: the running one, or the handlers in this process.
type ctl struct {
	data    string
	running bool // through the running controller's socket
	client  *http.Client

	// Only while the controller is stopped:
	lock *os.File
	st   *store.Store
	mgr  *cluster.Manager
}

// errNoAccess is returned when the data directory cannot be read: the command needs root or the controller's account.
var errNoAccess = errors.New("permission denied")

// openCtl connects to the controller that uses data. While it is stopped, the handlers run here; wait says how long to
// wait for the socket of a controller that holds the lock but does not answer yet (it is starting).
func openCtl(data string, wait time.Duration) (*ctl, error) {
	if _, err := os.Stat(data); err != nil {
		if os.IsPermission(err) {
			return nil, errNoAccess
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
	}
	if f, err := os.Open(data); err != nil && os.IsPermission(err) {
		return nil, errNoAccess
	} else if err == nil {
		f.Close()
	}
	deadline := time.Now().Add(wait)
	for {
		if c := dialCtl(data); c != nil {
			return c, nil
		}
		if err := os.MkdirAll(data, 0o700); err != nil {
			return nil, err
		}
		lock, err := ctlbackup.LockDataDir(data)
		switch {
		case errors.Is(err, ctlbackup.ErrLocked):
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("the controller that runs on %s does not answer on %s (it is older than this command, or still starting). Restart it: sudo systemctl restart virticle", data, socketPath(data))
			}
			time.Sleep(300 * time.Millisecond)
			continue
		case os.IsPermission(err):
			return nil, errNoAccess
		case err != nil:
			return nil, err
		}
		chownLike(data, filepath.Join(data, ctlbackup.LockName))
		return openLocal(data, lock)
	}
}

// waitForSocket waits until the controller answers on its socket; false after d.
func waitForSocket(data string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if c := dialCtl(data); c != nil {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func dialCtl(data string) *ctl {
	path := socketPath(data)
	c, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return nil
	}
	c.Close()
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}}
	return &ctl{data: data, running: true, client: &http.Client{Transport: tr}}
}

// openLocal runs the handlers in this process, on the database of a controller that is stopped.
func openLocal(data string, lock *os.File) (*ctl, error) {
	fail := func(err error) (*ctl, error) {
		lock.Close()
		return nil, err
	}
	keyFile := filepath.Join(data, "id_ed25519")
	pub, err := ensureKey(keyFile)
	if err != nil {
		return fail(fmt.Errorf("the controller's SSH key: %w", err))
	}
	dbPath, err := store.DBPath(data, func(string, ...any) {})
	if err != nil {
		return fail(fmt.Errorf("database: %w", err))
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return fail(fmt.Errorf("database: %w", err))
	}
	id, _ := ensureInstanceID(filepath.Join(data, "instance-id"))
	mgr := cluster.NewManager(keyFile, filepath.Join(data, "known_hosts"))
	mgr.Persist = st
	nodes, err := st.Nodes()
	if err != nil {
		st.Close()
		return fail(fmt.Errorf("database: %w", err))
	}
	for _, n := range nodes {
		mgr.Add(n)
	}
	srv := &api.Server{Store: st, Nodes: mgr, PubKey: pub, DataDir: data, InstanceID: id, NoScheduler: true}
	return &ctl{data: data, lock: lock, st: st, mgr: mgr, client: &http.Client{Transport: handlerTransport{srv.LocalHandler()}}}, nil
}

// handlerTransport answers requests with a handler in this process.
type handlerTransport struct{ h http.Handler }

func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	t.h.ServeHTTP(w, r)
	return w.Result(), nil
}

// Close ends the connection. In this process it stops the node connections, gives the files that were made here to
// the controller's account and lets go of the lock.
func (c *ctl) Close() {
	if c.running {
		return
	}
	for _, n := range c.mgr.Status() {
		c.mgr.Remove(n.ID)
	}
	c.st.Close()
	dbPath, _ := store.DBPath(c.data, func(string, ...any) {})
	chownLike(c.data, dbPath, dbPath+"-wal", dbPath+"-shm", filepath.Join(c.data, "known_hosts"), filepath.Join(c.data, "instance-id"),
		filepath.Join(c.data, "id_ed25519"), filepath.Join(c.data, "id_ed25519.pub"))
	c.lock.Close()
}

// call sends one request; a non-2xx answer becomes the error the API gave.
func (c *ctl) call(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://controller"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode/100 != 2 {
		var e struct{ Error string }
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("%s %s: %s", method, path, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (c *ctl) nodes() ([]cluster.NodeStatus, error) {
	var l struct {
		Nodes []cluster.NodeStatus `json:"nodes"`
	}
	err := c.call("GET", "/api/nodes", nil, &l)
	return l.Nodes, err
}

// settledNodes lists the nodes once none is still connecting, or after d.
func (c *ctl) settledNodes(d time.Duration) ([]cluster.NodeStatus, error) {
	deadline := time.Now().Add(d)
	for {
		ns, err := c.nodes()
		if err != nil {
			return nil, err
		}
		busy := false
		for _, n := range ns {
			busy = busy || n.State == "connecting" || n.State == ""
		}
		if !busy || time.Now().After(deadline) {
			return ns, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (c *ctl) version() string {
	var v struct {
		Version string `json:"version"`
	}
	if c.call("GET", "/api/version", nil, &v) != nil {
		return ""
	}
	return v.Version
}
