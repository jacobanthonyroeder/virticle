package nodecmd

import (
	"fmt"
	"os"
	"strconv"
	"strings"

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
// numbers are not used: an NFS mount (and an LVM volume) can get another one after a reboot.
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

// verbRemoveID removes a file only when it is the one with the identity given. The file is first renamed to a
// private name in the same directory (atomic: whatever file has the name at that instant is the one renamed), the
// identity is taken from the renamed file, and only then is it unlinked, or renamed back. So no other file can take
// the name between the check and the removal. A file system without RENAME_NOREPLACE (NFS) gets a plain rename to
// a random name that does not exist, and the way back is a hard link, which never replaces anything either.
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
	dfd := r.DirFD()
	tmp := ""
	plain := false
	for i := 0; i < 20; i++ {
		t := r.Name + ".virticle-rm." + randName(6)
		err = unix.Renameat2(dfd, r.Name, dfd, t, unix.RENAME_NOREPLACE)
		if errIs(err, unix.EINVAL) || errIs(err, unix.ENOSYS) || errIs(err, unix.ENOTSUP) {
			if _, serr := fstatat(dfd, t); serr == nil || !isNotExist(serr) {
				continue
			}
			plain = true
			err = unix.Renameat(dfd, r.Name, dfd, t)
		}
		if errIs(err, unix.EEXIST) {
			continue
		}
		tmp = t
		break
	}
	if err != nil {
		if isNotExist(err) {
			fmt.Fprintln(c.Stdout, "missing")
			return nil
		}
		return Fail("cannot remove %s: %v", arg, err)
	}
	id, _, regular, oerr := openIdentity(dfd, tmp)
	answer := "differs"
	if oerr == nil && regular && id != "" && id == want {
		answer = "inuse"
		if !lockedByOthers(dfd, tmp) {
			if err := unix.Unlinkat(dfd, tmp, 0); err != nil {
				return Fail("cannot remove %s (it is now named %s): %v", arg, tmp, err)
			}
			fmt.Fprintln(c.Stdout, "removed")
			return nil
		}
	}
	// Not the file: put it back under its name, without replacing a file that may have appeared there.
	var back error
	if plain {
		if back = unix.Linkat(dfd, tmp, dfd, r.Name, 0); back == nil {
			back = unix.Unlinkat(dfd, tmp, 0)
		}
	} else {
		back = unix.Renameat2(dfd, tmp, dfd, r.Name, unix.RENAME_NOREPLACE)
	}
	if back != nil {
		return Fail("%s was kept, but it is now named %s: %v", arg, r.Dir+"/"+tmp, back)
	}
	fmt.Fprintln(c.Stdout, answer)
	return nil
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
