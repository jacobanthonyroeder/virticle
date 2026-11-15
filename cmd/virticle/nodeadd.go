package main

// "virticle node add", "node list" and "node remove": what Nodes > Add node, the Nodes list and Remove node do in the web
// interface. The host is prepared here with the administrator's credential (the same steps as "node prepare" and the
// dialog: internal/prepare), and registered through the controller (ctlsock.go), by the same handler the dialog calls.

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"virticle/internal/api"
	"virticle/internal/cluster"
	"virticle/internal/prepare"
	"virticle/internal/store"
)

// addReq is one "node add".
type addReq struct {
	target string // [user@]host[:port] as given; "" for this machine
	data   string
	opts   prepare.Options // User, AllowRoot, TakeOver
	jump   string
	cred   credFlags
	// ask asks a yes/no question on a terminal; nil when there is none.
	ask func(question string) (answer string, ok bool)
	out io.Writer
}

func nodeAddCommand(args []string) int {
	fs := flag.NewFlagSet("node add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	var q addReq
	fs.BoolVar(&q.opts.AllowRoot, "root", false, "the controller signs in as root instead of a service account")
	fs.StringVar(&q.opts.User, "user", "", "the name of the service account (default virticle-node)")
	fs.StringVar(&q.jump, "jump", "", "user@host to connect through")
	fs.BoolVar(&q.opts.TakeOver, "take-over", false, "the host is managed by another Virticle controller: take it over")
	fs.StringVar(&q.data, "data", defaultDataDir(), "the controller's data directory")
	q.cred.register(fs)
	pos, err := parseMixed(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			fmt.Println(nodeUsage)
			return 0
		}
		return nodeFail(err.Error())
	}
	if msg := checkAddArgs(&q, pos, os.Geteuid()); msg != "" {
		return nodeFail(msg)
	}
	if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) {
		q.ask = stdinAsker(bufio.NewReader(os.Stdin))
	}
	q.out = os.Stdout
	return addNode(q, realCredIO())
}

// checkAddArgs settles the host from the arguments; a message means the command line is wrong.
func checkAddArgs(q *addReq, pos []string, euid int) string {
	switch {
	case len(pos) > 1:
		return "more than one host: " + strings.Join(pos, " ")
	case len(pos) == 1:
		q.target = strings.TrimSpace(pos[0])
		if _, addr := splitTarget(q.target); addr == "" || strings.HasPrefix(q.target, "@") {
			return "no host in " + strconv.Quote(pos[0])
		}
	}
	local := q.target == ""
	switch {
	case local && (q.jump != "" || q.cred.given()):
		return "no host given; --jump, --identity, --password-file and --password-stdin are for another host"
	case local && euid != 0:
		return "no host given. To add this machine, run as root: sudo virticle node add"
	case q.jump != "" && !strings.Contains(q.jump, "@"):
		return "--jump needs user@host"
	}
	if err := q.opts.Validate(); err != nil && !strings.HasPrefix(err.Error(), "controller key:") { // the key is read later
		return err.Error()
	}
	return ""
}

// sshAnswers says whether an SSH server listens on this machine; the controller reaches every node over SSH, this one too.
func sshAnswers() bool {
	c, err := net.DialTimeout("tcp", "127.0.0.1:22", 2*time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// addNode prepares the host when it needs it and registers it. It prints what it does and returns the exit code.
func addNode(q addReq, in credIO) int {
	out := q.out
	local := q.target == ""
	adminUser, addr := splitTarget(q.target)
	where := addr
	if local {
		if p := api.LocalNodeProblem(); p != "" {
			return errorLine(1, "this machine cannot run virtual machines: %s", lowerFirst(p))
		}
		if !sshAnswers() {
			return errorLine(1, "no SSH server answers on this machine (port 22), and the controller reaches every node over SSH, this one too. Install and start it (the package openssh-server), then run this again")
		}
		addr, where = api.LocalNodeAddress(), thisMachine()
	}
	keyFile, pub, err := controllerKey(q.data)
	if err != nil {
		return errorLine(1, "%v", err)
	}
	q.opts.ControllerKey, q.opts.InstanceID = pub, instanceIDOf(q.data)
	if err := q.opts.Validate(); err != nil {
		return errorLine(2, "%v", err)
	}

	c, err := openCtl(q.data, 15*time.Second)
	if err != nil {
		return errorLine(1, "%v%s", err, sudoHint(err))
	}
	defer c.Close()
	ns, err := c.nodes()
	if err != nil {
		return errorLine(1, "the controller's node list: %v", err)
	}
	if n, ok := alreadyListed(ns, addr, local); ok {
		return errorLine(1, "%s is already a node of this controller (%s, %s)", where, nodeName(n), n.State)
	}

	knownHosts := filepath.Join(q.data, "known_hosts")
	mgr := cluster.NewManager(keyFile, knownHosts)
	defer chownLike(q.data, knownHosts)

	// A host that this controller's key already gets into with a current node command needs no administrator: the
	// dialog's "Already prepared", decided here.
	user := ""
	if !local {
		accounts := []string{q.opts.ServiceUser()}
		if q.opts.User == "" && !q.opts.AllowRoot {
			accounts = append(accounts, prepare.LegacyUser) // the account of a node that 0.2.0 prepared
		}
	probe:
		for _, account := range accounts {
			res, _ := mgr.TestConnect(store.Node{Addr: addr, User: account, Jump: q.jump}, 20*time.Second)
			switch stage, detail := firstFailure(res); {
			case res.OK && res.Privilege != "" && res.Privilege != "none":
				fmt.Fprintf(out, "%s is prepared for this controller: it signs in as %s.\n", where, account)
				user = account
				break probe
			case stage == "address":
				return errorLine(1, "unknown host: %s", detail)
			case stage == "reach" || stage == "jump":
				return errorLine(1, "cannot reach %s: %s", addr, detail)
			}
		}
	}
	takeOver := q.opts.TakeOver
	if user == "" {
		var r *prepare.Runner
		if local {
			r = &prepare.Runner{N: localNode{}}
		} else {
			a, code, err := adminLogin(adminUser, addr, q.jump, q.cred, in)
			if err != nil {
				return errorLine(code, "%v", err)
			}
			conn, err := prepare.Dial(mgr, a)
			if err != nil {
				return errorLine(1, "could not sign in to %s as %s: %v", addr, adminUser, err)
			}
			defer conn.Close()
			r = &prepare.Runner{N: conn, SudoPassword: a.Password}
		}
		fmt.Fprintln(out, "Preparing", where, "(installing packages can take a few minutes)")
		o := q.opts
		rows := runPrepare(r, o, mgr, addr, q.jump)
		if otherController(rows) && !o.TakeOver {
			if q.ask == nil || !askYes(q.ask, "Take over the node from the other controller? [y/N] ", false) {
				again := strings.TrimSpace("sudo virticle node add --take-over " + addCommandArgs(q.target, q.jump, q.opts.ServiceUser()))
				return errorLine(1, "%s is managed by another Virticle controller. To take it over (that controller loses it): %s", where, again)
			}
			o.TakeOver, takeOver = true, true
			rows = runPrepare(r, o, mgr, addr, q.jump)
		}
		if prepare.HasError(rows) || r.User == "" {
			return errorLine(1, "%s was not added: %s", where, firstError(rows))
		}
		user = r.User
	}

	// The administrator's sign-in recorded the host key here as root; the controller must be able to read it now.
	chownLike(q.data, knownHosts)
	var n store.Node
	if err := c.call("POST", "/api/nodes", map[string]any{"addr": addr, "user": user, "jump": q.jump, "takeOver": takeOver}, &n); err != nil {
		return errorLine(1, "%s is prepared, but adding it failed: %v", where, err)
	}
	st := waitNode(c, n.ID, 30*time.Second)
	fmt.Fprintf(out, "Added %s (%s@%s): %s\n", nodeName(st), user, addr, st.State)
	if st.State != "online" {
		if st.Error != "" {
			fmt.Fprintf(out, "  %s\n", st.Error)
		}
		return 1
	}
	return 0
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// firstFailure is the step of a connection test that failed first, and what it said.
func firstFailure(res cluster.TestResult) (string, string) {
	for _, s := range res.Steps {
		if s.Status == cluster.StepError {
			return s.ID, s.Detail
		}
	}
	return "", ""
}

func otherController(rows []prepare.Row) bool {
	for _, r := range rows {
		if r.ID == "controller" && r.Status == prepare.Error {
			return true
		}
	}
	return false
}

// firstError is the detail of the first error row, in one line.
func firstError(rows []prepare.Row) string {
	for _, r := range rows {
		if r.Status == prepare.Error {
			d := r.Title
			if r.Detail != "" {
				d += ": " + r.Detail
			}
			return strings.Join(strings.Fields(d), " ")
		}
	}
	return "the preparation did not finish"
}

// askYes asks a yes/no question until it gets an answer; Enter and the end of input take def.
func askYes(ask func(string) (string, bool), question string, def bool) bool {
	for i := 0; i < 3; i++ {
		ans, ok := ask(question)
		switch strings.ToLower(strings.TrimSpace(ans)) {
		case "":
			if !ok {
				return false
			}
			return def
		case "y", "yes":
			return true
		case "n", "no":
			return false
		}
		question = "Please answer y or n. " + question
	}
	return false
}

// waitNode waits until the node has connected or failed to, at most d, and returns its status.
func waitNode(c *ctl, id int64, d time.Duration) cluster.NodeStatus {
	deadline := time.Now().Add(d)
	var last cluster.NodeStatus
	for {
		ns, err := c.nodes()
		if err == nil {
			for _, n := range ns {
				if n.ID == id {
					last = n
				}
			}
		}
		if (last.State != "" && last.State != "connecting") || time.Now().After(deadline) {
			if last.State == "" {
				last.State = "connecting"
			}
			return last
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// alreadyListed finds a node at addr (the host part; user and port do not matter), or for this machine any node that
// points here.
func alreadyListed(ns []cluster.NodeStatus, addr string, local bool) (cluster.NodeStatus, bool) {
	want := hostOf(addr)
	for _, n := range ns {
		if strings.EqualFold(hostOf(n.Addr), want) || (local && api.IsThisMachine(n.Addr)) {
			return n, true
		}
	}
	return cluster.NodeStatus{}, false
}

func hostOf(a string) string {
	if h, _, err := net.SplitHostPort(a); err == nil {
		return h
	}
	return strings.Trim(a, "[]")
}

// nodeName is what the web interface calls a node: its host name, else its address.
func nodeName(n cluster.NodeStatus) string {
	if n.Hostname != "" {
		return n.Hostname
	}
	return n.Addr
}

// Values of nodeLine.Virticle: whether the node runs this controller's version of the Virticle node command. The rule
// of the Nodes page (Virticle update needed, Not prepared).
const (
	vCurrent      = "current"
	vUpdateNeeded = "update needed"
	vNotPrepared  = "not prepared"
	vUnknown      = "unknown" // the node cannot be asked
)

func virticleState(n cluster.NodeStatus) string {
	if n.State != "online" {
		return vUnknown
	}
	if n.DetectedPrivilege != "none" {
		return vCurrent
	}
	if strings.Contains(n.PrivilegeNote, "older version") || strings.Contains(n.PrivilegeNote, "installed node command is version") {
		return vUpdateNeeded
	}
	return vNotPrepared
}

// prepareCommand is the command that installs this version's node command on a node (local: the node is this machine).
func prepareCommand(n cluster.NodeStatus, local bool) string {
	var b strings.Builder
	b.WriteString("sudo virticle node prepare")
	switch n.User {
	case "root":
		b.WriteString(" --root")
	case prepare.DefaultUser, "virticle", "":
	default:
		b.WriteString(" --user " + n.User)
	}
	if local {
		return b.String()
	}
	if n.Jump != "" {
		b.WriteString(" --jump " + n.Jump)
	}
	b.WriteString(" root@" + n.Addr)
	return b.String()
}

// stateText is the state as the commands print it: the connection state, or what the node needs and the command for it.
func stateText(n cluster.NodeStatus, local bool) string {
	switch virticleState(n) {
	case vUpdateNeeded:
		return "Virticle update needed: " + prepareCommand(n, local)
	case vNotPrepared:
		return "not prepared: " + prepareCommand(n, local)
	}
	return n.State
}

// nodeLine is a node as "node list --json" gives it.
type nodeLine struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Addr     string `json:"addr"`
	User     string `json:"user"`
	Jump     string `json:"jump,omitempty"`
	State    string `json:"state"`
	Virticle string `json:"virticle"`      // current, update needed, not prepared, unknown
	Fix      string `json:"fix,omitempty"` // the command that updates or prepares it
	Error    string `json:"error,omitempty"`
}

func lineOf(n cluster.NodeStatus, local bool) nodeLine {
	l := nodeLine{ID: n.ID, Name: nodeName(n), Addr: n.Addr, User: n.User, Jump: n.Jump, State: n.State, Virticle: virticleState(n), Error: n.Error}
	if l.Virticle == vUpdateNeeded || l.Virticle == vNotPrepared {
		l.Fix = prepareCommand(n, local)
	}
	return l
}

func printNodeTable(w io.Writer, ns []cluster.NodeStatus, isLocal func(string) bool) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tADDRESS\tSTATE")
	for _, n := range ns {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", nodeName(n), n.Addr, stateText(n, isLocal(n.Addr)))
	}
	tw.Flush()
}

func nodeListCommand(args []string) int {
	fs := flag.NewFlagSet("node list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	asJSON := fs.Bool("json", false, "print JSON")
	data := fs.String("data", defaultDataDir(), "the controller's data directory")
	pos, err := parseMixed(fs, args)
	if err == flag.ErrHelp {
		fmt.Println(nodeUsage)
		return 0
	}
	if err != nil {
		return nodeFail(err.Error())
	}
	if len(pos) > 0 {
		return nodeFail("node list takes no arguments")
	}
	c, err := openCtl(*data, 15*time.Second)
	if err != nil {
		return errorLine(1, "%v%s", err, sudoHint(err))
	}
	defer c.Close()
	ns, err := c.settledNodes(20 * time.Second)
	if err != nil {
		return errorLine(1, "%v", err)
	}
	if *asJSON {
		lines := []nodeLine{}
		for _, n := range ns {
			lines = append(lines, lineOf(n, localAddr(n.Addr)))
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(lines)
		return 0
	}
	if len(ns) == 0 {
		fmt.Println("No nodes. Add one: sudo virticle node add [user@]host")
		return 0
	}
	printNodeTable(os.Stdout, ns, localAddr)
	return 0
}

// matchNodes finds the nodes NAME means: an id, a host name, an address or its host part, or user@address.
func matchNodes(ns []cluster.NodeStatus, name string) []cluster.NodeStatus {
	name = strings.TrimSpace(name)
	var out []cluster.NodeStatus
	for _, n := range ns {
		switch {
		case strconv.FormatInt(n.ID, 10) == name,
			n.Hostname != "" && strings.EqualFold(n.Hostname, name),
			strings.EqualFold(n.Addr, name),
			strings.EqualFold(hostOf(n.Addr), hostOf(name)),
			strings.EqualFold(n.User+"@"+n.Addr, name):
			out = append(out, n)
		}
	}
	return out
}

func nodeNames(ns []cluster.NodeStatus) string {
	var names []string
	for _, n := range ns {
		names = append(names, nodeName(n))
	}
	return strings.Join(names, ", ")
}

func nodeRemoveCommand(args []string) int {
	fs := flag.NewFlagSet("node remove", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	yes := fs.Bool("yes", false, "ask nothing")
	data := fs.String("data", defaultDataDir(), "the controller's data directory")
	pos, err := parseMixed(fs, args)
	if err == flag.ErrHelp {
		fmt.Println(nodeUsage)
		return 0
	}
	if err != nil {
		return nodeFail(err.Error())
	}
	if len(pos) != 1 {
		return nodeFail("name one node: its host name or address")
	}
	c, err := openCtl(*data, 15*time.Second)
	if err != nil {
		return errorLine(1, "%v%s", err, sudoHint(err))
	}
	defer c.Close()
	// Settled first: removing a node also takes its key off the other nodes, which needs their connections.
	ns, err := c.settledNodes(20 * time.Second)
	if err != nil {
		return errorLine(1, "%v", err)
	}
	m := matchNodes(ns, pos[0])
	switch {
	case len(ns) == 0:
		return errorLine(1, "there is no node %s: this controller has no nodes", pos[0])
	case len(m) == 0:
		return errorLine(1, "there is no node %s. The nodes are: %s", pos[0], nodeNames(ns))
	case len(m) > 1:
		return errorLine(1, "%s means more than one node (%s); give the address", pos[0], nodeNames(m))
	}
	n := m[0]
	name := nodeName(n)
	if !*yes && term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) {
		fmt.Printf("%s (%s) is removed from this controller. Its virtual machines keep running; they are no longer shown here.\n", name, n.Addr)
		if !askYes(stdinAsker(bufio.NewReader(os.Stdin)), "Remove node "+name+"? [y/N] ", false) {
			fmt.Println("Nothing was changed.")
			return 1
		}
	}
	if err := c.call("DELETE", "/api/nodes/"+strconv.FormatInt(n.ID, 10), nil, nil); err != nil {
		return errorLine(1, "removing %s: %v", name, err)
	}
	fmt.Printf("Removed %s. Nothing was uninstalled from it. To take the controller's access away as well: virticle node unprepare %s\n", name, unprepareTarget(n))
	return 0
}

// unprepareTarget is the host argument (and the jump option) for "node unprepare" of a node.
func unprepareTarget(n cluster.NodeStatus) string {
	t := "root@" + n.Addr
	if n.Jump != "" {
		t = "--jump " + n.Jump + " " + t
	}
	return t
}
