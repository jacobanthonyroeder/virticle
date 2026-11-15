package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"virticle/internal/optrace"
	"virticle/internal/store"
)

// A request that changes something carries a trace; its task shares it, and both the audit entry and the task
// lead to the operations. Reads carry none.
func TestTraceFollowsRequestAndTask(t *testing.T) {
	e := newEnv(t)
	e.user("root", admin)
	e.user("view", viewer)
	var sawGET bool
	h := e.srv.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		infoOf(r).user = "root"
		if r.Method == http.MethodGet {
			sawGET = optrace.From(r.Context()) != nil
			return
		}
		err := e.srv.task(r, "test", "web-1", "n", func(func(float64)) error {
			optrace.From(r.Context()).Exec("node-a", "virt-customize -a /x.qcow2 --root-password password:hunter2")(0, nil)
			return nil
		})
		if err != nil {
			t.Error(err)
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/x", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/vnc", nil))
	if sawGET {
		t.Error("a GET has a trace")
	}

	entries, _ := e.srv.Store.Audit(store.AuditFilter{})
	if len(entries) != 1 || entries[0].Ops != 1 {
		t.Fatalf("audit %+v", entries)
	}
	tasks, _ := e.srv.Store.Tasks(10)
	if len(tasks) != 1 || tasks[0].Ops != 1 {
		t.Fatalf("tasks %+v", tasks)
	}
	for _, path := range []string{"/api/audit/" + itoa(entries[0].ID) + "/ops", "/api/tasks/" + itoa(tasks[0].ID) + "/ops"} {
		w := e.do("GET", path, nil, cookieOf(e, "root"))
		var res struct{ Ops []optrace.Op }
		decode(t, w, &res)
		if w.Code != 200 || len(res.Ops) != 1 || res.Ops[0].API != "virt-customize" || res.Ops[0].Node != "node-a" {
			t.Errorf("%s: %d %s", path, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "hunter2") {
			t.Errorf("%s: secret in the answer", path)
		}
		if w := e.do("GET", path, nil, cookieOf(e, "view")); w.Code != http.StatusForbidden {
			t.Errorf("%s for a viewer: %d", path, w.Code)
		}
	}
	if w := e.do("GET", "/api/audit/99999/ops", nil, cookieOf(e, "root")); w.Code != http.StatusNotFound {
		t.Errorf("missing entry: %d", w.Code)
	}
}
