package api

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"virticle/internal/store"
)

func openAlertStore(t *testing.T) *store.Store {
	st, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// The alerts survive a restart: the new tracker starts from what the old one knew, so a pass that finds the same
// alerts sends nothing, and one that finds an alert gone sends its clear.
func TestAlertsSurviveRestart(t *testing.T) {
	st := openAlertStore(t)
	n, rc := lifeNotifier(t)
	tr := newAlertTracker(alertConfig{})
	lifePass(tr, n, failedTask(1, "alice", "failed"))
	var saved string
	saveAlertState(st, tr, &saved)
	settle(rc, 1, t)

	tr2 := newAlertTracker(alertConfig{})
	restoreAlertState(st, tr2, time.Unix(1700000100, 0))
	lifePass(tr2, n, failedTask(1, "alice", "failed"))
	time.Sleep(40 * time.Millisecond)
	if rc.count() != 1 {
		t.Fatalf("a restart sent again: %v", events(rc))
	}
	// Cleared while the controller was down: the next controller says so.
	tr3 := newAlertTracker(alertConfig{})
	restoreAlertState(st, tr3, time.Unix(1700000100, 0))
	lifePass(tr3, n, failedTask(1, "alice", "failed"), failedTask(2, "alice", "ok"))
	got := settle(rc, 2, t)
	if got[1] != "clear job:backup:n1:vm1 alice" {
		t.Fatalf("got %v", got)
	}
}

// An unchanged state is not written again.
func TestAlertStateWrittenOnlyOnChange(t *testing.T) {
	st := openAlertStore(t)
	tr := newAlertTracker(alertConfig{})
	var saved string
	saveAlertState(st, tr, &saved)
	first := saved
	if err := st.DeleteWebSetting(alertStateKey); err != nil {
		t.Fatal(err)
	}
	saveAlertState(st, tr, &saved) // same text: no write
	if _, ok, _ := st.WebSetting(alertStateKey); ok || saved != first {
		t.Fatal("wrote an unchanged state")
	}
}

// An offline node's alert is kept across a restart without being cleared and raised again.
func TestNodeDownSurvivesRestart(t *testing.T) {
	st := openAlertStore(t)
	now := time.Unix(1700000000, 0)
	in := alertInput{Nodes: []alertNode{{ID: 1, Name: "n1"}}, Fresh: map[int64]bool{}}
	tr := newAlertTracker(alertConfig{})
	tr.step(in, now.Add(-10*time.Minute))
	raised, _, _ := tr.step(in, now)
	if len(raised) != 1 {
		t.Fatalf("raised %v", raised)
	}
	saveAlertState(st, tr, nil)
	tr2 := newAlertTracker(alertConfig{})
	restoreAlertState(st, tr2, now)
	raised, cleared, _ := tr2.step(in, now.Add(time.Minute))
	if len(raised)+len(cleared) != 0 {
		t.Fatalf("raised %v cleared %v", raised, cleared)
	}
}

// The record of a guest that may be frozen survives a restart.
func TestFrozenRecordSurvivesRestart(t *testing.T) {
	st := openAlertStore(t)
	noteFrozen("ag35-host", "ag35-uuid", "web", errors.New("no agent"))
	defer forgetFrozen("ag35-host", "ag35-uuid")
	saveAlertState(st, newAlertTracker(alertConfig{}), nil)
	forgetFrozen("ag35-host", "ag35-uuid") // the restart
	restoreAlertState(st, newAlertTracker(alertConfig{}), time.Now())
	got := frozenOn("ag35-host")
	if len(got) != 1 || got[0].Name != "web" || got[0].Err == "" {
		t.Fatalf("restored %v", got)
	}
}
