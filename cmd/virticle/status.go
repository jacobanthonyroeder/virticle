package main

// "virticle status": the controller and its nodes on one screen, with the command that fixes what needs it. The exit
// code is 1 when something needs attention.

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"virticle/internal/buildinfo"
	"virticle/internal/cluster"
	"virticle/internal/store"
)

const statusUsage = `usage: sudo virticle status [--data DIR]

Shows the controller's version, its web address, the service, the administrator and every node with its
state. For a node that is outdated or cannot be reached it prints the command that fixes it. Exit code 0
when all is well, 1 when something needs attention. Needs root (or the controller's account).`

// statusFacts is everything status shows; gathered by statusCommand, printed by printStatus.
type statusFacts struct {
	version     string // of this command
	running     string // of the running controller; "" when it does not run or cannot say
	answers     bool   // the controller answers on its socket
	unit        bool   // a systemd unit exists
	active      bool   // and it runs
	web         string // the address to open; "" when unknown
	admins      []string
	nodes       []cluster.NodeStatus
	nodesErr    error
	isLocalNode func(addr string) bool
}

func statusCommand(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	data := fs.String("data", defaultDataDir(), "the controller's data directory")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			fmt.Println(statusUsage)
			return 0
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n\n%s\n", err, statusUsage)
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "Error: status takes no arguments\n\n%s\n", statusUsage)
		return 2
	}
	f := statusFacts{version: buildinfo.Get().Version, isLocalNode: localAddr}
	out, err := localSh(unitCheckScript)
	f.unit = err == nil && strings.Contains(out, "unit=yes")
	f.active = strings.Contains(out, "active=active")
	if !store.HasDatabase(*data) {
		if _, err := os.Stat(*data); os.IsPermission(err) || (err == nil && !readable(*data)) {
			return errorLine(1, "cannot read %s (run it with sudo)", *data)
		}
	}
	if ws, _, _, err := storedWebSettings(*data)(); err == nil {
		f.web = openURL(ws, suggestedAddresses())
	} else if errors.Is(err, os.ErrPermission) || strings.Contains(err.Error(), "permission denied") {
		return errorLine(1, "cannot read the controller's database in %s (run it with sudo)", *data)
	}
	f.admins = adminNames(*data)
	c, err := openCtl(*data, 5*time.Second)
	if err != nil {
		if errors.Is(err, errNoAccess) {
			return errorLine(1, "cannot read %s (run it with sudo)", *data)
		}
		f.nodesErr = err
	} else {
		f.answers = c.running
		if c.running {
			f.running = c.version()
		}
		f.nodes, f.nodesErr = c.settledNodes(20 * time.Second)
		c.Close()
	}
	return printStatus(os.Stdout, f)
}

func readable(dir string) bool {
	d, err := os.Open(dir)
	if err != nil {
		return false
	}
	d.Close()
	return true
}

// localAddr says whether a node address is this machine's, for the command that fixes it.
func localAddr(addr string) bool {
	return anyLocalAddr([]store.Node{{Addr: addr}}, suggestedAddresses())
}

// adminNames are the enabled administrators in the database; nil when there are none or it cannot be read.
func adminNames(data string) []string {
	if !store.HasDatabase(data) {
		return nil
	}
	dbPath, err := store.DBPath(data, func(string, ...any) {})
	if err != nil {
		return nil
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return nil
	}
	defer st.Close()
	us, err := st.Users()
	if err != nil {
		return nil
	}
	var out []string
	for _, u := range us {
		if u.Role == store.RoleAdmin && !u.Disabled {
			out = append(out, u.Username)
		}
	}
	return out
}

// printStatus prints the facts and returns the exit code: 1 when something needs attention.
func printStatus(w io.Writer, f statusFacts) int {
	attention := false
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	line := func(k, v string) { fmt.Fprintf(tw, "%s\t%s\n", k, v) }
	fix := func(s string) { fmt.Fprintf(tw, "\t  %s\n", s); attention = true }

	ctlText := f.version
	switch {
	case f.answers && f.running != "" && f.running != f.version:
		ctlText = fmt.Sprintf("%s installed, %s running", f.version, f.running)
	}
	switch {
	case f.unit && f.active:
		ctlText += ", service running"
	case f.unit:
		ctlText += ", service stopped"
	case f.answers:
		ctlText += ", running (not as a service)"
	default:
		ctlText += ", not running"
	}
	line("Controller", ctlText)
	switch {
	case f.unit && !f.active:
		fix("Start it: sudo systemctl start virticle (why it stopped: journalctl -u virticle)")
	case !f.unit && !f.answers:
		fix("Start it: sudo virticle setup")
	case f.answers && f.running != "" && f.running != f.version:
		fix("Restart it to run the installed version: sudo systemctl restart virticle")
	case f.unit && f.active && !f.answers:
		fix("It does not answer the command line (still starting, or an older version). Restart it: sudo systemctl restart virticle")
	}

	web := f.web
	if web == "" {
		web = "unknown"
	}
	line("Web", web)

	switch {
	case len(f.admins) > 0:
		line("Administrator", strings.Join(f.admins, ", "))
	default:
		line("Administrator", "none")
		if f.web != "" {
			fix("Create it: sudo virticle setup, or open " + f.web)
		} else {
			fix("Create it: sudo virticle setup")
		}
	}

	switch {
	case f.nodesErr != nil:
		line("Nodes", "unknown")
		fix(f.nodesErr.Error())
	case len(f.nodes) == 0:
		line("Nodes", "none")
		fix("Add one: sudo virticle node add [user@]host (this machine: sudo virticle node add)")
	default:
		line("Nodes", fmt.Sprint(len(f.nodes)))
	}
	tw.Flush()

	if len(f.nodes) > 0 {
		isLocal := func(a string) bool { return f.isLocalNode != nil && f.isLocalNode(a) }
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, n := range f.nodes {
			state := n.State
			switch virticleState(n) {
			case vUpdateNeeded:
				state = "online, Virticle update needed"
			case vNotPrepared:
				state = "online, not prepared"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", nodeName(n), n.Addr, state)
		}
		tw.Flush()
		for _, n := range f.nodes {
			if why, cmd := nodeFix(n, isLocal(n.Addr)); why != "" {
				attention = true
				fmt.Fprintf(w, "  %s: %s\n", nodeName(n), why)
				if cmd != "" {
					fmt.Fprintf(w, "    Fix: %s\n", cmd)
				}
			}
		}
	}
	if attention {
		return 1
	}
	return 0
}

// nodeFix says what is wrong with a node and the command that fixes it; both empty when nothing is.
func nodeFix(n cluster.NodeStatus, local bool) (why, cmd string) {
	prep := prepareCommand(n, local)
	switch virticleState(n) {
	case vUpdateNeeded:
		return "Virticle update needed", prep
	case vNotPrepared:
		return "not prepared (" + strings.TrimSpace(n.PrivilegeNote) + ")", prep
	}
	switch n.State {
	case "online":
		return "", ""
	case "connecting":
		return "still connecting", "wait a moment and run status again"
	}
	e := n.Error
	if e == "" {
		e = "offline"
	}
	switch {
	case strings.Contains(e, "unable to authenticate") || strings.Contains(e, "no supported methods"):
		return "the controller's key is refused (" + e + ")", prep
	case strings.Contains(e, "key mismatch"):
		return "the host presents another SSH host key (" + e + ")", "if the host was reinstalled: Forget host key on the node's page in the web interface, then run status again"
	case strings.Contains(e, "libvirt") && strings.Contains(e, "older"):
		return e, "upgrade the host to libvirt 11.0 and QEMU 10.0 or newer"
	}
	return "cannot be reached (" + e + ")", "start the host or repair its network; the controller connects again by itself. If it is gone for good: sudo virticle node remove " + nodeName(n)
}
