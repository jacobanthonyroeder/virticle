package nodecmd

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// File identity: what lets the controller prove later that a file is the one it made, and not another file that
// took its name meanwhile. Used by migration (internal/api/migjournal.go).
//
// The identity is the file's handle (name_to_handle_at): the kernel's own stable name of the inode, the one NFS
// servers give out. On ext4, XFS and btrfs it holds the inode number and the inode's generation, which changes when
// an inode number is used again, so a file deleted and made again under the same name gets another handle. It does
// not change while the file is written to, renamed, chowned or relabelled, and it survives a reboot and a remount.
// On an NFS client it is built from the server's file handle, so it survives a remount and is the same on every
// node that mounts the same export: the same file seen through a share has the same identity on both nodes. Device
// numbers are not used: an NFS mount (and an LVM volume) can get another one after a reboot. Checked in the lab
// (Linux 6.12 clients, knfsd on XFS): both clients, mounting with NFS 3, 4.1 or 4.2, report the same handle for
// a file; it holds the server's inode generation, so a file deleted and made again under the same name (even with
// the same inode number) has another one; it is the same after the server restarted and after a remount.
//
// Where a file system gives no handle, the identity is the inode number with the birth time (statx). Where it has
// neither, there is no identity, and a file without one is never removed by remove-id.
//
//	file-id PATH...      one line per path: "MISSING", "OTHER" (not a regular file) or "ID|SIZE|BLOCKS"
//	                     (ID "-" when the file system offers no identity)
//	remove-id PATH ID    removes the regular file PATH only when its identity is ID and no process holds a
//	                     lock on it; prints "removed", "missing", "differs" or "inuse" (in the last two
//	                     cases the file is left as it was)
//	img-new PATH FORMAT BYTES [BACKING BACKINGFORMAT]
//	                     creates a new empty disk image (raw or qcow2) that never replaces anything; prints its ID

func init() {
	Register("file-id", verbFileID)
	Register("remove-id", verbRemoveID)
	Register("img-new", verbImgNew)
}

// fdIdentity is the identity of the open file fd. "" when the file system offers none.
func fdIdentity(fd int) string {
	return atIdentity(fd, "", unix.AT_EMPTY_PATH)
}

func atIdentity(dirfd int, name string, flags int) string {
	if h, _, err := unix.NameToHandleAt(dirfd, name, flags); err == nil && h.Size() > 0 {
		return fmt.Sprintf("fh-%d-%x", h.Type(), h.Bytes())
	}
	var stx unix.Statx_t
	if err := unix.Statx(dirfd, name, flags|unix.AT_SYMLINK_NOFOLLOW, unix.STATX_INO|unix.STATX_BTIME, &stx); err == nil &&
		stx.Mask&unix.STATX_BTIME != 0 && stx.Mask&unix.STATX_INO != 0 {
		return fmt.Sprintf("ib-%d-%d.%09d", stx.Ino, stx.Btime.Sec, stx.Btime.Nsec)
	}
	return ""
}

// ValidIdentity reports whether s has the form fdIdentity gives (and nothing a shell or a header could misread).
func ValidIdentity(s string) bool {
	if len(s) < 4 || len(s) > 300 || !(strings.HasPrefix(s, "fh-") || strings.HasPrefix(s, "ib-")) {
		return false
	}
	return strings.Trim(s[3:], "0123456789abcdef-.") == ""
}

// openIdentity opens name below dirfd (no symbolic link, no device) and returns its identity, its stat, and
// whether it is a regular file. Opening, rather than asking by name, makes an NFS client look the name up again
// at the server, so a file another node put there meanwhile is the one seen.
func openIdentity(dirfd int, name string) (id string, st unix.Stat_t, regular bool, err error) {
	fd, err := openat(dirfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY, 0)
	if err != nil {
		return "", st, false, err
	}
	defer unix.Close(fd)
	if err := unix.Fstat(fd, &st); err != nil {
		return "", st, false, err
	}
	if !isRegMode(st.Mode) {
		return "", st, false, nil
	}
	return fdIdentity(fd), st, true, nil
}

func verbFileID(c *Ctx) error {
	if err := c.Need(1, 200, "file-id PATH..."); err != nil {
		return err
	}
	if err := checkPlain(c.Args...); err != nil {
		return err
	}
	for _, a := range c.Args {
		r, err := c.parentRef(a)
		if err != nil {
			return err
		}
		lst, exists, err := r.Lstat()
		if err != nil {
			r.Close()
			return Fail("cannot stat %s", a)
		}
		if !exists {
			r.Close()
			fmt.Fprintln(c.Stdout, "MISSING")
			continue
		}
		if !isRegMode(lst.Mode) {
			r.Close()
			fmt.Fprintln(c.Stdout, "OTHER")
			continue
		}
		id, st, regular, err := openIdentity(r.DirFD(), r.Name)
		r.Close()
		switch {
		case err != nil && isNotExist(err):
			fmt.Fprintln(c.Stdout, "MISSING")
			continue
		case errIs(err, unix.ELOOP):
			fmt.Fprintln(c.Stdout, "OTHER")
			continue
		case err != nil:
			return Fail("cannot open %s", a)
		case !regular:
			fmt.Fprintln(c.Stdout, "OTHER")
			continue
		}
		if id == "" {
			id = "-"
		}
		fmt.Fprintf(c.Stdout, "%s|%d|%d\n", id, st.Size, st.Blocks*512)
	}
	return nil
}

// renameNoReplace is renameat2 with RENAME_NOREPLACE; a variable so that tests can play a file system without it.
var renameNoReplace = func(olddirfd int, oldname string, newdirfd int, newname string) error {
	return unix.Renameat2(olddirfd, oldname, newdirfd, newname, unix.RENAME_NOREPLACE)
}

// noReplaceMissing says that err is the answer of a file system that has no RENAME_NOREPLACE. The Linux NFS client
// answers EINVAL (checked on NFS 3, 4.1 and 4.2 with kernel 6.12) when the new name does not exist; an existing new
// name is refused with EEXIST before the file system is asked.
func noReplaceMissing(err error) bool {
	return errIs(err, unix.EINVAL) || errIs(err, unix.ENOSYS) || errIs(err, unix.ENOTSUP)
}

// verbRemoveID removes a file only when it is the one with the identity given. The file is first moved to a
// private name (atomic: whatever file has the name at that instant is the one moved), the identity is taken from
// the moved file, and only then is it unlinked, or put back. So no other file can take the name between the check
// and the removal.
//
// The private name is NAME.virticle-rm.RANDOM in the same directory, made with RENAME_NOREPLACE. A file system
// without it (NFS) gets a new private directory instead, made with mkdir, which fails when the name exists, so the
// directory is this call's own and empty: the file is renamed into it, where there is nothing to replace. The way
// back is a hard link, which never replaces anything either. When the process is stopped by a signal in between,
// the file is put back too.
func verbRemoveID(c *Ctx) error {
	if err := c.Need(2, 2, "remove-id PATH ID"); err != nil {
		return err
	}
	arg, want := c.Args[0], c.Args[1]
	if !ValidIdentity(want) {
		return Die("unsupported identity")
	}
	r, err := c.ResolveMod(arg)
	if err != nil {
		return err
	}
	defer r.Close()
	st, exists, err := r.Lstat()
	if err != nil {
		return Fail("cannot stat %s", arg)
	}
	if !exists {
		fmt.Fprintln(c.Stdout, "missing")
		return nil
	}
	if !isRegMode(st.Mode) {
		fmt.Fprintln(c.Stdout, "differs")
		return nil
	}
	a, err := asideOpen(r.DirFD(), r.Name)
	if err != nil {
		if isNotExist(err) {
			fmt.Fprintln(c.Stdout, "missing")
			return nil
		}
		return Fail("cannot remove %s: %v", arg, err)
	}
	cancel := c.Cleanup(func() { a.putBack() })
	defer cancel()
	id, _, regular, oerr := openIdentity(a.fd, a.name)
	answer := "differs"
	if oerr == nil && regular && id != "" && id == want {
		answer = "inuse"
		if !lockedByOthers(a.fd, a.name) {
			if err := a.remove(); err != nil {
				return Fail("cannot remove %s (it is now named %s): %v", arg, r.Dir+"/"+a.where(), err)
			}
			fmt.Fprintln(c.Stdout, "removed")
			return nil
		}
	}
	// Not the file: put it back under its name, without replacing a file that may have appeared there.
	if err := a.putBack(); err != nil {
		return Fail("%s was kept, but it is now named %s: %v", arg, r.Dir+"/"+a.where(), err)
	}
	fmt.Fprintln(c.Stdout, answer)
	return nil
}

// aside is a file moved to a private name by asideOpen.
type aside struct {
	dirfd int    // the directory the file came from
	orig  string // its name there
	fd    int    // the directory it is in now (dirfd, or the private directory)
	name  string // its name in fd
	sub   string // the private directory below dirfd, "" when it is next to orig
	mu    sync.Mutex
	done  bool // removed or put back
}

// asideOpen moves the file named orig below dirfd to a private name; see verbRemoveID.
func asideOpen(dirfd int, orig string) (*aside, error) {
	var err error
	for i := 0; i < 20; i++ {
		t := orig + ".virticle-rm." + randName(6)
		err = renameNoReplace(dirfd, orig, dirfd, t)
		switch {
		case err == nil:
			return &aside{dirfd: dirfd, orig: orig, fd: dirfd, name: t}, nil
		case errIs(err, unix.EEXIST):
			continue
		case noReplaceMissing(err):
			return asideDir(dirfd, orig)
		}
		return nil, err
	}
	return nil, err
}

// asideDir is asideOpen in a new private directory, for a file system without RENAME_NOREPLACE.
func asideDir(dirfd int, orig string) (*aside, error) {
	var err error
	for i := 0; i < 20; i++ {
		d := orig + ".virticle-rm." + randName(6)
		if err = unix.Mkdirat(dirfd, d, 0o700); errIs(err, unix.EEXIST) {
			continue
		} else if err != nil {
			return nil, err
		}
		sfd, err := openat(dirfd, d, oDir, 0)
		if err != nil {
			_ = unix.Unlinkat(dirfd, d, unix.AT_REMOVEDIR)
			return nil, err
		}
		// The directory is new and empty: the rename cannot replace anything in it.
		if err := unix.Renameat(dirfd, orig, sfd, "f"); err != nil {
			unix.Close(sfd)
			_ = unix.Unlinkat(dirfd, d, unix.AT_REMOVEDIR)
			return nil, err
		}
		return &aside{dirfd: dirfd, orig: orig, fd: sfd, name: "f", sub: d}, nil
	}
	return nil, err
}

func (a *aside) where() string {
	if a.sub != "" {
		return a.sub + "/" + a.name
	}
	return a.name
}

// remove unlinks the file and drops the private directory.
func (a *aside) remove() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.done {
		return nil
	}
	if err := unix.Unlinkat(a.fd, a.name, 0); err != nil {
		return err
	}
	a.done = true
	a.dropDir()
	return nil
}

// putBack gives the file its name again, never replacing a file that took the name meanwhile.
func (a *aside) putBack() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.done {
		return nil
	}
	var err error
	if a.sub == "" {
		err = renameNoReplace(a.fd, a.name, a.dirfd, a.orig)
	} else if err = unix.Linkat(a.fd, a.name, a.dirfd, a.orig, 0); err == nil {
		err = unix.Unlinkat(a.fd, a.name, 0)
	}
	if err != nil {
		return err
	}
	a.done = true
	a.dropDir()
	return nil
}

// dropDir removes the private directory. On NFS it stays when the file was open on this node when it was unlinked
// (the client keeps such a file as .nfsXXXX until it is closed); it is empty then once the file is closed.
func (a *aside) dropDir() {
	if a.sub == "" {
		return
	}
	unix.Close(a.fd)
	_ = unix.Unlinkat(a.dirfd, a.sub, unix.AT_REMOVEDIR)
}

// lockedByOthers says whether a process holds a lock on the file (QEMU locks the images it has open). A file
// whose locks cannot be read counts as locked.
func lockedByOthers(dirfd int, name string) bool {
	fd, err := openat(dirfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY, 0)
	if err != nil {
		return true
	}
	defer unix.Close(fd)
	lk := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	if err := unix.FcntlFlock(uintptr(fd), unix.F_OFD_GETLK, &lk); err != nil {
		return true
	}
	return lk.Type != unix.F_UNLCK
}

func verbImgNew(c *Ctx) error {
	if err := c.Need(3, 5, "img-new PATH FORMAT BYTES [BACKING BACKINGFORMAT]"); err != nil {
		return err
	}
	if len(c.Args) == 4 {
		return Die("usage: img-new PATH FORMAT BYTES [BACKING BACKINGFORMAT]")
	}
	format, sizeArg := c.Args[1], c.Args[2]
	if format != "raw" && format != "qcow2" {
		return Die("unsupported format")
	}
	if !numOK(sizeArg) {
		return Die("unsupported size")
	}
	size, err := strconv.ParseInt(sizeArg, 10, 64)
	if err != nil || size < 1 {
		return Die("unsupported size")
	}
	var backing, bfmt string
	if len(c.Args) == 5 {
		backing, bfmt = c.Args[3], c.Args[4]
		if format != "qcow2" {
			return Die("only a qcow2 image has a backing file")
		}
		switch bfmt {
		case "raw", "qcow2":
		default:
			return Die("unsupported backing format")
		}
		br, err := c.ResolveRead(backing)
		if err != nil {
			return err
		}
		bst, exists, _ := br.Lstat()
		br.Close()
		if !exists || !isRegMode(bst.Mode) {
			return Fail("the backing file %s is not a regular file", backing)
		}
		if bfmt == "qcow2" {
			if err := c.CheckChain(backing); err != nil {
				return err
			}
		}
	}
	if format == "qcow2" && !c.HasTool("qemu-img") {
		return Fail("qemu-img is not installed on this node")
	}
	dst, err := c.ResolveNew(c.Args[0])
	if err != nil {
		return err
	}
	defer dst.Close()
	id, err := c.CreateNewID(dst, 0o600, func(f *os.File) error {
		if format == "raw" {
			if err := f.Truncate(size); err != nil {
				return Fail("cannot size %s: %v", c.Args[0], err)
			}
			return nil
		}
		args := []string{"create", "-q", "-f", "qcow2"}
		if backing != "" {
			args = append(args, "-b", backing, "-F", bfmt)
		}
		args = append(args, "--", "/proc/self/fd/3", sizeArg)
		cmd, err := c.Command("qemu-img", args...)
		if err != nil {
			return err
		}
		cmd.ExtraFiles = []*os.File{f}
		if out, err := cmd.CombinedOutput(); err != nil {
			return Fail("qemu-img create failed: %s", strings.TrimSpace(string(out)))
		}
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintln(c.Stdout, idOrDash(id))
	return nil
}

func idOrDash(id string) string {
	if id == "" {
		return "-"
	}
	return id
}
