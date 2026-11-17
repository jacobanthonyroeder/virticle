package api

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestRTCPlan(t *testing.T) {
	for _, c := range []struct {
		offset, sw string
		universal  bool
		step       rtcStep
	}{
		// a VM with a waiting switch keeps what it has until Virticle shuts it down
		{"localtime", "utc", false, rtcKeep},
		{"localtime", "utc", true, rtcKeep},
		{"utc", "localtime", true, rtcKeep},
		{"utc", "localtime", false, rtcKeep},
		// an existing VM on local time is left alone; a stray value is removed
		{"localtime", "", false, rtcKeep},
		{"localtime", "", true, rtcRemove},
		// a UTC clock needs the value
		{"utc", "", true, rtcKeep},
		{"utc", "", false, rtcSet},
		// other offsets are not this code's business
		{"variable", "", true, rtcKeep},
		{"", "", true, rtcKeep},
	} {
		if step := rtcPlan(c.offset, c.sw, c.universal); step != c.step {
			t.Errorf("rtcPlan(%q, switch %q, value %v) = %v; want %v", c.offset, c.sw, c.universal, step, c.step)
		}
	}
}

// The checkbox shows where the VM is going; a switch waits only while the saved offset differs from it.
func TestClockSetting(t *testing.T) {
	for _, c := range []struct {
		offset, sw string
		on         bool
		pending    string
	}{
		{"localtime", "", false, ""},
		{"localtime", "utc", true, "utc"},
		{"utc", "", true, ""},
		{"utc", "localtime", false, "localtime"},
		{"utc", "utc", true, ""}, // a clone with guest setup: utc already, the inherited switch has nothing to do
		{"localtime", "localtime", false, ""},
	} {
		if on, p := clockSetting(c.offset, c.sw); on != c.on || p != c.pending {
			t.Errorf("clockSetting(%q, %q) = %v, %q; want %v, %q", c.offset, c.sw, on, p, c.on, c.pending)
		}
	}
	for _, c := range []struct {
		offset string
		on     bool
		sw     string
	}{
		{"localtime", true, "utc"}, {"localtime", false, ""}, {"utc", false, "localtime"}, {"utc", true, ""},
	} {
		if sw := switchFor(c.offset, c.on); sw != c.sw {
			t.Errorf("switchFor(%q, %v) = %q; want %q", c.offset, c.on, sw, c.sw)
		}
		// turning the setting back cancels the switch
		if on, p := clockSetting(c.offset, switchFor(c.offset, c.on)); on != c.on || (p != "") != (c.sw != "") {
			t.Errorf("after switchFor(%q, %v): setting %v, pending %q", c.offset, c.on, on, p)
		}
	}
}

func TestCreateUTCClock(t *testing.T) {
	off, on := false, true
	for _, c := range []struct {
		q    createVMRequest
		want bool
	}{
		{createVMRequest{OSType: "windows"}, true}, // old clients send nothing: on
		{createVMRequest{OSType: "windows", UTCClock: &on}, true},
		{createVMRequest{OSType: "windows", UTCClock: &off}, false},
		{createVMRequest{OSType: "linux", UTCClock: &on}, false},
		{createVMRequest{}, false},
	} {
		if got := c.q.utcClock(); got != c.want {
			t.Errorf("%+v: utcClock %v", c.q, got)
		}
	}
}

// A Windows VM made by Virticle has no libosinfo metadata: its Hyper-V enlightenments say Windows, whatever its
// clock offset.
func TestGuestFamilyVirticleWindows(t *testing.T) {
	win := build(t, createVMRequest{Name: "w", VCPUs: 1, MemoryMiB: 256, Pool: "default", OSType: "windows"})
	lin := build(t, createVMRequest{Name: "l", VCPUs: 1, MemoryMiB: 256, Pool: "default"})
	if g := guestFamily("", templateMark{}, win); g != "windows" {
		t.Errorf("local-time Windows VM: %s", g)
	}
	if g := guestFamily("", templateMark{}, strings.Replace(win, "offset='localtime'", "offset='utc'", 1)); g != "windows" {
		t.Errorf("UTC Windows VM: %s", g)
	}
	if g := guestFamily("", templateMark{}, lin); g != "linux" {
		t.Errorf("Linux VM: %s", g)
	}
}

func TestParseRTCQuery(t *testing.T) {
	const key = "\r\nHKEY_LOCAL_MACHINE\\SYSTEM\\CurrentControlSet\\Control\\TimeZoneInformation\r\n"
	for out, want := range map[string]bool{
		key + "    RealTimeIsUniversal    REG_DWORD    0x1\r\n\r\n": true,
		key + "    RealTimeIsUniversal    REG_QWORD    0x1\r\n\r\n": true,
		key + "    RealTimeIsUniversal    REG_DWORD    0x0\r\n\r\n": false,
		key + "    TimeZoneKeyName    REG_SZ    UTC\r\n":            false,
		"": false,
	} {
		if got := parseRTCQuery(out); got != want {
			t.Errorf("parseRTCQuery(%q) = %v", out, got)
		}
	}
}

func TestClockSwitchMetadata(t *testing.T) {
	for _, target := range []string{"utc", "localtime"} {
		meta, err := metaSetChild("<meta><clock sync='off'/></meta>", "utcclock", clockSwitchEl(target))
		if err != nil || metaClockSwitch(meta) != target || !strings.Contains(meta, "sync='off'") {
			t.Fatalf("set %s: %q %v", target, meta, err)
		}
		meta, err = metaSetChild(meta, "utcclock", "")
		if err != nil || metaClockSwitch(meta) != "" || !strings.Contains(meta, "sync='off'") {
			t.Fatalf("clear: %q %v", meta, err)
		}
	}
	// libvirt hands the children back with the namespace prefix
	if metaClockSwitch(`<virticle:meta xmlns:virticle="https://virticle.invalid/ns/1"><virticle:utcclock/></virticle:meta>`) != "utc" {
		t.Error("prefixed mark not read")
	}
	if metaClockSwitch("") != "" || metaClockSwitch("<meta><clock sync='off'/></meta>") != "" {
		t.Error("no mark read as a mark")
	}
}

func TestStartClockCheck(t *testing.T) {
	win := build(t, createVMRequest{Name: "w", VCPUs: 1, MemoryMiB: 256, Pool: "default", OSType: "windows"})
	lin := build(t, createVMRequest{Name: "l", VCPUs: 1, MemoryMiB: 256, Pool: "default"})
	if !startClockCheck(win) {
		t.Error("a Windows VM on local time is checked after a start")
	}
	if !startClockCheck(strings.Replace(win, "offset='localtime'", "offset='utc'", 1)) {
		t.Error("a Windows VM on UTC is checked after a start")
	}
	if startClockCheck(lin) {
		t.Error("a Linux VM on UTC is not checked after a start")
	}
	if !startClockCheck(strings.Replace(lin, "offset='utc'", "offset='localtime'", 1)) {
		t.Error("any guest on local time is checked after a start")
	}
}

func TestUnattendUTCClock(t *testing.T) {
	c := unattendConfig{ComputerName: "WIN-01", User: "adm", Password: "pw"}
	plain, err := c.unattendXML()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain, rtcValue) {
		t.Error("the value is set only for a UTC clock")
	}
	c.UTCClock = true
	x, err := c.unattendXML()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Settings []struct {
			Pass       string `xml:"pass,attr"`
			Components []struct {
				Name string   `xml:"name,attr"`
				Path []string `xml:"RunSynchronous>RunSynchronousCommand>Path"`
			} `xml:"component"`
		} `xml:"settings"`
	}
	if err := xml.Unmarshal([]byte(x), &doc); err != nil {
		t.Fatalf("not well-formed: %v\n%s", err, x)
	}
	want := `C:\Windows\System32\reg.exe add HKLM\SYSTEM\CurrentControlSet\Control\TimeZoneInformation /v RealTimeIsUniversal /t REG_DWORD /d 1 /f`
	found := false
	for _, s := range doc.Settings {
		for _, comp := range s.Components {
			if comp.Name == "Microsoft-Windows-Deployment" {
				found = s.Pass == "specialize" && len(comp.Path) == 1 && comp.Path[0] == want
			}
		}
	}
	if !found {
		t.Errorf("no specialize command %q:\n%s", want, x)
	}
}

func TestUTCSetupClone(t *testing.T) {
	if !utcSetupClone("windows", templateMark{Sealed: "windows"}) {
		t.Error("a sealed Windows template with guest setup gives a UTC clone")
	}
	if utcSetupClone("windows", templateMark{Template: true}) || utcSetupClone("linux", templateMark{Sealed: "linux"}) {
		t.Error("an unsealed Windows source (the answer file is not read) or a Linux one keeps its clock")
	}
}

// A restore defines the VM from the backup's definition, metadata included, and clears only the replication
// children: a switch that waited when the backup was made still waits, which fits the restored disk (no value yet).
func TestRestoreKeepsWaitingClockSwitch(t *testing.T) {
	for _, target := range []string{"utc", "localtime"} {
		meta, err := metaSetChild("<meta></meta>", "utcclock", clockSwitchEl(target))
		if err != nil {
			t.Fatal(err)
		}
		got, err := setMetaChildren(meta, map[string]string{replChild: "", replicaChild: "", staleChild: "", failedOverChd: ""})
		if err != nil {
			t.Fatal(err)
		}
		if metaClockSwitch(got) != target {
			t.Fatalf("%s: switch lost by the restore's metadata change: %s", target, got)
		}
	}
}

// A restore as a new VM goes through cloneDomainXML, which drops the app metadata (seen on the lab: the restored
// VM had no switch); the restore sets it again from the backup's definition.
func TestRestoreNewKeepsWaitingClockSwitch(t *testing.T) {
	for want, el := range map[string]string{"utc": `<virticle:utcclock/>`, "localtime": `<virticle:utcclock to="localtime"/>`} {
		dom := `<domain type='kvm'><name>w</name><metadata><virticle:meta xmlns:virticle="https://virticle.invalid/ns/1">` + el +
			`</virticle:meta></metadata><devices/></domain>`
		if got := defClockSwitch(dom); got != want {
			t.Errorf("%s: read %q", el, got)
		}
		out, err := cloneDomainXML(dom, cloneEdit{Name: "w2", Disks: map[string]string{}})
		if err != nil {
			t.Fatal(err)
		}
		if defClockSwitch(out) != "" {
			t.Errorf("cloneDomainXML kept the app metadata; the restore's own step is then not needed:\n%s", out)
		}
	}
	if defClockSwitch("<domain><metadata/></domain>") != "" {
		t.Error("no switch read as one")
	}
}

func TestSwitchWaitsNotes(t *testing.T) {
	c := &shutdownSwitch{sw: "utc"}
	if got := c.finish(poweredOff, nil); got != "hardware clock change still waits: the VM was powered off" {
		t.Fatal(got)
	}
	// started again between two looks: the guest is not touched (c.h is nil: any call into it would panic)
	if got := c.finish(startedAgain, nil); got != "hardware clock change still waits: the VM was started again" {
		t.Fatal(got)
	}
}
