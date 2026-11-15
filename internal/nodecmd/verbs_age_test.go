package nodecmd

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
)

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func TestAgeRoundTrip(t *testing.T) {
	n := newNode(t)
	id, _ := age.GenerateX25519Identity()
	work := ".virticle-bk.test1"
	os.Mkdir(n.pool(work), 0o700)
	// Data with long runs of zeros and some that is not.
	plain := make([]byte, 3<<20)
	rand.Read(plain[1<<20 : 1<<20+4096])
	copy(plain[len(plain)-7:], "MARKER!")
	os.WriteFile(n.pool(work+"/vda.qcow2"), plain, 0o600)
	out := n.ok("", "age-encrypt", n.pool(work+"/vda.qcow2"), n.pool(work+"/vda.qcow2.age"), id.Recipient().String())
	enc, err := os.ReadFile(n.pool(work + "/vda.qcow2.age"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(enc, []byte("age-encryption.org/v1\n-> X25519 ")) || bytes.Contains(enc, []byte("MARKER!")) {
		t.Fatal("not age ciphertext")
	}
	want := fmt.Sprintf("%s %d %s %d", sha(plain), len(plain), sha(enc), len(enc))
	if last := lastLine(out); last != want {
		t.Fatalf("encrypt said %q, want %q", last, want)
	}
	// Decrypt into another work directory (the restore's), the identity on stdin.
	rs := ".virticle-rs.test2"
	os.Mkdir(n.pool(rs), 0o700)
	out = n.ok(id.String()+"\n", "age-decrypt", n.pool(work+"/vda.qcow2.age"), n.pool(rs+"/vm.qcow2"))
	if last := lastLine(out); last != fmt.Sprintf("%s %d %s %d", sha(enc), len(enc), sha(plain), len(plain)) {
		t.Fatalf("decrypt said %q", last)
	}
	got, _ := os.ReadFile(n.pool(rs + "/vm.qcow2"))
	if !bytes.Equal(got, plain) {
		t.Fatal("round trip changed the data")
	}
	if strings.Contains(out, "AGE-SECRET-KEY") {
		t.Fatal("the identity came back in the output")
	}

	// Another key: refused, nothing written.
	other, _ := age.GenerateX25519Identity()
	if o, code := n.run(other.String()+"\n", "age-decrypt", n.pool(work+"/vda.qcow2.age"), n.pool(rs+"/x.qcow2")); code != 1 || !strings.Contains(o, "not encrypted to this backup key") {
		t.Fatalf("wrong identity: %d %s", code, o)
	}
	// Cut short, and one byte changed: refused, nothing written.
	os.WriteFile(n.pool(work+"/cut.age"), enc[:len(enc)-100], 0o600)
	if o, code := n.run(id.String()+"\n", "age-decrypt", n.pool(work+"/cut.age"), n.pool(rs+"/y.qcow2")); code != 1 || !strings.Contains(o, "damaged or incomplete") {
		t.Fatalf("truncated: %d %s", code, o)
	}
	bad := append([]byte{}, enc...)
	bad[len(bad)/2] ^= 1
	os.WriteFile(n.pool(work+"/bad.age"), bad, 0o600)
	if _, code := n.run(id.String()+"\n", "age-decrypt", n.pool(work+"/bad.age"), n.pool(rs+"/z.qcow2")); code != 1 {
		t.Fatal("a changed byte was not noticed")
	}
	for _, f := range []string{"x.qcow2", "y.qcow2", "z.qcow2"} {
		if n.exists("pools/default/" + rs + "/" + f) {
			t.Errorf("%s was left behind", f)
		}
	}
	if m := n.glob("pools/default/" + rs + "/*virticle-part*"); len(m) > 0 {
		t.Errorf("part files left: %v", m)
	}
}

func TestAgeRefusals(t *testing.T) {
	n := newNode(t)
	id, _ := age.GenerateX25519Identity()
	rec := id.Recipient().String()
	work := ".virticle-bk.t"
	os.Mkdir(n.pool(work), 0o700)
	n.put("pools/default/"+work+"/a", "data")
	n.put("pools/default/plain", "data")
	n.refused("bad recipient", "", "age-encrypt", n.pool(work+"/a"), n.pool(work+"/a.age"), "age1nope")
	n.refused("not in a work directory", "", "age-encrypt", n.pool("plain"), n.pool("plain.age"), rec)
	n.refused("not the same work directory", "", "age-encrypt", n.pool("plain"), n.pool(work+"/p.age"), rec)
	n.denied("outside the pools", "", "age-encrypt", n.abs("outside/x"), n.pool(work+"/x.age"), rec)
	n.denied("existing destination", "", "age-encrypt", n.pool(work+"/a"), n.pool(work+"/a"), rec)
	n.refused("no identity", "", "age-decrypt", n.pool(work+"/a"), n.pool(work+"/b"))
	n.refused("identity as garbage", "hello\n", "age-decrypt", n.pool(work+"/a"), n.pool(work+"/b"))
	n.refused("decrypt outside a work directory", id.String()+"\n", "age-decrypt", n.pool(work+"/a"), n.pool("b"))
	if o, code := n.run(id.String()+"\n", "age-decrypt", n.pool(work+"/a"), n.pool(work+"/b")); code != 1 || !strings.Contains(o, "not a readable age file") {
		t.Fatalf("plain file: %d %s", code, o)
	}
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestAgeProgress(t *testing.T) {
	out := &lockedBuf{}
	c := &Ctx{Stdout: out}
	var hc hashCount
	hc.n.Store(5)
	stop := c.ageProgress(&hc.n, 10)
	for i := 0; i < 50 && !strings.Contains(out.String(), "@@progress"); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	if got := out.String(); !strings.HasPrefix(got, "@@progress 5 10\n") {
		t.Fatalf("progress %q", got)
	}
}
