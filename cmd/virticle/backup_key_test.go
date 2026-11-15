package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"virticle/internal/backupkey"
	"virticle/internal/ctlbackup"
	"virticle/internal/store"
)

// A scheduled controller backup made while backup encryption is on is encrypted to the backup key. It is restored
// with the downloaded recovery key file (--key), or with the key still in the data directory, and its passphrase.
func TestRestoreKeyEncryptedBackup(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	os.Mkdir(data, 0o700)
	st, err := store.Open(filepath.Join(data, store.DBName))
	if err != nil {
		t.Fatal(err)
	}
	st.CreateFirstUser("admin", "correct horse battery")
	st.Close()
	os.WriteFile(filepath.Join(data, "id_ed25519"), []byte("KEY"), 0o600)
	rec, blob, err := backupkey.New("the backup key passphrase", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	backupkey.Write(data, blob)
	keyFile := filepath.Join(root, backupkey.DownloadName)
	os.WriteFile(keyFile, blob, 0o600)

	out := filepath.Join(root, "out")
	os.Mkdir(out, 0o700)
	enc := filepath.Join(out, ctlbackup.FileName("ctl", time.Now(), true))
	if _, _, err := ctlbackup.WriteFileTo(enc, controllerSource(data), rec); err != nil {
		t.Fatal(err)
	}
	if !ctlbackup.KeyEncrypted(enc) {
		t.Fatal("not encrypted to the key")
	}
	pf := filepath.Join(root, "pass")
	os.WriteFile(pf, []byte("the backup key passphrase\n"), 0o600)
	bad := filepath.Join(root, "bad")
	os.WriteFile(bad, []byte("not the passphrase\n"), 0o600)

	// A new machine: no data directory, so no key there.
	fresh := filepath.Join(root, "fresh")
	if err := runRestore(enc, fresh, pf, "", ctlbackup.SystemHost{}, &bytes.Buffer{}); !errors.Is(err, ctlbackup.ErrNeedKey) {
		t.Fatalf("without a key: %v", err)
	}
	if err := runRestore(enc, fresh, bad, keyFile, ctlbackup.SystemHost{}, &bytes.Buffer{}); !errors.Is(err, backupkey.ErrWrongPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	if _, err := os.Stat(fresh); err == nil {
		t.Fatal("a failed restore left a directory")
	}
	var sb bytes.Buffer
	if err := runRestore(enc, fresh, pf, keyFile, ctlbackup.SystemHost{}, &sb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), "every checksum matches") {
		t.Fatal(sb.String())
	}
	// The restored controller has the key again, so it can still restore VM backups.
	if got, err := backupkey.Read(fresh); err != nil || !bytes.Equal(got, blob) {
		t.Fatalf("the backup key is not in the restored data directory: %v", err)
	}

	// The same machine: the data directory still has the key, no --key needed; the old directory is moved aside.
	sb.Reset()
	if err := runRestore(enc, data, pf, "", ctlbackup.SystemHost{}, &sb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), "is now") {
		t.Fatal(sb.String())
	}
	// A key file of another key does not open it.
	_, other, _ := backupkey.New("the backup key passphrase", time.Now())
	otherFile := filepath.Join(root, "other.age")
	os.WriteFile(otherFile, other, 0o600)
	if err := runRestore(enc, filepath.Join(root, "x"), pf, otherFile, ctlbackup.SystemHost{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "another backup key") {
		t.Fatalf("other key: %v", err)
	}
}
