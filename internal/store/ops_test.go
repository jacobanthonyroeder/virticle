package store

import (
	"strings"
	"testing"
	"time"
)

func TestOpsFollowAuditAndTask(t *testing.T) {
	s := open(t)
	s.AddAudit(AuditEntry{Time: time.Now().Unix(), User: "u", IP: "i", Method: "POST", Path: "/no-ops", Status: 200})
	s.AddAudit(AuditEntry{Time: time.Now().Unix(), User: "u", IP: "i", Method: "POST", Path: "/snap", Status: 202, Trace: 42})
	task, _ := s.StartTask("snapshot", "web-1", "n", "u")
	s.SetTaskTrace(task.ID, 42)
	for _, d := range []string{`{"seq":1}`, `{"seq":2}`} {
		if err := s.AddOp(42, []byte(d)); err != nil {
			t.Fatal(err)
		}
	}
	e, _ := s.Audit(AuditFilter{})
	if len(e) != 2 || e[0].Ops != 2 || e[1].Ops != 0 {
		t.Fatalf("audit %+v", e)
	}
	tr, err := s.AuditTrace(e[0].ID)
	if err != nil || tr != 42 {
		t.Fatalf("trace %d %v", tr, err)
	}
	if tr, _ := s.AuditTrace(e[1].ID); tr != 0 {
		t.Errorf("entry without ops has trace %d", tr)
	}
	if _, err := s.AuditTrace(9999); err != ErrNotFound {
		t.Errorf("missing entry: %v", err)
	}
	ops, _ := s.Ops(42)
	if len(ops) != 2 || string(ops[1]) != `{"seq":2}` {
		t.Errorf("ops %s", ops)
	}
	if ops, _ := s.Ops(0); len(ops) != 0 {
		t.Errorf("trace 0 has ops")
	}
	tasks, _ := s.Tasks(10)
	if len(tasks) != 1 || tasks[0].Ops != 2 {
		t.Errorf("tasks %+v", tasks)
	}
	if tr, _ := s.TaskTrace(task.ID); tr != 42 {
		t.Errorf("task trace %d", tr)
	}
}

func TestOpsPrune(t *testing.T) {
	s := open(t)
	old := time.Now().Add(-auditMaxAge - time.Hour).Unix()
	s.db.Exec(`INSERT INTO ops(trace, time, size, data) VALUES(1, ?, 2, '{}')`, old)
	big := `{"x":"` + strings.Repeat("a", 1000) + `"}`
	for i := 0; i < 10; i++ {
		s.AddOp(2, []byte(big))
	}
	// The age limit, then the size limit: the newest rows that fit stay.
	if err := s.pruneOps(auditMaxAge, 3*int64(len(big))); err != nil {
		t.Fatal(err)
	}
	if o, _ := s.Ops(1); len(o) != 0 {
		t.Error("old op kept")
	}
	var n, maxID, minID int64
	s.db.QueryRow(`SELECT count(*), max(id), min(id) FROM ops`).Scan(&n, &maxID, &minID)
	if n != 3 || maxID-minID != 2 {
		t.Errorf("%d rows kept (ids %d..%d), want the newest 3", n, minID, maxID)
	}
	// Nothing to cut.
	if err := s.pruneOps(auditMaxAge, 1<<30); err != nil {
		t.Fatal(err)
	}
}
