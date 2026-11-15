package main

// "virticle setup": run once on the controller's own machine after the
// package is installed. It lets the installer choose the address and port of
// the web interface (asked on a terminal, or given with --address and
// --port), checks that the port is free, opens it in the machine's firewall
// (asked on a terminal; when one is active), makes sure the service runs, and
// says which address to open. "virticle setup --undo" takes the firewall rule
// away again. Packages never change the firewall; this explicit step does.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"virticle/internal/api"
	"virticle/internal/cluster"
	"virticle/internal/prepare"
	"virticle/internal/store"
	"virticle/internal/tlscert"
	"virticle/internal/webaccess"
)

const setupUsage = `usage: sudo virticle setup [--admin NAME --admin-password-file FILE] [--node | --no-node]
                           [--address ADDR] [--port N] [--no-firewall] [--yes] [--check] [--undo] [--data DIR]

Run once on the machine where the controller is installed, as root. Setup
  - on a terminal, asks which address and port the web interface listens on (Enter keeps the
    default: all addresses, port 8463). Not on a terminal it uses the defaults,
  - checks that the address and port are free and names the program that uses them if not,
  - creates the first administrator: on a terminal it asks for the name and password; scripts
    give --admin NAME and the password in --admin-password-file FILE or VIRTICLE_ADMIN_PASSWORD,
  - opens the web port in the firewall when firewalld or ufw is active (a service or rule
    named virticle; when the port was changed since the last run the old port is closed again).
    On a terminal it asks first. Without one it opens the port, unless --no-firewall is given,
  - makes sure the virticle service is enabled and running,
  - when this machine can run virtual machines, asks "Use this machine as a node too?" and adds
    it (as sudo virticle node add does),
  - ends with a summary and the address to open.
--admin NAME          the first administrator's name (default admin when a password is given).
--admin-password-file FILE
                      the administrator's password: the first line of FILE. The environment
                      variable VIRTICLE_ADMIN_PASSWORD works too. Never put it on the command line.
--node, --no-node     answer the node question. Without a terminal and without either, the
                      machine is not added.
--address ADDR        an IP address of this machine, or "all" (the default). Answers the question.
--port N              a port from 1 to 65535 (the default is 8463). Answers the question.
                      Both only apply while no administrator exists and the web settings are still
                      the first-start ones. Later, change them on the Web access page.
--no-firewall         does not change the firewall; prints the command to run yourself.
--yes                 asks nothing: takes the defaults, opens the firewall and adds this machine as
                      a node when it can run virtual machines.
--check               changes nothing and asks nothing.
--undo                removes the firewall rule that setup added and nothing else.
Running setup again changes nothing that is done and says so. --data defaults to /var/lib/virticle
on an installed machine.`

// adminPasswordEnv holds the first administrator's password for scripts.
const adminPasswordEnv = "VIRTICLE_ADMIN_PASSWORD"

// webFirewallService names the firewalld service and the ufw rule comment of
// the controller's web port.
const webFirewallService = "virticle"

const unitName = "virticle.service"

// setupEnv is what setup needs from the machine; tests replace it.
type setupEnv struct {
	euid     int
	sh       prepare.RunFunc // runs a shell script as root on this machine
	settings func() (ws webaccess.Settings, source string, hasUsers bool, err error)
	// pendingPrev returns the settings a web access change that waits for
	// confirmation would go back to; nil when no change waits (or in tests).
	pendingPrev func() *webaccess.Settings
	addrs       []string
	hasKVM      bool
	// hasLocalNode is true when this machine is already registered as a node.
	hasLocalNode bool

	// terminal says that standard input and output are a terminal, so that
	// ask may be used.
	terminal bool
	// ask prints the question and reads one line; ok is false at the end of input.
	ask func(question string) (answer string, ok bool)
	// ips are the addresses of this machine that --address and the question accept.
	ips []string
	// probe tries to listen on addr; nil when it works.
	probe func(addr string) error
	// holder names the program that listens on the port, and its process id;
	// empty and 0 when it cannot tell.
	holder func(port int) (program string, pid int)
	// save stores the web access settings before the service is started.
	save func(ws webaccess.Settings) error

	// vmProblem says why this machine cannot run virtual machines (hasKVM is false then); "" when it can.
	vmProblem string
	// admins are the administrators that exist; createAdmin makes the first one.
	admins      func() []string
	createAdmin func(name, password string) error
	// readPassword asks for a password on the terminal without showing it.
	readPassword func(prompt string) (string, error)
}

// setupOpts are the command-line choices of "virticle setup".
type setupOpts struct {
	check, undo bool
	port        string // --port, "" when not given
	address     string // --address, "" when not given
	noFirewall  bool
	yes         bool

	admin         string // --admin, "" when not given
	adminPassword string // from --admin-password-file or VIRTICLE_ADMIN_PASSWORD
	node, noNode  bool
}

// setupResult is what runSetupAll settled, beside the rows.
type setupResult struct {
	rows    []prepare.Row
	addNode bool   // add this machine as a node now
	url     string // the address to open
	hasUser bool   // an administrator exists (or was just made)
}

// canAsk says whether setup may ask questions: on a terminal, not with
// --check or --yes.
func (e setupEnv) canAsk(o setupOpts) bool {
	return e.terminal && e.ask != nil && !o.yes && !o.check
}

// webPorts are the TCP ports the controller listens on for other computers:
// the HTTPS or HTTP listener, and the redirect listener when there is one.
// Listeners on a loopback address need no firewall port.
func webPorts(ws webaccess.Settings) (ports []string) {
	addrs := []string{ws.Listen}
	if ws.IsTLS() {
		addrs = []string{ws.HTTPSListen, ws.HTTPRedirect}
	}
	for _, a := range addrs {
		if a == "" {
			continue
		}
		if tlscert.IsLoopback(a) {
			continue
		}
		if _, port, err := webaccess.SplitAddr(a); err == nil && port > 0 {
			p := fmt.Sprint(port)
			dup := false
			for _, q := range ports {
				dup = dup || p == q
			}
			if !dup {
				ports = append(ports, p)
			}
		}
	}
	return ports
}

func webFirewall(ports []string) prepare.FirewallSpec {
	return prepare.FirewallSpec{
		Service:     webFirewallService,
		Description: "Virticle: web interface",
		Ports:       ports,
		Actor:       "Setup",
		Purpose:     "for the web interface",
	}
}

const unitCheckScript = `
if ! systemctl cat ` + unitName + ` >/dev/null 2>&1; then echo unit=no; exit 0; fi
echo unit=yes
echo "active=$(systemctl is-active ` + unitName + ` 2>/dev/null)"
systemctl is-enabled --quiet ` + unitName + ` 2>/dev/null && echo enabled=yes || echo enabled=no
true`

const unitStartScript = `out=$(systemctl enable --now ` + unitName + ` 2>&1) || { echo "@@detail $(echo "$out" | head -n 1)"; echo @@failed; }
true`

const unitRestartScript = `out=$(systemctl enable ` + unitName + ` 2>&1 && systemctl restart ` + unitName + ` 2>&1) || { echo "@@detail $(echo "$out" | head -n 1)"; echo @@failed; }
true`

// serviceRow reports (and unless check is set, fixes) the state of the
// service. restart says that the web settings were changed, so a running
// service has to be restarted to use them. It returns an info row on a
// machine without the unit.
func serviceRow(e setupEnv, check, restart bool) []prepare.Row {
	out, err := e.sh(unitCheckScript)
	if err != nil || !strings.Contains(out, "unit=yes") {
		return []prepare.Row{{ID: "service", Title: "Service", Status: prepare.Info,
			Detail: "There is no systemd service " + unitName + " on this machine. Start virticle yourself."}}
	}
	running := strings.Contains(out, "active=active")
	enabled := strings.Contains(out, "enabled=yes")
	row := prepare.Row{ID: "service", Title: "Service"}
	script, done := unitStartScript, "Enabled and started "+unitName+"."
	switch {
	case running && restart:
		script, done = unitRestartScript, "Restarted "+unitName+" to use the new web address."
		if check {
			row.Status, row.Detail = prepare.Info, "Setup will restart "+unitName+" to use the new web address."
			return []prepare.Row{row}
		}
	case running && enabled:
		row.Status, row.Detail = prepare.OK, unitName+" is already running and enabled."
		return []prepare.Row{row}
	case check:
		row.Status, row.Detail = prepare.Info, "Setup will enable and start "+unitName+"."
		return []prepare.Row{row}
	}
	sout, serr := e.sh(script)
	if serr != nil || strings.Contains(sout, "@@failed") {
		detail := ""
		for _, l := range strings.Split(sout, "\n") {
			if strings.HasPrefix(l, "@@detail ") {
				detail = strings.TrimPrefix(l, "@@detail ")
			}
		}
		row.Status = prepare.Warning
		row.Detail = "Could not start " + unitName + " (" + strings.TrimSpace(detail+" "+errText(serr)) + "). Run: systemctl enable --now " + unitName + " and look at: journalctl -u virticle"
	} else {
		row.Status, row.Detail = prepare.OK, done
	}
	return []prepare.Row{row}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// notSetUp reports whether this machine has not been set up yet: no
// administrator exists and the web settings are nothing but what a first
// start stores (the port and address may differ). Only then may setup choose
// the listener. A machine that was set up keeps its listener; it is changed on
// the Web access page.
func notSetUp(ws webaccess.Settings, source string, hasUsers bool) bool {
	if hasUsers {
		return false
	}
	if source != "stored" {
		return true
	}
	a, b := ws.Normalize(), webaccess.FirstRunDefaults().Normalize()
	a.HTTPSListen, b.HTTPSListen = "", ""
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

// parsePort reads a port number from --port or an answer.
func parsePort(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("%q is not a port. Use a number from 1 to 65535", s)
	}
	return n, nil
}

// parseAddress reads --address or an answer: "all" (or empty) is every
// interface and gives ""; otherwise an IP address that this machine has.
func parseAddress(s string, ips []string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "all") || s == "0.0.0.0" || s == "::" {
		return "", nil
	}
	ip := net.ParseIP(strings.Trim(s, "[]"))
	if ip == nil {
		return "", fmt.Errorf("%q is not an IP address. Use an address of this machine, or all", s)
	}
	if ip.IsLoopback() {
		return ip.String(), nil
	}
	for _, a := range ips {
		if x := net.ParseIP(a); x != nil && x.Equal(ip) {
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("%s is not an address of this machine (it has %s). Use one of them, or all", ip, strings.Join(displayIPs(ips), ", "))
}

// sameListen compares two listen addresses; 0.0.0.0 and :: and nothing are
// the same host.
func sameListen(a, b string) bool {
	norm := func(x string) string {
		h, p, err := net.SplitHostPort(x)
		if err != nil {
			return x
		}
		if webaccess.IsWildcard(x) {
			h = ""
		}
		return net.JoinHostPort(h, p)
	}
	return norm(a) == norm(b)
}

// hostPort splits a listen address into its host ("" for every interface)
// and port; 0 when it cannot be read.
func hostPort(listen string) (string, int) {
	h, p, err := webaccess.SplitAddr(listen)
	if err != nil {
		return "", 0
	}
	if webaccess.IsWildcard(listen) {
		h = ""
	}
	return h, p
}

func hostText(h string) string {
	if h == "" {
		return "all addresses"
	}
	return h
}

// displayIPs are the addresses worth offering in the question.
func displayIPs(ips []string) []string {
	var out []string
	for _, a := range ips {
		if ip := net.ParseIP(a); ip != nil && !ip.IsLoopback() {
			out = append(out, a)
		}
	}
	return out
}

// addressQuestion lists the machine's addresses and asks which one to use.
func addressQuestion(ips []string, def, problem string) string {
	var b strings.Builder
	if problem != "" {
		b.WriteString(problem + "\n")
	}
	b.WriteString("Which address should the web interface listen on?\n  all   every address of this machine\n")
	for _, a := range displayIPs(ips) {
		b.WriteString("  " + a + "\n")
	}
	if def == "" {
		def = "all"
	}
	b.WriteString("Address [" + def + "]: ")
	return b.String()
}

func portQuestion(def int, problem string) string {
	q := fmt.Sprintf("Port [%d]: ", def)
	if problem != "" {
		q = problem + "\n" + q
	}
	return q
}

// askUntil asks the question until parse accepts the answer. An empty answer
// or the end of input takes def. After five wrong answers it gives up.
func askUntil[T any](e setupEnv, question func(problem string) string, def T, parse func(string) (T, error)) (T, error) {
	problem := ""
	for i := 0; i < 5; i++ {
		ans, ok := e.ask(question(problem))
		ans = strings.TrimSpace(ans)
		if !ok || ans == "" {
			return def, nil
		}
		v, err := parse(ans)
		if err == nil {
			return v, nil
		}
		problem = err.Error() + "."
	}
	var zero T
	return zero, errors.New("no valid answer was given")
}

func errorRow(id, title, detail string) prepare.Row {
	return prepare.Row{ID: id, Title: title, Status: prepare.Error, Detail: detail}
}

// servicePID is the main process of the service; 0 when it does not run.
func servicePID(e setupEnv) int {
	out, err := e.sh("systemctl show -p MainPID " + unitName + " 2>/dev/null")
	if err != nil {
		return 0
	}
	_, v, _ := strings.Cut(strings.TrimSpace(out), "MainPID=")
	n, _ := strconv.Atoi(strings.TrimSpace(v))
	return n
}

// listenersOf are the addresses the controller listens on for ws.
func listenersOf(ws webaccess.Settings) []string {
	var out []string
	add := func(a string) {
		if a != "" && !contains(out, a) {
			out = append(out, a)
		}
	}
	if ws.IsTLS() {
		add(ws.HTTPSListen)
		add(ws.HTTPRedirect)
	} else {
		add(ws.Listen)
	}
	return out
}

// portProblem says why addr cannot be listened on; "" when it can, or when
// the virticle service itself holds it. current are the addresses the service
// is set to use now, svc its process id (0 when it does not run).
func portProblem(e setupEnv, addr string, current []string, svc int) string {
	if e.probe == nil {
		return ""
	}
	err := e.probe(addr)
	if err == nil {
		return ""
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		return fmt.Sprintf("Cannot listen on %s (%s).", addr, lastPart(err))
	}
	host, port := hostPort(addr)
	what := fmt.Sprintf("TCP port %d on %s", port, hostText(host))
	prog, pid := "", 0
	if e.holder != nil {
		prog, pid = e.holder(port)
	}
	switch {
	case pid != 0 && pid == svc:
		return ""
	case pid == 0 && prog == "" && svc != 0 && containsListen(current, addr):
		return "" // setup cannot tell who it is, and it is the address the service is set to use
	case prog != "":
		return fmt.Sprintf("%s is already used by %s (process %d).", what, prog, pid)
	}
	return what + " is already used by another program (setup could not tell which one)."
}

func containsListen(list []string, addr string) bool {
	for _, l := range list {
		if sameListen(l, addr) {
			return true
		}
	}
	return false
}

// lastPart is the error text without the "listen tcp addr:" prefix.
func lastPart(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:]
	}
	return s
}

func portAdvice(chosen bool) string {
	if chosen {
		return " Nothing was changed. Free the port, or run setup again with another port: sudo virticle setup --port N"
	}
	return " Nothing was changed. Free the port, or choose another one on the Web access page."
}

// firewallRows opens (or, with no ports, closes) the web ports the way the
// options and the terminal allow.
func firewallRows(e setupEnv, o setupOpts, ports []string) []prepare.Row {
	spec := webFirewall(ports)
	if o.check {
		if len(ports) == 0 {
			return nil
		}
		f := prepare.ParseFirewallFacts(mustSh(e, prepare.FirewallCheckScript(spec)))
		return []prepare.Row{prepare.FirewallCheckRow(f, spec)}
	}
	apply := func() []prepare.Row {
		row, changed := prepare.ApplyFirewall(e.sh, spec)
		if len(ports) > 0 || changed || row.Status == prepare.Warning {
			return []prepare.Row{row}
		}
		return nil
	}
	if len(ports) == 0 {
		if o.noFirewall {
			return nil
		}
		return apply() // removes the rule of a port that is gone
	}
	if !o.noFirewall && !e.canAsk(o) {
		return apply()
	}
	f := prepare.ParseFirewallFacts(mustSh(e, prepare.FirewallCheckScript(spec)))
	if (f.Kind != "firewalld" && f.Kind != "ufw") || f.Open {
		if o.noFirewall {
			return []prepare.Row{prepare.FirewallCheckRow(f, spec)}
		}
		return apply()
	}
	text, cmd := prepare.PortsText(ports), prepare.ManualCommands(f.Kind, spec)
	if o.noFirewall {
		return []prepare.Row{{ID: "firewall", Title: "Firewall", Status: prepare.Info,
			Detail: fmt.Sprintf("%s is active. Left alone (--no-firewall). To open %s, run as root: %s", f.Kind, text, cmd)}}
	}
	later := prepare.Row{ID: "firewall", Title: "Firewall", Status: prepare.Info,
		Detail: fmt.Sprintf("%s is active. Not opened. To open %s later, run as root: %s", f.Kind, text, cmd)}
	question := fmt.Sprintf("Open %s in %s for the web interface? [Y/n] ", text, f.Kind)
	for i := 0; i < 3; i++ {
		ans, ok := e.ask(question)
		switch strings.ToLower(strings.TrimSpace(ans)) {
		case "":
			if ok {
				return apply()
			}
			return []prepare.Row{later} // no answer at all: do not change the firewall
		case "y", "yes":
			return apply()
		case "n", "no":
			return []prepare.Row{later}
		}
		question = "Please answer y or n. " + question
	}
	return []prepare.Row{later}
}

// runSetup does setup (or only looks, with check; or takes the firewall rule
// away, with undo) and returns the rows to print.
func runSetup(e setupEnv, o setupOpts) []prepare.Row { return runSetupAll(e, o).rows }

func runSetupAll(e setupEnv, o setupOpts) setupResult {
	if e.euid != 0 {
		return setupResult{rows: []prepare.Row{{ID: "root", Title: "Root", Status: prepare.Error,
			Detail: "setup changes the firewall and the service, so it needs root. Run: sudo virticle setup"}}}
	}
	if o.undo {
		return setupResult{rows: runSetupRows(e, o, nil)}
	}
	var res setupResult
	rows := runSetupRows(e, o, &res)
	res.rows = rows
	return res
}

// runSetupRows is setup itself; res (nil for --undo) receives what it settled.
func runSetupRows(e setupEnv, o setupOpts, res *setupResult) []prepare.Row {
	if o.undo {
		var row prepare.Row
		if o.check {
			f := prepare.ParseFirewallFacts(mustSh(e, prepare.FirewallCheckScript(webFirewall(nil))))
			row = prepare.Row{ID: "firewall", Title: "Firewall", Status: prepare.Info, Detail: "Setup --undo would remove the rule named " + webFirewallService + " from " + f.Kind + "."}
			if f.Kind != "firewalld" && f.Kind != "ufw" {
				row.Status, row.Detail = prepare.OK, "No firewall is active on this host. Nothing to remove."
			}
		} else {
			row, _ = prepare.ApplyFirewall(e.sh, webFirewall(nil))
		}
		return []prepare.Row{row, {ID: "summary", Title: "Result", Status: prepare.Info, Detail: "Only the firewall rule was removed. The service and its data are unchanged."}}
	}

	ws, source, hasUsers, err := e.settings()
	if err != nil {
		return []prepare.Row{{ID: "web", Title: "Web access", Status: prepare.Error, Detail: err.Error()}}
	}
	var rows []prepare.Row
	current := listenersOf(ws)
	svc := servicePID(e)

	// The listener: chosen here on a machine that is not set up yet, else kept.
	changed := false
	fresh := notSetUp(ws, source, hasUsers)
	kind := map[bool]string{true: "stored settings", false: "the default for a first start"}[source == "stored"]
	if o.port != "" || o.address != "" || e.canAsk(o) {
		next, stop := chooseListener(e, o, ws, source, hasUsers, current, svc)
		if stop != nil {
			return []prepare.Row{*stop}
		}
		if !sameListen(next.HTTPSListen, ws.HTTPSListen) {
			ws, changed, kind = next, true, "chosen in setup"
		}
	}

	// Before anything is changed: is the address free?
	var free []prepare.Row
	restart := changed
	for _, addr := range listenersOf(ws) {
		if msg := portProblem(e, addr, current, svc); msg != "" {
			return []prepare.Row{errorRow("port", "Port", msg+portAdvice(fresh))}
		}
		if fresh && svc != 0 && e.probe != nil && e.probe(addr) == nil {
			restart = true // the service runs but does not hold the address (it fell back earlier): restart it
		}
		if changed || o.check {
			host, port := hostPort(addr)
			free = append(free, prepare.Row{ID: "port", Title: "Port", Status: prepare.OK,
				Detail: fmt.Sprintf("TCP port %d on %s is free to use.", port, hostText(host))})
		}
	}

	// The questions and the script inputs, all before anything is changed.
	adminName, adminPW, stop := chooseAdmin(e, o, hasUsers)
	if stop != nil {
		return []prepare.Row{*stop}
	}
	addNode, stop := chooseNode(e, o)
	if stop != nil {
		return []prepare.Row{*stop}
	}

	if changed && !o.check {
		if err := e.save(ws); err != nil {
			return []prepare.Row{errorRow("web", "Web access", "Could not store the web access settings ("+err.Error()+"). Nothing was changed.")}
		}
	}
	// The administrator before the service starts, so that nobody else can claim the first-run page.
	adminRow := prepare.Row{ID: "admin", Title: "Administrator"}
	switch {
	case hasUsers:
		names := []string{}
		if e.admins != nil {
			names = e.admins()
		}
		adminRow.Status, adminRow.Detail = prepare.OK, "Already set up ("+strings.Join(names, ", ")+")."
		if len(names) == 0 {
			adminRow.Detail = "Users exist already."
		}
		if o.admin != "" && !contains(names, o.admin) {
			adminRow.Detail += " Setup creates no other; add users on the Users page."
		}
	case adminName != "" && o.check:
		adminRow.Status, adminRow.Detail = prepare.Info, "Setup will create the administrator "+adminName+"."
	case adminName != "":
		if err := e.createAdmin(adminName, adminPW); err != nil {
			return []prepare.Row{errorRow("admin", "Administrator", "Could not create "+adminName+": "+err.Error())}
		}
		adminRow.Status, adminRow.Detail = prepare.OK, "Created "+adminName+"."
		hasUsers = true
	default:
		adminRow.Status, adminRow.Detail = prepare.Info, "None yet. Create it in the web interface, or run: sudo virticle setup --admin NAME --admin-password-file FILE"
	}

	ports := webPorts(ws)
	var waiting []string
	if e.pendingPrev != nil {
		if prev := e.pendingPrev(); prev != nil {
			// The page asks for setup while the change is still unconfirmed:
			// the new port must be reachable to confirm it, and the old port
			// stays open in case the change is not confirmed.
			for _, p := range webPorts(*prev) {
				if !contains(ports, p) {
					ports = append(ports, p)
					waiting = append(waiting, p)
				}
			}
		}
	}
	desc := "HTTP"
	if ws.IsTLS() {
		desc = "HTTPS"
	}
	where := ""
	if h, _ := hostPort(listenOf(ws)); h != "" && !tlscert.IsLoopback(listenOf(ws)) {
		where = h + ", "
	}
	if len(ports) > 0 {
		rows = append(rows, prepare.Row{ID: "web", Title: "Web access", Status: prepare.Info,
			Detail: fmt.Sprintf("%s on %s%s (%s).", desc, where, prepare.PortsText(ports), kind)})
	} else {
		rows = append(rows, prepare.Row{ID: "web", Title: "Web access", Status: prepare.Info,
			Detail: fmt.Sprintf("%s on this computer only (%s), so no firewall port is needed (%s).", desc, listenOf(ws), kind)})
	}
	if changed && o.check {
		rows[0].Detail += " Setup will store it."
	}
	rows = append(rows, free...)

	if len(waiting) > 0 {
		rows = append(rows, prepare.Row{ID: "pending", Title: "Web access", Status: prepare.Info,
			Detail: "A change is waiting for confirmation on the Web access page. Setup keeps " + prepare.PortsText(waiting) +
				" open as well until you have confirmed it; run setup again after that to close it."})
	}

	rows = append(rows, adminRow)
	rows = append(rows, firewallRows(e, o, ports)...)
	rows = append(rows, serviceRow(e, o.check, restart)...)

	switch {
	case e.hasLocalNode:
		rows = append(rows, prepare.Row{ID: "node", Title: "Node", Status: prepare.OK, Detail: "This machine is a node already."})
	case addNode && o.check:
		rows = append(rows, prepare.Row{ID: "node", Title: "Node", Status: prepare.Info, Detail: "Setup will add this machine as a node."})
	case e.hasKVM && !addNode:
		rows = append(rows, prepare.Row{ID: "node", Title: "Node", Status: prepare.Info,
			Detail: "This machine is not a node. To run virtual machines on it too: sudo virticle node add"})
	}
	if res != nil {
		res.addNode = addNode && !o.check
		res.url = openURL(ws, e.addrs)
		res.hasUser = hasUsers
	}
	return rows
}

// chooseAdmin settles the first administrator from --admin and the password (file or environment), or from questions
// on a terminal. An empty name means none is made now. A returned row ends setup before anything is changed.
func chooseAdmin(e setupEnv, o setupOpts, hasUsers bool) (name, password string, stop *prepare.Row) {
	fail := func(detail string) (string, string, *prepare.Row) {
		r := errorRow("admin", "Administrator", detail)
		return "", "", &r
	}
	if hasUsers {
		return "", "", nil
	}
	name, password = strings.TrimSpace(o.admin), o.adminPassword
	if name == "" && password != "" {
		name = "admin"
	}
	terminal := e.terminal && e.readPassword != nil && !o.check
	if name == "" {
		if !e.canAsk(o) || e.readPassword == nil {
			return "", "", nil
		}
		ans, ok := e.ask("Administrator of the web interface [admin]: ")
		if !ok {
			return "", "", nil // no input: the browser creates it
		}
		if name = strings.TrimSpace(ans); name == "" {
			name = "admin"
		}
	}
	if err := api.ValidateUsername(name); err != nil {
		return fail(upperFirst(err.Error()) + ".")
	}
	if password == "" {
		if !terminal {
			if o.check {
				return name, "", nil
			}
			return fail("--admin " + name + " needs the password: --admin-password-file FILE, or the environment variable " + adminPasswordEnv)
		}
		for i := 0; ; i++ {
			a, err := e.readPassword("Password for " + name + ": ")
			if err != nil {
				return fail(err.Error())
			}
			msg := ""
			if err := api.ValidateFirstAdmin(name, a); err != nil {
				msg = err.Error()
			} else if b, err := e.readPassword("Again: "); err != nil {
				return fail(err.Error())
			} else if a != b {
				msg = "the two passwords differ"
			}
			if msg == "" {
				password = a
				break
			}
			if i == 2 {
				return fail(upperFirst(msg) + ". Run setup again.")
			}
			fmt.Println(upperFirst(msg) + ".")
		}
	}
	if err := api.ValidateFirstAdmin(name, password); err != nil {
		return fail(upperFirst(err.Error()) + ".")
	}
	return name, password, nil
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// chooseNode says whether this machine is to be added as a node: --node or --no-node, --yes, or the question on a
// terminal. Without either and without a terminal it is not.
func chooseNode(e setupEnv, o setupOpts) (bool, *prepare.Row) {
	fail := func(detail string) (bool, *prepare.Row) {
		r := errorRow("node", "Node", detail)
		return false, &r
	}
	switch {
	case o.node && o.noNode:
		return fail("--node and --no-node exclude each other.")
	case o.node && !e.hasKVM:
		why := e.vmProblem
		if why == "" {
			why = "it has no /dev/kvm"
		}
		return fail("--node: this machine cannot run virtual machines (" + lowerFirst(why) + "). Nothing was changed.")
	case !e.hasKVM || e.hasLocalNode || o.noNode:
		return false, nil
	case o.node || o.yes:
		return true, nil
	case e.canAsk(o):
		return askYes(e.ask, "Use this machine as a node too? [Y/n] ", true), nil
	}
	return false, nil
}

// chooseListener settles the HTTPS listener of a machine that is not set up
// yet, from --address and --port and, on a terminal, two questions. A
// machine that is set up keeps its listener; --address and --port that name
// another one are refused. A returned row ends setup.
func chooseListener(e setupEnv, o setupOpts, ws webaccess.Settings, source string, hasUsers bool, current []string, svc int) (webaccess.Settings, *prepare.Row) {
	fresh := notSetUp(ws, source, hasUsers)
	curHost, curPort := hostPort(ws.HTTPSListen)
	if !fresh || !ws.IsTLS() {
		curHost, curPort = hostPort(listenOf(ws))
	}
	host, port := curHost, curPort
	stop := func(id, title, detail string) (webaccess.Settings, *prepare.Row) {
		r := errorRow(id, title, detail)
		return ws, &r
	}
	var err error
	if o.address != "" {
		if host, err = parseAddress(o.address, e.ips); err != nil {
			return stop("address", "Address", "--address: "+err.Error()+".")
		}
	}
	if o.port != "" {
		if port, err = parsePort(o.port); err != nil {
			return stop("port", "Port", "--port: "+err.Error()+".")
		}
	}
	if !fresh {
		if sameListen(webaccess.ComposeListen(host, port), listenOf(ws)) {
			return ws, nil // the listener it has: nothing to change
		}
		return stop("web", "Web access", "This machine is already set up, so setup does not change its web address. "+
			"Change it on the Web access page (it asks you to confirm, and goes back by itself if you do not). "+
			"Or, with the service stopped, run: virticle web set -https-listen ADDRESS:PORT")
	}
	if e.canAsk(o) {
		if o.address == "" {
			host, err = askUntil(e, func(p string) string { return addressQuestion(e.ips, curHost, p) }, curHost,
				func(s string) (string, error) { return parseAddress(s, e.ips) })
			if err != nil {
				return stop("address", "Address", err.Error()+". Run setup again, or give --address.")
			}
		}
		if o.port == "" {
			port, err = askUntil(e, func(p string) string { return portQuestion(curPort, p) }, curPort, parsePort)
			if err != nil {
				return stop("port", "Port", err.Error()+". Run setup again, or give --port.")
			}
		}
		// A port that something else uses: offer another one.
		note := ""
		for i := 0; ; i++ {
			msg := portProblem(e, webaccess.ComposeListen(host, port), current, svc)
			if msg == "" {
				break
			}
			ans, ok := "", false
			if i < 5 {
				ans, ok = e.ask(note + msg + "\nAnother port, or press Enter to stop: ")
			}
			if !ok || strings.TrimSpace(ans) == "" {
				return stop("port", "Port", msg+portAdvice(true))
			}
			p, perr := parsePort(ans)
			if perr != nil {
				note = perr.Error() + ".\n"
				continue
			}
			port, note = p, ""
		}
	}
	ws.HTTPSListen = webaccess.ComposeListen(host, port)
	return ws, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func mustSh(e setupEnv, script string) string {
	out, _ := e.sh(script)
	return out
}

func listenOf(ws webaccess.Settings) string {
	if ws.IsTLS() {
		return ws.HTTPSListen
	}
	return ws.Listen
}

// localSh runs a script with sh on this machine.
func localSh(script string) (string, error) {
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	return string(out), err
}

// storedWebSettings reads the settings the controller would use at its next
// start, the way the controller chooses them. A machine without a database
// yet gets what the controller picks on its first start.
func storedWebSettings(data string) func() (webaccess.Settings, string, bool, error) {
	return func() (webaccess.Settings, string, bool, error) {
		if !store.HasDatabase(data) {
			c := chooseSettings(webaccess.Settings{}, false, false, legacyListen())
			return c.settings, c.source, false, nil
		}
		dbPath, err := store.DBPath(data, log.Printf)
		if err != nil {
			return webaccess.Settings{}, "", false, fmt.Errorf("database: %v", err)
		}
		st, err := store.Open(dbPath)
		if err != nil {
			return webaccess.Settings{}, "", false, fmt.Errorf("database: %v", err)
		}
		defer st.Close()
		m := &webaccess.Manager{Store: st, DataDir: data}
		// Peek, not Load: setup runs beside the controller and must not undo a
		// change that waits for confirmation.
		stored, _, ok, err := m.Peek()
		if err != nil {
			return webaccess.Settings{}, "", false, err
		}
		users, err := st.UserCount()
		if err != nil {
			return webaccess.Settings{}, "", false, fmt.Errorf("database: %v", err)
		}
		c := chooseSettings(stored, ok, users > 0, legacyListen())
		return c.settings, c.source, users > 0, nil
	}
}

// pendingWebSettings returns what an unconfirmed web access change would go
// back to, read-only; nil when none waits or there is no database.
func pendingWebSettings(data string) func() *webaccess.Settings {
	return func() *webaccess.Settings {
		if !store.HasDatabase(data) {
			return nil
		}
		dbPath, err := store.DBPath(data, log.Printf)
		if err != nil {
			return nil
		}
		st, err := store.Open(dbPath)
		if err != nil {
			return nil
		}
		defer st.Close()
		m := &webaccess.Manager{Store: st, DataDir: data}
		_, prev, _, _ := m.Peek()
		return prev
	}
}

func setupCommand(args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, setupUsage) }
	var o setupOpts
	fs.BoolVar(&o.check, "check", false, "look only; change nothing")
	fs.BoolVar(&o.undo, "undo", false, "remove the firewall rule that setup added")
	fs.StringVar(&o.address, "address", "", "IP address of this machine for the web interface, or all")
	fs.StringVar(&o.port, "port", "", "HTTPS port of the web interface")
	fs.BoolVar(&o.noFirewall, "no-firewall", false, "do not change the firewall")
	fs.BoolVar(&o.yes, "yes", false, "ask nothing; answer yes to the firewall and node questions")
	fs.StringVar(&o.admin, "admin", "", "the first administrator's name")
	pwFile := fs.String("admin-password-file", "", "the first administrator's password: the first line of this file")
	fs.BoolVar(&o.node, "node", false, "add this machine as a node")
	fs.BoolVar(&o.noNode, "no-node", false, "do not add this machine as a node")
	data := fs.String("data", defaultDataDir(), "the controller's data directory")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 || (o.undo && (o.address != "" || o.port != "" || o.admin != "" || *pwFile != "" || o.node)) {
		fmt.Fprintln(os.Stderr, setupUsage)
		return 2
	}
	if *pwFile != "" {
		pw, err := firstLineOf(*pwFile)
		if err != nil {
			return errorLine(2, "--admin-password-file: %v", err)
		}
		o.adminPassword = pw
	} else if v := os.Getenv(adminPasswordEnv); v != "" {
		o.adminPassword = v
	}
	e := setupEnv{euid: os.Geteuid(), sh: localSh, settings: storedWebSettings(*data), pendingPrev: pendingWebSettings(*data), addrs: suggestedAddresses()}
	e.vmProblem = api.LocalNodeProblem()
	e.hasKVM = e.vmProblem == ""
	e.hasLocalNode = e.hasKVM && hasLocalNode(*data, e.addrs)
	e.terminal = term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	e.ask = stdinAsker(bufio.NewReader(os.Stdin))
	e.readPassword = realCredIO().readPassword
	e.ips = machineIPs()
	e.probe = listenProbe
	e.holder = portHolder
	e.save = saveWebSettings(*data)
	e.admins = func() []string { return adminNames(*data) }
	e.createAdmin = createFirstAdmin(*data)
	res := runSetupAll(e, o)
	for _, r := range res.rows {
		printRow(r)
	}
	code := exitCode(res.rows)
	if code != 0 || o.check || o.undo {
		return code
	}
	running := false
	if out, err := localSh(unitCheckScript); err == nil && strings.Contains(out, "active=active") {
		running = true
		// The service was (re)started just now: let it open its socket before the commands below use it.
		waitForSocket(*data, 20*time.Second)
	}
	if res.addNode {
		fmt.Println()
		if c := addNode(addReq{data: *data, out: os.Stdout}, realCredIO()); c != 0 {
			code = c
		}
	}
	fmt.Println()
	setupSummary(os.Stdout, *data, running, res)
	return code
}

// setupSummary prints the three lines that end setup, and the address to open.
func setupSummary(w io.Writer, data string, running bool, res setupResult) {
	var ns []cluster.NodeStatus
	if c, err := openCtl(data, 5*time.Second); err == nil {
		ns, _ = c.settledNodes(20 * time.Second)
		c.Close()
	}
	fmt.Fprint(w, summaryText(running, res, adminNames(data), ns))
}

func summaryText(running bool, res setupResult, admins []string, ns []cluster.NodeStatus) string {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	ctlLine := "service running"
	if !running {
		ctlLine = "service not running"
	}
	if res.url != "" {
		ctlLine += ", " + res.url
	}
	fmt.Fprintf(tw, "Controller\t%s\n", ctlLine)
	if len(admins) > 0 {
		fmt.Fprintf(tw, "Administrator\t%s\n", strings.Join(admins, ", "))
	} else {
		fmt.Fprintf(tw, "Administrator\tnone yet\n")
	}
	var parts []string
	for _, n := range ns {
		parts = append(parts, nodeName(n)+" "+n.State)
	}
	if len(parts) == 0 {
		parts = []string{"none (add one: sudo virticle node add [user@]host)"}
	}
	fmt.Fprintf(tw, "Nodes\t%s\n", strings.Join(parts, ", "))
	tw.Flush()
	switch {
	case res.url != "" && len(admins) > 0:
		fmt.Fprintf(&b, "Open %s to sign in.\n", res.url)
	case res.url != "":
		fmt.Fprintf(&b, "Open %s to create the administrator.\n", res.url)
	}
	return b.String()
}

// createFirstAdmin makes the first administrator in the database, as the first-run page does. The running
// controller reads users from the database on every request, so it needs no restart.
func createFirstAdmin(data string) func(name, password string) error {
	return func(name, password string) error {
		if err := os.MkdirAll(data, 0o700); err != nil {
			return err
		}
		dbPath, err := store.DBPath(data, log.Printf)
		if err != nil {
			return err
		}
		st, err := store.Open(dbPath)
		if err != nil {
			return err
		}
		_, err = st.CreateFirstUser(name, password)
		if cerr := st.Close(); err == nil {
			err = cerr
		}
		if errors.Is(err, store.ErrConflict) {
			err = errors.New("an administrator was created meanwhile")
		}
		if err != nil {
			return err
		}
		return chownLike(data, dbPath, dbPath+"-wal", dbPath+"-shm")
	}
}

// stdinAsker prints a question and reads the answer line from in.
func stdinAsker(in *bufio.Reader) func(string) (string, bool) {
	return func(question string) (string, bool) {
		fmt.Print(question)
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			fmt.Println()
			return "", false
		}
		return strings.TrimSpace(line), true
	}
}

// machineIPs are the addresses of this machine's interfaces.
func machineIPs() []string {
	var out []string
	for _, in := range webaccess.Interfaces() {
		out = append(out, in.Addrs...)
	}
	return out
}

// listenProbe tries to listen on addr, then lets go of it again.
func listenProbe(addr string) error {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return l.Close()
}

// portHolder asks ss which program listens on the port.
func portHolder(port int) (string, int) {
	out, err := exec.Command("ss", "-ltnpH").Output()
	if err != nil {
		return "", 0
	}
	return parseSSHolder(string(out), port)
}

var ssProcess = regexp.MustCompile(`\("([^"]*)",pid=(\d+)`)

// parseSSHolder reads "ss -ltnpH" lines such as
//
//	LISTEN 0 4096 0.0.0.0:8463 0.0.0.0:* users:(("nginx",pid=812,fd=6))
//
// and returns the program and process id of the first listener on port.
func parseSSHolder(out string, port int) (string, int) {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || !strings.HasSuffix(f[3], ":"+strconv.Itoa(port)) {
			continue
		}
		if m := ssProcess.FindStringSubmatch(line); m != nil {
			pid, _ := strconv.Atoi(m[2])
			return m[1], pid
		}
		return "", 0
	}
	return "", 0
}

// saveWebSettings stores the web access settings the way "virticle web set"
// does. The files it creates belong to the owner of the data directory, so
// that the service (which runs as its own user) can open them.
func saveWebSettings(data string) func(webaccess.Settings) error {
	return func(ws webaccess.Settings) error {
		if err := os.MkdirAll(data, 0o700); err != nil {
			return err
		}
		dbPath, err := store.DBPath(data, log.Printf)
		if err != nil {
			return err
		}
		st, err := store.Open(dbPath)
		if err != nil {
			return err
		}
		b, _ := json.Marshal(ws.Normalize())
		err = st.SetWebSetting("web", string(b))
		if cerr := st.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		return chownLike(data, dbPath, dbPath+"-wal", dbPath+"-shm")
	}
}

// chownLike gives the files that exist the owner of the directory dir.
func chownLike(dir string, files ...string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || os.Geteuid() != 0 {
		return nil
	}
	for _, f := range files {
		if _, err := os.Lstat(f); err == nil {
			if err := os.Chown(f, int(st.Uid), int(st.Gid)); err != nil {
				return err
			}
		}
	}
	return nil
}

// hasLocalNode says whether the database in data already holds a node whose
// address is one of this machine's addresses or names.
func hasLocalNode(data string, local []string) bool {
	if !store.HasDatabase(data) {
		return false
	}
	dbPath, err := store.DBPath(data, log.Printf)
	if err != nil {
		return false
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return false
	}
	defer st.Close()
	nodes, err := st.Nodes()
	if err != nil {
		return false
	}
	return anyLocalAddr(nodes, local)
}

func anyLocalAddr(nodes []store.Node, local []string) bool {
	here := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	for _, l := range local {
		here[strings.ToLower(l)] = true
	}
	for _, n := range nodes {
		host := n.Addr
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if here[strings.ToLower(strings.Trim(host, "[]"))] {
			return true
		}
	}
	return false
}
