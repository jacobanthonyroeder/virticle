package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"virticle/internal/store"
)

func ver(v uint64, err error) func() (uint64, error) {
	return func() (uint64, error) { return v, err }
}

func TestSupportedNode(t *testing.T) {
	// libvirt 11.3.0 / QEMU 10.0.2 (Debian 13) and libvirt 11.10.0 / QEMU 10.1.0 (Rocky 10)
	if err := supportedNode(ver(11003000, nil), ver(10000002, nil)); err != nil {
		t.Errorf("Debian 13: %v", err)
	}
	if err := supportedNode(ver(11010000, nil), ver(10001000, nil)); err != nil {
		t.Errorf("Rocky 10: %v", err)
	}
	// Ubuntu 24.04: libvirt 10.0.0 / QEMU 8.2.2
	err := supportedNode(ver(10000000, nil), ver(8002002, nil))
	want := "This node has libvirt 10.0.0 and QEMU 8.2.2. Virticle needs libvirt 11.0 and QEMU 10.0 or newer."
	if err == nil || err.Error() != want {
		t.Errorf("Ubuntu 24.04: %v", err)
	}
	// a libvirt that does not answer is not a version problem
	bad := errors.New("no answer")
	if err := supportedNode(ver(0, bad), ver(8002002, nil)); err != nil {
		t.Errorf("libvirt error: %v", err)
	}
	if err := supportedNode(ver(10000000, nil), ver(0, bad)); err != nil {
		t.Errorf("hypervisor error: %v", err)
	}
}

// An unsupported node is shown offline with the sentence, is announced, and is
// left alone for versionRecheck (here: until the connection goes or the context ends).
func TestUnsupportedNodeShowsOfflineAndWaits(t *testing.T) {
	m := &Manager{nodes: map[int64]*node{}, subs: map[chan Event]struct{}{}}
	n := &node{cfg: store.Node{ID: 7, Addr: "192.0.2.7"}}
	m.nodes[7] = n
	events, stop := m.Subscribe()
	defer stop()
	err := supportedNode(ver(10000000, nil), ver(8002002, nil))

	gone := make(chan struct{})
	done := make(chan struct{})
	go func() { m.holdUnsupported(context.Background(), n, err, gone); close(done) }()
	select {
	case e := <-events:
		if e.Type != "node" || e.Node != 7 || e.Detail != "offline" {
			t.Errorf("event %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
	}
	select {
	case <-done:
		t.Fatal("did not wait")
	case <-time.After(50 * time.Millisecond):
	}
	st := m.Status()
	if len(st) != 1 || st[0].State != "offline" || !strings.Contains(st[0].Error, "libvirt 10.0.0 and QEMU 8.2.2") {
		t.Errorf("status %+v", st)
	}
	if _, herr := m.Host(7); !errors.Is(herr, ErrOffline) || !strings.Contains(herr.Error(), "Virticle needs libvirt 11.0") {
		t.Errorf("Host: %v", herr)
	}
	close(gone)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("did not return when the connection went")
	}
}

func TestUnsupportedNodeIsCheckedAgainAfterTheRecheckTime(t *testing.T) {
	old := versionRecheck
	versionRecheck = 20 * time.Millisecond
	defer func() { versionRecheck = old }()
	m := &Manager{nodes: map[int64]*node{}, subs: map[chan Event]struct{}{}}
	n := &node{cfg: store.Node{ID: 1, Addr: "192.0.2.1"}}
	start := time.Now()
	m.holdUnsupported(context.Background(), n, errors.New("too old"), make(chan struct{}))
	if d := time.Since(start); d < 15*time.Millisecond || d > 2*time.Second {
		t.Errorf("waited %v", d)
	}
	if versionRecheck != 20*time.Millisecond || old < time.Minute {
		t.Errorf("the re-check time must be slow: %v", old)
	}
}
