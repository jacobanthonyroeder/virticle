package prepare

import (
	"errors"
	"testing"

	"virticle/internal/hostconn"
)

func TestDialFailureNamesTheHop(t *testing.T) {
	refused := errors.New("ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey], no supported methods remain")
	for _, c := range []struct {
		err             error
		id, title, want string
	}{
		{&hostconn.HopError{Jump: true, Addr: "j", User: "u", Err: refused}, "jump", "Jump host", "u@j: the controller's key is not accepted"},
		{&hostconn.HopError{Jump: true, Addr: "j", User: "u", Login: true, Err: refused}, "jump", "Jump host", "u@j: sign-in refused (wrong password or key)"},
		{&hostconn.HopError{Addr: "10.0.0.5", User: "root", Err: refused}, "admin", "Administrator access", "10.0.0.5: sign-in as root refused (wrong password or key)"},
		{errors.New("boom"), "admin", "Administrator access", "could not sign in: boom"},
	} {
		r := DialFailure(c.err)
		if r.ID != c.id || r.Title != c.title || r.Detail != c.want || r.Status != Error {
			t.Errorf("%v: %+v", c.err, r)
		}
	}
}

func TestHasJumpLogin(t *testing.T) {
	for _, c := range []struct {
		a    Admin
		want bool
	}{
		{Admin{}, false},
		{Admin{JumpPassword: "x"}, false}, // no jump host
		{Admin{Jump: "u@j"}, false},
		{Admin{Jump: "u@j", JumpPassword: "x"}, true},
		{Admin{Jump: "u@j", JumpKeyPEM: []byte("k")}, true},
		{Admin{Jump: "u@j", JumpAgent: true}, true},
	} {
		if got := c.a.HasJumpLogin(); got != c.want {
			t.Errorf("%+v: %v", c.a, got)
		}
	}
}
