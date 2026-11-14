package hostconn

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestKeepaliveHealthyPeerStaysUp(t *testing.T) {
	stop := make(chan struct{})
	var sent atomic.Int32
	dead := make(chan error, 1)
	go keepalive(func() error { sent.Add(1); return nil }, stop, 5*time.Millisecond, 100*time.Millisecond, func(err error) { dead <- err })
	time.Sleep(100 * time.Millisecond)
	close(stop)
	select {
	case err := <-dead:
		t.Fatalf("a peer that answers was declared dead: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if sent.Load() < 3 {
		t.Fatalf("only %d keepalives", sent.Load())
	}
}

func TestKeepaliveSilentPeerIsDead(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	dead := make(chan error, 1)
	start := time.Now()
	go keepalive(func() error { <-block; return nil }, make(chan struct{}), 5*time.Millisecond, 50*time.Millisecond, func(err error) { dead <- err })
	select {
	case <-dead:
		if d := time.Since(start); d < 50*time.Millisecond {
			t.Fatalf("declared dead after %v, before the timeout", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a peer that never answers was not declared dead")
	}
}

func TestKeepaliveClosedConnectionIsDeadUnlessStopped(t *testing.T) {
	dead := make(chan error, 1)
	go keepalive(func() error { return errors.New("closed") }, make(chan struct{}), 5*time.Millisecond, time.Second, func(err error) { dead <- err })
	select {
	case <-dead:
	case <-time.After(2 * time.Second):
		t.Fatal("a failing keepalive was not reported")
	}
	// Closed on purpose (Close ran): not reported.
	stop := make(chan struct{})
	quiet := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		keepalive(func() error { close(stop); return errors.New("closed") }, stop, 5*time.Millisecond, time.Second, func(err error) { quiet <- err })
	}()
	<-done
	select {
	case err := <-quiet:
		t.Fatalf("a connection closed on purpose was reported: %v", err)
	default:
	}
}

// A host that accepts the TCP connection and then never speaks SSH (it hung, or was reset halfway) must not
// keep the dial waiting for ever.
func TestDialSSHHandshakeHasATimeLimit(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // accepted, never answered
		}
	}()
	tgt := Target{Addr: ln.Addr().String(), User: "u", Password: "x", Timeout: 300 * time.Millisecond}
	start := time.Now()
	if _, err := dialSSH(tgt, filepath.Join(t.TempDir(), "known_hosts"), nil); err == nil {
		t.Fatal("dial succeeded")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("the dial took %v", d)
	}
}

// The same through a jump host: the jump's SSH server takes the request to open a channel to the node and never
// answers it (as when its own TCP connect to a hung node waits). The keepalive of a healthy server is answered.
func TestDialViaHasATimeLimit(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	scfg := &ssh.ServerConfig{PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) { return nil, nil }}
	scfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, chans, reqs, err := ssh.NewServerConn(c, scfg)
		if err != nil {
			return
		}
		go ssh.DiscardRequests(reqs) // answers keepalive@openssh.com with a failure, as OpenSSH does
		var held []ssh.NewChannel
		for ch := range chans {
			held = append(held, ch) // neither accepted nor rejected
		}
	}()
	jump, err := dialSSH(Target{Addr: ln.Addr().String(), User: "u", Password: "x", Timeout: 2 * time.Second}, filepath.Join(t.TempDir(), "known_hosts"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer jump.Close()
	if err := sendKeepalive(jump); err != nil {
		t.Fatalf("keepalive on a healthy connection: %v", err)
	}
	start := time.Now()
	if _, err := dialVia(jump, "192.0.2.1:22", 200*time.Millisecond); err == nil {
		t.Fatal("dial through the jump succeeded")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("dialVia took %v", d)
	}
}
