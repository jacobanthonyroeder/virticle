package api

// stopper is what a long task shares with its stop function (POST /api/tasks/{id}/cancel). The task checks
// Stopped between its steps and passes Ctx to what can be interrupted; for work that runs on a node it
// registers an undo with onStop (kill the process, delete the work directory), which stop runs at once.
//
// Past the point of no return a task calls commit: the rest is short and finishing it is safer than
// breaking it off, so a later stop does nothing.

import (
	"context"
	"sync"
	"sync/atomic"
)

type stopper struct {
	ctx     context.Context
	cancel  context.CancelFunc
	stopped atomic.Bool

	mu        sync.Mutex
	committed bool
	next      int
	hooks     map[int]func()
}

func newStopper(parent context.Context) *stopper {
	ctx, cancel := context.WithCancel(parent)
	return &stopper{ctx: ctx, cancel: cancel, hooks: map[int]func(){}}
}

// Ctx is cancelled by stop.
func (s *stopper) Ctx() context.Context { return s.ctx }

// Stopped reports whether stop took effect.
func (s *stopper) Stopped() bool { return s.stopped.Load() }

// done releases the context; the task calls it when it ends.
func (s *stopper) done() { s.cancel() }

// stop asks the task to end: the flag is set, the context cancelled and every registered undo started (each in
// its own goroutine, since they talk to a node). It is the function of a taskStop.
func (s *stopper) stop() {
	s.mu.Lock()
	if s.committed || s.stopped.Load() {
		s.mu.Unlock()
		return
	}
	s.stopped.Store(true)
	hooks := make([]func(), 0, len(s.hooks))
	for _, fn := range s.hooks {
		hooks = append(hooks, fn)
	}
	s.mu.Unlock()
	s.cancel()
	for _, fn := range hooks {
		go fn()
	}
}

// onStop registers fn to run when the task is stopped, and returns the function that takes it back (call it
// when the step is over). When the task was stopped already, fn runs at once.
func (s *stopper) onStop(fn func()) (remove func()) {
	s.mu.Lock()
	if s.stopped.Load() {
		s.mu.Unlock()
		go fn()
		return func() {}
	}
	id := s.next
	s.next++
	s.hooks[id] = fn
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(s.hooks, id)
		s.mu.Unlock()
	}
}

// commit marks the point of no return. It returns false when the task was stopped before, and then the
// task must end as cancelled; after a true a stop request changes nothing.
func (s *stopper) commit() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped.Load() {
		return false
	}
	s.committed = true
	return true
}

// stoppedErr is the error of a task that was stopped. The task list shows its text.
func stoppedErr(what string) error { return stoppedError(what + " cancelled") }

// stoppedError marks the end of a task the user stopped. It is not a failure of the controller: fail answers
// it with 409 and does not log it as an internal error.
type stoppedError string

func (e stoppedError) Error() string { return string(e) }

// cancelled turns the error of a stopped task into the plain message: the real error is only the
// consequence of the stop (a killed process, a closed stream).
func (s *stopper) cancelled(what string, err error) error {
	if err == nil || !s.Stopped() {
		return err
	}
	return stoppedErr(what)
}
