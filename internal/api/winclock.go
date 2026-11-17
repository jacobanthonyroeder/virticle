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
// It reads the value only when it boots, and until then it writes the clock the old way whenever it sets its time
// (seen on Windows 11 26H2). So the value and the clock's content can only change over together at a power-off:
// set or removed in a running guest, a restart from inside the guest comes up off by its zone, either way.
//
// The setting ("UTC hardware clock", on by default for a new Windows VM) is the saved offset, 'utc' or
// 'localtime', plus a switch waiting in the app metadata: <utcclock/> (to UTC) or <utcclock to="localtime"/>.
// The switch is made when Virticle shuts the VM down with the agent answering: the value is set or removed, the
// saved definition changed, and the guest powers off. The next boot is a cold start where both agree. A new
// Windows VM starts with 'localtime' and the switch to UTC: Virticle has no hand in a guest installed from an ISO
// until its agent runs. Changing the setting later records (or cancels) a switch the same way.
//
// After every start, once the agent answers, the value of a Windows guest without a waiting switch is made to
// follow the saved definition (set for 'utc', removed for 'localtime'). A guest that matches costs one reg query.
//
// A clone with Windows guest setup of a sealed template gets 'utc' at once: its answer file sets the value in the
// specialize pass, before the first logon. Other clones keep the source's offset and its waiting switch.

const (
	rtcKey   = `HKLM\SYSTEM\CurrentControlSet\Control\TimeZoneInformation`
	rtcValue = "RealTimeIsUniversal"
	regExe   = `C:\Windows\System32\reg.exe`
)

// rtcAddArgs are reg.exe's arguments that set the value; the answer file runs the same command.
var rtcAddArgs = []string{"add", rtcKey, "/v", rtcValue, "/t", "REG_DWORD", "/d", "1", "/f"}

// clockSwitchEl is the metadata child of a waiting switch to target ("utc" or "localtime").
func clockSwitchEl(target string) string {
	if target == "localtime" {
		return `<utcclock to="localtime"/>`
	}
	return "<utcclock/>"
}

// metaClockSwitch reads the waiting switch from the app metadata: "utc", "localtime" or "" (none).
func metaClockSwitch(meta string) string {
	if strings.TrimSpace(meta) == "" {
		return ""
	}
	var root struct {
		UTC *struct {
			To string `xml:"to,attr"`
		} `xml:"utcclock"`
	}
	if xml.Unmarshal([]byte(meta), &root) != nil || root.UTC == nil {
		return ""
	}
	if root.UTC.To == "localtime" {
		return "localtime"
	}
	return "utc"
}

func clockSwitch(h *hostconn.Host, dom libvirt.Domain) string {
	raw, err := domainMeta(h, dom)
	if err != nil {
		return ""
	}
	return metaClockSwitch(raw)
}

// setClockSwitch records a switch to target, or removes it ("").
func setClockSwitch(h *hostconn.Host, dom libvirt.Domain, target string) error {
	old, err := domainMeta(h, dom)
	if err != nil {
		return err
	}
	if metaClockSwitch(old) == target {
		return nil
	}
	el := ""
	if target != "" {
		el = clockSwitchEl(target)
	}
	meta, err := metaSetChild(old, "utcclock", el)
	if err != nil {
		return err
	}
	return setDomainMeta(h, dom, meta)
}

// clockSetting is the "UTC hardware clock" setting of a Windows VM: whether it is on (where the VM is going), and
// the switch that waits for the next shutdown.
func clockSetting(offset, sw string) (on bool, pending string) {
	if sw == "utc" || sw == "localtime" {
		if (sw == "utc") != (offset == "utc") {
			return sw == "utc", sw
		}
	}
	return offset == "utc", ""
}

// switchFor is the waiting switch after the setting is turned on or off: none when the saved offset already is
// what was asked for.
func switchFor(offset string, on bool) string {
	switch {
	case on && offset != "utc":
		return "utc"
	case !on && offset == "utc":
		return "localtime"
	}
	return ""
}

// rtcStep is what happens to the guest's value after a start.
type rtcStep int

const (
	rtcKeep rtcStep = iota
	rtcSet
	rtcRemove
)

// rtcPlan decides after a start from the saved offset, the waiting switch and the guest's value what to do with
// the value. A VM with a waiting switch keeps what it has until its shutdown; an offset other than utc and
// localtime is left alone.
func rtcPlan(offset, sw string, universal bool) rtcStep {
	switch {
	case sw != "":
		return rtcKeep
	case offset == "utc" && !universal:
		return rtcSet
	case offset == "localtime" && universal:
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

// setGuestRTCUniversal sets or removes the value. Removing a value that is not there is not an error.
func setGuestRTCUniversal(h *hostconn.Host, dom libvirt.Domain, on bool) error {
	if !on {
		if has, err := guestRTCUniversal(h, dom); err == nil && !has {
			return nil
		}
	}
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

// defineOffset saves the definition with offset; the running VM keeps its clock until it is powered off.
func defineOffset(h *hostconn.Host, dom libvirt.Domain, offset string) error {
	raw, err := h.DomainGetXMLDesc(dom, libvirt.DomainXMLInactive)
	if err != nil {
		return err
	}
	if domxml.ClockOffset(raw) == offset {
		return nil
	}
	edited, err := domxml.SetClockOffset(raw, offset)
	if err != nil {
		return err
	}
	_, err = h.DomainDefineXMLFlags(edited, libvirt.DomainDefineValidate)
	return err
}

// clockAtShutdown runs before Virticle shuts a VM down. With a waiting switch, the agent connected and Windows in
// the guest it sets or removes the value and saves the definition with the new offset; the shutdown then powers
// the guest off, and the next start boots with both. It returns the clause for the shutdown's note ("" when
// nothing happened). A failure does not stop the shutdown: the switch is dropped, the VM keeps its clock, and the
// clause says so.
func clockAtShutdown(h *hostconn.Host, dom libvirt.Domain) string {
	sw := clockSwitch(h, dom)
	if sw == "" || !agentConnected(h, dom) {
		return ""
	}
	id, err := agentOS(h, dom)
	if err != nil {
		return "" // the agent did not answer: the next shutdown tries again
	}
	if id != "mswindows" {
		setClockSwitch(h, dom, "")
		return ""
	}
	raw, err := h.DomainGetXMLDesc(dom, libvirt.DomainXMLInactive)
	if err != nil {
		return ""
	}
	offset := domxml.ClockOffset(raw)
	if offset != "utc" && offset != "localtime" {
		setClockSwitch(h, dom, "")
		return ""
	}
	if offset != sw {
		if err = setGuestRTCUniversal(h, dom, sw == "utc"); err == nil {
			err = defineOffset(h, dom, sw)
		}
	}
	if merr := setClockSwitch(h, dom, ""); merr != nil && err == nil {
		err = merr
	}
	if err != nil {
		log.Printf("clock of %s: %v", dom.Name, err)
		return switchFailNote(offset, sw, err)
	}
	if offset == sw {
		return ""
	}
	return switchNote(sw)
}

// switchWaitNote is the hardware result's note when the setting changed and a switch now waits.
const switchWaitNote = "Changes over at the next Shut down from Virticle while the guest agent runs."

// utcSettingNote is the task's Result of a change of the setting.
func utcSettingNote(on, waits bool) string {
	s := "UTC hardware clock off"
	if on {
		s = "UTC hardware clock on"
	}
	if waits {
		s += ", at the next shutdown"
	}
	return s
}

func switchNote(sw string) string {
	if sw == "utc" {
		return rtcValue + " set in the guest, hardware clock in UTC from the next start"
	}
	return rtcValue + " removed from the guest, hardware clock in local time from the next start"
}

func switchFailNote(offset, sw string, err error) string {
	now := "local time"
	if offset == "utc" {
		now = "UTC"
	}
	verb := "set"
	if sw == "localtime" {
		verb = "remove"
	}
	return fmt.Sprintf("hardware clock stays in %s: could not %s %s in the guest (%v)", now, verb, rtcValue, err)
}

// alignWindowsRTC runs after a start once the agent answers. It changes nothing for a guest that is not Windows,
// that has a waiting switch, or whose value matches. A change is a task of its own, "change clock".
func (s *Server) alignWindowsRTC(user string, h *hostconn.Host, dom libvirt.Domain) {
	id, err := agentOS(h, dom)
	if err != nil || id != "mswindows" {
		return
	}
	raw, err := h.DomainGetXMLDesc(dom, libvirt.DomainXMLInactive)
	if err != nil {
		return
	}
	sw := clockSwitch(h, dom)
	if sw != "" {
		return
	}
	universal, err := guestRTCUniversal(h, dom)
	if err != nil {
		log.Printf("clock of %s: reading %s in the guest: %v", dom.Name, rtcValue, err)
		return // the agent did not answer: the next start looks again
	}
	step := rtcPlan(domxml.ClockOffset(raw), sw, universal)
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

// windowsDef reports whether a definition is a Windows guest's (libosinfo id, Hyper-V enlightenments, or a
// local-time clock).
func windowsDef(domXML string) bool {
	family, _, _ := guessOS(domXML)
	return family == "windows"
}
