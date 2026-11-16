package api

import (
	"strings"
	"testing"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/domxml"
)

func TestCreateSerialAndClock(t *testing.T) {
	base := createVMRequest{Name: "x", VCPUs: 1, MemoryMiB: 256, Pool: "default"}
	lin := build(t, base)
	for _, want := range []string{"<serial type='pty'/>", "<console type='pty'/>", "<timer name='rtc' tickpolicy='catchup'/>", "<timer name='pit' tickpolicy='delay'/>", "<timer name='hpet' present='no'/>", "<timer name='kvmclock' present='yes'/>"} {
		if !strings.Contains(lin, want) {
			t.Errorf("linux default lacks %s", want)
		}
	}
	if strings.Contains(lin, "hypervclock") {
		t.Error("a Linux guest must not get hypervclock")
	}
	off := false
	base.Serial = &off
	if x := build(t, base); strings.Contains(x, "<serial") || strings.Contains(x, "<console") {
		t.Errorf("serial=false still wrote a serial device:\n%s", x)
	}
	win := build(t, createVMRequest{Name: "w", VCPUs: 1, MemoryMiB: 256, Pool: "default", OSType: "windows"})
	if !strings.Contains(win, "hypervclock") || !strings.Contains(win, "<hyperv mode='custom'>") || !strings.Contains(win, "clock offset='localtime'") {
		t.Errorf("windows keeps its Hyper-V clock and enlightenments:\n%s", win)
	}
	if strings.Contains(win, "kvmclock") {
		t.Error("windows does not get kvmclock")
	}
}

func TestSerialPending(t *testing.T) {
	none := "<domain><devices></devices></domain>"
	pty := "<domain><devices><serial type='pty'/></devices></domain>"
	live := func(x string) []domxml.Serial { s, _ := domxml.Serials(x); return s }
	if got := serialPending(live(none), pty); got != "add" {
		t.Errorf("add: %q", got)
	}
	if got := serialPending(live(pty), none); got != "remove" {
		t.Errorf("remove: %q", got)
	}
	if got := serialPending(live(pty), pty); got != "" {
		t.Errorf("same: %q", got)
	}
}

func TestClockSyncMetadata(t *testing.T) {
	meta, err := metaSetChild("", "clock", "<clock sync='off'/>")
	if err != nil || !strings.Contains(meta, "<clock sync='off'/>") {
		t.Fatalf("%q %v", meta, err)
	}
	// other features' children survive
	meta, err = metaSetChild("<meta><tags><tag>a</tag></tags></meta>", "clock", "<clock sync='off'/>")
	if err != nil || !strings.Contains(meta, "<tag>a</tag>") || !strings.Contains(meta, "sync='off'") {
		t.Fatalf("%q %v", meta, err)
	}
	meta, err = metaSetChild(meta, "clock", "")
	if err != nil || strings.Contains(meta, "clock") || !strings.Contains(meta, "<tag>a</tag>") {
		t.Fatalf("%q %v", meta, err)
	}
}

// A Windows VM keeps its hardware clock in local time; a restore as a new VM
// (and a clone) carries the <clock> element over unchanged, timers and all.
func TestLocalTimeClockKeptByRestore(t *testing.T) {
	base := createVMRequest{Name: "w", VCPUs: 1, MemoryMiB: 256, Pool: "default"}
	if localTimeClock(build(t, base)) {
		t.Error("a Linux guest's clock is UTC")
	}
	win := base
	win.OSType = "windows"
	x := build(t, win)
	if !localTimeClock(x) {
		t.Fatal("a Windows guest's clock is local time")
	}
	clock := func(s string) string {
		i, j := strings.Index(s, "<clock"), strings.Index(s, "</clock>")
		if i < 0 || j < i {
			t.Fatalf("no <clock> in %s", s)
		}
		return s[i : j+len("</clock>")]
	}
	out, err := cloneDomainXML(x, cloneEdit{Name: "w2", Disks: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	if clock(out) != clock(x) {
		t.Errorf("clock changed:\n%s\n%s", clock(x), clock(out))
	}
}

// A start asks for a sync twice (the start call and the node's "started" event); only the first waits for the
// agent, and it takes over the person's name and the stricter limit of the later claims.
func TestClockClaims(t *testing.T) {
	key := "n/test-clock-claims"
	defer releaseClockSync(key)
	if !claimClockSync(key, clockClaim{"system", "start", guestClockSkew}) {
		t.Fatal("first claim refused")
	}
	if claimClockSync(key, clockClaim{"alice", "start", guestClockSkew}) {
		t.Fatal("second claim for the same VM accepted")
	}
	if c := clockClaimOf(key); c.user != "alice" || c.why != "start" || c.minSkew != guestClockSkew {
		t.Errorf("after the person's claim: %+v", c)
	}
	claimClockSync(key, clockClaim{"system", "resume", 0})
	if c := clockClaimOf(key); c.user != "alice" || c.why != "resume" || c.minSkew != 0 {
		t.Errorf("after a resume: %+v", c)
	}
	releaseClockSync(key)
	if !claimClockSync(key, clockClaim{"bob", "start", guestClockSkew}) {
		t.Error("a claim after the release is refused")
	}
}

func TestStartedRunning(t *testing.T) {
	if !startedRunning(libvirt.DomainRunning) || startedRunning(libvirt.DomainPaused) || startedRunning(libvirt.DomainShutoff) {
		t.Error("only a running domain gets its clock synced after a start")
	}
}

func TestClockOff(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want bool
	}{{0, false}, {guestClockSkew, false}, {-guestClockSkew, false}, {8 * time.Hour, true}, {-3 * time.Second, true}} {
		if got := clockOff(c.d, guestClockSkew); got != c.want {
			t.Errorf("clockOff(%v) = %v", c.d, got)
		}
	}
}
