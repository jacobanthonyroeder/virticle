package main

// Tests of "virticle setup" choosing the address and port, checking the port,
// and asking before it opens the firewall. Nothing here reads the real
// standard input, listens on a fixed port or touches the machine.

import (
	"errors"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"

	"virticle/internal/prepare"
	"virticle/internal/store"
	"virticle/internal/webaccess"
)

// asker scripts a terminal: the answers are given in order; once they are
// used up the input has ended.
type asker struct {
	answers []string
	prompts []string
}

func (a *asker) ask(q string) (string, bool) {
	a.prompts = append(a.prompts, q)
	if len(a.answers) == 0 {
		return "", false
	}
	ans := a.answers[0]
	a.answers = a.answers[1:]
	return ans, true
}

func (a *asker) asked(sub string) bool {
	for _, p := range a.prompts {
		if strings.Contains(p, sub) {
			return true
		}
	}
	return false
}

// busyPorts makes listening on these ports fail the way a taken port does.
func busyPorts(ports ...int) func(string) error {
	return func(addr string) error {
		_, p := hostPort(addr)
		for _, b := range ports {
			if b == p {
				return &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}
			}
		}
		return nil
	}
}

// lab is a fresh machine: no stored settings, no users, firewalld active, a
// service that is not running.
type lab struct {
	h     *host
	e     setupEnv
	saved []webaccess.Settings
	a     *asker
}

func newLab(terminal bool, answers ...string) *lab {
	l := &lab{h: &host{fwFacts: "fw=firewalld\nfw_open=no\n", fwApply: "@@kind firewalld\n@@changed\n", unit: "unit=yes\nactive=inactive\nenabled=no\n"}, a: &asker{answers: answers}}
	l.e = envFor(l.h, webaccess.FirstRunDefaults(), "default", false)
	l.e.hasKVM = false
	l.e.ips = []string{"192.0.2.10", "192.0.2.11"}
	l.e.terminal = terminal
	l.e.ask = l.a.ask
	l.e.probe = busyPorts()
	l.e.holder = func(int) (string, int) { return "", 0 }
	l.e.save = func(ws webaccess.Settings) error { l.saved = append(l.saved, ws); return nil }
	return l
}

// setUp makes the lab a machine that is set up: the service runs and the
// firewall is as wanted.
func (l *lab) setUp(ws webaccess.Settings, source string, users bool) {
	l.e.settings = func() (webaccess.Settings, string, bool, error) { return ws, source, users, nil }
	l.h.unit = "unit=yes\nactive=active\nenabled=yes\n"
	l.h.fwApply = "@@kind firewalld\n@@unchanged\n"
	l.h.fwFacts = "fw=firewalld\nfw_open=yes\n"
}

func (l *lab) ranFirewall() bool {
	for _, s := range l.h.log {
		if strings.Contains(s, "fw_firewalld() {") {
			return true
		}
	}
	return false
}

func (l *lab) ranRestart() bool {
	for _, s := range l.h.log {
		if strings.Contains(s, "systemctl restart") {
			return true
		}
	}
	return false
}

func TestSetupFlagsChooseTheListenerOfAFreshMachine(t *testing.T) {
	l := newLab(false)
	res := runSetupAll(l.e, setupOpts{address: "192.0.2.10", port: "9000"})
	rows := res.rows
	got := rowText(rows)
	if res.url != "https://192.0.2.10:9000" {
		t.Errorf("url %q", res.url)
	}
	if len(l.saved) != 1 || l.saved[0].HTTPSListen != "192.0.2.10:9000" || l.saved[0].Mode != webaccess.ModeSelfSigned {
		t.Fatalf("saved %+v", l.saved)
	}
	for _, must := range []string{
		"info | Web access | HTTPS on 192.0.2.10, TCP port 9000 (chosen in setup).",
		"ok | Port | TCP port 9000 on 192.0.2.10 is free to use.",
		"ok | Firewall | Opened TCP port 9000 in firewalld",
		"ok | Service | Enabled and started virticle.service.",
		"info | Administrator | None yet.",
	} {
		if !strings.Contains(got, must) {
			t.Errorf("lacks %q:\n%s", must, got)
		}
	}
	if !l.h.applied("9000") || exitCode(rows) != 0 {
		t.Errorf("firewall script %v, exit %d", l.h.log, exitCode(rows))
	}
	if len(l.a.prompts) != 0 {
		t.Errorf("a run without a terminal asked: %v", l.a.prompts)
	}
}

func TestSetupRestartsARunningServiceOnAChangedListener(t *testing.T) {
	l := newLab(false)
	l.h.unit = "unit=yes\nactive=active\nenabled=yes\n"
	got := rowText(runSetup(l.e, setupOpts{port: "9000"}))
	if !l.ranRestart() || !strings.Contains(got, "ok | Service | Restarted virticle.service to use the new web address.") {
		t.Errorf("%s", got)
	}
	if len(l.saved) != 1 || l.saved[0].HTTPSListen != ":9000" {
		t.Errorf("--port alone keeps every address: %+v", l.saved)
	}
	// Nothing changed: no restart.
	l = newLab(false)
	l.h.unit = "unit=yes\nactive=active\nenabled=yes\n"
	runSetup(l.e, setupOpts{})
	if l.ranRestart() || len(l.saved) != 0 {
		t.Error("the default was stored or the service restarted without a change")
	}
}

// A service that fell back to loopback because the port was taken at its first
// start is restarted once the port is free.
func TestSetupRestartsAServiceThatDoesNotHoldItsAddress(t *testing.T) {
	l := newLab(false)
	l.h.unit = "unit=yes\nactive=active\nenabled=yes\n"
	l.h.mainPID = "MainPID=4242\n"
	runSetup(l.e, setupOpts{})
	if !l.ranRestart() {
		t.Error("a running service that does not hold the free port was left alone")
	}
}

func TestSetupRefusesWrongFlagsAndSaysWhy(t *testing.T) {
	for _, c := range []struct {
		o    setupOpts
		want string
	}{
		{setupOpts{port: "0"}, `--port: "0" is not a port. Use a number from 1 to 65535.`},
		{setupOpts{port: "65536"}, `"65536" is not a port`},
		{setupOpts{port: "https"}, `"https" is not a port`},
		{setupOpts{address: "192.0.2.99"}, "--address: 192.0.2.99 is not an address of this machine (it has 192.0.2.10, 192.0.2.11). Use one of them, or all."},
		{setupOpts{address: "box"}, `--address: "box" is not an IP address. Use an address of this machine, or all.`},
	} {
		l := newLab(false)
		rows := runSetup(l.e, c.o)
		if len(rows) != 1 || rows[0].Status != prepare.Error || !strings.Contains(rows[0].Detail, c.want) || exitCode(rows) != 1 {
			t.Errorf("%+v: %s", c.o, rowText(rows))
		}
		if len(l.saved) != 0 || l.ranFirewall() || len(l.h.log) > 1 {
			t.Errorf("%+v changed something: %v", c.o, l.h.log)
		}
	}
	l := newLab(false)
	if rows := runSetup(l.e, setupOpts{address: "all", port: "443"}); exitCode(rows) != 0 || l.saved[0].HTTPSListen != ":443" {
		t.Errorf("all and 443 are fine: %s %+v", rowText(rows), l.saved)
	}
}

func TestSetupAsksTwoQuestionsOnATerminal(t *testing.T) {
	l := newLab(true, "192.0.2.11", "9100", "")
	rows := runSetup(l.e, setupOpts{})
	if len(l.saved) != 1 || l.saved[0].HTTPSListen != "192.0.2.11:9100" {
		t.Fatalf("saved %+v\n%s", l.saved, rowText(rows))
	}
	if len(l.a.prompts) != 3 {
		t.Fatalf("exactly an address, a port and the firewall are asked: %q", l.a.prompts)
	}
	for i, must := range []string{
		"Which address should the web interface listen on?\n  all   every address of this machine\n  192.0.2.10\n  192.0.2.11\nAddress [all]: ",
		"Port [8463]: ",
		"Open TCP port 9100 in firewalld for the web interface? [Y/n] ",
	} {
		if l.a.prompts[i] != must {
			t.Errorf("question %d:\n%q\nwant:\n%q", i, l.a.prompts[i], must)
		}
		if strings.Contains(l.a.prompts[i], "...") || strings.Contains(l.a.prompts[i], "\u2026") {
			t.Errorf("question %d has an ellipsis", i)
		}
	}
	if !l.h.applied("9100") {
		t.Error("an empty answer to the firewall question is yes")
	}
}

func TestSetupEnterKeepsTheDefaults(t *testing.T) {
	l := newLab(true, "", "", "y")
	rows := runSetup(l.e, setupOpts{})
	if len(l.saved) != 0 {
		t.Errorf("the default was stored by setup: %+v", l.saved)
	}
	if !strings.Contains(rowText(rows), "HTTPS on TCP port 8463 (the default for a first start)") || !l.h.applied("8463") {
		t.Errorf("%s", rowText(rows))
	}
	// End of input behaves like Enter for the two choices.
	l = newLab(true)
	runSetup(l.e, setupOpts{})
	if len(l.saved) != 0 {
		t.Error("end of input changed the listener")
	}
}

func TestSetupAsksAgainOnAWrongAnswerAndAsksOnlyWhatIsMissing(t *testing.T) {
	l := newLab(true, "abc", "10.9.9.9", "all", "", "n")
	runSetup(l.e, setupOpts{})
	if !strings.Contains(l.a.prompts[1], `"abc" is not an IP address. Use an address of this machine, or all.`) ||
		!strings.Contains(l.a.prompts[2], "10.9.9.9 is not an address of this machine") {
		t.Errorf("%q", l.a.prompts)
	}
	l = newLab(true, "70000", "9200", "n")
	runSetup(l.e, setupOpts{address: "192.0.2.10"})
	if l.a.asked("Which address") || !strings.Contains(l.a.prompts[1], `"70000" is not a port`) || l.saved[0].HTTPSListen != "192.0.2.10:9200" {
		t.Errorf("%q %+v", l.a.prompts, l.saved)
	}
	l = newLab(true, "", "n")
	runSetup(l.e, setupOpts{address: "192.0.2.10", port: "9300"})
	if l.a.asked("Port [") || l.a.asked("Address [") {
		t.Errorf("both choices were given, yet asked: %q", l.a.prompts)
	}
}

func TestSetupNeverAsksWithoutATerminalOrWithYesOrCheck(t *testing.T) {
	for name, tc := range map[string]struct {
		terminal bool
		o        setupOpts
	}{
		"piped":    {false, setupOpts{}},
		"yes":      {true, setupOpts{yes: true}},
		"check":    {true, setupOpts{check: true}},
		"no-fw":    {false, setupOpts{noFirewall: true}},
		"yes+flag": {true, setupOpts{yes: true, port: "9400"}},
	} {
		l := newLab(tc.terminal, "9999", "9999", "n")
		runSetup(l.e, tc.o)
		if len(l.a.prompts) != 0 {
			t.Errorf("%s asked: %q", name, l.a.prompts)
		}
	}
}

func TestSetupPortInUseByAnotherProgramChangesNothing(t *testing.T) {
	l := newLab(false)
	l.e.probe = busyPorts(8463)
	l.e.holder = func(port int) (string, int) { return "nginx", 812 }
	rows := runSetup(l.e, setupOpts{})
	want := "error | Port | TCP port 8463 on all addresses is already used by nginx (process 812). Nothing was changed. Free the port, or run setup again with another port: sudo virticle setup --port N\n"
	if rowText(rows) != want || exitCode(rows) != 1 {
		t.Errorf("%s", rowText(rows))
	}
	if len(l.saved) != 0 || l.ranFirewall() || len(l.h.log) > 1 {
		t.Errorf("something was changed: %v", l.h.log)
	}
	// The same with a port from the flag, and when ss cannot tell who it is.
	l = newLab(false)
	l.e.probe = busyPorts(9000)
	rows = runSetup(l.e, setupOpts{port: "9000"})
	if !strings.Contains(rowText(rows), "TCP port 9000 on all addresses is already used by another program (setup could not tell which one).") || exitCode(rows) != 1 || len(l.saved) != 0 {
		t.Errorf("%s", rowText(rows))
	}
	// A specific address is named.
	l = newLab(false)
	l.e.probe = busyPorts(9000)
	l.e.holder = func(int) (string, int) { return "caddy", 7 }
	rows = runSetup(l.e, setupOpts{port: "9000", address: "192.0.2.10"})
	if !strings.Contains(rowText(rows), "TCP port 9000 on 192.0.2.10 is already used by caddy (process 7).") {
		t.Errorf("%s", rowText(rows))
	}
}

func TestSetupOffersAnotherPortOnATerminal(t *testing.T) {
	l := newLab(true, "", "", "8500", "y")
	l.e.probe = busyPorts(8463)
	l.e.holder = func(int) (string, int) { return "nginx", 812 }
	rows := runSetup(l.e, setupOpts{})
	if len(l.saved) != 1 || l.saved[0].HTTPSListen != ":8500" || exitCode(rows) != 0 {
		t.Fatalf("%+v\n%s", l.saved, rowText(rows))
	}
	if !strings.Contains(l.a.prompts[2], "TCP port 8463 on all addresses is already used by nginx (process 812).\nAnother port, or press Enter to stop: ") {
		t.Errorf("%q", l.a.prompts)
	}
	// Enter stops.
	l = newLab(true, "", "", "")
	l.e.probe = busyPorts(8463)
	rows = runSetup(l.e, setupOpts{})
	if exitCode(rows) != 1 || len(l.saved) != 0 || l.ranFirewall() {
		t.Errorf("%s", rowText(rows))
	}
	// A wrong answer is told, and the question comes again.
	l = newLab(true, "", "", "nope", "8463", "")
	l.e.probe = busyPorts(8463)
	rows = runSetup(l.e, setupOpts{})
	if exitCode(rows) != 1 || !strings.Contains(l.a.prompts[3], `"nope" is not a port`) {
		t.Errorf("%q %s", l.a.prompts, rowText(rows))
	}
	// A port from --port is offered another one, too.
	l = newLab(true, "8600", "n")
	l.e.probe = busyPorts(9000)
	runSetup(l.e, setupOpts{port: "9000", address: "all"})
	if len(l.saved) != 1 || l.saved[0].HTTPSListen != ":8600" {
		t.Errorf("%+v", l.saved)
	}
	// --yes asks nothing, so it stops.
	l = newLab(true)
	l.e.probe = busyPorts(8463)
	if rows = runSetup(l.e, setupOpts{yes: true}); exitCode(rows) != 1 || len(l.a.prompts) != 0 {
		t.Errorf("%s", rowText(rows))
	}
}

func TestSetupPortHeldByTheServiceItselfIsNotAConflict(t *testing.T) {
	l := newLab(false)
	l.h.unit = "unit=yes\nactive=active\nenabled=yes\n"
	l.h.mainPID = "MainPID=812\n"
	l.e.probe = busyPorts(8463)
	l.e.holder = func(int) (string, int) { return "virticle", 812 }
	rows := runSetup(l.e, setupOpts{})
	if exitCode(rows) != 0 || strings.Contains(rowText(rows), "Port |") {
		t.Errorf("%s", rowText(rows))
	}
	// Another virticle process is another program.
	l.e.holder = func(int) (string, int) { return "virticle", 999 }
	if rows = runSetup(l.e, setupOpts{}); exitCode(rows) != 1 || !strings.Contains(rowText(rows), "used by virticle (process 999)") {
		t.Errorf("%s", rowText(rows))
	}
	// ss missing: the stored address of a running service is the service.
	l.e.holder = func(int) (string, int) { return "", 0 }
	l.e.settings = func() (webaccess.Settings, string, bool, error) {
		return webaccess.FirstRunDefaults(), "stored", false, nil
	}
	if rows = runSetup(l.e, setupOpts{}); exitCode(rows) != 0 {
		t.Errorf("%s", rowText(rows))
	}
	// Choosing the port the service holds, on one address, is fine: it is restarted.
	l = newLab(false)
	l.h.unit = "unit=yes\nactive=active\nenabled=yes\n"
	l.h.mainPID = "MainPID=812\n"
	l.e.probe = busyPorts(8463)
	l.e.holder = func(int) (string, int) { return "virticle", 812 }
	rows = runSetup(l.e, setupOpts{address: "192.0.2.10"})
	if exitCode(rows) != 0 || len(l.saved) != 1 || !l.ranRestart() {
		t.Errorf("%s", rowText(rows))
	}
}

func TestSetupCheckReportsAPortInUse(t *testing.T) {
	l := newLab(true)
	l.e.probe = busyPorts(8463)
	l.e.holder = func(int) (string, int) { return "nginx", 812 }
	rows := runSetup(l.e, setupOpts{check: true})
	if exitCode(rows) != 1 || !strings.Contains(rowText(rows), "already used by nginx (process 812)") {
		t.Errorf("%s", rowText(rows))
	}
	l = newLab(true)
	got := rowText(runSetup(l.e, setupOpts{check: true, port: "9000"}))
	for _, must := range []string{"HTTPS on TCP port 9000 (chosen in setup). Setup will store it.", "ok | Port | TCP port 9000 on all addresses is free to use.", "info | Firewall | firewalld is active. Setup will open TCP port 9000 for the web interface."} {
		if !strings.Contains(got, must) {
			t.Errorf("lacks %q:\n%s", must, got)
		}
	}
	if len(l.saved) != 0 || l.ranFirewall() || len(l.a.prompts) != 0 {
		t.Error("--check changed or asked something")
	}
}

func TestSetupChecksTheRedirectPortToo(t *testing.T) {
	ws := webaccess.FirstRunDefaults()
	ws.HTTPRedirect = ":80"
	l := newLab(false)
	l.e.settings = func() (webaccess.Settings, string, bool, error) { return ws, "stored", true, nil }
	l.e.probe = busyPorts(80)
	l.e.holder = func(int) (string, int) { return "apache2", 55 }
	rows := runSetup(l.e, setupOpts{})
	if exitCode(rows) != 1 || !strings.Contains(rowText(rows), "TCP port 80 on all addresses is already used by apache2 (process 55). Nothing was changed. Free the port, or choose another one on the Web access page.") {
		t.Errorf("%s", rowText(rows))
	}
	if l.ranFirewall() {
		t.Error("the firewall was changed")
	}
}

func TestSetupFirewallQuestion(t *testing.T) {
	// yes
	l := newLab(true, "", "", "y")
	got := rowText(runSetup(l.e, setupOpts{}))
	if !l.a.asked("Open TCP port 8463 in firewalld for the web interface? [Y/n] ") || !l.h.applied("8463") || !strings.Contains(got, "ok | Firewall | Opened TCP port 8463 in firewalld") {
		t.Errorf("%q\n%s", l.a.prompts, got)
	}
	// no: the exact command is printed and nothing is opened
	l = newLab(true, "", "", "n")
	got = rowText(runSetup(l.e, setupOpts{}))
	if l.ranFirewall() || !strings.Contains(got, "info | Firewall | firewalld is active. Not opened. To open TCP port 8463 later, run as root: firewall-cmd --permanent --new-service=virticle && firewall-cmd --permanent --service=virticle --add-port=8463/tcp && firewall-cmd --permanent --add-service=virticle && firewall-cmd --reload") {
		t.Errorf("%s", got)
	}
	// ufw names itself and its command
	l = newLab(true, "", "", "no")
	l.h.fwFacts = "fw=ufw\nfw_open=no\n"
	got = rowText(runSetup(l.e, setupOpts{}))
	if !l.a.asked("in ufw for the web interface") || !strings.Contains(got, "run as root: ufw allow 8463/tcp comment 'virticle'") {
		t.Errorf("%s", got)
	}
	// a wrong answer is asked again
	l = newLab(true, "", "", "maybe", "n")
	runSetup(l.e, setupOpts{})
	if !strings.HasPrefix(l.a.prompts[3], "Please answer y or n. Open ") || l.ranFirewall() {
		t.Errorf("%q", l.a.prompts)
	}
	// already open: nothing is asked
	l = newLab(true, "", "")
	l.h.fwFacts = "fw=firewalld\nfw_open=yes\n"
	l.h.fwApply = "@@kind firewalld\n@@unchanged\n"
	got = rowText(runSetup(l.e, setupOpts{}))
	if l.a.asked("Open TCP") || !strings.Contains(got, "TCP port 8463 is already open in firewalld") {
		t.Errorf("%q\n%s", l.a.prompts, got)
	}
	// no firewall active: nothing is asked, as before
	l = newLab(true, "", "")
	l.h.fwFacts = "fw=none\n"
	l.h.fwApply = "@@kind none\n@@unchanged\n"
	got = rowText(runSetup(l.e, setupOpts{}))
	if l.a.asked("Open TCP") || !strings.Contains(got, "ok | Firewall | No firewall is active on this host. Nothing to open.") {
		t.Errorf("%q\n%s", l.a.prompts, got)
	}
}

func TestSetupFirewallWithoutATerminal(t *testing.T) {
	// as before: the port is opened without asking
	l := newLab(false)
	got := rowText(runSetup(l.e, setupOpts{}))
	if !l.h.applied("8463") || !strings.Contains(got, "ok | Firewall | Opened TCP port 8463 in firewalld") {
		t.Errorf("%s", got)
	}
	// --no-firewall: not touched, the command is printed
	for _, terminal := range []bool{false, true} {
		l = newLab(terminal)
		got = rowText(runSetup(l.e, setupOpts{noFirewall: true}))
		if l.ranFirewall() || !strings.Contains(got, "info | Firewall | firewalld is active. Left alone (--no-firewall). To open TCP port 8463, run as root: firewall-cmd --permanent --new-service=virticle") {
			t.Errorf("terminal=%v:\n%s", terminal, got)
		}
		if l.a.asked("Open TCP") {
			t.Errorf("--no-firewall asked about the firewall: %q", l.a.prompts)
		}
	}
	// --no-firewall does not close an old port either
	l = newLab(false)
	l.e.settings = func() (webaccess.Settings, string, bool, error) { return webaccess.Defaults(), "stored", true, nil }
	runSetup(l.e, setupOpts{noFirewall: true})
	if l.ranFirewall() {
		t.Error("--no-firewall changed the firewall")
	}
	// --yes on a terminal opens it without asking
	l = newLab(true, "n")
	runSetup(l.e, setupOpts{yes: true})
	if !l.h.applied("8463") || len(l.a.prompts) != 0 {
		t.Errorf("%q", l.a.prompts)
	}
}

// A machine that is set up keeps its listener. --address and --port are
// refused when they name another one, and no question is asked.
func TestSetupLeavesAMachineThatIsSetUp(t *testing.T) {
	stored := webaccess.FirstRunDefaults()
	stored.HTTPSListen = ":8443"
	for name, c := range map[string]struct {
		ws     webaccess.Settings
		source string
		port   string
	}{
		"users and stored settings": {stored, "stored", "8463"},
		"users, nothing stored":     {webaccess.Defaults(), "default", "9000"},
	} {
		l := newLab(true, "9999", "9999")
		l.setUp(c.ws, c.source, true)
		rows := runSetup(l.e, setupOpts{port: c.port})
		got := rowText(rows)
		if exitCode(rows) != 1 || len(rows) != 1 || !strings.Contains(got, "This machine is already set up, so setup does not change its web address. Change it on the Web access page") || !strings.Contains(got, "virticle web set -https-listen") {
			t.Errorf("%s: %s", name, got)
		}
		if len(l.saved) != 0 || l.ranFirewall() || l.ranRestart() {
			t.Errorf("%s changed something", name)
		}
		// No flags, on a terminal: nothing is asked, nothing changes.
		l = newLab(true, "9999", "9999")
		l.setUp(c.ws, c.source, true)
		rows = runSetup(l.e, setupOpts{})
		if exitCode(rows) != 0 || len(l.a.prompts) != 0 || len(l.saved) != 0 || l.ranRestart() {
			t.Errorf("%s: asked or changed: %q\n%s", name, l.a.prompts, rowText(rows))
		}
	}
	// The listener it has, given as flags, is not a change.
	l := newLab(false)
	l.setUp(stored, "stored", true)
	if rows := runSetup(l.e, setupOpts{port: "8443", address: "all"}); exitCode(rows) != 0 || len(l.saved) != 0 {
		t.Errorf("%s", rowText(rows))
	}
}

// A first start of version 0.3.0 stored port 8443 on a machine without users.
// Setup may still choose, but Enter keeps the port that machine has.
func TestSetupDefaultIsThePortThatIsStored(t *testing.T) {
	stored := webaccess.FirstRunDefaults()
	stored.HTTPSListen = ":8443"
	l := newLab(true, "", "", "y")
	l.e.settings = func() (webaccess.Settings, string, bool, error) { return stored, "stored", false, nil }
	rows := runSetup(l.e, setupOpts{})
	if !l.a.asked("Port [8443]: ") || len(l.saved) != 0 || !strings.Contains(rowText(rows), "HTTPS on TCP port 8443 (stored settings)") {
		t.Errorf("%q\n%s", l.a.prompts, rowText(rows))
	}
	// Changed: the service is restarted, and the firewall script holds only the new port.
	l = newLab(true, "", "8500", "y")
	l.e.settings = func() (webaccess.Settings, string, bool, error) { return stored, "stored", false, nil }
	l.h.unit = "unit=yes\nactive=active\nenabled=yes\n"
	runSetup(l.e, setupOpts{})
	if len(l.saved) != 1 || l.saved[0].HTTPSListen != ":8500" || !l.h.applied("8500") || !l.ranRestart() {
		t.Errorf("%+v %v", l.saved, l.h.log)
	}
}

func TestSetupUndoAfterAPortChange(t *testing.T) {
	l := newLab(false)
	runSetup(l.e, setupOpts{port: "9000"})
	if !l.h.applied("9000") {
		t.Fatal("not opened")
	}
	l.h.log = nil
	l.h.fwApply = "@@kind firewalld\n@@changed\n"
	rows := runSetup(l.e, setupOpts{undo: true})
	if !l.h.applied("") || !strings.Contains(rowText(rows), "Removed the firewalld service virticle.") || len(l.saved) != 1 {
		t.Errorf("%v\n%s", l.h.log, rowText(rows))
	}
	for _, s := range l.h.log {
		if strings.Contains(s, "systemctl") {
			t.Error("undo touched the service")
		}
	}
	// --undo does not check or choose ports
	l = newLab(true, "x")
	l.e.probe = busyPorts(8463)
	runSetup(l.e, setupOpts{undo: true})
	if len(l.a.prompts) != 0 {
		t.Error("--undo asked")
	}
}

func TestSetupSaveFailureStopsBeforeTheFirewall(t *testing.T) {
	l := newLab(false)
	l.e.save = func(webaccess.Settings) error { return errors.New("disk full") }
	rows := runSetup(l.e, setupOpts{port: "9000"})
	if exitCode(rows) != 1 || !strings.Contains(rowText(rows), "Could not store the web access settings (disk full). Nothing was changed.") || l.ranFirewall() {
		t.Errorf("%s", rowText(rows))
	}
}

func TestNotSetUp(t *testing.T) {
	first := webaccess.FirstRunDefaults()
	moved := first
	moved.HTTPSListen = "192.0.2.10:9000"
	own := first
	own.Mode = webaccess.ModeOwn
	redirect := first
	redirect.HTTPRedirect = ":80"
	for _, c := range []struct {
		name   string
		ws     webaccess.Settings
		source string
		users  bool
		want   bool
	}{
		{"nothing stored", first, "default", false, true},
		{"first start stored", first, "stored", false, true},
		{"another port stored by a first start", moved, "stored", false, true},
		{"administrator exists", first, "stored", true, false},
		{"upgrade: users, no stored settings", webaccess.Defaults(), "default", true, false},
		{"own certificate chosen", own, "stored", false, false},
		{"redirect chosen", redirect, "stored", false, false},
	} {
		if got := notSetUp(c.ws, c.source, c.users); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// What a first start stores and setup reads back is "not set up".
func TestStoredFirstStartIsNotSetUp(t *testing.T) {
	dir := t.TempDir()
	if err := saveWebSettings(dir)(webaccess.FirstRunDefaults()); err != nil {
		t.Fatal(err)
	}
	ws, source, users, err := storedWebSettings(dir)()
	if err != nil || source != "stored" || users || !notSetUp(ws, source, users) {
		t.Fatalf("%+v %s %v %v", ws, source, users, err)
	}
	// setup's own save is read back the same way
	moved := webaccess.FirstRunDefaults()
	moved.HTTPSListen = "192.0.2.10:9000"
	if err := saveWebSettings(dir)(moved); err != nil {
		t.Fatal(err)
	}
	ws, source, users, err = storedWebSettings(dir)()
	if err != nil || ws.HTTPSListen != "192.0.2.10:9000" || !notSetUp(ws, source, users) {
		t.Fatalf("%+v %s %v %v", ws, source, users, err)
	}
}

// An upgraded machine that has users but never stored settings stays on its
// loopback listener, and setup does not move it.
func TestSetupDoesNotMoveAnUpgradedMachineWithoutStoredSettings(t *testing.T) {
	dir, st := userEnv(t)
	if _, err := st.CreateUser("admin", "a long enough password", store.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	st.Close()
	ws, source, users, err := storedWebSettings(dir)()
	if err != nil || !users || source != "default" || ws.Mode != webaccess.ModeOff || ws.Listen != "127.0.0.1:8080" {
		t.Fatalf("%+v %s %v %v", ws, source, users, err)
	}
	l := newLab(true, "9999", "9999")
	l.setUp(ws, source, users)
	l.e.settings = storedWebSettings(dir)
	rows := runSetup(l.e, setupOpts{})
	if len(l.saved) != 0 || len(l.a.prompts) != 0 || !strings.Contains(rowText(rows), "HTTP on this computer only (127.0.0.1:8080)") {
		t.Errorf("%q\n%s", l.a.prompts, rowText(rows))
	}
	if rows = runSetup(l.e, setupOpts{port: "8463"}); exitCode(rows) != 1 || len(l.saved) != 0 {
		t.Errorf("%s", rowText(rows))
	}
}

func TestParseSSHolder(t *testing.T) {
	out := `LISTEN 0      4096         0.0.0.0:22         0.0.0.0:*    users:(("sshd",pid=701,fd=3))
LISTEN 0      511          0.0.0.0:8463       0.0.0.0:*    users:(("nginx",pid=812,fd=6),("nginx",pid=813,fd=6))
LISTEN 0      4096            [::]:9443          [::]:*    users:(("caddy",pid=77,fd=8))
LISTEN 0      4096       127.0.0.1:18463        0.0.0.0:*
`
	for _, c := range []struct {
		port int
		prog string
		pid  int
	}{{8463, "nginx", 812}, {9443, "caddy", 77}, {22, "sshd", 701}, {843, "", 0}, {18463, "", 0}, {463, "", 0}} {
		if p, pid := parseSSHolder(out, c.port); p != c.prog || pid != c.pid {
			t.Errorf("port %d: %q %d, want %q %d", c.port, p, pid, c.prog, c.pid)
		}
	}
}

func TestListenProbeSeesATakenPort(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	addr := l.Addr().String()
	err = listenProbe(addr)
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("a taken port: %v", err)
	}
	l.Close()
	if err := listenProbe(addr); err != nil {
		t.Errorf("a free port: %v", err)
	}
}

func TestParseAddressAndPort(t *testing.T) {
	ips := []string{"192.0.2.10", "fd00::5"}
	for in, want := range map[string]string{"": "", "all": "", "ALL": "", "0.0.0.0": "", "::": "", "192.0.2.10": "192.0.2.10", "fd00::5": "fd00::5", "[fd00::5]": "fd00::5", "127.0.0.1": "127.0.0.1"} {
		if got, err := parseAddress(in, ips); err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"192.0.2.11", "fd00::6", "box", "1.2.3"} {
		if _, err := parseAddress(in, ips); err == nil {
			t.Errorf("%q was accepted", in)
		}
	}
	for in, want := range map[string]int{"1": 1, "8463": 8463, " 65535 ": 65535} {
		if got, err := parsePort(in); err != nil || got != want {
			t.Errorf("%q: %d %v", in, got, err)
		}
	}
	for _, in := range []string{"", "0", "-1", "65536", "80a", "8463.5"} {
		if _, err := parsePort(in); err == nil {
			t.Errorf("%q was accepted", in)
		}
	}
	if got := webaccess.ComposeListen("fd00::5", 8463); got != "[fd00::5]:8463" {
		t.Errorf("%s", got)
	}
}
