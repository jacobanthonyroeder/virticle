package store

import (
	"path/filepath"
	"testing"
)

func TestRunningTaskOn(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if k, _ := s.RunningTaskOn("n1", ""); k != "" {
		t.Fatalf("no tasks yet, got %q", k)
	}
	done, _ := s.StartTask("backup", "vm1", "n1", "u")
	s.FinishTask(&done, nil)
	if k, _ := s.RunningTaskOn("n1"); k != "" {
		t.Fatalf("a finished task counts: %q", k)
	}
	if _, err := s.StartTask("backup", "vm2", "n1", "u"); err != nil {
		t.Fatal(err)
	}
	if k, tg := s.RunningTaskOn("", "n1"); k != "backup" || tg != "vm2" {
		t.Fatalf("got %q %q", k, tg)
	}
	if k, _ := s.RunningTaskOn("n2"); k != "" {
		t.Fatalf("another node's task counts: %q", k)
	}
}
