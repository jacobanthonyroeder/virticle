package cluster

import (
	"context"
	"log"
	"time"

	"virticle/internal/minver"
)

// versionRecheck is how long a node below the minimum version is left alone
// before the controller connects to it again. An upgraded node comes back by
// itself after this time (or at once when the node is edited or reconnected
// by hand); the node is not hammered with connection attempts meanwhile.
var versionRecheck = 5 * time.Minute

// supportedNode is minver.Check on the two calls libvirt answers with the
// libvirt version (ConnectGetLibVersion) and the hypervisor version
// (ConnectGetVersion). It returns nil when the node is supported or a version
// cannot be read: a libvirt that does not answer fails the connection
// elsewhere, with its own message.
func supportedNode(libVersion, hypervisorVersion func() (uint64, error)) error {
	lib, err := libVersion()
	if err != nil {
		return nil
	}
	hv, err := hypervisorVersion()
	if err != nil {
		return nil
	}
	return minver.Check(lib, hv)
}

// holdUnsupported shows the node as offline with err and waits for the next
// look: until versionRecheck has passed, the connection is gone or ctx ends.
func (m *Manager) holdUnsupported(ctx context.Context, n *node, err error, gone <-chan struct{}) {
	log.Printf("node %s: %v", n.cfg.Addr, err)
	n.set(nil, "offline", err.Error())
	m.Publish(Event{Type: "node", Node: n.cfg.ID, Detail: "offline"})
	t := time.NewTimer(versionRecheck)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-gone:
	case <-t.C:
	}
}
