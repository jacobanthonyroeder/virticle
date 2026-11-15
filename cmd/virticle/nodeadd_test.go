package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"virticle/internal/api"
	"virticle/internal/cluster"
	"virticle/internal/prepare"
	"virticle/internal/store"
)

func TestCheckAddArgs(t *testing.T) {
	for _, c := range []struct {
		name string
		q    addReq
		pos  []string
		euid int
		want string // "" or a part of the message
		host string
	}{
		{"a host", addReq{}, []string{"root@192.0.2.5"}, 1000, "", "root@192.0.2.5"},
		{"this machine as root", addReq{}, nil, 0, "", ""},
		{"this machine needs root", addReq{}, nil, 1000, "run as root: sudo virticle node add", ""},
		{"two hosts", addReq{}, []string{"a", "b"}, 0, "more than one host", ""},
		{"no host after @", addReq{}, []string{"root@"}, 0, "no host in", ""},
		{"a jump for this machine", addReq{jump: "a@b"}, nil, 0, "are for another host", ""},
		{"a password file for this machine", addReq{cred: credFlags{passwordFile: "f"}}, nil, 0, "are for another host", ""},
		{"a jump without user", addReq{jump: "b"}, []string{"h"}, 0, "--jump needs user@host", ""},
		{"root and a user", addReq{opts: prepare.Options{AllowRoot: true, User: "x"}}, []string{"h"}, 0, "root", ""},
	} {
		q := c.q
		got := checkAddArgs(&q, c.pos, c.euid)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: %q", c.name, got)
		}
		if c.want == "" && q.target != c.host {
			t.Errorf("%s: target %q", c.name, q.target)
		}
	}
}

func fakeCred(tty bool, env map[string]string, stdin string, typed ...string) (credIO, *int) {
	asked := 0
	return credIO{
		tty: tty, stdin: strings.NewReader(stdin), getenv: func(k string) string { return env[k] },
		readPassword: func(string) (string, error) {
			asked++
			if len(typed) == 0 {
				return "", nil
			}
			p := typed[0]
			typed = typed[1:]
			return p, nil
		},
	}, &asked
}

func TestAdminLoginScriptInputs(t *testing.T) {
	dir := t.TempDir()
	pwFile := filepath.Join(dir, "pw")
	os.WriteFile(pwFile, []byte("from-file\nsecond line\n"), 0o600)
	empty := filepath.Join(dir, "empty")
	os.WriteFile(empty, []byte("\n"), 0o600)

	in, asked := fakeCred(true, nil, "")
	if a, _, err := adminLogin("root", "h", "", credFlags{passwordFile: pwFile}, in); err != nil || a.Password != "from-file" || *asked != 0 {
		t.Errorf("file: %q %v asked %d", a.Password, err, *asked)
	}
	in, asked = fakeCred(false, map[string]string{nodePasswordEnv: "from-env"}, "")
	if a, _, err := adminLogin("root", "h", "", credFlags{}, in); err != nil || a.Password != "from-env" || *asked != 0 {
		t.Errorf("env: %q %v", a.Password, err)
	}
	in, _ = fakeCred(false, nil, "from-stdin\n")
	if a, _, err := adminLogin("root", "h", "", credFlags{pwStdin: true}, in); err != nil || a.Password != "from-stdin" {
		t.Errorf("stdin: %q %v", a.Password, err)
	}
	in, asked = fakeCred(true, nil, "", "typed")
	if a, _, err := adminLogin("ann", "h", "j@k", credFlags{}, in); err != nil || a.Password != "typed" || *asked != 1 || a.User != "ann" || a.Jump != "j@k" {
		t.Errorf("terminal: %+v %v", a, err)
	}
	// An agent: no question.
	in, asked = fakeCred(true, nil, "")
	in.agent = true
	if a, _, err := adminLogin("root", "h", "", credFlags{}, in); err != nil || a.Password != "" || *asked != 0 || !a.Agent {
		t.Errorf("agent: %+v %v", a, err)
	}
	// Nothing at all.
	in, _ = fakeCred(false, nil, "")
	if _, code, err := adminLogin("root", "h", "", credFlags{}, in); err == nil || code != 2 || !strings.Contains(err.Error(), "--password-file FILE or VIRTICLE_NODE_PASSWORD") {
		t.Errorf("none: %d %v", code, err)
	}
	for _, f := range []string{filepath.Join(dir, "missing"), empty} {
		in, _ = fakeCred(true, nil, "")
		if _, code, err := adminLogin("root", "h", "", credFlags{passwordFile: f}, in); err == nil || code != 1 || !strings.HasPrefix(err.Error(), "--password-file: ") {
			t.Errorf("bad file %s: %d %v", f, code, err)
		}
	}
}

func status(id int64, addr, user, state, priv, note string) cluster.NodeStatus {
	return cluster.NodeStatus{Node: store.Node{ID: id, Addr: addr, User: user}, State: state, DetectedPrivilege: priv, PrivilegeNote: note}
}

func TestVirticleStateAndFix(t *testing.T) {
	older := hostconn_older
	for _, c := range []struct {
		n           cluster.NodeStatus
		local       bool
		state, text string
	}{
		{status(1, "192.0.2.5", "virticle-node", "online", "helper", ""), false, vCurrent, "online"},
		{status(1, "192.0.2.5", "virticle-node", "online", "none", older), false, vUpdateNeeded, "Virticle update needed: sudo virticle node prepare root@192.0.2.5"},
		{status(1, "192.0.2.5", "root", "online", "none", older), true, vUpdateNeeded, "Virticle update needed: sudo virticle node prepare --root"},
		{status(1, "192.0.2.5", "ops", "online", "none", "no node command"), false, vNotPrepared, "not prepared: sudo virticle node prepare --user ops root@192.0.2.5"},
		{status(1, "192.0.2.5", "virticle-node", "offline", "", ""), false, vUnknown, "offline"},
	} {
		if s := virticleState(c.n); s != c.state {
			t.Errorf("%+v: state %q", c.n, s)
		}
		if s := stateText(c.n, c.local); s != c.text {
			t.Errorf("%+v: text %q", c.n, s)
		}
	}
	j := status(1, "10.0.0.5", "virticle-node", "online", "none", older)
	j.Jump = "admin@gw"
	if s := prepareCommand(j, false); s != "sudo virticle node prepare --jump admin@gw root@10.0.0.5" {
		t.Error(s)
	}
}

// The note the controller gives a node whose node command is from another version.
const hostconn_older = "virticle-node: the installed node command is version 3 and this controller ships 4: run \"virticle node prepare\" to update it"

func TestMatchNodesAndAlreadyListed(t *testing.T) {
	ns := []cluster.NodeStatus{status(1, "192.0.2.5", "virticle-node", "online", "helper", ""), status(2, "192.0.2.6:2222", "root", "offline", "", "")}
	ns[0].Hostname = "Box"
	for name, want := range map[string]int64{"box": 1, "192.0.2.5": 1, "root@192.0.2.6:2222": 2, "192.0.2.6": 2, "2": 2, "virticle-node@192.0.2.5": 1} {
		m := matchNodes(ns, name)
		if len(m) != 1 || m[0].ID != want {
			t.Errorf("%s: %+v", name, m)
		}
	}
	if m := matchNodes(ns, "other"); len(m) != 0 {
		t.Errorf("%+v", m)
	}
	if n, ok := alreadyListed(ns, "192.0.2.6", false); !ok || n.ID != 2 {
		t.Error("the same host on another port is the same node")
	}
	if _, ok := alreadyListed(ns, "192.0.2.7", false); ok {
		t.Error("another host")
	}
}

func TestAddCommandArgs(t *testing.T) {
	for _, c := range [][4]string{
		{"root@h", "", "virticle-node", "root@h"},
		{"h", "a@j", "root", "--root --jump a@j h"},
		{"ann@h", "", "ops", "--user ops ann@h"},
	} {
		if got := addCommandArgs(c[0], c[1], c[2]); got != c[3] {
			t.Errorf("%v: %q", c, got)
		}
	}
}

func TestPrintStatus(t *testing.T) {
	ok := statusFacts{version: "1.0", unit: true, active: true, answers: true, running: "1.0", web: "https://192.0.2.10:8463",
		admins: []string{"ann"}, nodes: []cluster.NodeStatus{status(1, "192.0.2.5", "virticle-node", "online", "helper", "")}}
	var b bytes.Buffer
	if code := printStatus(&b, ok); code != 0 {
		t.Errorf("all is well, but exit %d:\n%s", code, b.String())
	}
	for _, must := range []string{"Controller     1.0, service running", "Web            https://192.0.2.10:8463", "Administrator  ann", "Nodes          1", "192.0.2.5  online"} {
		if !strings.Contains(b.String(), must) {
			t.Errorf("lacks %q:\n%s", must, b.String())
		}
	}

	for _, c := range []struct {
		name string
		edit func(*statusFacts)
		want string
	}{
		{"stopped", func(f *statusFacts) { f.active, f.answers = false, false }, "Start it: sudo systemctl start virticle"},
		{"older running", func(f *statusFacts) { f.running = "0.9" }, "Restart it to run the installed version"},
		{"no admin", func(f *statusFacts) { f.admins = nil }, "Create it: sudo virticle setup, or open https://192.0.2.10:8463"},
		{"no nodes", func(f *statusFacts) { f.nodes = nil }, "Add one: sudo virticle node add [user@]host"},
		{"update needed", func(f *statusFacts) {
			f.nodes = []cluster.NodeStatus{status(1, "192.0.2.5", "virticle-node", "online", "none", hostconn_older)}
		}, "Fix: sudo virticle node prepare root@192.0.2.5"},
		{"unreachable", func(f *statusFacts) {
			n := status(1, "192.0.2.5", "virticle-node", "offline", "", "")
			n.Error = "dial tcp 192.0.2.5:22: connect: no route to host"
			f.nodes = []cluster.NodeStatus{n}
		}, "If it is gone for good: sudo virticle node remove 192.0.2.5"},
		{"key refused", func(f *statusFacts) {
			n := status(1, "192.0.2.5", "virticle-node", "offline", "", "")
			n.Error = "ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey]"
			f.nodes = []cluster.NodeStatus{n}
		}, "Fix: sudo virticle node prepare root@192.0.2.5"},
	} {
		f := ok
		c.edit(&f)
		b.Reset()
		if code := printStatus(&b, f); code != 1 || !strings.Contains(b.String(), c.want) {
			t.Errorf("%s: exit %d, lacks %q:\n%s", c.name, code, c.want, b.String())
		}
	}
	f := ok
	f.nodes = []cluster.NodeStatus{status(1, "192.0.2.5", "virticle-node", "online", "none", hostconn_older)}
	b.Reset()
	printStatus(&b, f)
	if !strings.Contains(b.String(), "online, Virticle update needed") || !strings.Contains(b.String(), "Virticle update needed\n") {
		t.Errorf("%s", b.String())
	}
}

// With the controller stopped the commands run the handlers in their own process and hold the data directory's lock;
// with it running they go through its socket, and the controller knows the node at once.
func TestCtlStoppedAndRunning(t *testing.T) {
	data := t.TempDir()
	c, err := openCtl(data, 0)
	if err != nil || c.running {
		t.Fatalf("stopped: %v %+v", err, c)
	}
	var n store.Node
	if err := c.call("POST", "/api/nodes", map[string]any{"addr": "127.0.0.1:1", "user": "virticle-node"}, &n); err != nil || n.ID == 0 {
		t.Fatalf("add: %v", err)
	}
	if err := c.call("POST", "/api/nodes", map[string]any{"addr": "127.0.0.1:1", "user": "root"}, nil); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("the same address twice: %v", err)
	}
	// A second command while this one holds the lock waits for a socket that never comes.
	if _, err := openCtl(data, 0); err == nil || !strings.Contains(err.Error(), "does not answer") {
		t.Errorf("locked: %v", err)
	}
	c.Close()

	// A controller on the same directory: what main does.
	dbPath, _ := store.DBPath(data, func(string, ...any) {})
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mgr := cluster.NewManager(filepath.Join(data, "id_ed25519"), filepath.Join(data, "known_hosts"))
	ns, _ := st.Nodes()
	for _, x := range ns {
		mgr.Add(x)
	}
	srv := &api.Server{Store: st, Nodes: mgr, PubKey: "k", DataDir: data}
	serveLocalSocket(srv, data)
	if fi, err := os.Stat(socketPath(data)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket: %v %v", err, fi)
	}
	c, err = openCtl(data, time.Second)
	if err != nil || !c.running {
		t.Fatalf("running: %v", err)
	}
	defer c.Close()
	list, err := c.nodes()
	if err != nil || len(list) != 1 || list[0].Addr != "127.0.0.1:1" {
		t.Fatalf("list %v %+v", err, list)
	}
	if err := c.call("POST", "/api/nodes", map[string]any{"addr": "127.0.0.1:2", "user": "virticle-node"}, &n); err != nil {
		t.Fatal(err)
	}
	if _, ok := mgr.Config(n.ID); !ok {
		t.Error("the running controller does not manage the node added through its socket")
	}
	if err := c.call("DELETE", "/api/nodes/"+itoa64(n.ID), nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := mgr.Config(n.ID); ok {
		t.Error("still managed after the remove")
	}
	if c.version() == "" {
		t.Error("no version")
	}
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }
