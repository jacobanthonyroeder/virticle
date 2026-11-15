package api

import (
	"testing"
	"time"

	"virticle/internal/store"
)

func TestUserTaskFailures(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	task := func(id int64, kind, target, user, status, errText string) store.Task {
		return store.Task{ID: id, Kind: kind, Target: target, Node: "n1", User: user, Status: status, Error: errText, Finished: now.Unix()}
	}
	cases := []struct {
		name    string
		jobs    []store.Task
		want    int    // alerts after the pass
		user    string // User of the alert, when one is expected
		title   string
		message string
	}{
		{"user failure raises", []store.Task{task(1, "move disk", "vm1/vda", "alice", "failed", "no space")}, 1, "alice", "Move disk of vm1/vda failed", "move disk on n1 by alice failed at 2026-11-15 10:00:00: no space"},
		{"scheduler failure has no user", []store.Task{task(1, "snapshot create", "vm1", "scheduler", "failed", "x")}, 1, "", "Scheduled snapshot create of vm1 failed", "snapshot create on n1 failed at 2026-11-15 10:00:00: x"},
		{"later success of the same job clears", []store.Task{task(1, "backup", "vm1", "alice", "failed", "x"), task(2, "backup", "vm1", "alice", "ok", "")}, 0, "", "", ""},
		{"a scheduler success clears a user failure", []store.Task{task(1, "backup", "vm1", "alice", "failed", "x"), task(2, "backup", "vm1", "scheduler", "ok", "")}, 0, "", "", ""},
		{"a user failure after a scheduler success raises", []store.Task{task(1, "backup", "vm1", "scheduler", "ok", ""), task(2, "backup", "vm1", "bob", "failed", "x")}, 1, "bob", "Backup of vm1 failed", ""},
		{"success of another kind or target does not clear", []store.Task{task(1, "backup", "vm1", "alice", "failed", "x"), task(2, "restore", "vm1", "alice", "ok", ""), task(3, "backup", "vm2", "alice", "ok", "")}, 1, "alice", "Backup of vm1 failed", ""},
		{"a stop by the user does not raise", []store.Task{task(1, "migrate", "vm1 → n2", "alice", "failed", "migration cancelled")}, 0, "", "", ""},
		{"a stop does not hide the failure before it", []store.Task{task(1, "migrate", "vm1 → n2", "alice", "failed", "boom"), task(2, "migrate", "vm1 → n2", "alice", "failed", "migration cancelled: x")}, 1, "alice", "Migrate of vm1 failed", ""},
		{"the clock sync after a start is not worth an alert", []store.Task{task(1, "sync guest clock", "vm1", "alice", "failed", "after start: no agent")}, 0, "", "", ""},
		{"the rolling update has its own alert", []store.Task{task(1, "rolling-update", "all", "alice", "failed", "x")}, 0, "", "", ""},
		{"a replication keeps its title", []store.Task{task(1, "replicate", "vm1 → n2", "alice", "failed", "x")}, 1, "alice", "Replication of vm1 failed", ""},
		{"no user name is not the scheduler", []store.Task{task(1, "backup", "vm1", "", "failed", "x")}, 1, "unknown", "Backup of vm1 failed", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := newAlertTracker(alertConfig{})
			raised, _, all := tr.step(alertInput{Jobs: c.jobs}, now)
			if len(all) != c.want || len(raised) != c.want {
				t.Fatalf("alerts %+v raised %+v, want %d", all, raised, c.want)
			}
			if c.want == 0 {
				return
			}
			a := all[0]
			if a.Kind != "failed-job" || a.Severity != "warning" || a.Link != "/tasks" || a.User != c.user {
				t.Errorf("alert %+v, want user %q", a, c.user)
			}
			if a.Title != c.title {
				t.Errorf("title %q, want %q", a.Title, c.title)
			}
			if c.message != "" && a.Message != c.message {
				t.Errorf("message %q, want %q", a.Message, c.message)
			}
		})
	}
}

// A user failure and a scheduler failure of the same job are one alert: the newest run decides who is shown, and the
// change is raised again so the web UI sees it.
func TestJobAlertFollowsTheNewestRun(t *testing.T) {
	now := time.Now()
	tr := newAlertTracker(alertConfig{})
	user := store.Task{ID: 1, Kind: "backup", Target: "vm1", Node: "n1", User: "alice", Status: "failed", Error: "x", Finished: now.Unix()}
	sched := store.Task{ID: 2, Kind: "backup", Target: "vm1", Node: "n1", User: "scheduler", Status: "failed", Error: "y", Finished: now.Unix()}
	if r, _, _ := tr.step(alertInput{Jobs: []store.Task{user}}, now); len(r) != 1 || r[0].User != "alice" {
		t.Fatalf("raised %+v", r)
	}
	if r, c, _ := tr.step(alertInput{Jobs: []store.Task{user}}, now); len(r)+len(c) != 0 {
		t.Fatalf("no change, no event: %+v %+v", r, c)
	}
	r, c, all := tr.step(alertInput{Jobs: []store.Task{user, sched}}, now)
	if len(c) != 0 || len(r) != 1 || r[0].User != "" || len(all) != 1 || all[0].ID != r[0].ID {
		t.Fatalf("raised %+v cleared %+v all %+v", r, c, all)
	}
}

func TestAlertPokeDelay(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		last time.Time
		want time.Duration
	}{
		{time.Time{}, 0},
		{now.Add(-time.Minute), 0},
		{now.Add(-2 * time.Second), 0},
		{now.Add(-500 * time.Millisecond), 1500 * time.Millisecond},
		{now, alertMinGap},
	} {
		if got := alertPokeDelay(c.last, now); got != c.want {
			t.Errorf("last %v ago: delay %v, want %v", now.Sub(c.last), got, c.want)
		}
	}
	// A poke never blocks, with or without a loop that picks it up.
	var s Server
	for i := 0; i < 3; i++ {
		s.pokeAlerts()
	}
	select {
	case <-alertPoke:
	default:
		t.Fatal("the poke was lost")
	}
}
