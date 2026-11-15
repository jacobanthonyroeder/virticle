package main

// Tests of what "virticle setup" does beside the listener: the first administrator and this machine as a node.

import (
	"errors"
	"strings"
	"testing"

	"virticle/internal/cluster"
	"virticle/internal/store"
	"virticle/internal/webaccess"
)

// adminLab is a fresh machine that records the administrator setup creates.
type adminLab struct {
	*lab
	created   []string
	passwords []string // what the terminal answers to the hidden questions
}

func newAdminLab(terminal bool, answers ...string) *adminLab {
	l := &adminLab{lab: newLab(terminal, answers...)}
	l.e.createAdmin = func(name, pw string) error { l.created = append(l.created, name+":"+pw); return nil }
	return l
}

func (l *adminLab) withPasswords(pws ...string) *adminLab {
	l.passwords = pws
	l.e.readPassword = func(string) (string, error) {
		if len(l.passwords) == 0 {
			return "", errors.New("end of input")
		}
		p := l.passwords[0]
		l.passwords = l.passwords[1:]
		return p, nil
	}
	return l
}

func TestSetupCreatesTheAdministratorFromScriptInputs(t *testing.T) {
	l := newAdminLab(false)
	res := runSetupAll(l.e, setupOpts{admin: "ann", adminPassword: "correct horse"})
	if len(l.created) != 1 || l.created[0] != "ann:correct horse" {
		t.Fatalf("created %v", l.created)
	}
	if got := rowText(res.rows); !strings.Contains(got, "ok | Administrator | Created ann.") || !res.hasUser {
		t.Errorf("%s", got)
	}
	// A password alone means the name admin.
	l = newAdminLab(false)
	runSetupAll(l.e, setupOpts{adminPassword: "correct horse"})
	if len(l.created) != 1 || l.created[0] != "admin:correct horse" {
		t.Errorf("created %v", l.created)
	}
}

func TestSetupRefusesBadAdministratorInputsBeforeChangingAnything(t *testing.T) {
	for _, c := range []struct {
		name string
		o    setupOpts
		want string
	}{
		{"short password", setupOpts{admin: "ann", adminPassword: "short"}, "The password must be at least 10 characters."},
		{"bad name", setupOpts{admin: "a b", adminPassword: "correct horse"}, "The username must be"},
		{"no password, no terminal", setupOpts{admin: "ann"}, "--admin ann needs the password: --admin-password-file FILE, or the environment variable VIRTICLE_ADMIN_PASSWORD"},
	} {
		l := newAdminLab(false)
		rows := runSetup(l.e, c.o)
		if len(rows) != 1 || !strings.Contains(rows[0].Detail, c.want) || exitCode(rows) == 0 {
			t.Errorf("%s: %s", c.name, rowText(rows))
		}
		if len(l.created) > 0 || len(l.saved) > 0 || l.ranFirewall() || len(l.h.log) > 1 {
			t.Errorf("%s: something was changed: created %v saved %v log %v", c.name, l.created, l.saved, l.h.log)
		}
	}
}

func TestSetupAsksForTheAdministratorOnATerminal(t *testing.T) {
	// Address, port, administrator name; the firewall and node questions follow.
	l := newAdminLab(true, "", "", "ann", "y").withPasswords("short", "correct horse", "other horse", "correct horse", "correct horse")
	res := runSetupAll(l.e, setupOpts{})
	if len(l.created) != 1 || l.created[0] != "ann:correct horse" {
		t.Fatalf("created %v\n%s", l.created, rowText(res.rows))
	}
	if !l.a.asked("Administrator of the web interface [admin]: ") {
		t.Errorf("not asked: %q", l.a.prompts)
	}
	// Enter takes admin; the end of input before the name leaves the administrator to the browser.
	l = newAdminLab(true, "", "", "").withPasswords("correct horse", "correct horse")
	runSetupAll(l.e, setupOpts{})
	if len(l.created) != 1 || l.created[0] != "admin:correct horse" {
		t.Errorf("created %v", l.created)
	}
	l = newAdminLab(true, "", "").withPasswords()
	res = runSetupAll(l.e, setupOpts{})
	if len(l.created) != 0 || !strings.Contains(rowText(res.rows), "info | Administrator | None yet.") {
		t.Errorf("created %v\n%s", l.created, rowText(res.rows))
	}
	// Three wrong passwords end setup before anything is changed.
	l = newAdminLab(true, "", "", "ann").withPasswords("a", "b", "c")
	rows := runSetup(l.e, setupOpts{})
	if len(rows) != 1 || exitCode(rows) == 0 || len(l.saved) > 0 {
		t.Errorf("%s", rowText(rows))
	}
}

func TestSetupCheckOnlySaysItWouldCreateTheAdministrator(t *testing.T) {
	l := newAdminLab(true).withPasswords()
	got := rowText(runSetup(l.e, setupOpts{check: true, admin: "ann", adminPassword: "correct horse"}))
	if !strings.Contains(got, "info | Administrator | Setup will create the administrator ann.") || len(l.created) > 0 {
		t.Errorf("%s", got)
	}
}

func TestSetupAgainLeavesTheAdministrator(t *testing.T) {
	l := newAdminLab(true, "y").withPasswords("x")
	l.setUp(webaccess.FirstRunDefaults(), "stored", true)
	l.e.admins = func() []string { return []string{"ann"} }
	got := rowText(runSetup(l.e, setupOpts{admin: "bob", adminPassword: "correct horse"}))
	if !strings.Contains(got, "ok | Administrator | Already set up (ann). Setup creates no other; add users on the Users page.") || len(l.created) > 0 {
		t.Errorf("%s", got)
	}
	if l.a.asked("Administrator") {
		t.Error("asked for an administrator although there is one")
	}
}

func TestSetupNodeChoice(t *testing.T) {
	for _, c := range []struct {
		name               string
		terminal, kvm, has bool
		answers            []string
		o                  setupOpts
		want               bool
		row                string
	}{
		{"--node", false, true, false, nil, setupOpts{node: true}, true, ""},
		{"--yes", false, true, false, nil, setupOpts{yes: true}, true, ""},
		{"no terminal, no flag", false, true, false, nil, setupOpts{}, false, "info | Node | This machine is not a node."},
		{"--no-node", true, true, false, nil, setupOpts{noNode: true}, false, "info | Node | This machine is not a node."},
		{"Enter on the question", true, true, false, []string{"", "", "", ""}, setupOpts{}, true, ""},
		{"no on the question", true, true, false, []string{"", "", "n", ""}, setupOpts{}, false, "info | Node |"},
		{"a node already", true, true, true, []string{"", "", ""}, setupOpts{node: true}, false, "ok | Node | This machine is a node already."},
		{"no KVM", true, false, false, []string{"", "", ""}, setupOpts{}, false, ""},
	} {
		l := newAdminLab(c.terminal, c.answers...)
		l.e.hasKVM, l.e.hasLocalNode = c.kvm, c.has
		res := runSetupAll(l.e, c.o)
		if res.addNode != c.want || exitCode(res.rows) != 0 {
			t.Errorf("%s: addNode %v\n%s", c.name, res.addNode, rowText(res.rows))
		}
		if c.row != "" && !strings.Contains(rowText(res.rows), c.row) {
			t.Errorf("%s: lacks %q\n%s", c.name, c.row, rowText(res.rows))
		}
		if asked := l.a.asked("Use this machine as a node too? [Y/n] "); asked != (c.terminal && c.kvm && !c.has && !c.o.noNode && !c.o.node) {
			t.Errorf("%s: asked %v", c.name, asked)
		}
	}
}

func TestSetupRefusesANodeItCannotBe(t *testing.T) {
	l := newAdminLab(false)
	l.e.hasKVM, l.e.vmProblem = false, "/dev/kvm is missing (KVM is off in the firmware, or the kvm module is not loaded)"
	rows := runSetup(l.e, setupOpts{node: true, admin: "ann", adminPassword: "correct horse"})
	if len(rows) != 1 || rows[0].Detail != "--node: this machine cannot run virtual machines: /dev/kvm is missing (KVM is off in the firmware, or the kvm module is not loaded). Nothing was changed." {
		t.Errorf("%s", rowText(rows))
	}
	if len(l.created) > 0 || len(l.saved) > 0 || l.ranFirewall() {
		t.Error("something was changed")
	}
	rows = runSetup(newAdminLab(false).e, setupOpts{node: true, noNode: true})
	if len(rows) != 1 || exitCode(rows) == 0 {
		t.Errorf("%s", rowText(rows))
	}
}

func TestSetupSummary(t *testing.T) {
	res := setupResult{url: "https://192.0.2.10:8463", hasUser: true}
	ns := []cluster.NodeStatus{{Node: store.Node{ID: 1, Addr: "192.0.2.10"}, State: "online", Hostname: "box"}}
	got := summaryText(true, res, []string{"ann"}, ns)
	want := `Controller     service running, https://192.0.2.10:8463
Administrator  ann
Nodes          box online
Open https://192.0.2.10:8463 to sign in.
`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if got := summaryText(false, setupResult{}, nil, nil); !strings.Contains(got, "service not running") || !strings.Contains(got, "none yet") || !strings.Contains(got, "Nodes          none (add one: sudo virticle node add [user@]host)") {
		t.Errorf("%s", got)
	}
}
