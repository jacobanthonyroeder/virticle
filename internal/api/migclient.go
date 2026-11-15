package api

// Live migration coordinated by the controller.
//
// libvirt's peer-to-peer migration would make the source libvirtd open a
// qemu+ssh connection to the target as root, which needs SSH trust between the
// nodes and a key a service account does not have. So for every node, root or
// service account, the controller does what virDomainMigrate does on the
// client side: it calls the five phases of libvirt's migration protocol
// itself, on its own two libvirt connections.
//
//	Begin (source) -> Prepare (target) -> Perform (source) -> Finish (target) -> Confirm (source)
//
// The controller carries only the cookies and the domain XML. The memory
// stream and the NBD disk mirror still run between the two QEMU processes, node
// to node, to the address stored for the target. Nothing needs SSH between
// the nodes.

import (
	"fmt"
	"log"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/hostconn"
)

// withParam returns params with the string parameter field set to value.
func withParam(params []libvirt.TypedParam, field, value string) []libvirt.TypedParam {
	out := make([]libvirt.TypedParam, 0, len(params)+1)
	for _, p := range params {
		if p.Field != field {
			out = append(out, p)
		}
	}
	return append(out, libvirt.TypedParam{Field: field, Value: *libvirt.NewTypedParamValueString(value)})
}

// handover is what became of the VM in a migration that was started.
type handover int

const (
	handNone    handover = iota // the target did not take the VM over: it is on the source (confirmed)
	handDone                    // the VM runs on the target
	handUnknown                 // not known: the answer of the target was lost and the target cannot be asked
)

// settleWait is how long a migration whose last answer was lost waits for the target to be reachable again, and
// settlePoll how often it asks.
var (
	settleWait = 3 * time.Minute
	settlePoll = 5 * time.Second
)

// performFromController runs the migration phases and returns the error of the first that failed, with what
// became of the VM. The source is told to resume when the target is known not to run the VM, and the target to
// discard what it prepared.
//
// The one phase whose failure does not say that is Finish: libvirt on the target may have started the VM there and
// only its answer was lost (the connection broke). Telling the source to resume then would run the VM twice. So
// when Finish fails without an answer of libvirt, the source is not told anything until the target says where the
// VM is: up to settleWait in the task, and after that by the cleanup (migjournal.go) when the target comes back.
// Meanwhile the VM stays paused on the source.
func (m *migRun) performFromController(flags libvirt.DomainMigrateFlags, params []libvirt.TypedParam) (handover, error) {
	// Not peer-to-peer: the controller is the client of both libvirtds.
	flags &^= libvirt.MigratePeer2peer // (the caller does not set it; kept so the call is right by itself)
	// The target does not take the options that only the source acts on.
	destFlags := uint32(flags &^ (libvirt.MigrateAutoConverge | libvirt.MigrateAbortOnError))

	cookie, xml, err := m.src.DomainMigrateBegin3Params(m.dom, params, uint32(flags))
	if err != nil {
		return handNone, fmt.Errorf("starting the migration on %s: %w", m.src.Name, err)
	}
	settled := false
	defer func() {
		if settled {
			return
		}
		if err := m.src.DomainMigrateConfirm3Params(m.dom, params, nil, uint32(flags), 1); err != nil {
			log.Printf("migrate %s: ending the failed migration on %s: %v", m.plan.Name, m.src.Name, err)
		}
	}()

	dparams := withParam(withParam(params, libvirt.MigrateParamDestXML, xml), libvirt.MigrateParamDestName, m.dom.Name)
	cookie, uriOut, err := m.dst.DomainMigratePrepare3Params(dparams, cookie, destFlags)
	if err != nil {
		return handNone, fmt.Errorf("preparing %s: %w", m.dst.Name, err)
	}
	pparams := params
	if len(uriOut) > 0 && uriOut[0] != "" {
		pparams = withParam(params, libvirt.MigrateParamURI, uriOut[0])
	}

	cookie, perr := m.src.DomainMigratePerform3Params(m.dom, nil, pparams, cookie, flags)
	cancelled := int32(0)
	if perr != nil {
		cancelled = 1
	}
	// Without a successful Perform, Finish is told to cancel: the target never starts the VM then.
	_, fcookie, ferr := m.dst.DomainMigrateFinish3Params(dparams, cookie, destFlags, cancelled)
	if perr != nil {
		m.discardIncoming(m.dst)
		return handNone, perr
	}
	if ferr != nil {
		ferr = fmt.Errorf("finishing the migration on %s: %w", m.dst.Name, ferr)
		if libvirtAnswered(ferr) {
			m.discardIncoming(m.dst)
			return handNone, ferr
		}
		settled = true
		return m.settle(flags, params, ferr)
	}
	settled = true
	if err := m.src.DomainMigrateConfirm3Params(m.dom, params, fcookie, uint32(flags), 0); err != nil {
		// The VM runs on the target; the source could not tidy up.
		log.Printf("migrate %s: confirming on %s: %v", m.plan.Name, m.src.Name, err)
	}
	return handDone, nil
}

// settle finds out where the VM is after Finish failed without an answer, and tells the source.
func (m *migRun) settle(flags libvirt.DomainMigrateFlags, params []libvirt.TypedParam, ferr error) (handover, error) {
	m.settling.Store(true)
	m.finishLost.Store(true)
	defer m.settling.Store(false)
	confirm := func(moved bool) error {
		c := int32(1)
		if moved {
			c = 0
		}
		return m.src.DomainMigrateConfirm3Params(m.dom, params, nil, uint32(flags), c)
	}
	log.Printf("migrate %s: %v; waiting for %s to say where the VM is", m.plan.Name, ferr, m.dst.Name)
	hand := settleLoop(m.askTarget, func(h *hostconn.Host) { m.discardIncoming(h) }, func(moved bool) {
		if err := confirm(moved); err != nil {
			log.Printf("migrate %s: telling %s the outcome: %v", m.plan.Name, m.src.Name, err)
		}
	}, settleWait, settlePoll)
	if hand != handUnknown {
		return hand, ferr
	}
	// Still not known. The source keeps the VM paused; the cleanup settles it when the target answers.
	if m.jid != 0 {
		migJournal.Lock()
		id := m.jid
		migJournal.confirm[id] = func(moved bool) error {
			err := confirm(moved)
			if err == nil {
				migJournal.Lock()
				delete(migJournal.confirm, id)
				migJournal.Unlock()
			}
			return err
		}
		migJournal.Unlock()
	}
	return handUnknown, ferr
}

// settleLoop asks the target where the VM is until it says so clearly or wait is over, and tells the source:
// confirm(true) when the VM runs on the target, confirm(false) when it is not there. An unfinished incoming copy is
// discarded and asked about again. A question without an answer never leads to either: the result is then
// handUnknown and the source has been told nothing.
func settleLoop(ask func() (vmPresence, *hostconn.Host), discard func(*hostconn.Host), confirm func(moved bool), wait, poll time.Duration) handover {
	deadline := time.Now().Add(wait)
	for {
		where, h := ask()
		switch where {
		case vmRunning:
			confirm(true)
			return handDone
		case vmAbsent:
			confirm(false)
			return handNone
		case vmTransient:
			discard(h) // Finish never ran there; the next look finds it gone
		}
		if !time.Now().Before(deadline) {
			return handUnknown
		}
		time.Sleep(poll)
	}
}

// askTarget asks the target, on its current connection (the one the migration used may be gone), where the VM is.
// A defined but stopped VM, or no answer, is vmUnknown.
func (m *migRun) askTarget() (vmPresence, *hostconn.Host) {
	h, err := m.s.Nodes.Host(m.req.Target)
	if err != nil {
		return vmUnknown, nil
	}
	where, _, _, err := whereVM(h, m.dom.UUID)
	if err != nil || where == vmDefined {
		return vmUnknown, h
	}
	return where, h
}

// discardIncoming destroys the paused, not yet defined domain a failed
// migration can leave on the target (the pre-flight made sure that no domain of
// this UUID existed there before). A domain that is running, or defined, is
// left alone for the caller to judge.
func (m *migRun) discardIncoming(h *hostconn.Host) {
	if h == nil {
		return
	}
	d, err := h.DomainLookupByUUID(m.dom.UUID)
	if err != nil {
		return
	}
	if persistent, err := h.DomainIsPersistent(d); err != nil || persistent != 0 {
		return
	}
	if state, _, err := h.DomainGetState(d, 0); err != nil || stateName(uint8(state)) != "paused" {
		return
	}
	if err := h.DomainDestroy(d); err != nil {
		log.Printf("migrate %s: discarding the incoming domain on %s: %v", m.plan.Name, h.Name, err)
	}
}
