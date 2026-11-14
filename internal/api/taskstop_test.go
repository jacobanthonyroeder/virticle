package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestStopperStop(t *testing.T) {
	st := newStopper(context.Background())
	var ran, taken, late atomic.Int32
	st.onStop(func() { ran.Add(1) })
	st.onStop(func() { ran.Add(1) })
	undo := st.onStop(func() { taken.Add(1) })
	undo() // a step that ended before the stop

	if st.Stopped() || st.Ctx().Err() != nil {
		t.Fatal("stopped before it was asked to")
	}
	st.stop()
	st.stop() // asking twice changes nothing
	waitUntil(t, "the undo functions", func() bool { return ran.Load() == 2 })
	if !st.Stopped() || st.Ctx().Err() == nil {
		t.Error("the flag and the context say it is not stopped")
	}
	time.Sleep(50 * time.Millisecond)
	if ran.Load() != 2 || taken.Load() != 0 {
		t.Errorf("undo ran %d times, the removed one %d times", ran.Load(), taken.Load())
	}

	// A step that starts after the stop is undone at once.
	st.onStop(func() { late.Add(1) })
	waitUntil(t, "the late undo", func() bool { return late.Load() == 1 })
	if st.commit() {
		t.Error("commit succeeded after the stop")
	}
}

func TestStopperCommit(t *testing.T) {
	st := newStopper(context.Background())
	var ran atomic.Int32
	st.onStop(func() { ran.Add(1) })
	if !st.commit() {
		t.Fatal("commit refused although nothing was stopped")
	}
	st.stop()
	time.Sleep(50 * time.Millisecond)
	if st.Stopped() || st.Ctx().Err() != nil || ran.Load() != 0 {
		t.Error("a stop after the point of no return took effect")
	}
	st.done()
	if st.Ctx().Err() == nil {
		t.Error("done does not release the context")
	}
}

// The parent context ends the task as well (a download whose browser went away), without it counting as a stop.
func TestStopperParent(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	st := newStopper(parent)
	cancel()
	if st.Ctx().Err() == nil {
		t.Error("the context outlives its parent")
	}
	if st.Stopped() {
		t.Error("a cancelled parent is not a stop request")
	}
}

func TestStopperCancelled(t *testing.T) {
	st := newStopper(context.Background())
	boom := errors.New("qemu-img convert failed (exit 143)")
	if got := st.cancelled("restore", boom); got != boom {
		t.Errorf("not stopped: %v", got)
	}
	if got := st.cancelled("restore", nil); got != nil {
		t.Errorf("no error: %v", got)
	}
	st.stop()
	got := st.cancelled("restore", boom)
	if got == nil || got.Error() != "restore cancelled" {
		t.Errorf("stopped: %v", got)
	}
	if st.cancelled("restore", nil) != nil {
		t.Error("a task that ended well was turned into a failure")
	}
}

func TestCopyStopped(t *testing.T) {
	if got := copyStopped("clone", errCopyStopped).Error(); got != "clone cancelled" {
		t.Errorf("plain: %q", got)
	}
	left := fmt.Errorf("%w: the partial volume x could not be removed: busy", errCopyStopped)
	got := copyStopped("clone", left).Error()
	if !strings.HasPrefix(got, "clone cancelled; ") || !strings.Contains(got, "could not be removed: busy") {
		t.Errorf("with a note: %q", got)
	}
}

// An upload that is stopped fails its next read, also when the read itself was cut short by the stop.
func TestCountingReaderStopped(t *testing.T) {
	var stopped atomic.Bool
	c := &countingReader{r: bytes.NewReader(make([]byte, 5<<20)), total: 5 << 20, tick: func(float64) {}, stopped: stopped.Load}
	buf := make([]byte, 1<<20)
	if n, err := c.Read(buf); n != 1<<20 || err != nil {
		t.Fatalf("first read: %d %v", n, err)
	}
	stopped.Store(true)
	if _, err := c.Read(buf); !errors.Is(err, errUploadStopped) {
		t.Errorf("after the stop: %v", err)
	}

	// A read that fails because the stop cut it short is reported as the stop.
	stopped.Store(false)
	c = &countingReader{r: failingReader{func() { stopped.Store(true) }}, tick: func(float64) {}, stopped: stopped.Load}
	if _, err := c.Read(buf); !errors.Is(err, errUploadStopped) {
		t.Errorf("cut short: %v", err)
	}
}

type failingReader struct{ onRead func() }

func (f failingReader) Read([]byte) (int, error) {
	f.onRead()
	return 0, errors.New("i/o timeout")
}
