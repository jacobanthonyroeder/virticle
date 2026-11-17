package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
)

// Guests a backup may have left frozen. When every try to thaw a guest after a backup's freeze fails (the thaw
// right after backup-begin, or the one after a freeze that took effect after the wait), the guest is recorded
// here and becomes an alert. Each alert pass looks at the recorded guests of the nodes it reads: the record goes
// once the guest agent reports the file systems thawed, or the VM is gone or no longer running; a guest that the
// agent reports frozen is thawed again. The records are kept in the database with the alert state (alertpersist.go), so a
// restart does not forget them.

type frozenGuest struct {
	Host, UUID, Name string
	Err              string // the last thaw error
	At               time.Time
}

var frozenGuests = struct {
	sync.Mutex
	m map[string]frozenGuest // host name + "/" + uuid
}{m: map[string]frozenGuest{}}

// noteFrozen records a guest whose thaw failed on every try.
func noteFrozen(host, uuid, name string, err error) {
	frozenGuests.Lock()
	defer frozenGuests.Unlock()
	k := host + "/" + uuid
	g, had := frozenGuests.m[k]
	if !had {
		g = frozenGuest{Host: host, UUID: uuid, Name: name, At: time.Now()}
	}
	g.Err = freezeReason(err)
	frozenGuests.m[k] = g
	pokeAlertsNow() // the alert appears within seconds, not at the next minute
}

// pokeAlertsNow is pokeAlerts for code that has no server at hand.
func pokeAlertsNow() {
	select {
	case alertPoke <- struct{}{}:
	default:
	}
}

// allFrozen lists all records, for saving.
func allFrozen() []frozenGuest {
	frozenGuests.Lock()
	defer frozenGuests.Unlock()
	var out []frozenGuest
	for _, g := range frozenGuests.m {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host+"/"+out[i].UUID < out[j].Host+"/"+out[j].UUID })
	return out
}

// setFrozen adds saved records that are not known yet.
func setFrozen(list []frozenGuest) {
	frozenGuests.Lock()
	defer frozenGuests.Unlock()
	for _, g := range list {
		if _, ok := frozenGuests.m[g.Host+"/"+g.UUID]; !ok {
			frozenGuests.m[g.Host+"/"+g.UUID] = g
		}
	}
}

func forgetFrozen(host, uuid string) {
	frozenGuests.Lock()
	delete(frozenGuests.m, host+"/"+uuid)
	frozenGuests.Unlock()
}

// frozenOn lists the recorded guests of one node, oldest first.
func frozenOn(host string) []frozenGuest {
	frozenGuests.Lock()
	var out []frozenGuest
	for _, g := range frozenGuests.m {
		if g.Host == host {
			out = append(out, g)
		}
	}
	frozenGuests.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// frozenView is what the alert pass needs to know about one recorded guest.
type frozenView interface {
	// state says whether the domain still exists and whether it runs (running or paused).
	state() (exists, running bool, err error)
	// status is the agent's guest-fsfreeze-status: "thawed" or "frozen".
	status() (string, error)
	thaw() error
}

// frozenStill decides about one recorded guest in an alert pass: false means the guest is no longer frozen (or
// gone) and the record can go. busy is true while a backup of the VM runs: its own freeze is not touched.
func frozenStill(v frozenView, busy bool) (still bool, why string) {
	if busy {
		return true, ""
	}
	exists, running, err := v.state()
	switch {
	case err != nil:
		return true, ""
	case !exists:
		return false, "the VM is gone"
	case !running:
		return false, "the VM is not running"
	}
	st, err := v.status()
	if err != nil {
		return true, "" // the agent does not answer: nothing is known
	}
	if st != "frozen" {
		return false, "the guest agent reports the file systems " + st
	}
	if err := v.thaw(); err != nil {
		return true, ""
	}
	return false, "thawed again"
}

// agentView is frozenView on a node.
type agentView struct {
	h   *hostconn.Host
	dom libvirt.Domain
	uid [16]byte
}

func (a *agentView) state() (bool, bool, error) {
	d, err := a.h.DomainLookupByUUID(a.uid)
	if err != nil {
		var lv libvirt.Error
		if errors.As(err, &lv) && lv.Code == uint32(libvirt.ErrNoDomain) {
			return false, false, nil
		}
		return false, false, err
	}
	a.dom = d
	st, _, err := a.h.DomainGetState(d, 0)
	if err != nil {
		return true, false, err
	}
	s := stateName(uint8(st))
	return true, s == "running" || s == "paused", nil
}

func (a *agentView) status() (string, error) {
	raw, err := agentCmdT(a.h, a.dom, "guest-fsfreeze-status", nil, freezeInfoWait)
	if err != nil {
		return "", err
	}
	var st string
	if err := json.Unmarshal(raw, &st); err != nil {
		return "", fmt.Errorf("unreadable guest-fsfreeze-status reply: %w", err)
	}
	return st, nil
}

// thaw has a short limit: it runs in the alert pass, which must not wait long for one guest.
func (a *agentView) thaw() error {
	return within(freezeInfoWait*time.Second, "the guest did not answer the thaw in time", func() error {
		_, err := a.h.DomainFsthaw(a.dom, nil, 0)
		return err
	})
}

// gatherFrozen checks the recorded guests of one node and returns those that may still be frozen.
func gatherFrozen(h *hostconn.Host, node int64, nodeName string) []alertFrozen {
	var out []alertFrozen
	for _, g := range frozenOn(h.Name) {
		uid, err := cluster.ParseUUID(g.UUID)
		if err != nil {
			forgetFrozen(g.Host, g.UUID)
			continue
		}
		bkRuns.Lock()
		_, busy := bkRuns.m[g.UUID]
		bkRuns.Unlock()
		if still, why := frozenStill(&agentView{h: h, uid: uid}, busy); !still {
			log.Printf("backup: %s on %s is no longer frozen: %s", g.Name, nodeName, why)
			forgetFrozen(g.Host, g.UUID)
			continue
		}
		out = append(out, alertFrozen{Node: node, NodeName: nodeName, UUID: g.UUID, Name: g.Name, Err: g.Err, At: g.At})
	}
	return out
}
