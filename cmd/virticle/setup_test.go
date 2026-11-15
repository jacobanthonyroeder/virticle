package main

import (
	"strings"
	"testing"

	"virticle/internal/prepare"
	"virticle/internal/store"
	"virticle/internal/webaccess"
)

func TestWebPorts(t *testing.T) {
	https := webaccess.FirstRunDefaults()
	loop := webaccess.Defaults()
	redirect := https
	redirect.HTTPRedirect = ":8080"
	moved := https
	moved.HTTPSListen = "0.0.0.0:9443"
	lan := webaccess.Defaults()
	lan.Listen = "192.0.2.10:8080"
	same := https
	same.HTTPRedirect = "0.0.0.0:8463"
	for _, c := range []struct {
		name string
		ws   webaccess.Settings
		want string
	}{
		{"first start", https, "8463"},
		{"loopback http", loop, ""},
		{"https with redirect", redirect, "8463 8080"},
		{"moved", moved, "9443"},
		{"http on an interface", lan, "8080"},
		{"same port twice", same, "8463"},
	} {
		if got := strings.Join(webPorts(c.ws), " "); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// host scripts a machine: the answers of the firewall and service scripts.
type host struct {
	log      []string
	fwFacts  string // answer to the firewall check
	fwApply  string // answer to the firewall apply script
	unit     string
	startOut string
	mainPID  string // answer to "systemctl show -p MainPID"
}

func (h *host) sh(script string) (string, error) {
	h.log = append(h.log, script)
	switch {
	case strings.Contains(script, "fw_firewalld() {"):
		return h.fwApply, nil
	case strings.Contains(script, `echo "fw=$fwkind"`):
		return h.fwFacts, nil
	case strings.Contains(script, "MainPID"):
		return h.mainPID, nil
	case strings.Contains(script, "systemctl cat"):
		return h.unit, nil
	case strings.Contains(script, "enable --now"), strings.Contains(script, "systemctl restart"):
		return h.startOut, nil
	}
	return "", nil
}

func (h *host) applied(ports string) bool {
	for _, s := range h.log {
		if strings.Contains(s, "fw_firewalld() {") && strings.Contains(s, "fwports='"+ports+"'") {
			return true
		}
	}
	return false
}

func envFor(h *host, ws webaccess.Settings, source string, users bool) setupEnv {
	return setupEnv{euid: 0, sh: h.sh, addrs: []string{"192.0.2.10", "box"}, hasKVM: true,
		settings: func() (webaccess.Settings, string, bool, error) { return ws, source, users, nil }}
}

func rowText(rows []prepare.Row) string {
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(r.Status + " | " + r.Title + " | " + r.Detail + "\n")
	}
	return b.String()
}

func TestSetupNeedsRoot(t *testing.T) {
	e := envFor(&host{}, webaccess.FirstRunDefaults(), "default", false)
	e.euid = 1000
	rows := runSetup(e, setupOpts{})
	if len(rows) != 1 || rows[0].Status != prepare.Error || !strings.Contains(rows[0].Detail, "needs root. Run: sudo virticle setup") {
		t.Errorf("%+v", rows)
	}
}

func TestSetupOpensTheWebPortAndSaysWhereToGo(t *testing.T) {
	h := &host{fwApply: "@@kind firewalld\n@@changed\n", unit: "unit=yes\nactive=inactive\nenabled=no\n"}
	res := runSetupAll(envFor(h, webaccess.FirstRunDefaults(), "default", false), setupOpts{})
	want := `info | Web access | HTTPS on TCP port 8463 (the default for a first start).
info | Administrator | None yet. Create it in the web interface, or run: sudo virticle setup --admin NAME --admin-password-file FILE
ok | Firewall | Opened TCP port 8463 in firewalld (service virticle).
ok | Service | Enabled and started virticle.service.
info | Node | This machine is not a node. To run virtual machines on it too: sudo virticle node add
`
	if got := rowText(res.rows); got != want {
		t.Errorf("rows:\n%s\nwant:\n%s", got, want)
	}
	if res.url != "https://192.0.2.10:8463" || res.hasUser || res.addNode {
		t.Errorf("%+v", res)
	}
	if s := summaryText(true, res, nil, nil); !strings.HasSuffix(s, "Open https://192.0.2.10:8463 to create the administrator.\n") {
		t.Errorf("summary:\n%s", s)
	}
	if !h.applied("8463") {
		t.Error("the web port was not sent to the firewall script")
	}
}

func TestSetupAgainSaysAlreadyAndUsesStoredSettings(t *testing.T) {
	ws := webaccess.FirstRunDefaults()
	ws.HTTPSListen = ":9443"
	h := &host{fwApply: "@@kind firewalld\n@@unchanged\n", unit: "unit=yes\nactive=active\nenabled=yes\n"}
	e := envFor(h, ws, "stored", true)
	e.hasKVM = false
	e.admins = func() []string { return []string{"ann"} }
	res := runSetupAll(e, setupOpts{admin: "ann", adminPassword: "0123456789"})
	got := rowText(res.rows)
	want := `info | Web access | HTTPS on TCP port 9443 (stored settings).
ok | Administrator | Already set up (ann).
ok | Firewall | TCP port 9443 is already open in firewalld (service virticle).
ok | Service | virticle.service is already running and enabled.
`
	if res.url != "https://192.0.2.10:9443" || !res.hasUser {
		t.Errorf("%+v", res)
	}
	if got != want {
		t.Errorf("rows:\n%s\nwant:\n%s", got, want)
	}
	for _, s := range h.log {
		if strings.Contains(s, "enable --now") {
			t.Error("a running service was started again")
		}
	}
}

func TestSetupCheckChangesNothing(t *testing.T) {
	h := &host{fwFacts: "fw=ufw\nfw_open=no\n", unit: "unit=yes\nactive=inactive\nenabled=no\n"}
	got := rowText(runSetup(envFor(h, webaccess.FirstRunDefaults(), "default", false), setupOpts{check: true}))
	for _, must := range []string{"info | Firewall | ufw is active. Setup will open TCP port 8463 for the web interface.", "info | Service | Setup will enable and start virticle.service."} {
		if !strings.Contains(got, must) {
			t.Errorf("lacks %q:\n%s", must, got)
		}
	}
	for _, s := range h.log {
		if strings.Contains(s, "fw_firewalld() {") || strings.Contains(s, "enable --now") {
			t.Error("--check changed something")
		}
	}
}

func TestSetupLoopbackNeedsNoPortAndClosesAnOldOne(t *testing.T) {
	h := &host{fwApply: "@@kind firewalld\n@@changed\n", unit: "unit=yes\nactive=active\nenabled=yes\n"}
	res := runSetupAll(envFor(h, webaccess.Defaults(), "stored", true), setupOpts{})
	got := rowText(res.rows) + summaryText(true, res, []string{"ann"}, nil)
	for _, must := range []string{"HTTP on this computer only (127.0.0.1:8080), so no firewall port is needed", "Removed the firewalld service virticle.", "Open http://127.0.0.1:8080 to sign in."} {
		if !strings.Contains(got, must) {
			t.Errorf("lacks %q:\n%s", must, got)
		}
	}
	if !h.applied("") {
		t.Error("the old rule was not removed")
	}
}

func TestSetupFirewallFailureIsAWarningAndSetupGoesOn(t *testing.T) {
	h := &host{fwApply: "@@kind ufw\n@@failed ufw allow 8463/tcp comment 'virticle'\n@@detail boom\n", unit: "unit=yes\nactive=active\nenabled=yes\n"}
	rows := runSetup(envFor(h, webaccess.FirstRunDefaults(), "default", false), setupOpts{})
	got := rowText(rows)
	if !strings.Contains(got, "warning | Firewall | The firewall command `ufw allow 8463/tcp comment 'virticle'` failed (boom)") || !strings.Contains(got, "Run as root: ufw allow 8463/tcp comment 'virticle'") {
		t.Errorf("%s", got)
	}
	if !strings.Contains(got, "ok | Service |") || exitCode(rows) != 0 {
		t.Errorf("setup must go on and exit 0:\n%s", got)
	}
}

func TestSetupUndoOnlyRemovesTheFirewallRule(t *testing.T) {
	h := &host{fwApply: "@@kind ufw\n@@changed\n"}
	got := rowText(runSetup(envFor(h, webaccess.FirstRunDefaults(), "default", false), setupOpts{undo: true}))
	if !strings.Contains(got, "Removed the ufw rules marked virticle.") || strings.Contains(got, "Service") {
		t.Errorf("%s", got)
	}
	for _, s := range h.log {
		if strings.Contains(s, "systemctl") {
			t.Error("undo touched the service")
		}
	}
}

func TestSetupWithoutDatabaseUsesTheFirstStartDefault(t *testing.T) {
	ws, source, users, err := storedWebSettings(t.TempDir())()
	if err != nil || source != "default" || users || ws.Mode != webaccess.ModeSelfSigned || ws.HTTPSListen != webaccess.DefaultHTTPSListen {
		t.Errorf("%+v %s %v %v", ws, source, users, err)
	}
}

// The Web access page tells the administrator to run setup while a port change
// still waits for confirmation. The new port must be opened, and the old one
// stays open until the change is confirmed.
func TestSetupWhileAPortChangeWaitsOpensBothPorts(t *testing.T) {
	ws := webaccess.FirstRunDefaults()
	ws.HTTPSListen = ":9443"
	prev := webaccess.FirstRunDefaults()
	h := &host{fwApply: "@@kind firewalld\n@@changed\n", unit: "unit=yes\nactive=active\nenabled=yes\n"}
	e := envFor(h, ws, "stored", true)
	e.pendingPrev = func() *webaccess.Settings { return &prev }
	got := rowText(runSetup(e, setupOpts{}))
	if !strings.Contains(got, "waiting for confirmation") {
		t.Errorf("no row about the waiting change:\n%s", got)
	}
	if !h.applied("9443 8463") {
		t.Errorf("both ports must go to the firewall script: %v", h.log)
	}
}

func TestAnyLocalAddr(t *testing.T) {
	local := []string{"192.0.2.10", "Box"}
	for _, c := range []struct {
		addr string
		want bool
	}{{"192.0.2.10", true}, {"127.0.0.1", true}, {"box:22", true}, {"[::1]:22", true}, {"192.0.2.11", false}, {"other", false}} {
		if got := anyLocalAddr([]store.Node{{Addr: c.addr}}, local); got != c.want {
			t.Errorf("%s: %v, want %v", c.addr, got, c.want)
		}
	}
	if anyLocalAddr(nil, local) {
		t.Error("no nodes")
	}
	e := envFor(&host{fwApply: "@@kind firewalld\n@@unchanged\n", unit: "unit=yes\nactive=active\nenabled=yes\n"}, webaccess.FirstRunDefaults(), "stored", true)
	e.hasLocalNode = true
	if got := rowText(runSetup(e, setupOpts{})); !strings.Contains(got, "ok | Node | This machine is a node already.") {
		t.Errorf("a machine that is already a node must not be told to become one:\n%s", got)
	}
}
