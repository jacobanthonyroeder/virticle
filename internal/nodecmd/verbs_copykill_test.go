package nodecmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// copy-kill stops a qemu-img that holds the pool file open, and nothing else.
func TestCopyKill(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep")
	}
	n := newNode(t)
	n.cfg.ProcDir = "/proc"
	bin := n.abs("bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// A process whose name (comm) is qemu-img, and one that is not.
	if err := os.Symlink(sleep, filepath.Join(bin, "qemu-img")); err != nil {
		t.Fatal(err)
	}
	start := func(prog, dest string) *exec.Cmd {
		f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		cmd := exec.Command(prog, "30")
		cmd.Stdout = f
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		return cmd
	}

	n.refused("bad path", "", "copy-kill", "../x")
	n.refused("outside the pools", "", "copy-kill", n.abs("outside/x"))
	n.put("pools/default/idle.qcow2", "x")
	n.denied("nothing runs", "", "copy-kill", n.pool("idle.qcow2"))

	other := start(sleep, n.pool("other.img")) // not named qemu-img: left alone
	copyA := start(filepath.Join(bin, "qemu-img"), n.pool("a.qcow2"))
	copyB := start(filepath.Join(bin, "qemu-img"), n.pool("b.qcow2"))
	time.Sleep(200 * time.Millisecond)

	if out := n.ok("", "copy-kill", n.pool("a.qcow2")); !strings.Contains(out, "killed 1") {
		t.Errorf("answer: %q", out)
	}
	waited := make(chan error, 1)
	go func() { waited <- copyA.Wait() }()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("the copy is still running")
	}
	if copyB.ProcessState != nil {
		t.Error("another copy was stopped")
	}
	n.denied("other file that no qemu-img holds", "", "copy-kill", n.pool("other.img"))
	if other.ProcessState != nil {
		t.Error("a process that is not qemu-img was stopped")
	}
}
