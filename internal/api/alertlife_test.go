package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"virticle/internal/store"
)

// The lifecycle of a failed-job alert as the destinations see it: the tracker decides what is raised and cleared,
// the notifier sends it. These tests run both against the webhook stand-in.

func lifeNotifier(t *testing.T) (*notifier, *hookSink) {
	rc := newRecorder(t, false, nil)
	n, _ := testNotifier(t, notifyDest{ID: "1", Kind: "webhook", Name: "W", URL: rc.srv.URL, Severity: "warning", Cleared: true})
	return n, rc
}

// events reads what the sink got as "event id user" strings.
func events(rc *hookSink) []string {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	var out []string
	for _, b := range rc.bodies {
		var m webhookMsg
		json.Unmarshal([]byte(b), &m)
		out = append(out, m.Event+" "+m.Alert.ID+" "+m.Alert.User)
	}
	return out
}

func failedTask(id int64, user, status string) store.Task {
	return store.Task{ID: id, Kind: "backup", Target: "vm1", Node: "n1", User: user, Status: status, Error: "boom", Finished: 1700000000}
}

func lifePass(tr *alertTracker, n *notifier, jobs ...store.Task) {
	raised, cleared, _ := tr.step(alertInput{Jobs: jobs, Fresh: map[int64]bool{}}, time.Unix(1700000100, 0))
	n.dispatch(raised, cleared, "", nil)
}

func settle(rc *hookSink, want int, t *testing.T) []string {
	waitFor(t, "messages", func() bool { return rc.count() >= want })
	time.Sleep(40 * time.Millisecond)
	return events(rc)
}

// A task that a user started and that failed is sent once, naming the user and the task; the next pass sends
// nothing more; when the same action succeeds the alert is cleared once.
func TestUserFailureIsSentOnceAndCleared(t *testing.T) {
	n, rc := lifeNotifier(t)
	tr := newAlertTracker(alertConfig{})
	lifePass(tr, n, failedTask(1, "alice", "failed"))
	lifePass(tr, n, failedTask(1, "alice", "failed"))
	got := settle(rc, 1, t)
	if len(got) != 1 || got[0] != "raise job:backup:n1:vm1 alice" {
		t.Fatalf("after the failure: %v", got)
	}
	if !strings.Contains(rc.body(0), "alice") || !strings.Contains(rc.body(0), "Backup of vm1 failed") {
		t.Fatalf("message does not name user and task: %s", rc.body(0))
	}
	lifePass(tr, n, failedTask(1, "alice", "failed"), failedTask(2, "alice", "ok"))
	got = settle(rc, 2, t)
	if len(got) != 2 || got[1] != "clear job:backup:n1:vm1 alice" {
		t.Fatalf("after the success: %v", got)
	}
}

// A scheduler failure replaced by a user's failure of the same job: the destination was told of the first, is told
// of the change, and gets exactly one clear in the end.
func TestSchedulerAlertReplacedByUserIsClearedOnce(t *testing.T) {
	n, rc := lifeNotifier(t)
	tr := newAlertTracker(alertConfig{})
	lifePass(tr, n, failedTask(1, "scheduler", "failed"))
	lifePass(tr, n, failedTask(1, "scheduler", "failed"), failedTask(2, "bob", "failed"))
	lifePass(tr, n, failedTask(2, "bob", "failed"), failedTask(3, "bob", "ok"))
	got := settle(rc, 3, t)
	want := []string{"raise job:backup:n1:vm1 ", "raise job:backup:n1:vm1 bob", "clear job:backup:n1:vm1 bob"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}
