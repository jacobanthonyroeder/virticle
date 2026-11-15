package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"virticle/internal/hostconn"
	"virticle/internal/nodeproto"
	"virticle/internal/prepare"
	"virticle/internal/snapsched"
)

func lastTask(t *testing.T, s *Server) (kind, status, note, errText string) {
	t.Helper()
	ts, err := s.Store.Tasks(1)
	if err != nil || len(ts) != 1 {
		t.Fatalf("tasks: %v %v", ts, err)
	}
	return ts[0].Kind, ts[0].Status, ts[0].Note, ts[0].Error
}

func TestTaskEndsWithNote(t *testing.T) {
	s := newEnv(t).srv
	r := httptest.NewRequest("POST", "/x", nil)
	if err := s.task(r, "demo", "vm", "n1", func(func(float64)) error { return taskDone("%s, %d vCPU", "web-1", 2) }); err != nil {
		t.Fatalf("a note is not an error: %v", err)
	}
	if _, status, note, errText := lastTask(t, s); status != "ok" || note != "web-1, 2 vCPU" || errText != "" {
		t.Errorf("status %q note %q error %q", status, note, errText)
	}
	// A failure stays a failure and keeps its text.
	if err := s.taskStop(r, "demo", "vm", "n1", taskStop{}, func(func(float64)) error { return errors.New("boom") }); err == nil || err.Error() != "boom" {
		t.Fatalf("err = %v", err)
	}
	if _, status, note, errText := lastTask(t, s); status != "failed" || note != "" || errText != "boom" {
		t.Errorf("status %q note %q error %q", status, note, errText)
	}
	// No note: ok with an empty Result.
	if err := s.task(r, "demo", "vm", "n1", func(func(float64)) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, status, note, _ := lastTask(t, s); status != "ok" || note != "" {
		t.Errorf("status %q note %q", status, note)
	}
}

func TestBackgroundTaskEndsWithNote(t *testing.T) {
	s := newEnv(t).srv
	r := httptest.NewRequest("POST", "/x", nil)
	_, done, err := s.startBackgroundStop(r, "demo", "vm", "n1", taskStop{}, func(func(float64)) error { return taskDone("to node-2, live") })
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a note is not an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the task did not end")
	}
	if _, status, note, _ := lastTask(t, s); status != "ok" || note != "to node-2, live" {
		t.Errorf("status %q note %q", status, note)
	}
}

func TestTaskStopAsync(t *testing.T) {
	s := newEnv(t).srv
	fn := func(func(float64)) error { return taskDone("web.qcow2, 1.5 GiB") }
	// Without ?async=1 the call waits and the result is the task's.
	if err := s.taskStopAsync(httptest.NewRequest("POST", "/x", nil), "demo", "vm", "n1", taskStop{}, fn); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, status, note, _ := lastTask(t, s); status != "ok" || note != "web.qcow2, 1.5 GiB" {
		t.Errorf("sync: status %q note %q", status, note)
	}
	// With it the answer is "accepted" at once, and the task ends in the background.
	err := s.taskStopAsync(httptest.NewRequest("POST", "/x?async=1", nil), "demo", "vm", "n1", taskStop{}, fn)
	var acc accepted
	if !errors.As(err, &acc) || acc.Task == 0 {
		t.Fatalf("async: %v", err)
	}
	w := httptest.NewRecorder()
	fail(w, err)
	if w.Code != http.StatusAccepted {
		t.Errorf("status %d, want 202", w.Code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, status, note, _ := lastTask(t, s); status == "ok" && note == "web.qcow2, 1.5 GiB" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the background task did not end with its note")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNotedAndDoneOr(t *testing.T) {
	boom := errors.New("boom")
	got := noted(func(func(float64)) error { return boom }, func() string { return "never" })(func(float64) {})
	if got != boom {
		t.Errorf("a failure passes through: %v", got)
	}
	n := ""
	run := noted(func(func(float64)) error { n = "late"; return nil }, func() string { return n })
	if err := run(func(float64) {}); err != doneNote("late") {
		t.Errorf("the note is built after the function ran: %v", err)
	}
	if err := noted(func(func(float64)) error { return nil }, func() string { return "" })(func(float64) {}); err != nil {
		t.Errorf("an empty note ends with nothing to say: %v", err)
	}
	if err := doneOr(boom, "x"); err != boom {
		t.Errorf("doneOr failure: %v", err)
	}
	if err := doneOr(nil, "%s", "x"); err != doneNote("x") {
		t.Errorf("doneOr: %v", err)
	}
}

func TestFmtBytesDropsZeroDecimal(t *testing.T) {
	for in, want := range map[uint64]string{
		0: "0 B", 512: "512 B", 1024: "1 KiB", 1536: "1.5 KiB", 4 << 30: "4 GiB", 3435973837: "3.2 GiB", 612 << 20: "612 MiB",
	} {
		if got := fmtBytes(in); got != want {
			t.Errorf("fmtBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestHardwareNotes(t *testing.T) {
	live, saved := hwResult{Applied: "live+config"}, hwResult{Applied: "config", NeedsRestart: true}
	for _, c := range []struct{ got, want string }{
		{vcpuNote(4, 0, live), "4 vCPU"},
		{vcpuNote(4, 8, saved), "4 vCPU, max 8, after restart"},
		{memNote(4096, 0, live), "4 GiB"},
		{memNote(1536, 8192, live), "1.5 GiB, max 8 GiB"},
		{diskAddNote("vdb", "fast", "", 20, live), "vdb, 20 GiB, pool fast"},
		{diskAddNote("vdb", "fast", "data.qcow2", 0, saved), "vdb, data.qcow2, pool fast, after restart"},
		{nicChangeNote("52:54:00:aa:bb:cc", "default", "", "down", true, live), "52:54:00:aa:bb:cc, network default, link down, speed limit"},
		{nicChangeNote("52:54:00:aa:bb:cc", "", "br0", "", false, live), "52:54:00:aa:bb:cc, bridge br0"},
		{nicSideFact("", "br0"), "bridge br0"},
		{mediaFact(""), "ejected"},
		{mediaFact("win.iso"), "win.iso"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
	if newVolumeGiB(nil) != 0 || newVolumeGiB(&newVolumeReq{SizeGiB: 7}) != 7 {
		t.Error("newVolumeGiB")
	}
	if volumeDeletedFact(true) != "volume deleted" || volumeDeletedFact(false) != "" {
		t.Error("volumeDeletedFact")
	}
}

func TestVMNotes(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{powerNote("start"), "running"},
		{powerNote("resume"), "running"},
		{powerNote("pause"), "paused"},
		{powerNote("poweroff"), "shut off, forced"},
		{powerNote("shutdown"), "shutdown sent"},
		{powerNote("reboot"), "reboot sent"},
		{powerNote("reset"), "reset"},
		{shutdownNote(shutdownOutcome{}), "already shut off"},
		{shutdownNote(shutdownOutcome{Method: "agent"}), "shut off, by the guest agent"},
		{shutdownNote(shutdownOutcome{Method: "acpi", Forced: true}), "shut off, forced after the wait"},
		{deleteNote("web-1", 0), "web-1"},
		{deleteNote("web-1", 1), "web-1, 1 disk deleted"},
		{deleteNote("web-1", 2), "web-1, 2 disks deleted"},
		{createVMNote(2, 2048, false), "2 vCPU, 2 GiB"},
		{createVMNote(2, 2048, true), "2 vCPU, 2 GiB, running"},
		{cloneNote("web-2", true, false), "web-2, linked"},
		{cloneNote("web-2", false, true), "web-2, full copy, running"},
		{tagsNote([]string{"prod", "db"}), "prod, db"},
		{tagsNote(nil), "no tags"},
		{templateNote(false, "linux"), "VM"},
		{templateNote(true, ""), "template"},
		{templateNote(true, "windows"), "template, sealed for windows"},
		{snapshotNote("before-upgrade", true), "before-upgrade, disk and memory"},
		{snapshotNote("before-upgrade", false), "before-upgrade, disk only"},
		{snapshotsDeletedNote([]string{"a", "b"}), "a, b"},
		{snapshotsDeletedNote([]string{"a", "b", "c", "d"}), "4 snapshots"},
		{scheduleNote(nil), "removed"},
		{scheduleNote(&snapsched.Schedule{Enabled: true, Mode: "interval", EveryMinutes: 60, Keep: 7}), "every 60 min, keep 7"},
		{scheduleNote(&snapsched.Schedule{Mode: "daily", At: "03:00", Keep: 3}), "daily 03:00 UTC, keep 3, off"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

func TestClockNotes(t *testing.T) {
	yes := true
	for _, c := range []struct{ got, want string }{
		{clockSyncNote(-3*time.Second, true, false, ""), "was 3 s behind"},
		{clockSyncNote(2*time.Hour, true, true, "start"), "was 2 h ahead, hardware clock not set, after start"},
		{clockSyncNote(300*time.Millisecond, true, false, ""), "was in sync"},
		{clockSyncNote(0, false, false, ""), "clock set"},
		{clockChangeNote("utc", &yes, hwResult{}), "offset utc, sync on"},
		{clockChangeNote("", &yes, hwResult{}), "sync on"},
		{clockChangeNote("localtime", nil, hwResult{NeedsRestart: true}), "offset localtime, after restart"},
		{durationText(45 * time.Second), "45 s"},
		{durationText(12 * time.Minute), "12 min"},
		{durationText(3 * time.Hour), "3 h"},
		{durationText(72 * time.Hour), "3 d"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

func TestStorageAndNetworkNotes(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{poolCreateNote(poolRequest{Type: "dir", Target: "/srv/pool"}), "dir, /srv/pool"},
		{poolCreateNote(poolRequest{Type: "netfs", SourceHost: "nas", SourcePath: "/export"}), "netfs, nas:/export"},
		{poolCreateNote(poolRequest{Type: "logical", SourceName: "vg0"}), "logical, vg0"},
		{poolCreateNote(poolRequest{XML: "<pool/>"}), "from XML"},
		{poolActionNote("start"), "active"},
		{poolActionNote("remove"), "removed, data kept"},
		{volumeCreateNote(10<<30, "qcow2"), "10 GiB qcow2"},
		{volumeCreateNote(10<<30, ""), "10 GiB"},
		{volumeActionNote("resize", "", 40<<30), "40 GiB"},
		{volumeActionNote("clone", "copy.qcow2", 0), "copy.qcow2"},
		{networkCreateNote(networkRequest{Mode: "nat", Address: "10.77.1.1/24"}), "nat, 10.77.1.1/24"},
		{networkCreateNote(networkRequest{XML: "<network/>"}), "from XML"},
		{networkActionNote("delete"), "deleted"},
		{reservationNote(dhcpHost{MAC: "52:54:00:00:00:01", IP: "10.77.1.5", Name: "web"}), "52:54:00:00:00:01, 10.77.1.5, web"},
		{nodeCountNote("added", 1), "added on 1 node"},
		{nodeCountNote("removed", 3), "removed on 3 nodes"},
		{fileFact("image.qcow2", 612<<20), "image.qcow2, 612 MiB"},
		{hostnetNote(nodeproto.NetRequest{Action: "create", Bridge: "br1"}), "create, br1, confirmed"},
		{tlsNote(migTLSInfo{Dir: "/etc/pki/qemu"}, "installed"), "nothing to change, /etc/pki/qemu"},
		{tlsNote(migTLSInfo{Dir: "/etc/pki/qemu", Changes: []string{"a", "b"}}, "installed"), "installed, 2 changes, /etc/pki/qemu"},
		{importOVANote([]string{"vm-disk1.qcow2"}), "vm-disk1.qcow2"},
		{importOVANote([]string{"vm-disk1.qcow2", "vm-disk2.qcow2"}), "vm-disk1.qcow2, 2 disks"},
		{replStopNote(true), "stopped, copy deleted"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

func TestTuningNotes(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{tuningNote(hugepagesFact(0)), "ordinary pages, at next start"},
		{hugepagesFact(2048), "2 MiB pages"},
		{numaFact("", ""), "policy removed"},
		{numaFact("strict", "0-1"), "strict, 0-1"},
		{cpuModelFact("custom", "Skylake", 0, 0, 0, false), "Skylake"},
		{cpuModelFact("host-model", "", 1, 4, 2, false), "host-model, 1 sockets, 4 cores, 2 threads"},
		{cpuModelFact("host-model", "", 0, 0, 0, true), "host-model, no topology"},
		{pinFact(false, 2, true), "2 vCPU pinned, emulator pinned"},
		{pinFact(true, 0, false), "old pins cleared"},
		{pinFact(false, 0, false), "no pins"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

func TestJobNotes(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{moveNote("fast", true), "to pool fast, live"},
		{moveNote("fast", false), "to pool fast, offline"},
		{migrateNote("node-2", "live", false), "to node-2, live"},
		{migrateNote("node-2", "offline", true), "to node-2, offline, encrypted"},
		{evacuateNote(0), "no VMs to move"},
		{evacuateNote(1), "1 VM moved"},
		{evacuateNote(5), "5 VMs moved"},
		{ctlBackupNote(5<<20, true), "5 MiB, encrypted"},
		{ctlBackupNote(5<<20, false), "5 MiB"},
		{backupFact("full", []bkDisk{{Size: 3 << 30}, {Size: 200 << 20}}), "full, 3.2 GiB"},
		{backupFact("incremental", nil), "incremental"},
		{backupDeleteNote("web-20260101", 0), "web-20260101"},
		{backupDeleteNote("web-20260101", 2), "web-20260101, 2 later backups"},
		{restoreNote(&restorePlan{name: "web", dst: &hostconn.Host{Name: "node-2"}, req: restoreReq{Start: true}}), "web, on node-2, running"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
	res := map[string]any{"dir": "web-20260101"}
	if got := backupImportNote(&res); got != "web-20260101" {
		t.Errorf("backupImportNote = %q", got)
	}
	if got := backupImportNote(nil); got != "" {
		t.Errorf("backupImportNote(nil) = %q", got)
	}
}

func TestRollNote(t *testing.T) {
	r := &rollRun{Nodes: []*rollNode{{Status: "done"}, {Status: "done"}, {Status: "current"}, {Status: "skipped"}}}
	if got := rollNote(r); got != "2 nodes updated, 1 already up to date, 1 left out" {
		t.Errorf("rollNote = %q", got)
	}
	if got := rollNote(&rollRun{Nodes: []*rollNode{{Status: "done"}}}); got != "1 node updated" {
		t.Errorf("rollNote = %q", got)
	}
}

func TestReplicateResult(t *testing.T) {
	boom := errors.New("boom")
	r := &replRun{Kind: "incremental", Sent: 100 << 20, dst: &hostconn.Host{Name: "node-2"}}
	if err := replicateResult(r, boom); err != boom {
		t.Errorf("a failure passes through: %v", err)
	}
	if err := replicateResult(r, nil); err != doneNote("incremental, 100 MiB, to node-2") {
		t.Errorf("good run: %v", err)
	}
	if err := replicateResult(r, doneNote(frozenText)); err != doneNote("incremental, 100 MiB, to node-2; "+frozenText) {
		t.Errorf("good run with the freeze note: %v", err)
	}
	r.Skipped = true
	if err := replicateResult(r, nil); err != doneNote("up to date") {
		t.Errorf("run with nothing new: %v", err)
	}
}

func TestPrepareNote(t *testing.T) {
	rows := []prepare.Row{{ID: "admin", Status: "ok"}, {ID: "do-a", Status: "ok", Detail: "Installed"}, {ID: "do-b", Status: "ok", Detail: "Already in place. x"}, {ID: "do-c", Status: "ok"}}
	if got := prepareNote(rows, false); got != "2 steps done" {
		t.Errorf("got %q", got)
	}
	if got := prepareNote(rows[2:3], false); got != "current" {
		t.Errorf("got %q", got)
	}
	if got := prepareNote([]prepare.Row{{ID: "x", Status: "warning"}, {ID: "y", Status: "info"}}, true); got != "1 warning" {
		t.Errorf("got %q", got)
	}
	if got := prepareNote(nil, true); got != "no warnings" {
		t.Errorf("got %q", got)
	}
}
