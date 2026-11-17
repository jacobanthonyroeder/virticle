package hostconn

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// jumpServer is an SSH server in the test: it accepts one password and the public keys in its authorized_keys file
// (the whole line is not read: the key's base64 part decides), runs "exec" requests with sh and HOME set to home, and
// forwards nothing.
type jumpServer struct {
	addr string
	home string
	mu   sync.Mutex
}

func newJumpServer(t *testing.T, password string) *jumpServer {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostPriv)
	s := &jumpServer{home: t.TempDir()}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if string(pw) == password {
				return nil, nil
			}
			return nil, errors.New("wrong password")
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			b, _ := os.ReadFile(filepath.Join(s.home, ".ssh", "authorized_keys"))
			blob := strings.Fields(string(ssh.MarshalAuthorizedKey(k)))[1]
			if strings.Contains(string(b), blob) {
				return nil, nil
			}
			return nil, errors.New("unknown key")
		},
	}
	cfg.AddHostKey(hostSigner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s.addr = ln.Addr().String()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c, cfg)
		}
	}()
	return s
}

func (s *jumpServer) serve(c net.Conn, cfg *ssh.ServerConfig) {
	conn, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		c.Close()
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "no")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer ch.Close()
			for r := range creqs {
				if r.Type != "exec" {
					r.Reply(false, nil)
					continue
				}
				r.Reply(true, nil)
				n := binary.BigEndian.Uint32(r.Payload)
				cmd := exec.Command("sh", "-c", string(r.Payload[4:4+n]))
				cmd.Env = []string{"HOME=" + s.home, "PATH=" + os.Getenv("PATH")}
				cmd.Stdin = ch
				out, err := cmd.CombinedOutput()
				ch.Write(out)
				code := uint32(0)
				if err != nil {
					code = 1
				}
				ch.SendRequest("exit-status", false, binary.BigEndian.AppendUint32(nil, code))
				return
			}
		}()
	}
}

func testKey(t *testing.T) (file, pub string) {
	t.Helper()
	p, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, _ := ssh.MarshalPrivateKey(priv, "")
	file = filepath.Join(t.TempDir(), "id")
	if err := os.WriteFile(file, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	sp, _ := ssh.NewPublicKey(p)
	return file, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp))) + " virticle-controller"
}

func TestJumpLoginAddsTheControllersKeyOnce(t *testing.T) {
	srv := newJumpServer(t, "jump-pw")
	keyFile, pub := testKey(t)
	kh := filepath.Join(t.TempDir(), "known_hosts")
	jump := Target{Addr: srv.addr, User: "u", KeyFile: keyFile, Timeout: 5 * time.Second}

	// The controller's key is not accepted and no login is given: the error names the jump host and the reason.
	_, _, err := dialJump(Target{Jump: &jump}, kh)
	if !JumpAuthRefused(err) || !strings.HasPrefix(err.Error(), "jump host u@"+srv.addr+": the controller's key is not accepted") {
		t.Fatalf("no login: %v", err)
	}

	// A wrong password: refused, and nothing is written.
	_, _, err = dialJump(Target{Jump: &jump, JumpLogin: &Target{Password: "bad"}, JumpInstallKey: pub}, kh)
	if !JumpAuthRefused(err) || !strings.Contains(err.Error(), "sign-in refused") {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := os.Stat(filepath.Join(srv.home, ".ssh")); err == nil {
		t.Fatal("something was written after a refused login")
	}

	// A login without the key to install: signs in and writes nothing (a check).
	c, added, err := dialJump(Target{Jump: &jump, JumpLogin: &Target{Password: "jump-pw"}}, kh)
	if err != nil || added {
		t.Fatalf("login only: %v %v", err, added)
	}
	c.Close()
	if _, err := os.Stat(filepath.Join(srv.home, ".ssh")); err == nil {
		t.Fatal("a run without the key to install wrote something")
	}

	// The login works: the key is added; a second login (the key is there but not offered) adds nothing more.
	for i := 0; i < 2; i++ {
		c, added, err := dialJump(Target{Jump: &Target{Addr: srv.addr, User: "u", Timeout: 5 * time.Second}, JumpLogin: &Target{Password: "jump-pw"}, JumpInstallKey: pub}, kh)
		if err != nil || !added {
			t.Fatalf("login %d: %v %v", i, err, added)
		}
		c.Close()
	}
	b, _ := os.ReadFile(filepath.Join(srv.home, ".ssh", "authorized_keys"))
	line, _ := JumpKeyLine(pub)
	if string(b) != line+"\n" {
		t.Fatalf("authorized_keys:\n%s\nwant:\n%s", b, line)
	}
	if fi, _ := os.Stat(filepath.Join(srv.home, ".ssh")); fi.Mode().Perm() != 0o700 {
		t.Errorf(".ssh mode %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Join(srv.home, ".ssh", "authorized_keys")); fi.Mode().Perm() != 0o600 {
		t.Errorf("authorized_keys mode %v", fi.Mode().Perm())
	}

	// From now on the controller's key alone passes the jump host, with no login.
	c, added, err = dialJump(Target{Jump: &jump}, kh)
	if err != nil || added {
		t.Fatalf("with the key: %v %v", err, added)
	}
	c.Close()
}

func TestJumpKeyIsAppendedToAFileWithoutFinalNewline(t *testing.T) {
	srv := newJumpServer(t, "pw")
	if err := os.MkdirAll(filepath.Join(srv.home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	existing := "ssh-ed25519 AAAAexisting someone"
	if err := os.WriteFile(filepath.Join(srv.home, ".ssh", "authorized_keys"), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	keyFile, pub := testKey(t)
	jump := Target{Addr: srv.addr, User: "u", KeyFile: keyFile, Timeout: 5 * time.Second}
	c, added, err := dialJump(Target{Jump: &jump, JumpLogin: &Target{Password: "pw"}, JumpInstallKey: pub}, filepath.Join(t.TempDir(), "kh"))
	if err != nil || !added {
		t.Fatalf("%v %v", err, added)
	}
	c.Close()
	b, _ := os.ReadFile(filepath.Join(srv.home, ".ssh", "authorized_keys"))
	line, _ := JumpKeyLine(pub)
	if string(b) != existing+"\n"+line+"\n" {
		t.Fatalf("authorized_keys:\n%s", b)
	}
}

func TestJumpKeyLine(t *testing.T) {
	line, blob := JumpKeyLine("ssh-ed25519 AAAAkey virticle-controller")
	if line != `restrict,port-forwarding,command="/bin/false" ssh-ed25519 AAAAkey virticle-controller` || blob != "AAAAkey" {
		t.Errorf("%q %q", line, blob)
	}
	if line, _ := JumpKeyLine("ssh-ed25519 AAAAkey"); !strings.HasSuffix(line, "AAAAkey virticle-controller") {
		t.Errorf("no comment: %q", line)
	}
	if line, _ := JumpKeyLine("nonsense"); line != "" {
		t.Errorf("a bad key gave %q", line)
	}
}

func TestHopErrorNamesTheHop(t *testing.T) {
	refused := errors.New("ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey], no supported methods remain")
	for _, c := range []struct {
		e    *HopError
		want string
	}{
		{&HopError{Jump: true, Addr: "j:22", User: "u", Err: refused}, "jump host u@j:22: the controller's key is not accepted"},
		{&HopError{Jump: true, Addr: "j", User: "u", Login: true, Err: refused}, "jump host u@j: sign-in refused (wrong password or key)"},
		{&HopError{Jump: true, Addr: "j", User: "u", Err: errors.New("dial tcp: refused")}, "jump host u@j: dial tcp: refused"},
		{&HopError{Addr: "10.0.0.5", User: "root", Err: refused}, "ssh 10.0.0.5: sign-in as root refused (wrong password or key)"},
		{&HopError{Addr: "10.0.0.5", User: "root", Err: errors.New("i/o timeout")}, "ssh 10.0.0.5: i/o timeout"},
	} {
		if got := c.e.Error(); got != c.want {
			t.Errorf("got %q, want %q", got, c.want)
		}
	}
	if !JumpAuthRefused(&HopError{Jump: true, Err: refused}) || JumpAuthRefused(&HopError{Err: refused}) || JumpAuthRefused(errors.New("x")) {
		t.Error("JumpAuthRefused: only a jump host that refused")
	}
}
