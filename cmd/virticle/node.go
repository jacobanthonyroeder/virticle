package main

// "virticle node ...": add, list and remove (nodeadd.go), and prepare and
// unprepare: make a host ready to be managed, and take that away again. The
// administrator's credential (the SSH agent, a key file, or a password from a
// terminal, a file, the environment or standard input) is used for one run and
// never stored.

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
	"virticle/internal/prepare"
	"virticle/internal/store"
)

// nodeShort follows an error; the whole text (nodeUsage) is for --help.
const nodeShort = `  sudo virticle node add [user@]host         prepare a host if needed and add it as a node
  sudo virticle node add                     this machine
  sudo virticle node list [--json]
  sudo virticle node remove NAME
  virticle node --help                       all commands and options`

const nodeUsage = `usage:
  sudo virticle node add [user@]host[:port]   prepare a host if needed and add it as a node
  sudo virticle node add                      the same for this machine
  sudo virticle node list [--json]            the nodes: name, address, state
  sudo virticle node remove NAME              stop managing a node (NAME: its host name or address)

node add signs in to the host as the administrator (root, or a user that may use sudo) and prepares it, the
way Nodes > Add node does in the web interface. The credential is used for this run only: your SSH agent,
--identity FILE, or a password, asked for on a terminal; scripts give --password-file FILE or the
environment variable VIRTICLE_NODE_PASSWORD. A host that this controller's key already gets into needs no
credential. The running controller shows the node at once; it needs no restart. When the controller is
stopped, the commands work on its data directory and it finds the node at its next start.
These commands need the controller's data directory: run them as root (sudo) or as the controller's account.

Options of node add:
  --root                the controller signs in as root (no service account; a service account is safer)
  --user NAME           the name of the service account (default virticle-node)
  --jump user@host      reach the host through a jump host
  --jump-identity FILE  a private key for the jump host, if it does not accept the controller's key
  --jump-password-file FILE  a password for the jump host (first line of FILE; VIRTICLE_JUMP_PASSWORD works too, a terminal asks).
                        The controller's key is then added to that account's authorized_keys, for forwarding only
  --take-over           the host is managed by another Virticle controller: take it over (asked on a terminal)
  --identity FILE       the administrator's private key file
  --password-file FILE  the administrator's password: the first line of FILE
  --password-stdin      the administrator's password: one line on standard input
  --data DIR            the controller's data directory (default /var/lib/virticle on an installed machine)
Options of node remove: --yes (asks nothing on a terminal), --data DIR. Of node list: --json, --data DIR.

Prepare only (node add runs it first; it does not add the node):
  sudo virticle node prepare [--check] [--minimal] [--root] [--user NAME] [--from ADDR] [--take-over]
  virticle node prepare   [--check] [--minimal] [--root] [--user NAME] [--use-existing-user[=NAME]] [--confirm]
                          [--from ADDR] [--take-over] [--jump user@host] [--identity FILE]
                          [--password-file FILE] [--password-stdin] [--jump-identity FILE] [--jump-password-file FILE]
                          [--data DIR] [user@]host[:port]
  virticle node unprepare [--user NAME] [--root] [--jump user@host] [--identity FILE]
                          [--password-file FILE] [--password-stdin] [--data DIR] [user@]host[:port]

Without a host, prepare works on this machine and must run as root. It installs libvirt and QEMU,
installs the node command (virticle-node) and tests the connection. The controller reaches the node in
one of two ways:
  Service account (default, recommended): prepare creates the account virticle-node for the
    controller with a restricted SSH key and a sudo rule that lets it run the node command as root,
    and nothing else. (The account virticle belongs to the controller package and is never used.
    A node prepared by an earlier version keeps its account virticle.)
  Root (--root): the controller signs in as root and runs the node command directly. No account
    is created.
Packages: libvirt, QEMU and the guest disk tools (virt-sysprep, used by "sysprep on clone").
The node needs libvirt 11.0 and QEMU 10.0 or newer from its distribution. prepare refuses an
older host (installed, or what the repositories would install) and changes nothing on it.
--minimal leaves the guest disk tools out; clone with sysprep then needs a sealed template.
--use-existing-user (with --confirm) is for an account you manage yourself.
--check changes nothing. Running prepare again changes nothing. unprepare removes the account
(virticle-node, or virticle when an earlier version made it), key, node command and sudo rule;
packages, VMs, pools and networks stay. An account that Virticle did not create is never removed.`

// useExistingFlag is a boolean flag that also accepts --use-existing-user=NAME.
type useExistingFlag struct {
	on   bool
	name string
}

func (f *useExistingFlag) IsBoolFlag() bool { return true }
func (f *useExistingFlag) String() string   { return fmt.Sprint(f.on) }
func (f *useExistingFlag) Set(s string) error {
	switch s {
	case "true":
		f.on = true
	case "false":
		f.on = false
	default:
		f.on, f.name = true, s
	}
	return nil
}

// localNode runs the steps on this machine, for "virticle node prepare" without a host.
type localNode struct{}

func (localNode) Run(cmd string) (string, error) { return localNode{}.RunInput(cmd, "") }
func (localNode) RunInput(cmd, stdin string) (string, error) {
	c := exec.Command("sh", "-c", cmd)
	if stdin != "" {
		c.Stdin = strings.NewReader(stdin)
	}
	out, err := c.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// nodeFail prints what is wrong and the short usage.
func nodeFail(msg string) int {
	fmt.Fprintf(os.Stderr, "Error: %s\n\n%s\n", msg, nodeShort)
	return 2
}

// errorLine prints a one-line error (no usage: the command was right, the situation is not) and returns code.
func errorLine(code int, format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "Error: "+format+"\n", a...)
	return code
}

func nodeCommand(args []string) int {
	if len(args) == 0 {
		return nodeFail("say what to do: add, list, remove, prepare or unprepare")
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Println(nodeUsage)
		return 0
	case "add":
		return nodeAddCommand(args[1:])
	case "list":
		return nodeListCommand(args[1:])
	case "remove":
		return nodeRemoveCommand(args[1:])
	case "prepare", "unprepare":
		return nodePrepareCommand(args[0], args[1:])
	}
	return nodeFail("unknown command " + args[0])
}

// credFlags are how the administrator signs in to another host.
type credFlags struct {
	identity, passwordFile string
	pwStdin                bool
	// The login on the jump host, for a jump host that does not accept the controller's key yet.
	jumpIdentity, jumpPasswordFile string
}

func (c *credFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.identity, "identity", "", "administrator's private key file (the SSH agent is used as well)")
	fs.StringVar(&c.passwordFile, "password-file", "", "read the administrator's password from the first line of this file")
	fs.BoolVar(&c.pwStdin, "password-stdin", false, "read the administrator's password from standard input")
	fs.StringVar(&c.jumpIdentity, "jump-identity", "", "private key file for the jump host, when it does not accept the controller's key")
	fs.StringVar(&c.jumpPasswordFile, "jump-password-file", "", "password for the jump host (first line of this file), when it does not accept the controller's key")
}

func (c credFlags) given() bool {
	return c.identity != "" || c.passwordFile != "" || c.pwStdin || c.jumpIdentity != "" || c.jumpPasswordFile != ""
}

// jumpPasswordEnv holds the password of the jump host: for the running controller (every connection) and for the
// commands (a login on a jump host that does not accept the controller's key yet).
const jumpPasswordEnv = "VIRTICLE_JUMP_PASSWORD"

// nodePasswordEnv holds the administrator's password for scripts.
const nodePasswordEnv = "VIRTICLE_NODE_PASSWORD"

// credIO is what adminLogin needs from the terminal; tests replace it.
type credIO struct {
	tty          bool
	stdin        io.Reader
	readPassword func(prompt string) (string, error)
	getenv       func(string) string
	agent        bool
}

func realCredIO() credIO {
	fd := int(os.Stdin.Fd())
	return credIO{
		tty: term.IsTerminal(fd), stdin: os.Stdin, getenv: os.Getenv, agent: os.Getenv("SSH_AUTH_SOCK") != "",
		readPassword: func(prompt string) (string, error) {
			fmt.Fprint(os.Stderr, prompt)
			b, err := term.ReadPassword(fd)
			fmt.Fprintln(os.Stderr)
			return string(b), err
		},
	}
}

// firstLineOf is the first line of a password file, without the line end.
func firstLineOf(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(b), "\n")
	line = strings.TrimRight(line, "\r")
	if line == "" {
		return "", fmt.Errorf("the first line of %s is empty", path)
	}
	return line, nil
}

// adminLogin builds the administrator's credential for adminUser at addr: a key file, the SSH agent, and a password
// from --password-file, the environment, --password-stdin or (on a terminal, when there is no key) a question. The
// code is the exit code for the error.
func adminLogin(adminUser, addr, jump string, c credFlags, in credIO) (prepare.Admin, int, error) {
	a := prepare.Admin{Addr: addr, User: adminUser, Jump: jump, Agent: in.agent}
	if c.identity != "" {
		b, err := os.ReadFile(c.identity)
		if err != nil {
			return a, 1, fmt.Errorf("--identity: %v", err)
		}
		a.KeyPEM = b
	}
	switch {
	case c.passwordFile != "":
		pw, err := firstLineOf(c.passwordFile)
		if err != nil {
			return a, 1, fmt.Errorf("--password-file: %v", err)
		}
		a.Password = pw
	case in.getenv(nodePasswordEnv) != "":
		a.Password = in.getenv(nodePasswordEnv)
	case in.tty && (c.pwStdin || (len(a.KeyPEM) == 0 && !a.Agent)):
		// On a terminal the password is asked for and not shown.
		pw, err := in.readPassword(fmt.Sprintf("Password for %s@%s: ", adminUser, addr))
		if err != nil {
			return a, 1, err
		}
		a.Password = pw
	case c.pwStdin:
		line, err := bufio.NewReader(in.stdin).ReadString('\n')
		if err != nil && err != io.EOF {
			return a, 1, err
		}
		a.Password = strings.TrimRight(line, "\r\n")
	}
	if jump != "" {
		if c.jumpIdentity != "" {
			b, err := os.ReadFile(c.jumpIdentity)
			if err != nil {
				return a, 1, fmt.Errorf("--jump-identity: %v", err)
			}
			a.JumpKeyPEM = b
		}
		switch {
		case c.jumpPasswordFile != "":
			pw, err := firstLineOf(c.jumpPasswordFile)
			if err != nil {
				return a, 1, fmt.Errorf("--jump-password-file: %v", err)
			}
			a.JumpPassword = pw
		default:
			a.JumpPassword = in.getenv(jumpPasswordEnv)
		}
		a.JumpAgent = in.agent
	}
	if a.Password == "" && len(a.KeyPEM) == 0 && !a.Agent {
		return a, 2, fmt.Errorf("no way to sign in to %s as %s: no SSH agent, no --identity FILE, no password (--password-file FILE or %s), and no terminal to ask for it", addr, adminUser, nodePasswordEnv)
	}
	return a, 0, nil
}

// dialAdmin signs in as the administrator, through the jump host when there is one. The jump host is passed with the
// controller's key; when it does not accept that key, the login for the jump host is used (the flags, the environment,
// the SSH agent, or on a terminal a password that is asked for) and the controller's key is added there, so that the
// controller passes it from then on (controllerKey, empty: not added, for a run that changes nothing). The error says
// which hop failed.
func dialAdmin(mgr *cluster.Manager, a prepare.Admin, controllerKey string, in credIO) (*hostconn.Conn, error) {
	a.ControllerKey = controllerKey
	conn, err := prepare.Dial(mgr, a)
	if err != nil && hostconn.JumpAuthRefused(err) && in.tty && a.JumpPassword == "" && len(a.JumpKeyPEM) == 0 {
		u, h, _ := strings.Cut(a.Jump, "@")
		if pw, perr := in.readPassword(fmt.Sprintf("The jump host does not accept the controller's key. Password for %s@%s: ", u, h)); perr == nil && pw != "" {
			a.JumpPassword = pw
			conn, err = prepare.Dial(mgr, a)
		}
	}
	if err != nil {
		return nil, signInError(err, a)
	}
	if conn.JumpKeyAdded {
		fmt.Fprintf(os.Stderr, "Added the controller's key to %s (~/.ssh/authorized_keys, for forwarding only)\n", a.Jump)
	}
	return conn, nil
}

// signInError says which hop failed and, for a jump host that does not accept the controller's key, what to give.
func signInError(err error, a prepare.Admin) error {
	if hostconn.JumpAuthRefused(err) && a.JumpPassword == "" && len(a.JumpKeyPEM) == 0 {
		return fmt.Errorf("%v. Give a login for it: --jump-identity FILE, --jump-password-file FILE or the environment variable %s", err, jumpPasswordEnv)
	}
	var h *hostconn.HopError
	if errors.As(err, &h) {
		return err
	}
	return fmt.Errorf("could not sign in to %s as %s: %v", a.Addr, a.User, err)
}

// controllerKey makes sure the controller's key pair exists in data and returns the key file and the public key.
func controllerKey(data string) (string, string, error) {
	if err := os.MkdirAll(data, 0o700); err != nil {
		return "", "", fmt.Errorf("the controller's data directory %s: %v%s", data, err, sudoHint(err))
	}
	keyFile := filepath.Join(data, "id_ed25519")
	pub, err := ensureKey(keyFile)
	if err != nil {
		return "", "", fmt.Errorf("the controller's SSH key in %s: %v%s", data, err, sudoHint(err))
	}
	if os.Geteuid() == 0 {
		chownLike(data, keyFile, keyFile+".pub") // a key made now belongs to the controller's account
	}
	return keyFile, pub, nil
}

func sudoHint(err error) string {
	if (os.IsPermission(err) || errors.Is(err, errNoAccess)) && os.Geteuid() != 0 {
		return " (run it with sudo)"
	}
	return ""
}

func instanceIDOf(data string) string {
	if b, err := os.ReadFile(filepath.Join(data, "instance-id")); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}

// thisMachine names this machine in the messages.
func thisMachine() string {
	where := "this machine"
	if h, err := os.Hostname(); err == nil {
		where += " (" + h + ")"
	}
	return where
}

// runPrepare prepares a host the way the web dialog does, printing the rows as they settle, and returns them all.
// verifyAddr is the address the connection test (and the node record) uses.
func runPrepare(r *prepare.Runner, o prepare.Options, mgr *cluster.Manager, verifyAddr, jump string) []prepare.Row {
	printed := 0
	rows := r.Prepare(o, func(rs []prepare.Row) {
		// Print rows as they settle; the final list is printed below for anything not shown yet.
		for printed < len(rs) && rs[printed].Status != prepare.Skipped {
			printRow(rs[printed])
			printed++
		}
	}, func() []prepare.Row { return prepare.Verify(mgr, verifyAddr, r.User, jump) })
	for ; printed < len(rows); printed++ {
		printRow(rows[printed])
	}
	return rows
}

// splitTarget splits [user@]host[:port] into the administrator and the address.
func splitTarget(target string) (adminUser, addr string) {
	if u, a, ok := strings.Cut(target, "@"); ok {
		return u, a
	}
	return "root", target
}

func nodePrepareCommand(verb string, args []string) int {
	unprepare := verb == "unprepare"
	fs := flag.NewFlagSet("node "+verb, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	var (
		check    = fs.Bool("check", false, "read-only preflight; change nothing")
		user     = fs.String("user", "", "service account name (default virticle-node)")
		existing useExistingFlag
		confirm  = fs.Bool("confirm", false, "with --use-existing-user: add the account to the libvirt and kvm groups and give it the sudo rule for the node command")
		root     = fs.Bool("root", false, "Root access: the controller signs in as root instead of using a service account (a service account is safer)")
		from     = fs.String("from", "", "restrict the controller's key to this source address (comma-separated patterns)")
		minimal  = fs.Bool("minimal", false, "leave out the guest disk tools (virt-sysprep); clone with sysprep then needs a sealed template")
		jump     = fs.String("jump", "", "user@host to connect through")
		takeOver = fs.Bool("take-over", false, "the node was managed by another Virticle controller: remove that controller's SSH key from the node")
		data     = fs.String("data", defaultDataDir(), "the controller's data directory (its key and known_hosts)")
		cred     credFlags
	)
	cred.register(fs)
	fs.Var(&existing, "use-existing-user", "the account already exists: only add the key (and with --confirm groups and the sudo rule)")

	pos, err := parseMixed(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			fmt.Println(nodeUsage)
			return 0
		}
		return nodeFail(err.Error())
	}
	target := ""
	for _, p := range pos {
		if existing.on && existing.name == "" && !strings.Contains(p, "@") && len(pos) == 2 && target == "" && p == pos[0] {
			existing.name = p // --use-existing-user NAME
			continue
		}
		if target != "" {
			return nodeFail("more than one host: " + target + " and " + p)
		}
		target = p
	}
	local := target == ""
	switch {
	case local && unprepare:
		return nodeFail("unprepare needs a host")
	case local && (*jump != "" || cred.given()):
		return nodeFail("no host given; --jump, --identity, --password-file and --password-stdin are for another host")
	case local && os.Geteuid() != 0:
		return nodeFail("no host given. To prepare this machine, run as root: sudo virticle node prepare")
	}
	if local {
		target = "127.0.0.1"
	}
	adminUser, addr := splitTarget(target)

	keyFile, pub, err := controllerKey(*data)
	if err != nil {
		return errorLine(1, "%v", err)
	}
	o := prepare.Options{User: *user, UseExisting: existing.on, Confirm: *confirm, AllowRoot: *root,
		From: *from, Minimal: *minimal, ControllerKey: pub, InstanceID: instanceIDOf(*data), TakeOver: *takeOver}
	if existing.name != "" {
		o.User = existing.name
	}
	if err := o.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	mgr := cluster.NewManager(keyFile, filepath.Join(*data, "known_hosts"))
	defer chownLike(*data, filepath.Join(*data, "known_hosts"))
	var r *prepare.Runner
	where := target
	if local {
		where = thisMachine()
		r = &prepare.Runner{N: localNode{}}
	} else {
		a, code, err := adminLogin(adminUser, addr, *jump, cred, realCredIO())
		if err != nil {
			return errorLine(code, "%v", err)
		}
		key := pub
		if *check || unprepare {
			key = "" // changes nothing on the jump host
		}
		conn, err := dialAdmin(mgr, a, key, realCredIO())
		if err != nil {
			return errorLine(1, "%v", err)
		}
		defer conn.Close()
		r = &prepare.Runner{N: conn, SudoPassword: a.Password}
	}
	switch {
	case unprepare:
		fmt.Println("Removing controller access from", where)
	case *check:
		fmt.Println("Checking", where)
	default:
		fmt.Println("Preparing", where, "(installing packages can take a few minutes)")
	}

	var rows []prepare.Row
	switch {
	case unprepare:
		rows = r.Unprepare(o)
	case *check:
		rows, _, _ = r.Check(o)
	default:
		rows = runPrepare(r, o, mgr, addr, *jump)
		if code := exitCode(rows); code != 0 || r.User == "" {
			return code
		}
		if local {
			if hasLocalNode(*data, suggestedAddresses()) {
				fmt.Println("\nThe controller already lists this machine.")
				return 0
			}
			fmt.Println("\nNext: sudo virticle node add")
			return 0
		}
		if nodeListed(*data, addr) {
			fmt.Println("\nThe controller already lists this node.")
			return 0
		}
		fmt.Printf("\nNext: sudo virticle node add %s\n", addCommandArgs(target, *jump, r.User))
		return 0
	}
	for _, r := range rows {
		printRow(r)
	}
	return exitCode(rows)
}

// addCommandArgs are the arguments of the "node add" that follows a prepare: the host as given, and the options that
// make it use the same account.
func addCommandArgs(target, jump, account string) string {
	var b strings.Builder
	switch account {
	case "root":
		b.WriteString("--root ")
	case prepare.DefaultUser, "virticle", "":
	default:
		b.WriteString("--user " + account + " ")
	}
	if jump != "" {
		b.WriteString("--jump " + jump + " ")
	}
	b.WriteString(target)
	return b.String()
}

func printRow(r prepare.Row) {
	word := map[string]string{prepare.OK: "Ok", prepare.Warning: "Warning", prepare.Error: "Error", prepare.Info: "Info", prepare.Skipped: "Not run"}[r.Status]
	if r.Detail != "" && r.Status != prepare.Skipped {
		fmt.Printf("%-8s %s: %s\n", word, r.Title, r.Detail)
	} else {
		fmt.Printf("%-8s %s\n", word, r.Title)
	}
	if r.Help != "" && r.Status != prepare.Skipped {
		fmt.Printf("         %s\n", r.Help)
	}
}

func exitCode(rows []prepare.Row) int {
	if prepare.HasError(rows) {
		return 1
	}
	return 0
}

// nodeListed reports whether the controller's data directory already has a node at addr (the host part; a port or a
// user does not matter). Any trouble reading the database counts as "not listed": the hint is then merely repeated.
func nodeListed(data, addr string) bool {
	if !store.HasDatabase(data) {
		return false
	}
	dbPath, err := store.DBPath(data, func(string, ...any) {})
	if err != nil {
		return false
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return false
	}
	defer st.Close()
	ns, err := st.Nodes()
	if err != nil {
		return false
	}
	return listedIn(ns, addr)
}

func listedIn(ns []store.Node, addr string) bool {
	host := func(a string) string {
		if h, _, err := net.SplitHostPort(a); err == nil {
			return h
		}
		return strings.Trim(a, "[]")
	}
	want := host(addr)
	for _, n := range ns {
		if strings.EqualFold(host(n.Addr), want) {
			return true
		}
	}
	return false
}
