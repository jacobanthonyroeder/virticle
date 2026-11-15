package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"virticle/internal/cluster"
	"virticle/internal/store"
)

func localServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Server{Store: st, Nodes: cluster.NewManager(filepath.Join(dir, "key"), filepath.Join(dir, "known_hosts")), PubKey: "ssh-ed25519 AAAA test"}
}

func localDo(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var b bytes.Buffer
	if body != nil {
		json.NewEncoder(&b).Encode(body)
	}
	r := httptest.NewRequest(method, path, &b)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// The command line adds, lists and removes nodes through the same handlers as the web UI, as the administrator, and
// the running controller's event stream hears of it.
func TestLocalHandlerNodes(t *testing.T) {
	s := localServer(t)
	h := s.LocalHandler()
	events, stop := s.Nodes.Subscribe()
	defer stop()

	w := localDo(t, h, "POST", "/api/nodes", map[string]any{"addr": "192.0.2.7", "user": "virticle-node"})
	if w.Code != 200 {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	var n store.Node
	json.Unmarshal(w.Body.Bytes(), &n)
	if e := <-events; e.Type != "node" || e.Node != n.ID || e.Detail != "added" {
		t.Errorf("event %+v", e)
	}
	if w := localDo(t, h, "POST", "/api/nodes", map[string]any{"addr": "192.0.2.7", "user": "root"}); w.Code != 400 {
		t.Errorf("the same address twice: %d %s", w.Code, w.Body)
	}
	w = localDo(t, h, "GET", "/api/nodes", nil)
	var l struct {
		Nodes     []cluster.NodeStatus
		PublicKey string
	}
	json.Unmarshal(w.Body.Bytes(), &l)
	if len(l.Nodes) != 1 || l.Nodes[0].Addr != "192.0.2.7" || l.PublicKey == "" {
		t.Fatalf("list: %s", w.Body)
	}
	if w := localDo(t, h, "DELETE", "/api/nodes/"+itoa(n.ID), nil); w.Code != 200 {
		t.Fatalf("remove: %d %s", w.Code, w.Body)
	}
	if ns, _ := s.Store.Nodes(); len(ns) != 0 {
		t.Errorf("still stored: %+v", ns)
	}
	audit, _ := s.Store.Audit(store.AuditFilter{Limit: 10})
	found := false
	for _, a := range audit {
		found = found || (a.User == LocalUser && a.Method == "POST" && a.Path == "/api/nodes")
	}
	if !found {
		t.Errorf("the add is not in the audit log as %q: %+v", LocalUser, audit)
	}
}

// Nothing but the few routes is served on the socket.
func TestLocalHandlerOnlyItsRoutes(t *testing.T) {
	h := localServer(t).LocalHandler()
	for _, p := range []string{"/api/users", "/api/signin", "/", "/api/nodes/1/vms"} {
		if w := localDo(t, h, "GET", p, nil); w.Code != 404 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
}

func TestLocalProblem(t *testing.T) {
	if p := fakeLocal(fakeLocalRoot(t, "vmx", "dev/kvm")).problem(); p != "" {
		t.Errorf("capable: %q", p)
	}
	if p := fakeLocal(fakeLocalRoot(t, "vmx")).problem(); p != "" {
		t.Errorf("no /dev/kvm is no problem, prepare loads the module: %q", p)
	}
	if p := fakeLocal(fakeLocalRoot(t, "")).problem(); p == "" {
		t.Error("a CPU without virtualisation is a problem")
	}
	if p := fakeLocal(fakeLocalRoot(t, "vmx", ".dockerenv", "dev/kvm")).problem(); p != "This is a container" {
		t.Errorf("container: %q", p)
	}
}
