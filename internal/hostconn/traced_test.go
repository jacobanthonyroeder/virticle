package hostconn

import (
	"testing"

	"virticle/internal/optrace"
)

func TestTracedViewSharesTheHost(t *testing.T) {
	h := &Host{Name: "10.0.0.5"}
	if h.Traced(nil, "n") != h {
		t.Error("no trace should give the host itself")
	}
	tr := optrace.New(nil)
	v := h.Traced(tr, "node-a")
	if v == h || v.Base() != h || h.Base() != h || v.Trace() != tr || h.Trace() != nil {
		t.Fatalf("view %+v", v)
	}
	// A view of a view is a view of the host.
	if w := v.Traced(optrace.New(nil), "node-a"); w.Base() != h {
		t.Error("nested view lost the host")
	}
	v.SetPrivilege(PrivRoot)
	if h.Privilege() != PrivRoot || !v.CanRoot() {
		t.Error("privilege not shared")
	}
	var nilHost *Host
	if nilHost.Base() != nil || nilHost.Traced(tr, "x") != nil {
		t.Error("nil host")
	}
	if exitStatus(nil) != 0 {
		t.Error("exit status of no error")
	}
}
