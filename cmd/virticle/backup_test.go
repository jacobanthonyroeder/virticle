package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"virticle/internal/ctlbackup"
	"virticle/internal/store"
)

func TestBackupAndRestoreCommands(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	os.Mkdir(data, 0o700)
	st, err := store.Open(filepath.Join(data, store.DBName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateFirstUser("admin", "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	os.WriteFile(filepath.Join(data, "id_ed25519"), []byte("KEY"), 0o600)

	// a plain backup into a directory
	out := filepath.Join(root, "out")
	os.Mkdir(out, 0o700)
	if err := runBackup(data, out, false, ""); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(out, "virticle-backup-*.tar.gz"))
	if len(files) != 1 {
		t.Fatalf("files: %v", files)
	}
	if fi, _ := os.Stat(files[0]); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}

	// an encrypted one, passphrase from a file; a short passphrase is refused
	pf := filepath.Join(root, "pass")
	os.WriteFile(pf, []byte("short\n"), 0o600)
	if err := runBackup(data, filepath.Join(out, "x.tar.gz.age"), true, pf); err == nil {
		t.Error("short passphrase accepted")
	}
	os.WriteFile(pf, []byte("a long passphrase\n"), 0o600)
	enc := filepath.Join(out, "x.tar.gz.age")
	if err := runBackup(data, enc, true, pf); err != nil {
		t.Fatal(err)
	}
	if err := runBackup(data, enc, true, pf); err == nil {
		t.Error("overwrote an existing backup")
	}
	if err := runBackup(filepath.Join(root, "none"), out, false, ""); err == nil {
		t.Error("backup of a directory without a database")
	}

	// restore both into new directories
	for _, c := range []struct{ file, pass string }{{files[0], ""}, {enc, pf}} {
		target := filepath.Join(root, "restored-"+filepath.Base(c.file))
		var sb bytes.Buffer
		if err := runRestore(c.file, target, c.pass, "", ctlbackup.SystemHost{}, &sb); err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		if !strings.Contains(sb.String(), "every checksum matches") || !strings.Contains(sb.String(), "systemctl start virticle") {
			t.Errorf("output: %s", sb.String())
		}
		if b, _ := os.ReadFile(filepath.Join(target, "id_ed25519")); string(b) != "KEY" {
			t.Error("key not restored")
		}
	}
	// an encrypted backup without its passphrase file cannot be read in a test (no terminal)
	if err := runRestore(enc, filepath.Join(root, "nope"), "", "", ctlbackup.SystemHost{}, &bytes.Buffer{}); err == nil {
		t.Error("restored without a passphrase")
	}
	if _, err := os.Stat(filepath.Join(root, "nope")); err == nil {
		t.Error("a failed restore left a directory")
	}
}

func TestBackupCommandPrunesWithKeepFlags(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	os.Mkdir(data, 0o700)
	st, err := store.Open(filepath.Join(data, store.DBName))
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	out := filepath.Join(root, "out")
	os.Mkdir(out, 0o700)
	// Forty back-dated daily backups, one of them encrypted, and a file that is not ours.
	now := time.Now()
	for i := 1; i <= 40; i++ {
		os.WriteFile(filepath.Join(out, ctlbackup.FileName("old", now.AddDate(0, 0, -i), i == 3)), []byte("x"), 0o600)
	}
	os.WriteFile(filepath.Join(out, "notes.txt"), nil, 0o600)
	// Without keep flags nothing goes.
	if err := runBackup(data, out, false, ""); err != nil {
		t.Fatal(err)
	}
	if fs, _ := ctlbackup.Files(out); len(fs) != 41 {
		t.Fatalf("without flags: %d files", len(fs))
	}
	time.Sleep(1100 * time.Millisecond) // file names have one-second stamps
	if code := backupCommand([]string{"-data", data, "-o", out, "--keep-daily", "5", "--keep-weekly", "3"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	fs, _ := ctlbackup.Files(out)
	// 5 dailies (today, with two backups of which the newer counts, and four days back) plus the newest of the
	// weeks before them that are not covered yet.
	if len(fs) < 6 || len(fs) > 8 {
		t.Fatalf("kept %d: %v", len(fs), fs)
	}
	if _, err := os.Stat(filepath.Join(out, "notes.txt")); err != nil {
		t.Fatal("a file that is not a backup was deleted")
	}
	if code := backupCommand([]string{"-data", data, "-o", filepath.Join(out, "x.tar.gz"), "--keep-last", "1"}); code != 2 {
		t.Fatalf("keep flags with -o FILE: exit %d", code)
	}
	if code := backupCommand([]string{"-data", data, "-o", out, "--keep-last", "-1"}); code != 2 {
		t.Fatalf("negative keep: exit %d", code)
	}
}
