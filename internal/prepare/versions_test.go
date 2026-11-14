package prepare

import (
	"strings"
	"testing"
)

// factsWith adds lines to the Debian 13 facts (a host with no libvirt yet).
func factsWith(extra string) string { return debian13Facts + extra }

const installedOld = "virsh=/usr/bin/virsh\nlibvirt_version=10.0.0\nqemu_version=QEMU emulator version 8.2.2 (Debian 1:8.2.2+ds-0ubuntu1)\n"
const installedGood = "virsh=/usr/bin/virsh\nlibvirt_version=11.3.0\nqemu_version=QEMU emulator version 10.0.2 (Debian 1:10.0.2+ds-2)\n"

func TestParseFactsVersions(t *testing.T) {
	f := ParseFacts(installedGood + "cand_libvirt=1:11.3.0-3\ncand_qemu=1:10.0.2+ds-2\n")
	if f.LibvirtVersion != "11.3.0" || !strings.Contains(f.QEMUVersion, "10.0.2") || f.CandLibvirt != "1:11.3.0-3" || f.CandQEMU != "1:10.0.2+ds-2" {
		t.Errorf("%+v", f)
	}
}

func TestVersionRow(t *testing.T) {
	o := Options{ControllerKey: testKey}
	needs := "Virticle needs libvirt 11.0 and QEMU 10.0 or newer."
	cases := []struct {
		name, facts, status string
		has                 []string
	}{
		{"Debian 13 installed", strings.Replace(debian13Facts, "virsh=\n", installedGood, 1), OK, []string{"libvirt 11.3.0 and QEMU 10.0.2"}},
		{"Rocky 10 installed", rocky10Facts, OK, []string{"libvirt 11.10.0 and QEMU 10.1.0"}},
		{"installed and too old", strings.Replace(debian13Facts, "virsh=\n", installedOld, 1), Error,
			[]string{"This node has libvirt 10.0.0 and QEMU 8.2.2. " + needs, "Nothing was changed"}},
		{"QEMU too old only", strings.Replace(debian13Facts, "virsh=\n", strings.Replace(installedGood, "10.0.2", "9.2.4", 1), 1), Error,
			[]string{"libvirt 11.3.0 and QEMU 9.2.4"}},
		{"not installed, repositories new enough", factsWith("cand_libvirt=1:11.3.0-3\ncand_qemu=1:10.0.2+ds-2\n"), Info,
			[]string{"would install libvirt 11.3.0 and QEMU 10.0.2", "Debian GNU/Linux 13 (trixie)"}},
		{"not installed, repositories too old", factsWith("cand_libvirt=9.0.0-4+deb12u1\ncand_qemu=1:7.2.15+ds-1\n"), Error,
			[]string{"Debian GNU/Linux 13 (trixie) would give this node libvirt 9.0.0 and QEMU 7.2.15. " + needs, "Nothing was installed or changed"}},
		{"not installed, no candidate known", factsWith(""), Info, []string{"checked after the install", needs}},
		{"libvirt installed, QEMU not, candidate too old", strings.Replace(debian13Facts, "virsh=\n", "virsh=/usr/bin/virsh\nlibvirt_version=11.3.0\ncand_qemu=1:8.2.2+ds-0ubuntu1\n", 1), Error,
			[]string{"would give this node libvirt 11.3.0 and QEMU 8.2.2"}},
		{"installed, QEMU unreadable", strings.Replace(debian13Facts, "virsh=\n", "virsh=/usr/bin/virsh\nlibvirt_version=11.3.0\n", 1), OK, []string{"could not be read"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := find(Evaluate(plan(c.facts), o), "libvirt")
			if r.Status != c.status {
				t.Errorf("%+v, want %s", r, c.status)
			}
			for _, h := range c.has {
				if !strings.Contains(r.Detail, h) {
					t.Errorf("%q lacks %q", r.Detail, h)
				}
			}
			if strings.Contains(r.Detail, "...") || strings.Contains(r.Detail, "…") {
				t.Errorf("ellipsis in %q", r.Detail)
			}
		})
	}
}

// A host that is too old is an error row: nothing is installed or changed.
func TestOldHostIsRefusedAndNothingChanges(t *testing.T) {
	for name, facts := range map[string]string{
		"installed":    strings.Replace(debian13Facts, "virsh=\n", installedOld, 1),
		"repositories": factsWith("cand_libvirt=1:10.0.0-2ubuntu8\ncand_qemu=1:8.2.2+ds-0ubuntu1\n"),
	} {
		t.Run(name, func(t *testing.T) {
			n := hostOnDebian(true, func(cmd, stdin string) (string, bool, error) {
				if strings.Contains(cmd, "os_id=") {
					return facts, true, nil
				}
				return "", false, nil
			})
			rows := (&Runner{N: n}).Prepare(Options{ControllerKey: testKey}, nil, nil)
			if r := find(rows, "libvirt"); r.Status != Error {
				t.Fatalf("libvirt row: %+v", r)
			}
			for _, id := range []string{"do-packages", "do-services", "do-key", "do-nodecmd", "do-guard"} {
				if r := find(rows, id); r.Status != Skipped {
					t.Errorf("%s: %+v", id, r)
				}
			}
			for _, c := range n.log {
				for _, w := range []string{"apt-get", "dnf install", "useradd", "install -d"} {
					if strings.Contains(c, w) {
						t.Errorf("ran %q although the host is too old", w)
					}
				}
			}
		})
	}
}

// Without a known candidate the packages are installed and the versions are
// checked afterwards: the error says what was installed and that the node
// cannot be used; the steps after it do not run.
func TestVersionsCheckedAfterTheInstall(t *testing.T) {
	run := func(after string) []Row {
		n := hostOnDebian(true, func(cmd, stdin string) (string, bool, error) {
			if strings.Contains(cmd, `echo "libvirt_version=`) && !strings.Contains(cmd, "os_id=") {
				return after, true, nil
			}
			return "", false, nil
		})
		return (&Runner{N: n}).Prepare(Options{ControllerKey: testKey}, nil, nil)
	}
	rows := run("libvirt_version=10.0.0\nqemu_version=QEMU emulator version 8.2.2 (Debian)\n")
	r := find(rows, "do-packages")
	if r.Status != Error || !strings.Contains(r.Detail, "installed libvirt 10.0.0 and QEMU 8.2.2") || !strings.Contains(r.Detail, "This node cannot be used") {
		t.Errorf("do-packages: %+v", r)
	}
	if r := find(rows, "do-key"); r.Status != Skipped {
		t.Errorf("a step ran after the refusal: %+v", r)
	}
	rows = run("libvirt_version=11.3.0\nqemu_version=QEMU emulator version 10.0.2 (Debian)\n")
	if r := find(rows, "do-packages"); r.Status != OK {
		t.Errorf("good versions: %+v", r)
	}
}

func TestFactsScriptDoesNotInstallOrUpdate(t *testing.T) {
	for _, w := range []string{"apt-get", "apt install", "dnf install", "dnf makecache"} {
		if strings.Contains(FactsScript, w) {
			t.Errorf("the facts script contains %q", w)
		}
	}
	for _, w := range []string{"apt-cache policy", "repoquery", "qemu_version="} {
		if !strings.Contains(FactsScript, w) {
			t.Errorf("the facts script lacks %q", w)
		}
	}
}
