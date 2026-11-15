package api

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestChooseSetup(t *testing.T) {
	all := guestTools{guestfish: true, virtCustomize: true}
	debian := guestFacts{Type: "linux", Distro: "debian", Product: "13.7", Major: "13", PackageManagement: "apt", Systems: 1, Useradd: true}
	withCI := debian
	withCI.CloudInit = true
	disabledCI := withCI
	disabledCI.CloudInitDisabled = true
	rocky := guestFacts{Type: "linux", Distro: "rocky", Product: "Rocky Linux 9.6 (Blue Onyx)", Major: "9", PackageManagement: "dnf", Systems: 1, Useradd: true, SELinux: true}
	nixos := guestFacts{Type: "linux", Distro: "nixos", Major: "25", Systems: 1, Useradd: true}
	noUseradd := debian
	noUseradd.Useradd = false
	win := guestFacts{Type: "windows", Distro: "windows", Product: "Windows Server 2022 Standard", Systems: 1}
	bsd := guestFacts{Type: "freebsd", Distro: "freebsd", Major: "14", Systems: 1}
	cases := []struct {
		name      string
		guest     string
		tools     guestTools
		facts     *guestFacts
		err       error
		setup     string
		problem   string
		unchecked string
		system    string
	}{
		{"windows family: answer file, never looked into", "windows", guestTools{}, nil, nil, setupUnattend, "", "", ""},
		{"no tools: seed as before, said so", "linux", guestTools{}, nil, nil, setupSeed, "", "no guest disk tools", ""},
		{"cloud-init: seed disc", "linux", all, &withCI, nil, setupSeed, "", "", "Debian 13"},
		{"cloud-init without virt-customize: seed disc", "linux", guestTools{guestfish: true}, &withCI, nil, setupSeed, "", "", "Debian 13"},
		{"no cloud-init: into the disk", "linux", all, &debian, nil, setupDisk, "", "", "Debian 13"},
		{"cloud-init disabled: into the disk", "linux", all, &disabledCI, nil, setupDisk, "", "", "Debian 13"},
		{"RHEL family: into the disk", "linux", all, &rocky, nil, setupDisk, "", "", "Rocky Linux 9.6 (Blue Onyx)"},
		{"no virt-customize", "linux", guestTools{guestfish: true}, &debian, nil, "", "virt-customize is not installed", "", "Debian 13"},
		{"unknown distribution", "linux", all, &nixos, nil, "", "NixOS 25 without cloud-init: the guest disk tools cannot set it up", "", "NixOS 25"},
		{"no useradd", "linux", all, &noUseradd, nil, "", "no useradd in Debian 13", "", "Debian 13"},
		{"nothing recognised", "linux", all, &guestFacts{}, nil, "", "no system the guest disk tools recognise", "", ""},
		{"encrypted", "linux", all, &guestFacts{Encrypted: true}, nil, "", "the disk is encrypted", "", ""},
		{"Windows on a Linux-defined VM", "linux", all, &win, nil, "", "not defined as a Windows guest", "", "Windows Server 2022 Standard"},
		{"another system", "linux", all, &bsd, nil, "", "freebsd 14 is not supported", "", "freebsd 14"},
		{"inspection failed", "linux", all, nil, errors.New("appliance failed"), "", "could not look into the disk: appliance failed", "", ""},
	}
	for _, c := range cases {
		g := chooseSetup(c.guest, c.tools, c.facts, c.err)
		if g.Setup != c.setup || !strings.Contains(g.Problem, c.problem) || (c.problem == "") != (g.Problem == "") ||
			!strings.Contains(g.Unchecked, c.unchecked) || (c.unchecked == "") != (g.Unchecked == "") || g.System != c.system {
			t.Errorf("%s: got %+v", c.name, g)
		}
	}
}

func TestDiskRequest(t *testing.T) {
	debian := &guestFacts{Type: "linux", Distro: "debian", Major: "13", PackageManagement: "apt", Systems: 1, Useradd: true}
	g := guestSetup{Setup: setupDisk, System: "Debian 13", facts: debian}
	c := cloneCustomize{User: "bob", Password: "s3cret pass", SSHKeys: []string{" ssh-ed25519 AAAA bob@x ", ""}, Packages: []string{"htop"}}
	out, err := g.diskRequest("web1", c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "s3cret") {
		t.Fatalf("plain password in the request: %s", out)
	}
	var q struct {
		Hostname, User, PasswordHash string
		SSHKeys, Packages            []string
		SELinux                      bool
	}
	if err := json.Unmarshal([]byte(out), &q); err != nil {
		t.Fatal(err)
	}
	if q.Hostname != "web1" || q.User != "bob" || !strings.HasPrefix(q.PasswordHash, "$6$") || len(q.SSHKeys) != 1 || q.SSHKeys[0] != "ssh-ed25519 AAAA bob@x" || q.SELinux {
		t.Errorf("request %+v", q)
	}
	// The guest agent (as the seed disc brings it) and sudo (for the user's group) are added when missing.
	if strings.Join(q.Packages, " ") != "htop qemu-guest-agent sudo" {
		t.Errorf("packages %q", q.Packages)
	}
	debian.GuestAgent, debian.Sudo, debian.SELinux = true, true, true
	out, _ = g.diskRequest("web1", cloneCustomize{User: "bob", SSHKeys: []string{"ssh-ed25519 AAAA"}})
	if strings.Contains(out, "packages") || strings.Contains(out, "passwordHash") || !strings.Contains(out, `"selinux":true`) {
		t.Errorf("keys only on a guest with agent and sudo: %s", out)
	}

	for name, cc := range map[string]cloneCustomize{
		"profile":      {User: "bob", Profile: 3},
		"raw":          {RawUserData: "#cloud-config\n"},
		"bad user":     {User: "Bob!"},
		"pw, no user":  {Password: "x"},
		"bad package":  {User: "bob", Packages: []string{"--x"}},
		"not a key":    {User: "bob", SSHKeys: []string{"hello"}},
		"bad hostname": {Hostname: "a b", User: "bob"},
	} {
		if _, err := g.diskRequest("web1", cc); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Packages where the package manager is unknown.
	g2 := guestSetup{Setup: setupDisk, System: "X", facts: &guestFacts{Type: "linux", Distro: "debian", Useradd: true}}
	if _, err := g2.diskRequest("web1", cloneCustomize{Packages: []string{"htop"}}); err == nil || !strings.Contains(err.Error(), "package manager") {
		t.Errorf("unknown package manager: %v", err)
	}
	// Not possible: refused with the reason.
	no := guestSetup{Problem: "the disk is encrypted"}
	if _, err := no.diskRequest("web1", cloneCustomize{User: "bob"}); err == nil || !strings.Contains(err.Error(), "the disk is encrypted") {
		t.Errorf("problem: %v", err)
	}
	// The seed disc: nothing for the disk.
	if out, err := (guestSetup{Setup: setupSeed}).diskRequest("web1", cloneCustomize{User: "bob"}); err != nil || out != "" {
		t.Errorf("seed: %q %v", out, err)
	}
}
