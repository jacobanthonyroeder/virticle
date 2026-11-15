package nodecmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var testHash = "$6$saltsalt$" + strings.Repeat("aB3./", 17) + "x"

func guestReq(t *testing.T, q GuestRequest) string {
	t.Helper()
	b, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestGuestCustomizeArgsHoldNoSecret(t *testing.T) {
	q := GuestRequest{Hostname: "web1", User: "admin", PasswordHash: testHash, SSHKeys: []string{"ssh-ed25519 AAAAC3Nz me@host"}, Packages: []string{"htop", "curl"}, SELinux: true}
	args := GuestCustomizeArgs([]string{"qcow2", "raw"}, []string{"/p/a.qcow2", "/p/b.img"}, q, "/run/x/virticle-guest-setup.sh")
	want := []string{"--no-network", "--format", "qcow2", "-a", "/p/a.qcow2", "--format", "raw", "-a", "/p/b.img",
		"--hostname", "web1", "--run", "/run/x/virticle-guest-setup.sh", "--firstboot-install", "htop,curl", "--selinux-relabel"}
	if strings.Join(args, "\n") != strings.Join(want, "\n") {
		t.Errorf("args\n%q\nwant\n%q", args, want)
	}
	for _, a := range args {
		if strings.Contains(a, "$6$") || strings.Contains(a, "AAAAC3Nz") {
			t.Errorf("secret or key on the command line: %q", a)
		}
	}
	// Only the host name: no script, no packages, no relabel.
	args = GuestCustomizeArgs([]string{"qcow2"}, []string{"/p/a.qcow2"}, GuestRequest{Hostname: "h"}, "")
	if strings.Join(args, " ") != "--no-network --format qcow2 -a /p/a.qcow2 --hostname h" {
		t.Errorf("host name only: %q", args)
	}
}

func TestGuestScript(t *testing.T) {
	if s := GuestScript(GuestRequest{Hostname: "h"}); s != "" {
		t.Errorf("no user, no script: %q", s)
	}
	s := GuestScript(GuestRequest{User: "admin", PasswordHash: testHash, SSHKeys: []string{"ssh-ed25519 AAAA it's me"}})
	for _, w := range []string{"u='admin'", "useradd -m", "for x in sudo wheel", "NOPASSWD:ALL", "usermod -p '" + testHash + "' \"$u\"",
		"PasswordAuthentication yes", `'ssh-ed25519 AAAA it'\''s me'`, `chmod 600 "$f"`, `chmod 700 "$h/.ssh"`} {
		if !strings.Contains(s, w) {
			t.Errorf("script lacks %q:\n%s", w, s)
		}
	}
	// Keys only: no password change, no password login.
	s = GuestScript(GuestRequest{User: "admin", SSHKeys: []string{"ssh-ed25519 AAAA"}})
	if strings.Contains(s, "usermod") || strings.Contains(s, "PasswordAuthentication") {
		t.Errorf("keys only:\n%s", s)
	}
}

func TestGuestCustomizeRefuses(t *testing.T) {
	n := newNode(t)
	raw := n.put("pools/default/raw.img", strings.Repeat("\x00", 4096))
	n.put("outside/outside.img", "x")
	ok := GuestRequest{Hostname: "h", User: "admin", PasswordHash: testHash}
	for _, p := range badPaths(n.abs("pools/default")) {
		n.refused("guest-customize "+p, guestReq(t, ok), "guest-customize", p)
	}
	n.refused("no disk", guestReq(t, ok), "guest-customize")
	n.refused("outside the pools", guestReq(t, ok), "guest-customize", n.abs("outside/outside.img"))
	n.refused("option as disk", guestReq(t, ok), "guest-customize", "--run")
	for why, stdin := range map[string]string{
		"empty":            "",
		"not JSON":         "user=admin",
		"unknown field":    `{"user":"admin","run":"rm -rf /"}`,
		"nothing":          `{}`,
		"bad user":         guestReq(t, GuestRequest{User: "Root;x"}),
		"bad host":         guestReq(t, GuestRequest{Hostname: "a b"}),
		"plain password":   guestReq(t, GuestRequest{User: "admin", PasswordHash: "secret"}),
		"password no user": guestReq(t, GuestRequest{PasswordHash: testHash}),
		"key no user":      guestReq(t, GuestRequest{SSHKeys: []string{"ssh-ed25519 AAAA"}}),
		"not a key":        guestReq(t, GuestRequest{User: "admin", SSHKeys: []string{"hello world"}}),
		"key with newline": guestReq(t, GuestRequest{User: "admin", SSHKeys: []string{"ssh-ed25519 AAAA\nrm -rf /"}}),
		"option package":   guestReq(t, GuestRequest{Packages: []string{"--run=x"}}),
		"comma package":    guestReq(t, GuestRequest{Packages: []string{"a,b"}}),
	} {
		n.refused(why, stdin, "guest-customize", raw)
	}
	n.cfg.Tools["virt-customize"] = ""
	if out, code := n.run(guestReq(t, ok), "guest-customize", raw); code != 1 || !strings.Contains(out, "virt-customize is not installed") {
		t.Errorf("no tool: exit %d: %s", code, out)
	}
}

func TestGuestCustomizeRuns(t *testing.T) {
	n := newNode(t)
	raw := n.put("pools/default/raw.img", strings.Repeat("\x00", 4096))
	n.put("pools/default/disk.qcow2", string(qcowHeader("")))
	rec := filepath.Join(t.TempDir(), "rec")
	// The stand-in records its arguments and the script it was given (which
	// must exist while it runs and be gone afterwards).
	n.cfg.Tools["virt-customize"] = fakeTool(t, "virt-customize", `for a in "$@"; do printf '%s\n' "$a"; done > `+rec+`.args
s=
while [ $# -gt 0 ]; do if [ "$1" = --run ]; then s=$2; fi; shift; done
cat "$s" > `+rec+`.script
stat -c %a "$s" > `+rec+`.mode
echo "$s" > `+rec+`.path
echo "backend=$LIBGUESTFS_BACKEND"
`)
	q := GuestRequest{Hostname: "web1", User: "admin", PasswordHash: testHash, SSHKeys: []string{"ssh-ed25519 AAAA me"}, Packages: []string{"htop"}}
	out := n.ok(guestReq(t, q), "guest-customize", raw, n.pool("disk.qcow2"))
	if !strings.Contains(out, "backend=direct") {
		t.Errorf("output %q", out)
	}
	args, _ := os.ReadFile(rec + ".args")
	if strings.Contains(string(args), "$6$") {
		t.Errorf("the hash is on the command line:\n%s", args)
	}
	if !strings.Contains(string(args), "--format\nraw\n-a\n"+raw+"\n--format\nqcow2\n-a\n"+n.pool("disk.qcow2")+"\n--hostname\nweb1\n--run\n") ||
		!strings.HasSuffix(string(args), "--firstboot-install\nhtop\n") {
		t.Errorf("arguments:\n%s", args)
	}
	script, _ := os.ReadFile(rec + ".script")
	if string(script) != GuestScript(q) {
		t.Errorf("script given to virt-customize:\n%s", script)
	}
	if mode, _ := os.ReadFile(rec + ".mode"); strings.TrimSpace(string(mode)) != "700" {
		t.Errorf("script mode %s", mode)
	}
	p, _ := os.ReadFile(rec + ".path")
	if dir := filepath.Dir(strings.TrimSpace(string(p))); !strings.HasPrefix(dir, n.abs("run/virticle-node/guest-")) {
		t.Errorf("script directory %s", dir)
	} else if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the script directory is left behind: %v", err)
	}
	// The status of virt-customize is passed on.
	n.cfg.Tools["virt-customize"] = fakeTool(t, "virt-customize", "echo failed; exit 3\n")
	if o, code := n.run(guestReq(t, q), "guest-customize", raw); code != 3 || !strings.Contains(o, "failed") {
		t.Errorf("failure: exit %d: %s", code, o)
	}
}

// fakeGuestfish answers the remote commands of guest-inspect like a guestfish
// with a Debian system that has no cloud-init; its arguments go to rec.
func fakeGuestfish(t *testing.T, rec, answers string) string {
	return fakeTool(t, "guestfish", `printf '%s\n' "$*" >> `+rec+`
case "$1" in
--listen) echo "GUESTFISH_PID=999999; export GUESTFISH_PID"; exit 0 ;;
esac
[ "$1" = --remote=999999 ] || { echo "bad remote $1" >&2; exit 9; }
shift
[ "$1" = exit ] && exit 0
[ "$1" = -- ] && shift
cmd=${1#-}; shift
`+answers)
}

func TestGuestInspect(t *testing.T) {
	n := newNode(t)
	raw := n.put("pools/default/raw.img", strings.Repeat("\x00", 4096))
	n.refused("no disk", "", "guest-inspect")
	n.put("outside/outside.img", "x")
	n.refused("outside the pools", "", "guest-inspect", n.abs("outside/outside.img"))
	rec := filepath.Join(t.TempDir(), "rec")
	n.cfg.Tools["guestfish"] = fakeGuestfish(t, rec, `case "$cmd $*" in
"run ") ;;
"inspect-os ") echo /dev/sda1 ;;
"inspect-get-type /dev/sda1") echo linux ;;
"inspect-get-distro /dev/sda1") echo debian ;;
"inspect-get-product-name /dev/sda1") echo 13.7 ;;
"inspect-get-major-version /dev/sda1") echo 13 ;;
"inspect-get-package-format /dev/sda1") echo deb ;;
"inspect-get-package-management /dev/sda1") echo apt ;;
"inspect-get-mountpoints /dev/sda1") printf '/boot/efi: /dev/sda15\n/: /dev/sda1\n' ;;
"mount-ro "*) ;;
"is-file /usr/sbin/useradd followsymlinks:true"|"is-file /usr/bin/sudo followsymlinks:true") echo true ;;
"is-file "*) echo false ;;
"exists "*) echo false ;;
*) echo "libguestfs: error: unexpected $cmd" >&2 ;;
esac
`)
	out := n.ok("", "guest-inspect", raw)
	var f GuestFacts
	if err := json.Unmarshal([]byte(out), &f); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	want := GuestFacts{Type: "linux", Distro: "debian", Product: "13.7", Major: "13", PackageFormat: "deb", PackageManagement: "apt", Systems: 1, Useradd: true, Sudo: true}
	if f != want {
		t.Errorf("facts %+v\nwant  %+v", f, want)
	}
	log, _ := os.ReadFile(rec)
	if !strings.HasPrefix(string(log), "--listen --ro --format=raw -a "+raw+"\n") {
		t.Errorf("start: %s", log)
	}
	// The root is mounted before /boot/efi, and the server is told to exit.
	if i, j := strings.Index(string(log), "-mount-ro /dev/sda1 /"), strings.Index(string(log), "-mount-ro /dev/sda15 /boot/efi"); i < 0 || j < i {
		t.Errorf("mount order:\n%s", log)
	}
	if !strings.Contains(string(log), "--remote=999999 exit") {
		t.Errorf("no exit:\n%s", log)
	}

	// No system, an encrypted volume.
	_ = os.Remove(rec)
	n.cfg.Tools["guestfish"] = fakeGuestfish(t, rec, `case "$cmd" in
inspect-os) ;;
list-filesystems) echo "/dev/sda1: crypto_LUKS" ;;
esac
`)
	out = n.ok("", "guest-inspect", raw)
	if f = (GuestFacts{}); json.Unmarshal([]byte(out), &f) != nil || f.Type != "" || !f.Encrypted || f.Systems != 0 {
		t.Errorf("encrypted: %s", out)
	}
	// The appliance does not start.
	n.cfg.Tools["guestfish"] = fakeGuestfish(t, rec, `[ "$cmd" = run ] && echo "libguestfs: error: could not create appliance" >&2
`)
	if out, code := n.run("", "guest-inspect", raw); code != 1 || !strings.Contains(out, "could not create appliance") {
		t.Errorf("no appliance: exit %d: %s", code, out)
	}
}
