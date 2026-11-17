package api

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestRTCPlan(t *testing.T) {
	for _, c := range []struct {
		offset             string
		pending, universal bool
		step               rtcStep
	}{
		// a new Windows VM keeps what it has until Virticle shuts it down
		{"localtime", true, false, rtcKeep},
		{"localtime", true, true, rtcKeep},
		// an existing VM on local time is left alone; a stray value is removed
		{"localtime", false, false, rtcKeep},
		{"localtime", false, true, rtcRemove},
		// a UTC clock needs the value
		{"utc", false, true, rtcKeep},
		{"utc", false, false, rtcSet},
		{"utc", true, false, rtcSet},
		// other offsets are not this code's business
		{"variable", false, true, rtcKeep},
		{"timezone", true, false, rtcKeep},
		{"", false, true, rtcKeep},
	} {
		if step := rtcPlan(c.offset, c.pending, c.universal); step != c.step {
			t.Errorf("rtcPlan(%q, pending %v, value %v) = %v; want %v", c.offset, c.pending, c.universal, step, c.step)
		}
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

func TestUTCPendingMetadata(t *testing.T) {
	meta, err := metaSetChild("<meta><clock sync='off'/></meta>", "utcclock", utcPendingEl)
	if err != nil || !metaHasUTCPending(meta) || !strings.Contains(meta, "sync='off'") {
		t.Fatalf("set: %q %v", meta, err)
	}
	meta, err = metaSetChild(meta, "utcclock", "")
	if err != nil || metaHasUTCPending(meta) || !strings.Contains(meta, "sync='off'") {
		t.Fatalf("clear: %q %v", meta, err)
	}
	if metaHasUTCPending("") || metaHasUTCPending("<meta><clock sync='off'/></meta>") {
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
