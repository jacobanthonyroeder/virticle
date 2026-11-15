package api

import (
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeFrozenView struct {
	exists, running bool
	stateErr        error
	st              string
	stErr, thawErr  error
	thawed          int
}

func (f *fakeFrozenView) state() (bool, bool, error) { return f.exists, f.running, f.stateErr }
func (f *fakeFrozenView) status() (string, error)    { return f.st, f.stErr }
func (f *fakeFrozenView) thaw() error {
	f.thawed++
	return f.thawErr
}

func TestFrozenStill(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name      string
		v         fakeFrozenView
		busy      bool
		still     bool
		wantThaws int
	}{
		{"a backup of the VM runs", fakeFrozenView{exists: true, running: true, st: "frozen"}, true, true, 0},
		{"node does not answer", fakeFrozenView{stateErr: boom}, false, true, 0},
		{"VM gone", fakeFrozenView{}, false, false, 0},
		{"VM shut off", fakeFrozenView{exists: true}, false, false, 0},
		{"agent does not answer", fakeFrozenView{exists: true, running: true, stErr: boom}, false, true, 0},
		{"agent says thawed", fakeFrozenView{exists: true, running: true, st: "thawed"}, false, false, 0},
		{"frozen, thaw works", fakeFrozenView{exists: true, running: true, st: "frozen"}, false, false, 1},
		{"frozen, thaw fails", fakeFrozenView{exists: true, running: true, st: "frozen", thawErr: boom}, false, true, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := c.v
			still, why := frozenStill(&v, c.busy)
			if still != c.still || v.thawed != c.wantThaws {
				t.Fatalf("still %v (%q), thaws %d; want %v, %d", still, why, v.thawed, c.still, c.wantThaws)
			}
			if !still && why == "" {
				t.Fatal("a cleared record needs a reason for the log")
			}
		})
	}
}

func TestFrozenRecords(t *testing.T) {
	frozenGuests.Lock()
	saved := frozenGuests.m
	frozenGuests.m = map[string]frozenGuest{}
	frozenGuests.Unlock()
	defer func() {
		frozenGuests.Lock()
		frozenGuests.m = saved
		frozenGuests.Unlock()
	}()
	noteFrozen("n1", "u1", "web", errors.New("first"))
	time.Sleep(time.Millisecond)
	noteFrozen("n1", "u2", "db", errors.New("x"))
	noteFrozen("n2", "u3", "other", errors.New("x"))
	first := frozenOn("n1")[0].At
	noteFrozen("n1", "u1", "web", errors.New("second\nline two")) // again: keeps its time, takes the new error
	got := frozenOn("n1")
	if len(got) != 2 || got[0].UUID != "u1" || got[1].UUID != "u2" {
		t.Fatalf("frozenOn(n1) = %+v", got)
	}
	if got[0].Err != "second" || !got[0].At.Equal(first) {
		t.Fatalf("record not updated in place: %+v", got[0])
	}
	forgetFrozen("n1", "u1")
	if got := frozenOn("n1"); len(got) != 1 || got[0].UUID != "u2" {
		t.Fatalf("after forget: %+v", got)
	}
}

func TestLateThaw(t *testing.T) {
	thawPause = time.Millisecond
	frozenGuests.Lock()
	saved := frozenGuests.m
	frozenGuests.m = map[string]frozenGuest{}
	frozenGuests.Unlock()
	defer func() {
		frozenGuests.Lock()
		frozenGuests.m = saved
		frozenGuests.Unlock()
	}()
	boom := errors.New("the guest agent is not connected")
	ok := &fakeFreezer{thawErrs: []error{boom, nil}}
	lateThaw(ok, "n1", "u1", "web")
	if got := strings.Join(ok.events, ","); got != "thaw,thaw" || len(frozenOn("n1")) != 0 {
		t.Fatalf("a thaw that works on the second try: events %s, records %+v", got, frozenOn("n1"))
	}
	bad := &fakeFreezer{thawErrs: []error{boom}}
	lateThaw(bad, "n1", "u1", "web")
	if n := strings.Count(strings.Join(bad.events, ","), "thaw"); n != thawTries {
		t.Fatalf("%d thaw tries, want %d", n, thawTries)
	}
	if got := frozenOn("n1"); len(got) != 1 || got[0].Name != "web" || got[0].Err != boom.Error() {
		t.Fatalf("records %+v", got)
	}
}

func TestFrozenAlert(t *testing.T) {
	tr := newAlertTracker(alertConfig{})
	now := time.Unix(1700000000, 0)
	at := now.Add(-time.Minute)
	in := alertInput{Fresh: map[int64]bool{3: true}, Frozen: []alertFrozen{{Node: 3, NodeName: "kvm3", UUID: "u1", Name: "web", Err: "the guest agent is not connected", At: at}}}
	raised, _, _ := tr.step(in, now)
	if len(raised) != 1 {
		t.Fatalf("raised %+v", raised)
	}
	a := raised[0]
	if a.Kind != "guest-frozen" || a.Severity != "critical" || a.Node != 3 || a.Link != "/vm/3/u1" || a.Title != "web may be frozen" {
		t.Fatalf("alert %+v", a)
	}
	for _, want := range []string{"web on kvm3", "the guest agent is not connected", "virsh domfsthaw web", "clears"} {
		if !strings.Contains(a.Message, want) {
			t.Errorf("message %q lacks %q", a.Message, want)
		}
	}
	// The record went (the guest was seen thawed): the alert clears.
	_, cleared, _ := tr.step(alertInput{Fresh: map[int64]bool{3: true}}, now.Add(time.Minute))
	if len(cleared) != 1 || cleared[0].ID != a.ID {
		t.Fatalf("cleared %+v", cleared)
	}
}
