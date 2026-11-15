package backupkey

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"filippo.io/age/armor"
)

func init() { workFactor = 10 } // fast scrypt in tests

func TestNewUnwrapRewrap(t *testing.T) {
	rec, blob, err := New("correct horse battery", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !ValidRecipient(rec) || !strings.HasPrefix(rec, "age1") {
		t.Fatalf("recipient %q", rec)
	}
	if !bytes.HasPrefix(blob, []byte(armor.Header)) {
		t.Fatalf("not armored: %q", blob[:40])
	}
	if bytes.Contains(blob, []byte("AGE-SECRET-KEY")) {
		t.Fatal("the identity is in clear")
	}
	id, err := Unwrap(blob, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if id.Recipient().String() != rec {
		t.Fatal("identity does not belong to the recipient")
	}
	if _, err := Unwrap(blob, "wrong passphrase!"); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	if _, err := Rewrap(blob, "wrong passphrase!", "a new passphrase"); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("rewrap with a wrong passphrase: %v", err)
	}
	if _, err := Rewrap(blob, "correct horse battery", "short"); err == nil {
		t.Fatal("a short new passphrase was accepted")
	}
	nb, err := Rewrap(blob, "correct horse battery", "a new passphrase")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := Unwrap(nb, "a new passphrase")
	if err != nil || id2.String() != id.String() {
		t.Fatalf("rewrap changed the identity: %v", err)
	}
	if _, err := Unwrap(nb, "correct horse battery"); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatal("the old passphrase still opens the rewrapped key")
	}
	// What the backup key encrypts, the unwrapped identity decrypts (and the file says how it is encrypted).
	var c bytes.Buffer
	w, _ := age.Encrypt(&c, id.Recipient())
	w.Write([]byte("guest data"))
	w.Close()
	if got := Stanza(c.Bytes()); got != "X25519" {
		t.Fatalf("stanza %q", got)
	}
	r, err := age.Decrypt(bytes.NewReader(c.Bytes()), id2)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(r); string(b) != "guest data" {
		t.Fatal("round trip")
	}
}

func TestPolicyAndFiles(t *testing.T) {
	if _, _, err := New("short", time.Now()); err == nil {
		t.Fatal("short passphrase accepted")
	}
	dir := t.TempDir()
	if _, err := Read(dir); !errors.Is(err, ErrNoKey) {
		t.Fatalf("no key: %v", err)
	}
	_, blob, _ := New("0123456789", time.Now())
	if err := Write(dir, blob); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(filepath.Join(dir, FileName))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	got, err := Read(dir)
	if err != nil || !bytes.Equal(got, blob) {
		t.Fatal("read back")
	}
	if Stanza([]byte("plain")) != "" || Stanza(blob) != "" {
		t.Fatal("stanza of a file that is no binary age file")
	}
	var c bytes.Buffer
	sr, _ := age.NewScryptRecipient("0123456789")
	sr.SetWorkFactor(10)
	w, _ := age.Encrypt(&c, sr)
	w.Close()
	if Stanza(c.Bytes()) != "scrypt" {
		t.Fatal("scrypt stanza")
	}
	if ValidRecipient("age1nope") || ValidRecipient("") {
		t.Fatal("bad recipient accepted")
	}
}
