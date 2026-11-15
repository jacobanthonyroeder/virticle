package nodecmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The guest disk verbs: they look into or change the system on the disks of a
// shut-off VM with the guest disk tools (libguestfs).
//
//	guest-inspect DISK...    read only: what system is on the disks (JSON)
//	guest-customize DISK...  user, password, SSH keys, host name, first-boot packages (request on stdin)
//
// guest-inspect starts one guestfish appliance (--ro) and asks it, through
// guestfish's remote control, for the inspection result and a few files.
// guest-customize runs virt-customize with an option set and a guest script
// that this program builds itself from a checked request. The request comes on
// standard input, so the password hash is never on a command line.

func init() {
	Register("guest-inspect", verbGuestInspect)
	Register("guest-customize", verbGuestCustomize)
}

// guestDisks checks the disk arguments the way sysprep does (inside a root,
// not a link, a regular file, qcow2 with its chain inside the roots or raw)
// and returns the format and canonical path of each.
func guestDisks(c *Ctx) (formats, paths []string, err error) {
	for _, a := range c.Args {
		r, err := c.ResolveMod(a)
		if err != nil {
			return nil, nil, err
		}
		format, err := func() (string, error) {
			defer r.Close()
			st, exists, _ := r.Lstat()
			if !exists || !isRegMode(st.Mode) {
				return "", Fail("%s is not a regular file", a)
			}
			f, err := r.Open(unix.O_RDONLY, 0)
			if err != nil {
				return "", Fail("cannot read %s", a)
			}
			defer f.Close()
			format, err := sniffFormat(f)
			if err != nil {
				return "", Fail("cannot read %s", a)
			}
			switch format {
			case "qcow2":
				if err := c.CheckChain(r.Path); err != nil {
					return "", err
				}
			case "raw":
			default:
				return "", Fail("%s is neither qcow2 nor raw", a)
			}
			return format, nil
		}()
		if err != nil {
			return nil, nil, err
		}
		formats, paths = append(formats, format), append(paths, r.Path)
	}
	return formats, paths, nil
}

// GuestFacts is what guest-inspect prints, as JSON.
type GuestFacts struct {
	// Type is linux, windows, freebsd, ... as libguestfs names it, "" when no
	// system was found.
	Type              string `json:"type"`
	Distro            string `json:"distro,omitempty"`
	Product           string `json:"product,omitempty"`
	Major             string `json:"major,omitempty"`
	PackageFormat     string `json:"packageFormat,omitempty"`
	PackageManagement string `json:"packageManagement,omitempty"`
	// Encrypted: no system was found, and a disk holds an encrypted volume.
	Encrypted bool `json:"encrypted,omitempty"`
	// Systems is the number of systems found (more than one: multi-boot).
	Systems int `json:"systems"`
	// The files below are looked for only on Linux.
	CloudInit         bool `json:"cloudInit"`
	CloudInitDisabled bool `json:"cloudInitDisabled,omitempty"`
	Useradd           bool `json:"useradd"`
	Sudo              bool `json:"sudo"`
	GuestAgent        bool `json:"guestAgent"`
	SELinux           bool `json:"selinux"`
}

var guestfishPIDRE = regexp.MustCompile(`GUESTFISH_PID=([0-9]{1,10})`)

const guestInspectTimeout = 5 * time.Minute

func verbGuestInspect(c *Ctx) error {
	if err := c.Need(1, 8, "guest-inspect DISK..."); err != nil {
		return err
	}
	formats, paths, err := guestDisks(c)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(c.Context, guestInspectTimeout)
	defer cancel()
	cc := *c
	cc.Context = ctx
	args := []string{"--listen", "--ro"}
	for i := range paths {
		args = append(args, "--format="+formats[i], "-a", paths[i])
	}
	start, err := cc.Command("guestfish", args...)
	if err != nil {
		return err
	}
	start.Env = append(start.Env, "LIBGUESTFS_BACKEND=direct")
	// The server guestfish forks keeps standard error open: a file, not a
	// pipe, so that waiting for the starter does not wait for the server.
	ef, err := os.CreateTemp("", "virticle-guestfish-")
	if err != nil {
		return Fail("cannot create a temporary file: %v", err)
	}
	_ = os.Remove(ef.Name())
	defer ef.Close()
	start.Stderr = ef
	out, err := start.Output()
	m := guestfishPIDRE.FindSubmatch(out)
	if err != nil || m == nil {
		_, _ = ef.Seek(0, io.SeekStart)
		eb, _ := io.ReadAll(io.LimitReader(ef, 4096))
		return Fail("guestfish did not start: %s", strings.TrimSpace(string(eb)+" "+string(out)))
	}
	pid := string(m[1])
	stop := func() {
		// Not c.Context: this also runs after a signal has cancelled it.
		if p, err := c.Tool("guestfish"); err == nil {
			sctx, done := context.WithTimeout(context.Background(), 30*time.Second)
			q := exec.CommandContext(sctx, p, "--remote="+pid, "exit")
			q.Env = []string{"PATH=" + FixedPath, "LC_ALL=C", "LIBGUESTFS_BACKEND=direct"}
			q.Dir = "/"
			_ = q.Run()
			done()
		}
		// A server that did not exit is ended by its process ID, if it is still guestfish.
		if n, _ := strconv.Atoi(pid); n > 1 && procIsNamed(n, "guestfish") {
			_ = syscall.Kill(n, syscall.SIGTERM)
		}
	}
	undo := c.Cleanup(stop)
	defer func() { stop(); undo() }()
	remote := func(cmd string, a ...string) (string, error) {
		// The leading "-" keeps the server running when the command fails.
		q, err := cc.Command("guestfish", append([]string{"--remote=" + pid, "--", "-" + cmd}, a...)...)
		if err != nil {
			return "", err
		}
		q.Env = append(q.Env, "LIBGUESTFS_BACKEND=direct")
		var o, e bytes.Buffer
		q.Stdout, q.Stderr = &o, &e
		if err := q.Run(); err != nil {
			return "", Fail("guestfish %s: %s", cmd, strings.TrimSpace(e.String()))
		}
		if e.Len() > 0 {
			return "", fmt.Errorf("%s", strings.TrimSpace(e.String()))
		}
		return strings.TrimSpace(o.String()), nil
	}
	if _, err := remote("run"); err != nil {
		return Fail("the guest disk tools cannot start their appliance: %s", errMsg(err))
	}
	facts, err := inspectGuest(remote)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(facts)
	fmt.Fprintln(c.Stdout, string(b))
	return nil
}

// inspectGuest asks a running guestfish (through remote) about the system on
// its disks. A failing command counts as "not there".
func inspectGuest(remote func(cmd string, args ...string) (string, error)) (GuestFacts, error) {
	var f GuestFacts
	roots, err := remote("inspect-os")
	if err != nil {
		return f, Fail("inspection failed: %s", errMsg(err))
	}
	var list []string
	for _, l := range strings.Split(roots, "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "/dev/") {
			list = append(list, l)
		}
	}
	f.Systems = len(list)
	if len(list) == 0 {
		fs, _ := remote("list-filesystems")
		f.Encrypted = strings.Contains(fs, "crypto_LUKS") || strings.Contains(fs, "BitLocker") || strings.Contains(fs, "crypto_BITLK")
		return f, nil
	}
	root := list[0]
	get := func(what string) string {
		v, _ := remote("inspect-get-"+what, root)
		if v == "unknown" {
			return ""
		}
		return v
	}
	f.Type = get("type")
	f.Distro = get("distro")
	f.Product = get("product-name")
	f.Major = get("major-version")
	f.PackageFormat = get("package-format")
	f.PackageManagement = get("package-management")
	if f.Type != "linux" {
		return f, nil
	}
	// Mount the file systems of the system, shortest mount point first.
	mps, _ := remote("inspect-get-mountpoints", root)
	type mp struct{ dir, dev string }
	var mounts []mp
	for _, l := range strings.Split(mps, "\n") {
		dir, dev, ok := strings.Cut(strings.TrimSpace(l), ": ")
		if ok && strings.HasPrefix(dir, "/") && strings.HasPrefix(dev, "/dev/") {
			mounts = append(mounts, mp{dir, dev})
		}
	}
	sort.Slice(mounts, func(i, j int) bool { return len(mounts[i].dir) < len(mounts[j].dir) })
	if len(mounts) == 0 || mounts[0].dir != "/" {
		mounts = append([]mp{{"/", root}}, mounts...)
	}
	for _, m := range mounts {
		_, _ = remote("mount-ro", m.dev, m.dir)
	}
	file := func(paths ...string) bool {
		for _, p := range paths {
			if v, err := remote("is-file", p, "followsymlinks:true"); err == nil && v == "true" {
				return true
			}
		}
		return false
	}
	f.CloudInit = file("/usr/bin/cloud-init", "/usr/local/bin/cloud-init")
	if v, err := remote("exists", "/etc/cloud/cloud-init.disabled"); err == nil && v == "true" {
		f.CloudInitDisabled = true
	}
	f.Useradd = file("/usr/sbin/useradd", "/sbin/useradd", "/usr/bin/useradd")
	f.Sudo = file("/usr/bin/sudo")
	f.GuestAgent = file("/usr/bin/qemu-ga", "/usr/sbin/qemu-ga")
	f.SELinux = file("/etc/selinux/config")
	return f, nil
}

// GuestRequest is what guest-customize reads from standard input (JSON).
type GuestRequest struct {
	Hostname string `json:"hostname"`
	User     string `json:"user"`
	// PasswordHash is a SHA-512 crypt hash; the plain password never reaches the node.
	PasswordHash string   `json:"passwordHash"`
	SSHKeys      []string `json:"sshKeys"`
	// Packages are installed when the guest first starts.
	Packages []string `json:"packages"`
	// SELinux asks for a relabel (older virt-customize does not relabel by itself).
	SELinux bool `json:"selinux"`
}

var (
	guestHostRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)
	guestUserRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	guestHashRE = regexp.MustCompile(`^\$6\$(rounds=[0-9]{4,9}\$)?[./0-9A-Za-z]{1,16}\$[./0-9A-Za-z]{86}$`)
	guestPkgRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+_:=~-]{0,127}$`)
)

// check refuses anything the guest script could not take safely.
func (q GuestRequest) check() error {
	if q.Hostname != "" && (len(q.Hostname) > 253 || !guestHostRE.MatchString(q.Hostname)) {
		return Die("invalid host name")
	}
	if q.User != "" && !guestUserRE.MatchString(q.User) {
		return Die("invalid user name")
	}
	if q.User == "" && (q.PasswordHash != "" || len(q.SSHKeys) > 0) {
		return Die("a password or SSH key needs a user name")
	}
	if q.PasswordHash != "" && !guestHashRE.MatchString(q.PasswordHash) {
		return Die("the password is not a SHA-512 crypt hash")
	}
	if len(q.SSHKeys) > 64 || len(q.Packages) > 100 {
		return Die("too many SSH keys or packages")
	}
	for _, k := range q.SSHKeys {
		f := strings.Fields(k)
		if len(k) > 16384 || len(f) < 2 || !(strings.HasPrefix(f[0], "ssh-") || strings.HasPrefix(f[0], "ecdsa-") || strings.HasPrefix(f[0], "sk-")) {
			return Die("not an SSH public key")
		}
		for _, r := range k {
			if r < 0x20 || r == 0x7f {
				return Die("an SSH key contains a control character")
			}
		}
	}
	for _, p := range q.Packages {
		if !guestPkgRE.MatchString(p) {
			return Die("invalid package name %q", p)
		}
	}
	if q.Hostname == "" && q.User == "" && len(q.Packages) == 0 {
		return Die("nothing to change")
	}
	return nil
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// GuestScript is the script virt-customize runs inside the guest for q: it
// creates the user when missing (home directory, the sudo or wheel group,
// password-less sudo, as cloud-init's default user), sets the password hash
// and adds the SSH keys. "" when q has no user.
func GuestScript(q GuestRequest) string {
	if q.User == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(`#!/bin/sh
# Written by virticle-node guest-customize.
set -e
u=` + shq(q.User) + `
if ! getent passwd "$u" >/dev/null 2>&1; then
  g=
  for x in sudo wheel; do
    if getent group "$x" >/dev/null 2>&1; then g=$x; break; fi
  done
  s=/bin/sh
  if [ -x /bin/bash ]; then s=/bin/bash; fi
  if [ -n "$g" ]; then useradd -m -s "$s" -G "$g" "$u"; else useradd -m -s "$s" "$u"; fi
  mkdir -p /etc/sudoers.d
  chmod 750 /etc/sudoers.d
  printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$u" > "/etc/sudoers.d/90-virticle-$u"
  chmod 440 "/etc/sudoers.d/90-virticle-$u"
fi
`)
	if q.PasswordHash != "" {
		b.WriteString(`usermod -p ` + shq(q.PasswordHash) + ` "$u"
if [ -d /etc/ssh/sshd_config.d ]; then
  printf 'PasswordAuthentication yes\n' > /etc/ssh/sshd_config.d/00-virticle-password.conf
fi
`)
	}
	if len(q.SSHKeys) > 0 {
		b.WriteString(`h=$(getent passwd "$u" | cut -d: -f6)
gr=$(id -gn "$u")
if [ ! -d "$h" ]; then mkdir -p "$h"; chown "$u:$gr" "$h"; chmod 700 "$h"; fi
mkdir -p "$h/.ssh"
chown "$u:$gr" "$h/.ssh"
chmod 700 "$h/.ssh"
f=$h/.ssh/authorized_keys
touch "$f"
if [ -s "$f" ] && [ -n "$(tail -c 1 "$f")" ]; then echo >> "$f"; fi
for k in`)
		for _, k := range q.SSHKeys {
			b.WriteString(" \\\n  " + shq(strings.TrimSpace(k)))
		}
		b.WriteString(`; do
  grep -qxF -- "$k" "$f" || printf '%s\n' "$k" >> "$f"
done
chown "$u:$gr" "$f"
chmod 600 "$f"
`)
	}
	return b.String()
}

// GuestCustomizeArgs is the virt-customize command line for the disks and q.
// script is the path of the guest script ("" for none). Nothing secret is in
// it: the password hash is only in the script file.
func GuestCustomizeArgs(formats, paths []string, q GuestRequest, script string) []string {
	args := []string{"--no-network"}
	for i := range paths {
		args = append(args, "--format", formats[i], "-a", paths[i])
	}
	if q.Hostname != "" {
		args = append(args, "--hostname", q.Hostname)
	}
	if script != "" {
		args = append(args, "--run", script)
	}
	if len(q.Packages) > 0 {
		args = append(args, "--firstboot-install", strings.Join(q.Packages, ","))
	}
	if q.SELinux {
		args = append(args, "--selinux-relabel")
	}
	return args
}

func verbGuestCustomize(c *Ctx) error {
	if err := c.Need(1, 8, "guest-customize DISK..."); err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(c.Stdin, 1<<20+1))
	if err != nil || len(raw) > 1<<20 {
		return Die("the request on standard input is unreadable or too long")
	}
	var q GuestRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&q); err != nil {
		return Die("the request on standard input is not valid JSON")
	}
	if err := q.check(); err != nil {
		return err
	}
	formats, paths, err := guestDisks(c)
	if err != nil {
		return err
	}
	if _, err := c.Tool("virt-customize"); err != nil {
		return err
	}
	script := ""
	if s := GuestScript(q); s != "" {
		base := c.poolRunDir()
		if err := os.MkdirAll(base, 0o700); err != nil {
			return Fail("cannot create %s: %v", base, err)
		}
		dir, err := os.MkdirTemp(base, "guest-")
		if err != nil {
			return Fail("cannot create a work directory: %v", err)
		}
		rm := func() { _ = os.RemoveAll(dir) }
		undo := c.Cleanup(rm)
		defer func() { rm(); undo() }()
		script = filepath.Join(dir, "virticle-guest-setup.sh")
		if err := os.WriteFile(script, []byte(s), 0o700); err != nil {
			return Fail("cannot write the guest script: %v", err)
		}
	}
	cmd, err := c.Command("virt-customize", GuestCustomizeArgs(formats, paths, q, script)...)
	if err != nil {
		return err
	}
	cmd.Env = append(cmd.Env, "LIBGUESTFS_BACKEND=direct")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	c.Stdout.Write(out.Bytes())
	if err != nil {
		return ExitCode(ExitStatus(err))
	}
	return nil
}
