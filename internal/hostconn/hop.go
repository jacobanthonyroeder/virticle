package hostconn

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// HopError is a failed SSH connection, with the hop that failed: the jump host or the node. The text always says which.
type HopError struct {
	Jump  bool   // the jump host; else the node
	Addr  string // the host
	User  string
	Login bool // a jump host: a login (not the controller's key) was tried as well
	Err   error
}

func (e *HopError) Unwrap() error { return e.Err }

// AuthRefused says the host was reached but accepted none of the credentials.
func (e *HopError) AuthRefused() bool { return authRefused(e.Err) }

// Where names the host: user@address for a jump host, the address for the node.
func (e *HopError) Where() string {
	if e.Jump {
		return e.User + "@" + e.Addr
	}
	return e.Addr
}

// Reason is why the connection failed, in words.
func (e *HopError) Reason() string {
	switch {
	case e.Jump && e.AuthRefused() && e.Login:
		return "sign-in refused (wrong password or key)"
	case e.Jump && e.AuthRefused():
		return "the controller's key is not accepted"
	case e.AuthRefused():
		return "sign-in as " + e.User + " refused (wrong password or key)"
	}
	return e.Err.Error()
}

func (e *HopError) Error() string {
	if e.Jump {
		return "jump host " + e.Where() + ": " + e.Reason()
	}
	return "ssh " + e.Where() + ": " + e.Reason()
}

// JumpAuthRefused reports whether err is a jump host that did not accept the controller's key (and no login was
// given, or it did not work either): the case in which the administrator is asked for a login on the jump host.
func JumpAuthRefused(err error) bool {
	var h *HopError
	return errors.As(err, &h) && h.Jump && h.AuthRefused()
}

func authRefused(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "unable to authenticate") || strings.Contains(m, "no supported methods remain")
}

// dialJump opens the SSH connection to the jump host. It first signs in as the controller does (its key). When that
// is refused and t.JumpLogin is set, it signs in with that login instead and, when t.JumpInstallKey is set, adds the
// controller's key to the login account's authorized_keys (JumpKeyLine), so that the controller passes the jump host
// from then on (keyAdded: it was done now).
func dialJump(t Target, knownHosts string) (c *ssh.Client, keyAdded bool, err error) {
	j, err := dialSSH(*t.Jump, knownHosts, nil)
	if err == nil {
		return j, false, nil
	}
	h := &HopError{Jump: true, Addr: t.Jump.Addr, User: t.Jump.User, Err: err}
	if t.JumpLogin == nil || !h.AuthRefused() {
		return nil, false, h
	}
	login := *t.Jump
	login.KeyFile, login.Password, login.KeyPEM, login.Agent = "", t.JumpLogin.Password, t.JumpLogin.KeyPEM, t.JumpLogin.Agent
	j, err = dialSSH(login, knownHosts, nil)
	if err != nil {
		return nil, false, &HopError{Jump: true, Addr: t.Jump.Addr, User: t.Jump.User, Login: true, Err: err}
	}
	if t.JumpInstallKey != "" {
		if err := installJumpKey(j, t.JumpInstallKey); err != nil {
			j.Close()
			line, _ := JumpKeyLine(t.JumpInstallKey)
			return nil, false, &HopError{Jump: true, Addr: t.Jump.Addr, User: t.Jump.User, Login: true,
				Err: fmt.Errorf("signed in, but could not add the controller's key to ~/.ssh/authorized_keys (%v). Add this line there: %s", err, line)}
		}
		keyAdded = true
	}
	return j, keyAdded, nil
}

// JumpKeyOptions restrict the controller's key on a jump host to forwarding connections: no shell, no command, no
// terminal, no agent or X11 forwarding.
const JumpKeyOptions = `restrict,port-forwarding,command="/bin/false"`

// JumpKeyLine is the line that is added to the jump host account's authorized_keys, and the key's base64 part, from
// the controller's public key (type, key, comment).
func JumpKeyLine(pub string) (line, blob string) {
	f := strings.Fields(pub)
	if len(f) < 2 {
		return "", ""
	}
	comment := "virticle-controller"
	if len(f) > 2 {
		comment = strings.Join(f[2:], " ")
	}
	return JumpKeyOptions + " " + f[0] + " " + f[1] + " " + comment, f[1]
}

// jumpKeyScript reads the line from standard input and adds it to the account's authorized_keys unless the key is in
// it already ($1 is the key). The file and the directory are made when missing (mode 600 and 700) and get their
// SELinux label where restorecon exists.
const jumpKeyScript = `umask 077
d="$HOME/.ssh"; f="$d/authorized_keys"
IFS= read -r line || exit 1
[ -d "$d" ] || mkdir "$d" || exit 1
if [ -f "$f" ] && grep -qF -- "$1" "$f"; then exit 0; fi
if [ -s "$f" ] && [ -n "$(tail -c1 "$f")" ]; then echo >> "$f" || exit 1; fi
printf '%s\n' "$line" >> "$f" || exit 1
command -v restorecon >/dev/null 2>&1 && restorecon -F "$d" "$f" >/dev/null 2>&1
exit 0`

func installJumpKey(c *ssh.Client, pub string) error {
	line, blob := JumpKeyLine(pub)
	if line == "" || strings.ContainsAny(blob, "'\n ") {
		return errors.New("the controller's public key is not valid")
	}
	s, err := c.NewSession()
	if err != nil {
		return err
	}
	defer s.Close()
	s.Stdin = strings.NewReader(line + "\n")
	out, err := s.CombinedOutput("sh -c '" + strings.ReplaceAll(jumpKeyScript, "'", `'\''`) + "' sh '" + blob + "'")
	if err != nil {
		if m := strings.TrimSpace(string(out)); m != "" {
			return fmt.Errorf("%v: %s", err, strings.Join(strings.Fields(m), " "))
		}
		return err
	}
	return nil
}
