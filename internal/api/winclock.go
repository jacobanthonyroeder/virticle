package api

import (
	"encoding/xml"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cluster"
	"virticle/internal/domxml"
	"virticle/internal/hostconn"
)

// Hardware clock of Windows guests. Windows reads the hardware clock as local time unless the registry value
// RealTimeIsUniversal (HKLM\SYSTEM\CurrentControlSet\Control\TimeZoneInformation, REG_DWORD 1) says it is UTC.
// It reads the value only when it boots, and until then it also writes local time into the clock whenever it
// sets its time (seen on Windows 11 26H2). A definition with offset 'utc' and no value, or the value and a clock
// that holds local time, leaves the guest off by its zone at the next boot.
//
// A new Windows VM starts with 'localtime' and a mark in the app metadata (<utcclock/>): Virticle has no hand in a
// guest installed from an ISO until its agent runs. The switch happens when Virticle shuts the VM down with the
// agent connected: the value is set, the saved definition becomes 'utc', and the guest powers off. The next boot
// is a cold start with a UTC clock and the value. Switching earlier, in a running guest, would leave the clock
// holding local time with the value set: a restart from inside the guest would come up off by its zone.
//
// After every start, once the agent answers, the value of a Windows guest is made to follow the saved definition
// (set for 'utc', removed for 'localtime' without the mark). A guest that matches costs one reg query.
//
// A clone with Windows guest setup of a sealed template gets 'utc' at once: its answer file sets the value in the
// specialize pass, before the first logon.

const (
	rtcKey   = `HKLM\SYSTEM\CurrentControlSet\Control\TimeZoneInformation`
	rtcValue = "RealTimeIsUniversal"
	regExe   = `C:\Windows\System32\reg.exe`
)

// rtcAddArgs are reg.exe's arguments that set the value; the answer file runs the same command.
var rtcAddArgs = []string{"add", rtcKey, "/v", rtcValue, "/t", "REG_DWORD", "/d", "1", "/f"}

// utcPendingEl is the metadata child that marks a VM whose definition becomes 'utc' at its next shutdown.
const utcPendingEl = "<utcclock/>"

func utcPending(h *hostconn.Host, dom libvirt.Domain) bool {
	raw, err := domainMeta(h, dom)
	return err == nil && metaHasUTCPending(raw)
}

func metaHasUTCPending(meta string) bool {
	if strings.TrimSpace(meta) == "" {
		return false
	}
	var root struct {
		UTC *struct{} `xml:"utcclock"`
	}
	return xml.Unmarshal([]byte(meta), &root) == nil && root.UTC != nil
}

func setUTCPending(h *hostconn.Host, dom libvirt.Domain, on bool) error {
	old, err := domainMeta(h, dom)
	if err != nil {
		return err
	}
	if metaHasUTCPending(old) == on {
		return nil
	}
	el := ""
	if on {
		el = utcPendingEl
	}
	meta, err := metaSetChild(old, "utcclock", el)
	if err != nil {
		return err
	}
	return setDomainMeta(h, dom, meta)
}

// rtcStep is what happens to the guest's value after a start.
type rtcStep int

const (
	rtcKeep rtcStep = iota
	rtcSet
	rtcRemove
)

// rtcPlan decides after a start from the saved offset, the mark and the guest's value what to do with the value.
// A VM with the mark keeps what it has until its shutdown; an offset other than utc and localtime is left alone.
func rtcPlan(offset string, pending, universal bool) rtcStep {
	switch {
	case offset == "utc" && !universal:
		return rtcSet
	case offset == "localtime" && !pending && universal:
		return rtcRemove
	}
	return rtcKeep
}

// parseRTCQuery reads the output of "reg query KEY /v RealTimeIsUniversal": true when the value is there and not 0.
func parseRTCQuery(out string) bool {
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) == 3 && strings.EqualFold(f[0], rtcValue) && strings.HasPrefix(f[1], "REG_") {
			n, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(f[2]), "0x"), 16, 64)
			return err == nil && n != 0
		}
	}
	return false
}

// runReg runs reg.exe in the guest and waits for it.
func runReg(h *hostconn.Host, dom libvirt.Domain, args ...string) (execResult, error) {
	pid, err := agentStart(h, dom, regExe, args...)
	if err != nil {
		return execResult{}, err
	}
	return agentWait(h, dom, pid, 30*time.Second)
}

// guestRTCUniversal asks the guest for its value. reg query exits with 1 when the value is not there.
func guestRTCUniversal(h *hostconn.Host, dom libvirt.Domain) (bool, error) {
	r, err := runReg(h, dom, "query", rtcKey, "/v", rtcValue)
	if err != nil {
		return false, err
	}
	switch r.ExitCode {
	case 0:
		return parseRTCQuery(r.Out), nil
	case 1:
		return false, nil
	}
	return false, regError("query", r)
}

func setGuestRTCUniversal(h *hostconn.Host, dom libvirt.Domain, on bool) error {
	args := rtcAddArgs
	if !on {
		args = []string{"delete", rtcKey, "/v", rtcValue, "/f"}
	}
	r, err := runReg(h, dom, args...)
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return regError(args[0], r)
	}
	return nil
}

func regError(op string, r execResult) error {
	msg := strings.TrimSpace(r.Err + " " + r.Out)
	if msg == "" {
		msg = "no output"
	}
	return fmt.Errorf("reg %s exited with %d: %s", op, r.ExitCode, shortReason(fmt.Errorf("%s", msg)))
}

// defineUTC makes the saved definition 'utc'; the running VM keeps its clock until it is powered off.
func defineUTC(h *hostconn.Host, dom libvirt.Domain) error {
	raw, err := h.DomainGetXMLDesc(dom, libvirt.DomainXMLInactive)
	if err != nil {
		return err
	}
	if domxml.ClockOffset(raw) != "localtime" {
		return nil
	}
	edited, err := domxml.SetClockOffset(raw, "utc")
	if err != nil {
		return err
	}
	_, err = h.DomainDefineXMLFlags(edited, libvirt.DomainDefineValidate)
	return err
}

// utcAtShutdown runs before Virticle shuts down a VM with the mark. With the agent connected and Windows in the
// guest it sets the value and saves the definition with 'utc'; the shutdown then powers the guest off, and the
// next start boots with both. It returns the clause for the shutdown's note ("" when nothing happened). A failure
// does not stop the shutdown: the mark is dropped and the VM stays on local time, which the clause says.
func utcAtShutdown(h *hostconn.Host, dom libvirt.Domain) string {
	if !utcPending(h, dom) || !agentConnected(h, dom) {
		return ""
	}
	id, err := agentOS(h, dom)
	if err != nil {
		return "" // the agent did not answer: the next shutdown tries again
	}
	if id != "mswindows" {
		setUTCPending(h, dom, false)
		return ""
	}
	raw, err := h.DomainGetXMLDesc(dom, libvirt.DomainXMLInactive)
	if err != nil {
		return ""
	}
	if domxml.ClockOffset(raw) == "localtime" {
		if err = setGuestRTCUniversal(h, dom, true); err == nil {
			err = defineUTC(h, dom)
		}
	}
	if merr := setUTCPending(h, dom, false); merr != nil && err == nil {
		err = merr
	}
	if err != nil {
		log.Printf("clock of %s: %v", dom.Name, err)
		return fmt.Sprintf("hardware clock stays in local time: could not set %s in the guest (%v)", rtcValue, err)
	}
	if domxml.ClockOffset(raw) != "localtime" {
		return ""
	}
	return rtcValue + " set in the guest, hardware clock in UTC from the next start"
}

// alignWindowsRTC runs after a start once the agent answers. It changes nothing for a guest that is not Windows
// or whose value matches. A change is a task of its own, "change clock".
func (s *Server) alignWindowsRTC(user string, h *hostconn.Host, dom libvirt.Domain) {
	id, err := agentOS(h, dom)
	if err != nil || id != "mswindows" {
		return
	}
	raw, err := h.DomainGetXMLDesc(dom, libvirt.DomainXMLInactive)
	if err != nil {
		return
	}
	universal, err := guestRTCUniversal(h, dom)
	if err != nil {
		log.Printf("clock of %s: reading %s in the guest: %v", dom.Name, rtcValue, err)
		return // the agent did not answer: the next start looks again
	}
	step := rtcPlan(domxml.ClockOffset(raw), utcPending(h, dom), universal)
	if step == rtcKeep {
		return
	}
	t, err := s.Store.StartTask("change clock", dom.Name, h.Name, user)
	if err != nil {
		log.Printf("clock of %s: %v", dom.Name, err)
		return
	}
	s.Nodes.Publish(cluster.Event{Type: "task", Task: t})
	err = setGuestRTCUniversal(h, dom, step == rtcSet)
	if err != nil {
		err = rtcError(step, err)
		log.Printf("clock of %s: %v", dom.Name, err)
	}
	s.Store.FinishTaskNote(&t, err, rtcNote(step))
	s.Nodes.Publish(cluster.Event{Type: "task", Task: t})
}

// rtcNote is the Result of a change after a start.
func rtcNote(step rtcStep) string {
	switch step {
	case rtcSet:
		return rtcValue + " set in the guest to match its UTC hardware clock, read at the guest's next boot"
	case rtcRemove:
		return rtcValue + " removed from the guest to match its local-time hardware clock, read at the guest's next boot"
	}
	return ""
}

func rtcError(step rtcStep, err error) error {
	if step == rtcRemove {
		return fmt.Errorf("could not remove %s from the guest: %w; Windows reads its local-time hardware clock as UTC", rtcValue, err)
	}
	return fmt.Errorf("could not set %s in the guest: %w; Windows reads its UTC hardware clock as local time", rtcValue, err)
}
