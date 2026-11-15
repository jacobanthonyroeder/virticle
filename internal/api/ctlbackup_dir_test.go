package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCtlBackupDirCheck(t *testing.T) {
	data := t.TempDir()
	other := t.TempDir()
	s := &Server{DataDir: data}
	if err := s.cbCheckDir(filepath.Join(data, "backups")); err != nil {
		t.Fatalf("the default folder: %v", err)
	}
	for _, bad := range []string{"relative/dir", "/", data, filepath.Join(data, "elsewhere")} {
		if err := s.cbCheckDir(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	dir := filepath.Join(other, "a", "b")
	if err := s.cbCheckDir(dir); err != nil {
		t.Fatalf("a new folder outside the data directory: %v", err)
	}
	if ents, err := os.ReadDir(dir); err != nil || len(ents) != 0 {
		t.Fatalf("the folder exists and the test file is gone: %v %v", ents, err)
	}
	if os.Geteuid() != 0 { // root may write anywhere
		ro := filepath.Join(other, "ro")
		os.Mkdir(ro, 0o500)
		err := s.cbCheckDir(filepath.Join(ro, "x"))
		if err == nil || !strings.Contains(err.Error(), "writable by the account") {
			t.Errorf("a folder that cannot be written: %v", err)
		}
	}
}
