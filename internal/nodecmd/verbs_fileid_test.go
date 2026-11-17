package nodecmd

import (
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// idOf returns the identity file-id reports for p.
func (n *tnode) idOf(p string) string {
	n.t.Helper()
	out := n.ok("", "file-id", p)
	id, _, ok := strings.Cut(out, "|")
	if !ok || !ValidIdentity(id) {
		n.t.Fatalf("file-id %s: %q", p, out)
	}
	return id
}

func TestFileIDNamesTheFileNotTheName(t *testing.T) {
	n := newNode(t)
	a := n.put("pools/default/a.img", "one")
	id1 := n.idOf(a)
	if n.idOf(a) != id1 {
		t.Fatal("the identity of an unchanged file changed")
	}
	// Writing, chmod and rename keep the identity.
	if err := os.WriteFile(a, []byte("other content, longer"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Chmod(a, 0o640)
	if n.idOf(a) != id1 {
		t.Error("writing or chmod changed the identity")
	}
	// A file replaced by another under the same name has another identity, also when the inode number is reused.
	for i := 0; i < 5; i++ {
		os.Remove(a)
		n.put("pools/default/a.img", "one")
		if n.idOf(a) == id1 {
			t.Fatalf("a new file under the same name has the old identity (round %d)", i)
		}
	}
	os.Mkdir(n.pool("sub"), 0o755)
	out := n.ok("", "file-id", n.pool("missing"), n.pool("sub"), a)
	lines := strings.Split(out, "\n")
	if len(lines) != 3 || lines[0] != "MISSING" || lines[1] != "OTHER" || !strings.HasSuffix(lines[2], "|3|"+strings.Split(lines[2], "|")[2]) {
		t.Errorf("file-id answer %q", out)
	}
	os.Symlink(a, n.pool("link"))
	if got := n.ok("", "file-id", n.pool("link")); got != "OTHER" {
		t.Errorf("a symbolic link: %q", got)
	}
	n.denied("outside the pools", "", "file-id", n.abs("outside/x"))
}

func TestRemoveIDRemovesOnlyThatFile(t *testing.T) { testRemoveID(t) }

// Without RENAME_NOREPLACE (the Linux NFS client answers EINVAL) the file is moved into a new private directory.
func TestRemoveIDWithoutNoReplace(t *testing.T) {
	old := renameNoReplace
	renameNoReplace = func(int, string, int, string) error { return unix.EINVAL }
	defer func() { renameNoReplace = old }()
	testRemoveID(t)

	// The way back never replaces a file that took the name while the file was aside.
	n := newNode(t)
	a := n.put("pools/default/c.img", "mine")
	d, err := os.Open(n.pool(""))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	as, err := asideOpen(int(d.Fd()), "c.img")
	if err != nil || as.sub == "" {
		t.Fatalf("aside: %v %+v", err, as)
	}
	n.put("pools/default/c.img", "someone else's")
	if err := as.putBack(); err == nil {
		t.Fatal("put back over a file that took the name")
	}
	if b, _ := os.ReadFile(a); string(b) != "someone else's" {
		t.Errorf("the other file was replaced: %q", b)
	}
	if b, _ := os.ReadFile(n.pool(as.where())); string(b) != "mine" {
		t.Errorf("the file aside is gone: %q", b)
	}
}

func testRemoveID(t *testing.T) {
	n := newNode(t)
	a := n.put("pools/default/a.img", "mine")
	id := n.idOf(a)

	// Replaced by another file with the same name: kept, under its name, content untouched.
	os.Remove(a)
	n.put("pools/default/a.img", "someone else's")
	if got := n.ok("", "remove-id", a, id); got != "differs" {
		t.Fatalf("replaced file: %q", got)
	}
	if b, err := os.ReadFile(a); err != nil || string(b) != "someone else's" {
		t.Fatalf("the other file was touched: %q %v", b, err)
	}
	if len(n.glob("pools/default/*virticle-rm*")) != 0 {
		t.Error("a private name was left behind")
	}

	// The right file, but a process holds a lock on it (QEMU does on the images it has open): kept.
	id2 := n.idOf(a)
	lf, err := os.Open(a)
	if err != nil {
		t.Fatal(err)
	}
	lk := unix.Flock_t{Type: unix.F_RDLCK, Start: 100, Len: 1}
	if err := unix.FcntlFlock(lf.Fd(), unix.F_OFD_SETLK, &lk); err != nil {
		t.Fatal(err)
	}
	if got := n.ok("", "remove-id", a, id2); got != "inuse" || !n.exists("pools/default/a.img") {
		t.Fatalf("locked file: %q", got)
	}
	lf.Close()

	// The right file goes.
	if got := n.ok("", "remove-id", a, id2); got != "removed" || n.exists("pools/default/a.img") {
		t.Fatalf("own file: %q", got)
	}
	if got := n.ok("", "remove-id", a, id2); got != "missing" {
		t.Errorf("gone file: %q", got)
	}

	// Not a regular file: never removed.
	os.Mkdir(n.pool("dir"), 0o755)
	n.denied("a pool directory itself", "", "remove-id", n.abs("pools/default"), id2)
	if got := n.ok("", "remove-id", n.pool("dir"), id2); got != "differs" || !n.exists("pools/default/dir") {
		t.Errorf("directory: %q", got)
	}
	b := n.put("pools/default/b.img", "b")
	os.Symlink(b, n.pool("blink"))
	n.denied("a symbolic link", "", "remove-id", n.pool("blink"), n.idOf(b))
	if !n.exists("pools/default/b.img") {
		t.Error("the link target was removed")
	}
	n.refused("bad identity", "", "remove-id", b, "x; rm -rf /")
	n.refused("empty identity", "", "remove-id", b, "")
	n.denied("outside the pools", "", "remove-id", n.abs("outside/x"), id2)
}

func TestCreatedFilesReportTheirIdentity(t *testing.T) {
	n := newNode(t)
	// peer-recv answers with the identity of what it made.
	img := n.pool("new.img")
	out := n.ok("virticle-peer 1 recv 600 5 "+img+"\nhello", "peer-recv")
	if out != "@@received 5 "+n.idOf(img) {
		t.Errorf("peer-recv: %q", out)
	}
	// An existing file is never replaced and no identity is given for it.
	if out, code := n.run("virticle-peer 1 recv 600 5 "+img+"\nworld", "peer-recv"); code == 0 || strings.Contains(out, "@@received") {
		t.Errorf("existing file: %d %q", code, out)
	}

	// img-new: raw, sparse, the given size, mode 600, never over an existing file.
	raw := n.pool("disk.raw")
	out = n.ok("", "img-new", raw, "raw", "1048576")
	if out != n.idOf(raw) {
		t.Errorf("img-new answer %q", out)
	}
	if st, err := os.Stat(raw); err != nil || st.Size() != 1048576 || st.Mode().Perm() != 0o600 {
		t.Errorf("raw image: %v %v", st, err)
	}
	if _, code := n.run("", "img-new", raw, "raw", "10"); code == 0 {
		t.Error("img-new replaced a file")
	}
	n.refused("bad format", "", "img-new", n.pool("x"), "vmdk", "10")
	n.refused("bad size", "", "img-new", n.pool("x"), "raw", "-1")
	n.refused("backing for raw", "", "img-new", n.pool("x"), "raw", "10", raw, "raw")
	n.refused("half a backing", "", "img-new", n.pool("x"), "qcow2", "10", raw)
	n.denied("backing outside", "", "img-new", n.pool("x"), "qcow2", "10", n.abs("outside/b"), "raw")

	// qcow2 runs qemu-img on the part file.
	tool := n.abs("fake-qemu-img")
	n.put("fake-qemu-img", "#!/bin/sh\necho \"$@\" > "+n.abs("qargs")+"\n")
	os.Chmod(tool, 0o755)
	n.cfg.Tools["qemu-img"] = tool
	q := n.pool("disk.qcow2")
	out = n.ok("", "img-new", q, "qcow2", "2048", raw, "raw")
	if out != n.idOf(q) {
		t.Errorf("qcow2 answer %q", out)
	}
	if a, _ := os.ReadFile(n.abs("qargs")); strings.TrimSpace(string(a)) != "create -q -f qcow2 -b "+raw+" -F raw -- /proc/self/fd/3 2048" {
		t.Errorf("qemu-img args %q", a)
	}
}
