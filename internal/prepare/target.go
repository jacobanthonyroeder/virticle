package prepare

import (
	"errors"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
	"virticle/internal/store"
)

// Admin is the administrator's credential for one run. Nothing here is stored.
type Admin struct {
	Addr, User, Jump string
	Password         string // also used for "sudo -S" when the user is not root
	KeyPEM           []byte
	Agent            bool

	// The login on the jump host, for a jump host that does not accept the controller's key yet (a new controller has a
	// new key). Used for that hop only, and only when the controller's key is refused there.
	JumpPassword string
	JumpKeyPEM   []byte
	JumpAgent    bool
	// ControllerKey, the controller's public key, is added to the jump host account's authorized_keys when the login
	// was needed (hostconn.JumpKeyLine says what is written): the controller passes the jump host with it from then
	// on. Empty for a run that changes nothing (a check) and for unprepare.
	ControllerKey string
}

// HasJumpLogin says a login for the jump host is given.
func (a Admin) HasJumpLogin() bool {
	return a.Jump != "" && (a.JumpPassword != "" || len(a.JumpKeyPEM) > 0 || a.JumpAgent)
}

// Dial opens the administrator's SSH connection. The jump host (if any) is
// reached the way the controller reaches jump hosts (its key); only when that is refused and the administrator gave a
// login for the jump host is that used (and the controller's key added there, see Admin.ControllerKey). The final hop
// uses the administrator's credential, never the controller's own key. A failure is a *hostconn.HopError: it says
// whether the jump host or the node failed.
func Dial(m *cluster.Manager, a Admin) (*hostconn.Conn, error) {
	t, err := m.Target(store.Node{Addr: a.Addr, User: a.User, Jump: a.Jump})
	if err != nil {
		return nil, err
	}
	t.KeyFile = ""
	t.Password, t.KeyPEM, t.Agent = a.Password, a.KeyPEM, a.Agent
	if a.HasJumpLogin() {
		t.Jump.Password = "" // the controller's key first; the login only when it is refused
		t.JumpLogin = &hostconn.Target{Password: a.JumpPassword, KeyPEM: a.JumpKeyPEM, Agent: a.JumpAgent}
		t.JumpInstallKey = a.ControllerKey
	}
	return hostconn.DialConn(t, m.KnownHostsFile())
}

// DialFailure is the row for a Dial that failed: the hop that failed (the jump host or the node) and why.
func DialFailure(err error) Row {
	var h *hostconn.HopError
	if errors.As(err, &h) && h.Jump {
		return Row{ID: "jump", Title: "Jump host", Status: Error, Detail: h.Where() + ": " + h.Reason()}
	}
	if errors.As(err, &h) {
		return Row{ID: "admin", Title: "Administrator access", Status: Error, Detail: h.Where() + ": " + h.Reason()}
	}
	return Row{ID: "admin", Title: "Administrator access", Status: Error, Detail: "could not sign in: " + err.Error()}
}
