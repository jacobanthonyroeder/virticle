package nodecmd

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// verbCopyKill stops the qemu-img process that is writing the file DEST, such as the copy libvirt starts for
// a volume created from another volume. libvirt then fails the call and deletes the partial volume itself.
// The process is found by the file it holds open, so only a qemu-img that writes exactly this pool file is
// touched. It answers "killed N", or fails when none is running.
//
//	copy-kill DEST
func verbCopyKill(c *Ctx) error {
	if err := c.Need(1, 1, "copy-kill DEST"); err != nil {
		return err
	}
	r, err := c.ResolveRead(c.Args[0])
	if err != nil {
		return err
	}
	defer r.Close()
	proc := c.Cfg.proc()
	ents, _ := os.ReadDir(proc)
	var pids []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 || !procNamed(proc, pid, "qemu-img") {
			continue
		}
		fds, err := os.ReadDir(fmt.Sprintf("%s/%d/fd", proc, pid))
		if err != nil {
			continue
		}
		for _, fe := range fds {
			if t, err := os.Readlink(fmt.Sprintf("%s/%d/fd/%s", proc, pid, fe.Name())); err == nil && t == r.Path {
				pids = append(pids, pid)
				break
			}
		}
	}
	if len(pids) == 0 {
		return Fail("no copy into %s is running", c.Args[0])
	}
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	for i := 0; i < 20; i++ {
		alive := false
		for _, pid := range pids {
			alive = alive || procNamed(proc, pid, "qemu-img")
		}
		if !alive {
			break
		}
		time.Sleep(100 * time.Millisecond)
		if i == 14 {
			for _, pid := range pids {
				if procNamed(proc, pid, "qemu-img") {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			}
		}
	}
	fmt.Fprintf(c.Stdout, "killed %d\n", len(pids))
	return nil
}

// procNamed reports whether the process pid exists under proc and its name (comm) is name.
func procNamed(proc string, pid int, name string) bool {
	b, err := os.ReadFile(fmt.Sprintf("%s/%d/comm", proc, pid))
	return err == nil && strings.TrimSpace(string(b)) == name
}
