package api

import (
	"strings"
	"testing"
	"time"
)

// An alert about a task that a person started was shown to that person in the web UI: destinations get the alerts
// without a user only, raised or cleared.
func TestAlertsOfUserTasksAreNotSent(t *testing.T) {
	rc := newRecorder(t, false, nil)
	n, _ := testNotifier(t, notifyDest{ID: "1", Kind: "webhook", Name: "W", URL: rc.srv.URL, Severity: "warning", Cleared: true})
	byUser := Alert{ID: "job:backup:n1:vm1", Kind: "failed-job", Severity: "warning", Title: "Backup of vm1 failed", User: "alice"}
	bySched := Alert{ID: "job:snapshot create:n1:vm2", Kind: "failed-job", Severity: "warning", Title: "Scheduled snapshot create of vm2 failed"}
	n.dispatch([]Alert{byUser, bySched}, []Alert{byUser}, "", nil)
	waitFor(t, "the scheduler alert", func() bool { return rc.count() == 1 })
	time.Sleep(30 * time.Millisecond)
	if rc.count() != 1 || !strings.Contains(rc.bodies[0], "vm2") || strings.Contains(rc.bodies[0], "alice") {
		t.Fatalf("sent %d: %v", rc.count(), rc.bodies)
	}
	// Only user alerts: nothing at all.
	n.dispatch([]Alert{byUser}, []Alert{byUser}, "", nil)
	time.Sleep(30 * time.Millisecond)
	if rc.count() != 1 {
		t.Fatalf("user alerts were sent: %v", rc.bodies)
	}
}
