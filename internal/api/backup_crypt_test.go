package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"virticle/internal/backupkey"
	"virticle/internal/ctlbackup"
)

const keyPass = "the backup key passphrase"

type keyAnswer struct {
	Status  bkEncStatus `json:"status"`
	KeyFile string      `json:"keyFile"`
}

func TestBackupEncryptionKey(t *testing.T) {
	e := backupEnv(t)
	e.user("vic", viewer)
	on := func(user string, body map[string]any) (int, keyAnswer) {
		w := e.do("POST", "/api/backup-encryption/on", body, cookieOf(e, user))
		var a keyAnswer
		if w.Code == 200 {
			decode(t, w, &a)
		}
		return w.Code, a
	}
	if c, _ := on("bob", map[string]any{"passphrase": keyPass, "again": keyPass}); c != 403 {
		t.Fatalf("operator: %d", c)
	}
	if c, _ := on("ann", map[string]any{"passphrase": keyPass, "again": "something else"}); c != 400 {
		t.Fatalf("differing passphrases: %d", c)
	}
	if c, _ := on("ann", map[string]any{"passphrase": "short", "again": "short"}); c != 400 {
		t.Fatalf("short passphrase: %d", c)
	}
	c, a := on("ann", map[string]any{"passphrase": keyPass, "again": keyPass})
	if c != 200 || !a.Status.On || !a.Status.HasKey || !backupkey.ValidRecipient(a.Status.Recipient) || !strings.HasPrefix(a.KeyFile, "-----BEGIN AGE ENCRYPTED FILE-----") {
		t.Fatalf("turn on: %d %+v", c, a)
	}
	rec := a.Status.Recipient
	if e.srv.backupRecipient() != rec {
		t.Fatal("new backups would not be encrypted")
	}
	// The passphrase is nowhere on the controller; the identity only inside the recovery key file.
	for _, f := range []string{backupkey.FileName, "t.db"} {
		b, _ := os.ReadFile(filepath.Join(e.srv.DataDir, f))
		if bytes.Contains(b, []byte(keyPass)) || bytes.Contains(b, []byte("AGE-SECRET-KEY")) {
			t.Fatalf("%s holds a secret in clear", f)
		}
	}
	// Everyone sees whether backups are encrypted.
	var st bkEncStatus
	decode(t, e.do("GET", "/api/backup-encryption", nil, cookieOf(e, "vic")), &st)
	if !st.On || st.Recipient != rec {
		t.Fatalf("viewer status: %+v", st)
	}

	// Off keeps the key; on again needs its passphrase and keeps the same key.
	if w := e.do("POST", "/api/backup-encryption/off", nil, cookieOf(e, "ann")); w.Code != 200 {
		t.Fatalf("off: %d", w.Code)
	}
	if e.srv.backupRecipient() != "" {
		t.Fatal("still encrypting")
	}
	decode(t, e.do("GET", "/api/backup-encryption", nil, cookieOf(e, "ann")), &st)
	if st.On || !st.HasKey || st.Recipient != rec {
		t.Fatalf("off status: %+v", st)
	}
	if c, _ := on("ann", map[string]any{"passphrase": "not the passphrase"}); c != 400 {
		t.Fatalf("on with a wrong passphrase: %d", c)
	}
	c, a = on("ann", map[string]any{"passphrase": keyPass})
	if c != 200 || a.Status.Recipient != rec || a.KeyFile != "" || !a.Status.On {
		t.Fatalf("on again: %d %+v", c, a)
	}

	// A new passphrase for the same key.
	pw := func(body map[string]any) int {
		return e.do("POST", "/api/backup-encryption/passphrase", body, cookieOf(e, "ann")).Code
	}
	if c := pw(map[string]any{"current": "not the passphrase", "new": "a new passphrase", "again": "a new passphrase"}); c != 400 {
		t.Fatalf("change with a wrong passphrase: %d", c)
	}
	if c := pw(map[string]any{"current": keyPass, "new": "a new passphrase", "again": "a new passphrase"}); c != 200 {
		t.Fatalf("change: %d", c)
	}
	blob, _ := backupkey.Read(e.srv.DataDir)
	id, err := backupkey.Unwrap(blob, "a new passphrase")
	if err != nil || id.Recipient().String() != rec {
		t.Fatalf("new passphrase: %v", err)
	}
	if _, err := backupkey.Unwrap(blob, keyPass); err == nil {
		t.Fatal("the old passphrase still works")
	}

	// The recovery key file, with the password again.
	if w := e.do("POST", "/api/backup-encryption/key", map[string]any{"password": "wrong password!"}, cookieOf(e, "ann")); w.Code != 403 {
		t.Fatalf("key with a wrong password: %d", w.Code)
	}
	w := e.do("POST", "/api/backup-encryption/key", map[string]any{"password": testPW}, cookieOf(e, "ann"))
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), blob) || !strings.Contains(w.Header().Get("Content-Disposition"), backupkey.DownloadName) {
		t.Fatalf("key download: %d", w.Code)
	}

	// The scheduled controller backup is encrypted to the key, and holds the key for a restored controller.
	if err := e.srv.dailyBackupRun(); err != nil {
		t.Fatal(err)
	}
	files, _ := ctlbackup.Files(filepath.Join(e.srv.DataDir, "backups"))
	if len(files) != 1 || !strings.HasSuffix(files[0].Name, ".tar.gz.age") {
		t.Fatalf("files %+v", files)
	}
	path := filepath.Join(e.srv.DataDir, "backups", files[0].Name)
	if !ctlbackup.KeyEncrypted(path) {
		t.Fatal("not encrypted to the key")
	}
	if recs := ctlbackup.ReadRecords(e.srv.DataDir); !recs["daily"].Encrypted {
		t.Fatal("the record does not say encrypted")
	}
	f, _ := os.Open(path)
	defer f.Close()
	plain, err := ctlbackup.OpenWith(f, ctlbackup.Unlock{Identity: func() (age.Identity, error) { return id, nil }})
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	m, err := ctlbackup.Extract(plain, out)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(out, backupkey.FileName)); !bytes.Equal(got, blob) {
		t.Fatalf("the controller backup does not hold the backup key (%d files)", len(m.Files))
	}
	decode(t, e.do("GET", "/api/backup-encryption?count=1", nil, cookieOf(e, "ann")), &st)
	if st.CtlBackups == nil || *st.CtlBackups != 1 || st.VMBackups == nil {
		t.Fatalf("count: %+v", st)
	}
	// With encryption off the next copy is plain again.
	e.do("POST", "/api/backup-encryption/off", nil, cookieOf(e, "ann"))
	if _, _, err := e.srv.writeDailyBackup(); err != nil {
		t.Fatal(err)
	}
	files, _ = ctlbackup.Files(filepath.Join(e.srv.DataDir, "backups"))
	if len(files) == 0 || !strings.HasSuffix(files[0].Name, ".tar.gz") || ctlbackup.KeyEncrypted(filepath.Join(e.srv.DataDir, "backups", files[0].Name)) {
		t.Fatalf("plain copy: %+v", files)
	}
}

// A wrong passphrase is refused, counts like a wrong password and locks after a few tries; the right one gives
// the identity.
func TestUnlockBackupKey(t *testing.T) {
	e := backupEnv(t)
	if w := e.do("POST", "/api/backup-encryption/on", map[string]any{"passphrase": keyPass, "again": keyPass}, cookieOf(e, "ann")); w.Code != 200 {
		t.Fatal(w.Code)
	}
	r := reqAs("ann")
	if _, err := e.srv.unlockBackupKey(r, ""); err == nil {
		t.Fatal("no passphrase")
	}
	id, err := e.srv.unlockBackupKey(r, keyPass)
	if err != nil || !strings.HasPrefix(string(id), "AGE-SECRET-KEY-1") {
		t.Fatalf("unlock: %v", err)
	}
	for i := 0; i < limPair; i++ {
		if _, err := e.srv.unlockBackupKey(r, "wrong passphrase"); err == nil || err.Error() != errWrongBackupPass.Error() {
			t.Fatalf("wrong passphrase %d: %v", i, err)
		}
	}
	if _, err := e.srv.unlockBackupKey(r, keyPass); err == nil || !strings.Contains(err.Error(), "too many") {
		t.Fatalf("not locked: %v", err)
	}
	// Another controller's key is named, not tried.
	other := &backupManifest{Recipient: testRecipient, Disks: []backupDiskRec{{Encrypted: true}}}
	if err := e.srv.checkRecipients([]chainLink{{Dir: "x", Man: other}}); err == nil || !strings.Contains(err.Error(), "another backup key") {
		t.Fatalf("other key: %v", err)
	}
}

func TestExportTicket(t *testing.T) {
	id := []byte("AGE-SECRET-KEY-1XYZ")
	tk := newExportTicket("ann", "/p/a.vmbackup", id)
	if takeExportTicket(tk, "bob", "/p/a.vmbackup") != nil {
		t.Fatal("another user took the ticket")
	}
	tk = newExportTicket("ann", "/p/a.vmbackup", []byte("AGE-SECRET-KEY-1XYZ"))
	if got := takeExportTicket(tk, "ann", "/p/a.vmbackup"); string(got) != "AGE-SECRET-KEY-1XYZ" {
		t.Fatal("ticket")
	}
	if takeExportTicket(tk, "ann", "/p/a.vmbackup") != nil {
		t.Fatal("a ticket worked twice")
	}
}

func reqAs(user string) *http.Request {
	r := httptest.NewRequest("POST", "/", nil)
	return r.WithContext(context.WithValue(r.Context(), ctxKey{}, user))
}
