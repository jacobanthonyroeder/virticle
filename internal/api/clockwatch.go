package api

import (
	"log"
	"sync"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cluster"
)

// The clock of a guest with a local-time hardware clock is synced after every start that the controller sees,
// not only after a start from the VM's page or a restore: a clone or a new VM that starts, a failover, a
// rolling update that starts the VMs again, a VM that libvirt starts at the node's boot or that someone starts
// with virsh. The node's lifecycle event "started" says so. When a node's connection comes (back) up, the
// running VMs with such a clock are looked at once: a node that rebooted has started its autostart VMs before
// the controller was connected again, and a controller that was down missed the events. The sync itself only
// happens when the guest is off by more than guestClockSkew, so a guest that is right costs one guest-get-time.

var clockWatchOnce sync.Once

func (s *Server) startClockWatch() {
	if s.Nodes == nil {
		return
	}
	clockWatchOnce.Do(func() {
		events, _ := s.Nodes.Subscribe()
		go func() {
			for ev := range events {
				switch {
				case ev.Type == "domain" && ev.Detail == "started":
					go s.clockAfterStartEvent(ev.Node, ev.UUID)
				case ev.Type == "node" && ev.Detail == "online":
					go s.clockCheckNode(ev.Node)
				}
			}
		}()
	})
}

// startedRunning says whether a domain that just started runs a guest: a domain started paused (the replica
// copy of a shut-off VM, the target of an incoming migration) has no guest clock to sync yet; a migration
// syncs on its own when it ends.
func startedRunning(state libvirt.DomainState) bool { return state == libvirt.DomainRunning }

func (s *Server) clockAfterStartEvent(node int64, uuid string) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("clock sync after a start: %v", p)
		}
	}()
	h, err := s.Nodes.Host(node)
	if err != nil {
		return
	}
	u, err := cluster.ParseUUID(uuid)
	if err != nil {
		return
	}
	dom, err := h.DomainLookupByUUID(u)
	if err != nil {
		return
	}
	if st, _, err := h.DomainGetState(dom, 0); err != nil || !startedRunning(libvirt.DomainState(st)) {
		return
	}
	s.syncLocalTimeClock("system", "start", h, dom)
}

// clockCheckNode looks once at the running VMs of a node whose connection just came up.
func (s *Server) clockCheckNode(node int64) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("clock check of a node: %v", p)
		}
	}()
	h, err := s.Nodes.Host(node)
	if err != nil {
		return
	}
	doms, _, err := h.ConnectListAllDomains(1, libvirt.ConnectListDomainsActive)
	if err != nil {
		return
	}
	for _, dom := range doms {
		if st, _, err := h.DomainGetState(dom, 0); err == nil && startedRunning(libvirt.DomainState(st)) {
			s.syncLocalTimeClock("system", "node connected", h, dom)
		}
	}
}
