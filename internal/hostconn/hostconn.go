// Package hostconn connects to a libvirt host the agentless way: one SSH
// connection per host, with the libvirt RPC protocol spoken over a channel
// forwarded to the host's libvirt UNIX socket. The same SSH connection is
// reused for host-level commands that libvirt has no API for.
package hostconn

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"virticle/internal/optrace"
)

// Sockets tried in order: the monolithic/proxy socket first, then the modular
// QEMU daemon's own socket (hosts running virtqemud without virtproxyd).
var libvirtSockets = []string{
	"/run/libvirt/libvirt-sock",
	"/run/libvirt/virtqemud-sock",
}

// Target describes how to reach one host over SSH.
type Target struct {
	Addr     string // host or host:port
	User     string
	KeyFile  string // private key path; optional if Password is set
	Password string // optional
	KeyPEM   []byte // optional private key held in memory (a one-time administrator key)
	Agent    bool   // also offer the keys of the SSH agent in SSH_AUTH_SOCK
	Jump     *Target
	Timeout  time.Duration // per SSH connection attempt; default 15s
}

// Host is a live connection to one libvirt host.
type Host struct {
	Name string
	*libvirt.Libvirt
	ssh  *ssh.Client
	jump *ssh.Client
	priv Privilege // see privilege.go
	gate storGate  // see storgate.go

	stop     chan struct{} // closed by Close: ends the keepalives
	stopOnce sync.Once

	// A view made by Traced: root is the shared host it was made from, tr the trace its calls go to, under
	// the node name node. All three are empty on the shared host.
	root *Host
	tr   *optrace.Trace
	node string
}

// Traced returns a view of h that records in tr, under the node name node, the libvirt calls that change
// something and every command run on the node (see internal/optrace). The view shares h's connection; only
// the calls made through it are recorded, so the shared host the collectors use costs nothing extra. A nil tr
// returns h itself.
func (h *Host) Traced(tr *optrace.Trace, node string) *Host {
	if tr == nil || h == nil {
		return h
	}
	b := h.Base()
	v := &Host{Name: b.Name, ssh: b.ssh, jump: b.jump, root: b, tr: tr, node: node}
	if b.Libvirt != nil {
		v.Libvirt = b.Libvirt.WithCallHook(func(program, proc uint32, payload []byte) func(error) {
			return tr.Libvirt(node, program, proc, payload)
		})
	}
	return v
}

// Base is the shared host of a view made by Traced, or h itself. Use it where a host is a key (caches, maps):
// views of one host are different pointers.
func (h *Host) Base() *Host {
	if h != nil && h.root != nil {
		return h.root
	}
	return h
}

// Trace is the trace a view records in, or nil.
func (h *Host) Trace() *optrace.Trace { return h.tr }

// exec records a command run through a view; the function it returns takes the command's error.
func (h *Host) exec(cmd string) func(error) {
	if h.tr == nil {
		return func(error) {}
	}
	done := h.tr.Exec(h.node, cmd)
	if done == nil {
		return func(error) {}
	}
	return func(err error) { done(exitStatus(err), err) }
}

// exitStatus is a command's exit status: 0 without an error, -1 when it did not end on its own.
func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	var ee *ssh.ExitError
	if errors.As(err, &ee) {
		return ee.ExitStatus()
	}
	return -1
}

// SSH keepalives. A peer that vanishes without closing its TCP connection (a node that hangs, is frozen or loses
// power, a jump host that does) leaves every call on the connection waiting for ever: TCP itself only gives up
// after many minutes, or never when nothing is being sent. Every keepaliveInterval the controller asks each SSH
// server of the connection (the node, and the jump host) for an answer; one that has not answered after
// keepaliveTimeout ends the connection, which ends every call waiting on it (Disconnected() fires and the cluster
// reconnects). The libvirt watchdog (internal/cluster/watchdog.go) is the same check one layer up.
var (
	keepaliveInterval = 15 * time.Second
	keepaliveTimeout  = 30 * time.Second
)

// Dial opens the SSH connection and the libvirt session on top of it.
// knownHosts is a known_hosts file used trust-on-first-use. Every step has a
// time limit (see dialSSH; the libvirt handshake has twice the SSH timeout), so
// Dial always returns, also when the host stops answering halfway.
func Dial(t Target, knownHosts string) (*Host, error) {
	var jump *ssh.Client
	if t.Jump != nil {
		j, err := dialSSH(*t.Jump, knownHosts, nil)
		if err != nil {
			return nil, fmt.Errorf("jump host %s: %w", t.Jump.Addr, err)
		}
		jump = j
	}
	client, err := dialSSH(t, knownHosts, jump)
	if err != nil {
		if jump != nil {
			jump.Close()
		}
		return nil, fmt.Errorf("ssh %s: %w", t.Addr, err)
	}
	h := &Host{Name: t.Addr, ssh: client, jump: jump, stop: make(chan struct{})}
	h.Libvirt = libvirt.NewWithDialer(sockDialer{client})
	// The libvirt handshake has no time limit of its own: closing the SSH connection ends it.
	limit := 2 * dialTimeout(t)
	expired := time.AfterFunc(limit, h.Abort)
	err = h.Libvirt.Connect()
	if !expired.Stop() {
		if err == nil {
			h.Close()
		}
		return nil, fmt.Errorf("libvirt on %s: no answer within %s", t.Addr, limit)
	}
	if err != nil {
		h.Close()
		return nil, fmt.Errorf("libvirt on %s: %w", t.Addr, err)
	}
	// The first keepalive to fail says why; the other one then only finds the connection closed.
	var deadOnce sync.Once
	dead := func(format string, args ...any) {
		deadOnce.Do(func() {
			log.Printf(format, args...)
			h.Abort()
		})
	}
	go keepalive(func() error { return sendKeepalive(client) }, h.stop, keepaliveInterval, keepaliveTimeout, func(err error) {
		dead("node %s: %v; closing the connection", t.Addr, err)
	})
	if jump != nil {
		go keepalive(func() error { return sendKeepalive(jump) }, h.stop, keepaliveInterval, keepaliveTimeout, func(err error) {
			dead("node %s: jump host %s: %v; closing the connection", t.Addr, t.Jump.Addr, err)
		})
	}
	return h, nil
}

func sendKeepalive(c *ssh.Client) error {
	// OpenSSH answers an unknown global request with a failure, which is an answer.
	_, _, err := c.SendRequest("keepalive@openssh.com", true, nil)
	return err
}

// keepalive calls send every interval until stop is closed. send blocks until the peer answers; one that has
// not returned after timeout, or that fails (the connection is closed), calls dead once and ends the loop.
func keepalive(send func() error, stop <-chan struct{}, interval, timeout time.Duration, dead func(error)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		res := make(chan error, 1)
		go func() { res <- send() }()
		timer := time.NewTimer(timeout)
		select {
		case err := <-res:
			timer.Stop()
			if err != nil {
				select {
				case <-stop: // closed on purpose
				default:
					dead(fmt.Errorf("SSH connection lost: %w", err))
				}
				return
			}
		case <-timer.C:
			dead(fmt.Errorf("no SSH keepalive answer within %s", timeout))
			return
		case <-stop:
			timer.Stop()
			return
		}
	}
}

// Abort closes the SSH connection(s) at once, without the polite libvirt
// close that Close sends first. Every call waiting on the connection fails;
// Close still has to be called.
func (h *Host) Abort() {
	if h.root != nil {
		h.root.Abort()
		return
	}
	h.ssh.Close()
	if h.jump != nil {
		h.jump.Close()
	}
}

// Close tears down the libvirt session and the SSH connection(s).
func (h *Host) Close() {
	if h.root != nil {
		h.root.Close()
		return
	}
	if h.stop != nil {
		h.stopOnce.Do(func() { close(h.stop) })
	}
	if h.Libvirt != nil {
		// Disconnect sends a close request first, which blocks for ever on a
		// wedged connection (the case the cluster watchdog reconnects for),
		// so do not wait for it: the SSH close below ends the transport.
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.Libvirt.Disconnect()
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	h.ssh.Close()
	if h.jump != nil {
		h.jump.Close()
	}
}

// DialNet opens a connection from the host itself, for example to a VM's VNC
// server listening on the host's loopback ("tcp", "127.0.0.1:5900") or on a
// UNIX socket ("unix", path).
func (h *Host) DialNet(network, addr string) (net.Conn, error) {
	return h.ssh.Dial(network, addr)
}

// Run executes a shell command on the host and returns its combined output.
func (h *Host) Run(cmd string) (string, error) {
	s, err := h.ssh.NewSession()
	if err != nil {
		return "", err
	}
	defer s.Close()
	done := h.exec(cmd)
	// CombinedOutput serialises the two copy goroutines; sharing a plain
	// bytes.Buffer between Stdout and Stderr is a data race.
	out, err := s.CombinedOutput(cmd)
	done(err)
	return strings.TrimSpace(string(out)), err
}

// Stream runs a long-lived command on the host and calls line for every line
// it prints, until the command ends or ctx is cancelled. A terminal is
// requested so that closing the session hangs up the remote process instead
// of leaving it behind.
func (h *Host) Stream(ctx context.Context, cmd string, line func(string)) error {
	s, err := h.ssh.NewSession()
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.RequestPty("dumb", 24, 4096, ssh.TerminalModes{ssh.ECHO: 0}); err != nil {
		return err
	}
	out, err := s.StdoutPipe()
	if err != nil {
		return err
	}
	rec := h.exec(cmd)
	if err := s.Start(cmd); err != nil {
		rec(err)
		return err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			s.Close()
		case <-done:
		}
	}()
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		line(strings.TrimRight(sc.Text(), "\r"))
	}
	if ctx.Err() != nil {
		rec(ctx.Err())
		return nil
	}
	err = s.Wait()
	rec(err)
	return err
}

// tailBuf keeps the last few KiB written to it (a command's stderr).
type tailBuf struct {
	mu sync.Mutex
	b  []byte
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > 4096 {
		t.b = t.b[len(t.b)-4096:]
	}
	return len(p), nil
}

func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.b))
}

// Pipe runs cmd on the host with stdin fed from stdin and stdout copied to
// stdout (either may be nil), without buffering the data in memory. No
// terminal is requested, so binary data passes unchanged. Cancelling ctx
// closes the session, which ends the remote command. The command's stderr is
// returned as part of the error.
func (h *Host) Pipe(ctx context.Context, cmd string, stdin io.Reader, stdout io.Writer) error {
	s, err := h.ssh.NewSession()
	if err != nil {
		return err
	}
	defer s.Close()
	var errs tailBuf
	s.Stdin, s.Stdout, s.Stderr = stdin, stdout, &errs
	rec := h.exec(cmd)
	if err := s.Start(cmd); err != nil {
		rec(err)
		return err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			s.Close()
		case <-done:
		}
	}()
	if err := s.Wait(); err != nil {
		if ctx.Err() != nil {
			rec(ctx.Err())
			return ctx.Err()
		}
		rec(err)
		if msg := errs.String(); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	rec(nil)
	return nil
}

type sockDialer struct{ c *ssh.Client }

func (d sockDialer) Dial() (net.Conn, error) {
	var errs []error
	for _, p := range libvirtSockets {
		conn, err := d.c.Dial("unix", p)
		if err == nil {
			return conn, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", p, err))
	}
	return nil, errors.Join(errs...)
}

func dialTimeout(t Target) time.Duration {
	if t.Timeout > 0 {
		return t.Timeout
	}
	return 15 * time.Second
}

func withPort(addr string) string {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return net.JoinHostPort(addr, "22")
	}
	return addr
}

func dialSSH(t Target, knownHosts string, via *ssh.Client) (*ssh.Client, error) {
	var auth []ssh.AuthMethod
	if t.KeyFile != "" {
		pem, err := os.ReadFile(t.KeyFile)
		if err != nil {
			return nil, err
		}
		signer, err := ssh.ParsePrivateKey(pem)
		if err != nil {
			return nil, err
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if len(t.KeyPEM) > 0 {
		signer, err := ssh.ParsePrivateKey(t.KeyPEM)
		if err != nil {
			return nil, fmt.Errorf("private key: %w", err)
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if t.Agent {
		auth = append(auth, ssh.PublicKeysCallback(agentSigners))
	}
	if t.Password != "" {
		auth = append(auth, ssh.Password(t.Password))
	}
	cfg := &ssh.ClientConfig{
		User:            t.User,
		Auth:            auth,
		HostKeyCallback: tofu(knownHosts),
		Timeout:         dialTimeout(t),
	}
	addr := withPort(t.Addr)
	// Ask the server for the key type we already have on record; otherwise it
	// may present a different type and look like a changed key.
	cfg.HostKeyAlgorithms = knownKeyTypes(knownHosts, addr)
	conn, err := dialVia(via, addr, cfg.Timeout)
	if err != nil {
		return nil, err
	}
	// ssh.Dial and NewClientConn have no time limit for the handshake: a host
	// that accepts the TCP connection and then stops answering (it hangs, or it
	// was reset in the middle) would keep it waiting for ever.
	expired := time.AfterFunc(cfg.Timeout, func() { conn.Close() })
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if !expired.Stop() {
		if err == nil {
			c.Close()
		}
		conn.Close()
		return nil, fmt.Errorf("no SSH handshake within %s", cfg.Timeout)
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return ssh.NewClient(c, chans, reqs), nil
}

// dialVia opens the TCP connection to addr, directly or through the jump
// host's SSH connection, within timeout. A jump host waits for its own TCP
// connect to a host that does not answer for minutes; the open request is left
// to finish in the background (the caller closes the jump connection on error,
// which ends it) and a connection that arrives late is closed.
func dialVia(via *ssh.Client, addr string, timeout time.Duration) (net.Conn, error) {
	if via == nil {
		return net.DialTimeout("tcp", addr, timeout)
	}
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := via.Dial("tcp", addr)
		ch <- res{c, err}
	}()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case r := <-ch:
		return r.c, r.err
	case <-t.C:
		go func() {
			if r := <-ch; r.c != nil {
				r.c.Close()
			}
		}()
		return nil, fmt.Errorf("the jump host could not reach %s within %s", addr, timeout)
	}
}

// knownKeyTypes returns the host key algorithms recorded for addr, or nil
// (meaning the library defaults) when the host is not yet known.
func knownKeyTypes(path, addr string) []string {
	check, err := knownhosts.New(path)
	if err != nil {
		return nil
	}
	tcp, _ := net.ResolveTCPAddr("tcp", addr)
	if tcp == nil {
		tcp = &net.TCPAddr{}
	}
	var ke *knownhosts.KeyError
	if !errors.As(check(addr, tcp, probeKey{}), &ke) {
		return nil
	}
	var types []string
	for _, k := range ke.Want {
		t := k.Key.Type()
		if t == ssh.KeyAlgoRSA {
			types = append(types, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256)
		}
		types = append(types, t)
	}
	return types
}

// probeKey is a key that matches nothing, used to list a host's known keys.
type probeKey struct{}

func (probeKey) Type() string                        { return "probe" }
func (probeKey) Marshal() []byte                     { return nil }
func (probeKey) Verify([]byte, *ssh.Signature) error { return errors.New("probe key") }

var khMu sync.Mutex

// tofu accepts and records a host key the first time a host is seen and
// rejects any later change of that key.
func tofu(path string) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		khMu.Lock()
		defer khMu.Unlock()
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		check, err := knownhosts.New(path)
		if err != nil {
			return err
		}
		err = check(hostname, remote, key)
		var ke *knownhosts.KeyError
		if errors.As(err, &ke) && len(ke.Want) == 0 {
			_, werr := fmt.Fprintln(f, knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key))
			return werr
		}
		return err
	}
}
