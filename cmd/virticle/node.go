package main

// "virticle node prepare" and "virticle node unprepare": make a host ready to
// be managed, and take that away again. The administrator's credential (the
// SSH agent, or a key file, or a password on standard input) is used for this
// one run and never stored.

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"virticle/internal/cluster"
	"virticle/internal/prepare"
)

// nodeShort follows an error; the whole text (nodeUsage) is for --help.
const nodeShort = `  sudo virticle node prepare [--check]             this machine
  virticle node prepare [--check] [user@]host      another host, over SSH
  virticle node unprepare [user@]host
  virticle node prepare --help                     all options`

const nodeUsage = `usage:
  sudo virticle node prepare [--check] [--minimal] [--root] [--user NAME] [--from ADDR] [--take-over]
  virticle node prepare   [--check] [--minimal] [--root] [--user NAME] [--use-existing-user[=NAME]] [--confirm]
                          [--from ADDR] [--take-over] [--jump user@host]
                          [--identity FILE] [--password-stdin] [--data DIR] [user@]host[:port]
  virticle node unprepare [--user NAME] [--root] [--jump user@host] [--identity FILE]
                          [--password-stdin] [--data DIR] [user@]host[:port]

Without a host, prepare works on this machine and must run as root. With a host it signs in as the
administrator (your SSH agent, or --identity, or a password: asked for on a terminal, read from
standard input with --password-stdin). It installs libvirt and QEMU, installs the node command (virticle-node) and tests
the connection. There are two ways for the controller to reach the node:
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

func nodeCommand(args []string) int {
	if len(args) == 0 {
		return nodeFail("say what to do: prepare or unprepare")
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Println(nodeUsage)
		return 0
	}
	if args[0] != "prepare" && args[0] != "unprepare" {
		return nodeFail("unknown command " + args[0])
	}
	unprepare := args[0] == "unprepare"
	fs := flag.NewFlagSet("node "+args[0], flag.ContinueOnError)
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
		identity = fs.String("identity", "", "administrator's private key file (the SSH agent is used as well)")
		pwStdin  = fs.Bool("password-stdin", false, "read the administrator's password from standard input")
		takeOver = fs.Bool("take-over", false, "the node was managed by another Virticle controller: remove that controller's SSH key from the node")
		data     = fs.String("data", defaultDataDir(), "the controller's data directory (its key and known_hosts)")
	)
	fs.Var(&existing, "use-existing-user", "the account already exists: only add the key (and with --confirm groups and the sudo rule)")

	var pos []string
	rest := args[1:]
	for {
		if err := fs.Parse(rest); err != nil {
			if err == flag.ErrHelp {
				fmt.Println(nodeUsage)
				return 0
			}
			return nodeFail(err.Error())
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		rest = fs.Args()[1:]
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
	case local && (*jump != "" || *identity != "" || *pwStdin):
		return nodeFail("no host given; --jump, --identity and --password-stdin are for another host")
	case local && os.Geteuid() != 0:
		return nodeFail("no host given. To prepare this machine, run as root: sudo virticle node prepare")
	}
	if local {
		target = "127.0.0.1"
	}
	adminUser, addr := "root", target
	if u, a, ok := strings.Cut(target, "@"); ok {
		adminUser, addr = u, a
	}

	if err := os.MkdirAll(*data, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "Error: the controller's data directory %s: %v\n", *data, err)
		return 1
	}
	keyFile := filepath.Join(*data, "id_ed25519")
	pub, err := ensureKey(keyFile)
	if err != nil {
		hint := ""
		if os.IsPermission(err) && os.Geteuid() != 0 {
			hint = " (run it with sudo)"
		}
		fmt.Fprintf(os.Stderr, "Error: the controller's SSH key in %s: %v%s\n", *data, err, hint)
		return 1
	}
	if os.Geteuid() == 0 {
		chownLike(*data, keyFile, keyFile+".pub") // a key made now belongs to the controller's account
	}
	instance := ""
	if b, err := os.ReadFile(filepath.Join(*data, "instance-id")); err == nil {
		instance = strings.TrimSpace(string(b))
	}

	o := prepare.Options{User: *user, UseExisting: existing.on, Confirm: *confirm, AllowRoot: *root,
		From: *from, Minimal: *minimal, ControllerKey: pub, InstanceID: instance, TakeOver: *takeOver}
	if existing.name != "" {
		o.User = existing.name
	}
	if err := o.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	mgr := cluster.NewManager(keyFile, filepath.Join(*data, "known_hosts"))
	var r *prepare.Runner
	where := target
	if local {
		where = "this machine"
		if h, err := os.Hostname(); err == nil {
			where += " (" + h + ")"
		}
		r = &prepare.Runner{N: localNode{}}
	} else {
		a := prepare.Admin{Addr: addr, User: adminUser, Jump: *jump, Agent: os.Getenv("SSH_AUTH_SOCK") != ""}
		if *identity != "" {
			b, err := os.ReadFile(*identity)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			a.KeyPEM = b
		}
		tty := term.IsTerminal(int(os.Stdin.Fd()))
		switch {
		case tty && (*pwStdin || (len(a.KeyPEM) == 0 && !a.Agent)):
			// On a terminal the password is asked for and not shown.
			fmt.Fprintf(os.Stderr, "Password for %s@%s: ", adminUser, addr)
			b, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			a.Password = string(b)
		case *pwStdin:
			line, err := bufio.NewReader(os.Stdin).ReadString('\n')
			if err != nil && err != io.EOF {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			a.Password = strings.TrimRight(line, "\r\n")
		}
		if a.Password == "" && len(a.KeyPEM) == 0 && !a.Agent {
			fmt.Fprintf(os.Stderr, "Error: no way to sign in to %s as %s: no SSH agent, no --identity FILE, and no terminal to ask for the password (--password-stdin reads it from standard input)\n", addr, adminUser)
			return 2
		}
		conn, err := prepare.Dial(mgr, a)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: could not sign in to %s as %s: %v\n", addr, adminUser, err)
			return 1
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
		printed := 0
		rows = r.Prepare(o, func(rs []prepare.Row) {
			// Print rows as they settle; the final list is printed below for
			// anything not shown yet.
			for printed < len(rs) && rs[printed].Status != prepare.Skipped {
				printRow(rs[printed])
				printed++
			}
		}, func() []prepare.Row { return prepare.Verify(mgr, addr, r.User, *jump) })
		for ; printed < len(rows); printed++ {
			printRow(rows[printed])
		}
		if code := exitCode(rows); code != 0 || r.User == "" {
			return code
		}
		jumpArg := ""
		if *jump != "" {
			jumpArg = " with the jump host " + *jump
		}
		if local {
			fmt.Printf("\nNext: add the node in the web interface (Nodes, Add node) with this machine's address and the SSH user %s.\n", r.User)
			return 0
		}
		fmt.Printf("\nNext: add the node in the web interface (Nodes, Add node) with the address %s and the SSH user %s%s.\n", addr, r.User, jumpArg)
		return 0
	}
	for _, r := range rows {
		printRow(r)
	}
	return exitCode(rows)
}

func printRow(r prepare.Row) {
	word := map[string]string{prepare.OK: "Ok", prepare.Warning: "Warning", prepare.Error: "Error", prepare.Info: "Info", prepare.Skipped: "Not run"}[r.Status]
	if r.Detail != "" && r.Status != prepare.Skipped {
		fmt.Printf("%-8s %s: %s\n", word, r.Title, r.Detail)
	} else {
		fmt.Printf("%-8s %s\n", word, r.Title)
	}
}

func exitCode(rows []prepare.Row) int {
	if prepare.HasError(rows) {
		return 1
	}
	return 0
}
