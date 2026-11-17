package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"virticle/internal/ctlbackup"
)

func thisHost() string { h, _ := os.Hostname(); return h }

func TestCbWriteAtOnce(t *testing.T) {
	for _, c := range []struct {
		wasOn bool
		was   string
		on    bool
		dir   string
		want  bool
	}{
		{false, "/a", true, "/a", true},  // switched on
		{true, "/a", true, "/b", true},   // new folder
		{true, "/a", true, "/a", false},  // time or retention only
		{true, "/a", false, "/a", false}, // switched off
		{false, "/a", false, "/b", false},
	} {
		if got := cbWriteAtOnce(c.wasOn, c.was, c.on, c.dir); got != c.want {
			t.Errorf("%+v: %v", c, got)
		}
	}
}

// A new folder gets a copy at once; the old folder keeps its copies; the new folder's retention leaves other
// hosts' files alone; a changed time does not write a copy.
func TestControllerBackupNewFolder(t *testing.T) {
	e := backupEnv(t)
	put := func(body map[string]any) {
		t.Helper()
		if c := e.do("PUT", "/api/controller-backup", body, cookieOf(e, "ann")).Code; c != 200 {
			t.Fatalf("put: %d", c)
		}
	}
	count := func(dir string) int {
		fs, _ := ctlbackup.Files(dir)
		return len(fs)
	}
	wait := func(dir string, n int) {
		t.Helper()
		for i := 0; i < 200 && count(dir) != n; i++ {
			time.Sleep(25 * time.Millisecond)
		}
		if got := count(dir); got != n {
			t.Fatalf("%s holds %d copies, want %d", dir, got, n)
		}
	}
	def := filepath.Join(e.srv.DataDir, "backups")
	put(map[string]any{"daily": true, "keepDaily": 2})
	wait(def, 1)
	put(map[string]any{"at": "05:00"}) // time only: no extra copy
	time.Sleep(200 * time.Millisecond)
	if n := count(def); n != 1 {
		t.Fatalf("a changed time wrote a copy: %d", n)
	}
	dir := filepath.Join(t.TempDir(), "new")
	os.MkdirAll(dir, 0o700)
	other := ctlbackup.FileName("another-host", time.Now().Add(-48*time.Hour), false)
	os.WriteFile(filepath.Join(dir, other), []byte("x"), 0o600)
	put(map[string]any{"dir": dir})
	wait(dir, 2)
	if n := count(def); n != 1 {
		t.Fatalf("the old folder changed: %d", n)
	}
	if _, err := os.Stat(filepath.Join(dir, other)); err != nil {
		t.Fatalf("another host's copy was removed: %v", err)
	}
}

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
