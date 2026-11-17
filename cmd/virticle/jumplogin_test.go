package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
	"virticle/internal/prepare"
)

func TestAdminLoginJumpCredentials(t *testing.T) {
	dir := t.TempDir()
	pw := filepath.Join(dir, "jpw")
	os.WriteFile(pw, []byte("from-file\nsecond\n"), 0o600)
	key := filepath.Join(dir, "jkey")
	os.WriteFile(key, []byte("KEY"), 0o600)
	env := map[string]string{nodePasswordEnv: "admin-pw", jumpPasswordEnv: "from-env"}

	in, _ := fakeCred(false, env, "")
	a, _, err := adminLogin("root", "h", "j@jh", credFlags{}, in)
	if err != nil || a.JumpPassword != "from-env" || a.Password != "admin-pw" {
		t.Errorf("env: %+v %v", a, err)
	}
	a, _, err = adminLogin("root", "h", "j@jh", credFlags{jumpPasswordFile: pw}, in)
	if err != nil || a.JumpPassword != "from-file" {
		t.Errorf("file wins over the environment: %q %v", a.JumpPassword, err)
	}
	a, _, err = adminLogin("root", "h", "j@jh", credFlags{jumpIdentity: key}, in)
	if err != nil || string(a.JumpKeyPEM) != "KEY" || !a.HasJumpLogin() {
		t.Errorf("identity: %+v %v", a, err)
	}
	in.agent = true
	if a, _, _ = adminLogin("root", "h", "j@jh", credFlags{}, in); !a.JumpAgent {
		t.Error("the SSH agent is offered to the jump host too")
	}
	// No jump host: no jump login, whatever the environment says.
	if a, _, _ = adminLogin("root", "h", "", credFlags{}, in); a.HasJumpLogin() || a.JumpPassword != "" {
		t.Errorf("no jump host: %+v", a)
	}
	if _, code, err := adminLogin("root", "h", "j@jh", credFlags{jumpIdentity: filepath.Join(dir, "missing")}, in); err == nil || code != 1 || !strings.HasPrefix(err.Error(), "--jump-identity:") {
		t.Errorf("missing identity: %v %d", err, code)
	}
	empty := filepath.Join(dir, "empty")
	os.WriteFile(empty, []byte("\n"), 0o600)
	if _, _, err := adminLogin("root", "h", "j@jh", credFlags{jumpPasswordFile: empty}, in); err == nil || !strings.HasPrefix(err.Error(), "--jump-password-file:") {
		t.Errorf("empty password file: %v", err)
	}
}

func TestJumpFlagsAreNotForThisMachine(t *testing.T) {
	q := addReq{cred: credFlags{jumpPasswordFile: "f"}}
	if got := checkAddArgs(&q, nil, 0); !strings.Contains(got, "are for another host") {
		t.Errorf("%q", got)
	}
}

func TestJumpHostFree(t *testing.T) {
	a := cluster.NodeStatus{}
	a.ID, a.Jump = 1, "u@j"
	b := cluster.NodeStatus{}
	b.ID, b.Jump = 2, "u@j"
	c := cluster.NodeStatus{}
	c.ID = 3
	if jumpHostFree([]cluster.NodeStatus{a, b, c}, a) {
		t.Error("another node still uses the jump host")
	}
	if !jumpHostFree([]cluster.NodeStatus{a, c}, a) {
		t.Error("the last node through the jump host")
	}
	if jumpHostFree([]cluster.NodeStatus{a, c}, c) {
		t.Error("a node without a jump host")
	}
}

func TestSignInErrorSaysWhatToGive(t *testing.T) {
	refused := errors.New("ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey], no supported methods remain")
	a := prepare.Admin{Addr: "n", User: "root", Jump: "u@j"}
	err := signInError(&hostconn.HopError{Jump: true, Addr: "j", User: "u", Err: refused}, a)
	for _, want := range []string{"jump host u@j: the controller's key is not accepted", "--jump-identity FILE", "--jump-password-file FILE", jumpPasswordEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q misses %q", err, want)
		}
	}
	// A login was given and refused: no advice to give one.
	a.JumpPassword = "x"
	if err := signInError(&hostconn.HopError{Jump: true, Addr: "j", User: "u", Login: true, Err: refused}, a); strings.Contains(err.Error(), "Give a login") {
		t.Errorf("%q", err)
	}
	// The node refused: the node is named, not the jump host.
	err = signInError(&hostconn.HopError{Addr: "n", User: "root", Err: refused}, a)
	if err.Error() != "ssh n: sign-in as root refused (wrong password or key)" {
		t.Errorf("%q", err)
	}
	if err := signInError(errors.New("x"), a); err.Error() != "could not sign in to n as root: x" {
		t.Errorf("%q", err)
	}
}
