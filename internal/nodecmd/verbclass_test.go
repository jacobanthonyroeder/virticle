package nodecmd

import (
	"slices"
	"testing"

	"virticle/internal/nodeproto"
)

// Every verb is either read-only or changing, in internal/nodeproto/verbs.go, so that a new verb cannot be
// forgotten: the operation trace leaves out the read-only ones.
func TestVerbsClassified(t *testing.T) {
	ro, ch := map[string]bool{}, map[string]bool{}
	for _, v := range nodeproto.ReadOnlyVerbs {
		if ro[v] {
			t.Errorf("%s is listed twice as read-only", v)
		}
		ro[v] = true
	}
	for _, v := range nodeproto.ChangingVerbs {
		if ch[v] {
			t.Errorf("%s is listed twice as changing", v)
		}
		if ro[v] {
			t.Errorf("%s is listed as read-only and as changing", v)
		}
		ch[v] = true
	}
	reg := Verbs()
	for _, v := range reg {
		if v == "zz-test-block" { // registered by the tests
			continue
		}
		if !ro[v] && !ch[v] {
			t.Errorf("verb %q is in neither list of internal/nodeproto/verbs.go: add it to ReadOnlyVerbs (it changes nothing on the node) or to ChangingVerbs", v)
		}
	}
	for _, v := range nodeproto.ClassifiedVerbs() {
		if !slices.Contains(reg, v) {
			t.Errorf("internal/nodeproto/verbs.go lists %q, which is not a verb", v)
		}
	}
}
