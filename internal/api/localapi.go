package api

// The command line's way into the controller: "virticle node add|list|remove" and "virticle status" call these routes,
// through the controller's local socket while it runs (see cmd/virticle/ctlsock.go), or in their own process on the same
// data directory while it is stopped. They are the handlers the web UI uses, so the result is the same either way.
//
// The socket is a file in the data directory (mode 0700) with mode 0600, and the listener only accepts root and the
// controller's own account. Whoever can open it can read the database and the controller's SSH key anyway, so it acts as
// the administrator. It is never served on a network listener.

import (
	"context"
	"net/http"
	"strings"

	"virticle/internal/store"
)

// LocalUser is the name the audit log and the task list show for requests from the command line.
const LocalUser = "command line"

// LocalHandler serves the routes the command line uses, as the administrator, without a sign-in. Only for the local
// socket and for the command's own process.
func (s *Server) LocalHandler() http.Handler {
	mux := http.NewServeMux()
	local := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			info := infoOf(r)
			info.pattern, info.user = pattern, LocalUser
			p := store.Principal{User: store.User{Username: LocalUser, Role: store.RoleAdmin}, Role: store.RoleAdmin}
			ctx := context.WithValue(r.Context(), ctxKey{}, LocalUser)
			ctx = context.WithValue(ctx, principalKey{}, p)
			h(w, r.WithContext(ctx))
		})
	}
	local("GET /api/version", s.version)
	local("GET /api/nodes", s.listNodes)
	local("POST /api/nodes", s.addNode)
	local("DELETE /api/nodes/{node}", s.deleteNode)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "not found", http.StatusNotFound) })
	return s.wrap(mux)
}

// ValidateUsername checks a user name the way the web interface does.
func ValidateUsername(name string) error { return checkUsername(name) }

// ValidateFirstAdmin checks a name and password for the first administrator the way the first-run page does.
func ValidateFirstAdmin(name, password string) error {
	if err := checkUsername(name); err != nil {
		return err
	}
	return checkPassword(password)
}

// LocalNodeAddress is the address under which this machine is added as a node: the one the first-run panel offers.
func LocalNodeAddress() string { return realLocalProbe().address() }

// LocalNodeProblem says why this machine cannot run virtual machines ("" when it can), the way the first-run panel
// decides it, without a closing full stop.
func LocalNodeProblem() string { return realLocalProbe().problem() }

func (p localProbe) problem() string {
	i := p.info(nil)
	switch {
	case !i.Capable:
		r := strings.TrimSuffix(i.Reason, "; add another machine as a node.")
		return strings.TrimSuffix(r, ".")
	case !i.KVMDevice:
		return "/dev/kvm is missing (KVM is off in the firmware, or the kvm module is not loaded)"
	}
	return ""
}

// IsThisMachine says whether a node address points to the machine this runs on.
func IsThisMachine(addr string) bool {
	h, _ := realLocalProbe().Hostname()
	return realLocalProbe().isThisMachine(addr, h)
}
