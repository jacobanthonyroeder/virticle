package api

import (
	"fmt"
	"net/http"
	"testing"

	"virticle/internal/store"
)

// A running task that registered a way to stop says so in the task list, and POST /api/tasks/{id}/cancel
// reaches it for a caller with the role it asks for. A finished task can no longer be stopped.
func TestTaskCancel(t *testing.T) {
	e := newEnv(t)
	e.user("root", admin)
	e.user("op", operator)
	e.user("view", viewer)

	start := func(level string) (id int64, stopped <-chan struct{}, done <-chan error) {
		ch := make(chan struct{})
		stop := taskStop{level, func() { close(ch) }}
		id, done, err := e.srv.startBackgroundStop(userRequest("root"), "backup", "vm1", "n1", stop, func(func(float64)) error {
			<-ch
			return fmt.Errorf("cancelled")
		})
		if err != nil {
			t.Fatal(err)
		}
		return id, ch, done
	}
	listed := func(id int64) store.Task {
		var list []store.Task
		decode(t, e.do("GET", "/api/tasks", nil, cookieOf(e, "view")), &list)
		for _, x := range list {
			if x.ID == id {
				return x
			}
		}
		t.Fatalf("task %d is not listed", id)
		return store.Task{}
	}
	cancel := func(id int64, user string) int {
		return e.do("POST", fmt.Sprintf("/api/tasks/%d/cancel", id), nil, cookieOf(e, user)).Code
	}

	id, stopped, done := start(operator)
	if got := listed(id); got.Cancel != operator || got.Status != "running" {
		t.Fatalf("running task: cancel=%q status=%q", got.Cancel, got.Status)
	}
	if c := cancel(id, "view"); c != http.StatusForbidden {
		t.Errorf("viewer: %d", c)
	}
	if c := cancel(id, "op"); c != http.StatusOK {
		t.Fatalf("operator: %d", c)
	}
	<-stopped
	<-done
	if got := listed(id); got.Cancel != "" || got.Status != "failed" || got.Error != "cancelled" {
		t.Errorf("stopped task: %+v", got)
	}
	if c := cancel(id, "op"); c != http.StatusBadRequest {
		t.Errorf("finished task: %d", c)
	}

	// A task of an admin action is not stopped by an operator.
	id, stopped, done = start(admin)
	if c := cancel(id, "op"); c != http.StatusForbidden {
		t.Errorf("operator on an admin task: %d", c)
	}
	if c := cancel(id, "root"); c != http.StatusOK {
		t.Fatalf("admin: %d", c)
	}
	<-stopped
	<-done

	// A task without a way to stop.
	plain, pdone, err := e.srv.startBackgroundWait(userRequest("root"), "start", "vm1", "n1", func(func(float64)) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	<-pdone
	if c := cancel(plain, "root"); c != http.StatusBadRequest {
		t.Errorf("plain task: %d", c)
	}
}
