package api

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/hostconn"
	"virticle/internal/store"
)

// A journalled run as a failed migration leaves it, and the facts of a target and a source on which every clause
// of the rule holds. Each test changes one fact.
func leftRun(state string) store.MigRun {
	return store.MigRun{ID: 7, UUID: "6f1c1f2e-0d55-4b3a-9a4e-0123456789ab", Name: "web", Src: 1, Dst: 2, SrcName: "n3", DstName: "n4",
		State: state, Files: []store.MigFile{
			{Path: "/var/lib/libvirt/images/web.qcow2", Ident: "fh-1-aa", Size: 10, State: store.MigFilePresent},
			{Path: "/var/lib/libvirt/images/web-seed.iso", Ident: "fh-1-bb", Size: 1, State: store.MigFilePresent},
		}}
}

func goodFacts() runFacts {
	return runFacts{OnTarget: vmAbsent, OnSource: vmRunning, SrcState: "running", Files: map[string]fileFacts{
		"/var/lib/libvirt/images/web.qcow2": {
			Now:    hostconn.FileID{Exists: true, Regular: true, ID: "fh-1-aa"},
			SrcNow: hostconn.FileID{Exists: true, Regular: true, ID: "fh-1-11"},
		},
		"/var/lib/libvirt/images/web-seed.iso": {
			Now:    hostconn.FileID{Exists: true, Regular: true, ID: "fh-1-bb"},
			SrcNow: hostconn.FileID{Exists: true, Regular: true, ID: "fh-1-22"},
		},
	}}
}

func file0(run store.MigRun) store.MigFile { return run.Files[0] }

func TestRuleRemovesOnlyWhenEveryClauseHolds(t *testing.T) {
	run := leftRun(store.MigFailed)
	v := judgeRun(run, goodFacts(), cleanAuto)
	if !v.Go || v.State != "" {
		t.Fatalf("all clauses hold: %+v", v)
	}
	for _, f := range run.Files {
		if a, why := judgeFile(f, goodFacts().Files[f.Path]); a != fileRemove {
			t.Errorf("%s: %s %s", f.Path, a, why)
		}
	}
	// A run cut short before the hand-over (the controller stopped during the copy) is as good as failed.
	if v := judgeRun(leftRun(store.MigPrepare), goodFacts(), cleanAuto); !v.Go {
		t.Errorf("prepare: %+v", v)
	}
}

func TestRuleFileReplacedByAnotherWithTheSameName(t *testing.T) {
	run := leftRun(store.MigFailed)
	ff := goodFacts().Files[file0(run).Path]
	ff.Now.ID = "fh-1-cc" // deleted and made again, or replaced by hand
	a, why := judgeFile(file0(run), ff)
	if a != fileKeepFor || !strings.Contains(why, "another file") {
		t.Errorf("replaced file: %s %q", a, why)
	}
	ff.Now.ID = "" // a file system without identity: cannot be proved the same
	if a, _ := judgeFile(file0(run), ff); a != fileKeepFor {
		t.Errorf("no identity now: %s", a)
	}
	ff = goodFacts().Files[file0(run).Path]
	ff.Now.Regular = false
	if a, _ := judgeFile(file0(run), ff); a != fileKeepFor {
		t.Errorf("not a regular file: %s", a)
	}
	f := file0(run)
	f.Ident = "" // the making was not confirmed
	if a, _ := judgeFile(f, goodFacts().Files[f.Path]); a != fileKeepFor {
		t.Errorf("unconfirmed file: %s", a)
	}
	f = file0(run)
	f.State = store.MigFileKept
	if a, _ := judgeFile(f, goodFacts().Files[f.Path]); a != fileKeepFor {
		t.Errorf("kept file: %s", a)
	}
	ff = goodFacts().Files[file0(run).Path]
	ff.Now.Exists = false
	if a, _ := judgeFile(file0(run), ff); a != fileGone {
		t.Errorf("gone file: %s", a)
	}
}

func TestRuleVMActiveOnTargetWhileTheLookupFirstErrs(t *testing.T) {
	run := leftRun(store.MigFailed)
	f := goodFacts()
	f.TargetErr = "connection reset"
	f.OnTarget = vmUnknown
	if v := judgeRun(run, f, cleanAuto); v.Go {
		t.Fatalf("target unreadable: %+v", v)
	}
	if v := judgeRun(run, f, cleanByPerson); v.Go {
		t.Fatalf("target unreadable, by a person: %+v", v)
	}
	f = goodFacts()
	f.OnTarget = vmRunning // the connection is back: the VM runs there
	v := judgeRun(run, f, cleanAuto)
	if v.Go || v.State != store.MigAttention || !strings.Contains(v.Note, "running on n4") {
		t.Fatalf("running on target: %+v", v)
	}
	if v := judgeRun(run, f, cleanByPerson); v.Go {
		t.Errorf("a person cannot remove the files of a VM that runs on the target: %+v", v)
	}
	f.OnTarget = vmDefined
	if v := judgeRun(run, f, cleanByPerson); v.Go || v.State != store.MigAttention {
		t.Errorf("defined on target: %+v", v)
	}
	f.OnTarget = vmTransient
	if v := judgeRun(run, f, cleanAuto); v.Go {
		t.Errorf("an unfinished incoming copy: %+v", v)
	}

	// After a lost answer of Finish, the source is told nothing until the target answers clearly.
	var told []bool
	asks := []vmPresence{vmUnknown, vmUnknown, vmRunning}
	i := 0
	hand := settleLoop(func() (vmPresence, *hostconn.Host) {
		p := asks[i]
		if i < len(asks)-1 {
			i++
		}
		return p, nil
	}, func(*hostconn.Host) {}, func(moved bool) { told = append(told, moved) }, time.Second, time.Millisecond)
	if hand != handDone || len(told) != 1 || !told[0] {
		t.Errorf("settle after errors then running: %v %v", hand, told)
	}
	told = nil
	hand = settleLoop(func() (vmPresence, *hostconn.Host) { return vmUnknown, nil }, func(*hostconn.Host) {},
		func(moved bool) { told = append(told, moved) }, 5*time.Millisecond, time.Millisecond)
	if hand != handUnknown || len(told) != 0 {
		t.Errorf("no answer: the source must not be told anything: %v %v", hand, told)
	}
	asks, i, told = []vmPresence{vmTransient, vmAbsent}, 0, nil
	discarded := 0
	hand = settleLoop(func() (vmPresence, *hostconn.Host) {
		p := asks[i]
		if i < len(asks)-1 {
			i++
		}
		return p, nil
	}, func(*hostconn.Host) { discarded++ }, func(moved bool) { told = append(told, moved) }, time.Second, time.Millisecond)
	if hand != handNone || discarded != 1 || len(told) != 1 || told[0] {
		t.Errorf("unfinished copy, then gone: %v %d %v", hand, discarded, told)
	}

	// Only an answer of libvirt says what the node did; a lost connection does not.
	if !libvirtAnswered(libvirt.Error{Code: 1}) || libvirtAnswered(errors.New("connection reset by peer")) {
		t.Error("libvirtAnswered")
	}
}

func TestRuleSharedStorageSeenFromBothNodes(t *testing.T) {
	run := leftRun(store.MigFailed)
	ff := goodFacts().Files[file0(run).Path]
	ff.SrcNow.ID = ff.Now.ID // the same file through a share the planner did not know of
	if a, why := judgeFile(file0(run), ff); a != fileKeep || !strings.Contains(why, "source's own file") {
		t.Errorf("same file on both nodes: %s %q", a, why)
	}
	ff.SrcNow.ID = "" // cannot be told apart
	if a, _ := judgeFile(file0(run), ff); a != fileKeep {
		t.Errorf("source file without identity: %s", a)
	}
	ff = goodFacts().Files[file0(run).Path]
	ff.Shared = "on network storage (pool nfs) that n3 mounts at the same place"
	if a, _ := judgeFile(file0(run), ff); a != fileKeep {
		t.Errorf("shared pool: %s", a)
	}

	src := map[string]poolDesc{"/nfs": {Name: "nfs", Type: "netfs", Server: "a"}, "/var/lib/libvirt/images": {Name: "default", Type: "dir"}}
	dst := map[string]poolDesc{"/nfs": {Name: "nfs", Type: "netfs", Server: "b"}, "/var/lib/libvirt/images": {Name: "default", Type: "dir"}, "/local": {Name: "l", Type: "dir"}}
	got := sharedReasons("n3", "n4", src, dst, []string{"/nfs", "/var/lib/libvirt/images", "/local", "/elsewhere", nvramDir}, nil)
	if got["/nfs"] == "" || got["/elsewhere"] == "" {
		t.Errorf("network storage on both nodes, or no pool, must count as shared: %v", got)
	}
	if got["/var/lib/libvirt/images"] != "" || got["/local"] != "" || got[nvramDir] != "" {
		t.Errorf("local storage counted as shared: %v", got)
	}

	// Two shares proven different (another export at the same place) are not shared; servers that cannot be told
	// apart are.
	src["/nfs"] = poolDesc{Name: "nfs", Type: "netfs", Server: "s", Export: "/srv/a"}
	dst["/nfs"] = poolDesc{Name: "nfs", Type: "netfs", Server: "s", Export: "/srv/b"}
	byExport := func(s, d poolDesc) bool { return s.Export != d.Export }
	if got := sharedReasons("n3", "n4", src, dst, []string{"/nfs"}, byExport); got["/nfs"] != "" {
		t.Errorf("different exports counted as shared: %v", got)
	}
	dst["/nfs"] = poolDesc{Name: "nfs", Type: "netfs", Server: "s2", Export: "/srv/a"}
	if got := sharedReasons("n3", "n4", src, dst, []string{"/nfs"}, byExport); got["/nfs"] == "" {
		t.Errorf("the same export of a server that may be the same: %v", got)
	}
}

func TestRuleControllerRestartBetweenFailureAndCleanup(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	run := leftRun(store.MigPrepare)
	id, err := st.AddMigRun(run)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range run.Files {
		if err := st.AddMigFile(id, f); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	st, err = store.Open(filepath.Join(dir, "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	runs, err := st.MigRuns()
	if err != nil || len(runs) != 1 || len(runs[0].Files) != 2 || runs[0].Files[0].Ident != "fh-1-aa" || runs[0].State != store.MigPrepare {
		t.Fatalf("journal after a restart: %+v %v", runs, err)
	}
	// Cut short during the copy: the new process may clean up by itself.
	if v := judgeRun(runs[0], goodFacts(), cleanAuto); !v.Go {
		t.Errorf("prepare after restart: %+v", v)
	}
	// Cut short during the hand-over: its outcome was never seen, so nothing goes by itself.
	runs[0].State = store.MigSwitching
	v := judgeRun(runs[0], goodFacts(), cleanAuto)
	if v.Go || v.State != store.MigAttention || !v.Unsure {
		t.Errorf("switching after restart: %+v", v)
	}
	// While the migration of this process still holds it, nobody else touches it.
	f := goodFacts()
	f.Held = true
	if v := judgeRun(leftRun(store.MigFailed), f, cleanByPerson); v.Go {
		t.Errorf("held run: %+v", v)
	}
	if err := st.SetMigState(id, store.MigAttention, true, "x"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMigState(id, store.MigFailed, false, "y"); err != nil {
		t.Fatal(err)
	}
	if r, _ := st.MigRun(id); !r.Unsure {
		t.Error("unsure must never be cleared")
	}
	st.DeleteMigRun(id)
	if runs, _ := st.MigRuns(); len(runs) != 0 {
		t.Errorf("kept: %+v", runs)
	}
}

func TestRuleTargetOfflineThenOnline(t *testing.T) {
	run := leftRun(store.MigFailed)
	f := runFacts{TargetErr: "it is not connected"}
	if v := judgeRun(run, f, cleanAuto); v.Go || v.State != "" || !strings.Contains(v.Note, "n4 cannot be checked") {
		t.Errorf("offline: %+v", v)
	}
	f = goodFacts()
	f.SourceErr = "it is not connected"
	if v := judgeRun(run, f, cleanAuto); v.Go {
		t.Errorf("source offline: %+v", v)
	}
	if v := judgeRun(run, goodFacts(), cleanAuto); !v.Go {
		t.Errorf("back online: %+v", v)
	}
}

func TestRuleBackingChainOnTheTarget(t *testing.T) {
	live := `<domain><name>other</name><devices>
	  <disk type='file' device='disk'><driver type='qcow2'/><source file='/var/lib/libvirt/images/other.qcow2'/>
	    <backingStore type='file'><source file='/var/lib/libvirt/images/mid.qcow2'/>
	      <backingStore type='file'><source file='/var/lib/libvirt/images/web.qcow2'/><backingStore/></backingStore>
	    </backingStore>
	  </disk>
	  <disk type='volume' device='disk'><source pool='p' volume='v'/></disk>
	</devices><os><nvram>/var/lib/libvirt/qemu/nvram/other_VARS.fd</nvram></os></domain>`
	df, err := parseDomFiles(live)
	if err != nil {
		t.Fatal(err)
	}
	if len(df.Disks) != 2 || df.NVRAM != "/var/lib/libvirt/qemu/nvram/other_VARS.fd" {
		t.Fatalf("%+v", df)
	}
	if b := df.Disks[0].Backing; len(b) != 2 || b[1] != "/var/lib/libvirt/images/web.qcow2" {
		t.Errorf("backing chain %v", b)
	}
	if df.Disks[1].Pool != "p" || df.Disks[1].Volume != "v" {
		t.Errorf("volume disk %+v", df.Disks[1])
	}
	run := leftRun(store.MigFailed)
	ff := goodFacts().Files[file0(run).Path]
	ff.UsedBy = "VM other (backing chain)"
	if a, why := judgeFile(file0(run), ff); a != fileKeep || !strings.Contains(why, "VM other") {
		t.Errorf("in a backing chain: %s %q", a, why)
	}
	ff = goodFacts().Files[file0(run).Path]
	ff.Claim = "the migration of db that is running"
	if a, _ := judgeFile(file0(run), ff); a != fileKeep {
		t.Errorf("claimed: %s", a)
	}
}

func TestRuleVMGoneEverywhere(t *testing.T) {
	run := leftRun(store.MigFailed)
	f := goodFacts()
	f.OnSource = vmAbsent
	v := judgeRun(run, f, cleanByPerson)
	if v.Go || v.State != store.MigAttention || !strings.Contains(v.Note, "neither") {
		t.Errorf("VM nowhere: %+v", v)
	}
	f = goodFacts()
	f.SrcMiss = []string{"/var/lib/libvirt/images/web.qcow2"}
	if v := judgeRun(run, f, cleanByPerson); v.Go || v.State != store.MigAttention {
		t.Errorf("source disk missing: %+v", v)
	}
	f = goodFacts()
	f.OnSource = vmTransient
	if v := judgeRun(run, f, cleanByPerson); v.Go {
		t.Errorf("source VM not defined: %+v", v)
	}
	f = goodFacts()
	f.OnSource = vmUnknown
	if v := judgeRun(run, f, cleanByPerson); v.Go {
		t.Errorf("source unknown: %+v", v)
	}
	f = goodFacts()
	f.Other = "another migration of web is running"
	if v := judgeRun(run, f, cleanByPerson); v.Go {
		t.Errorf("another migration: %+v", v)
	}
}

func TestRuleAttentionNeedsAPerson(t *testing.T) {
	run := leftRun(store.MigAttention)
	run.Unsure = true
	if v := judgeRun(run, goodFacts(), cleanAuto); v.Go || !strings.Contains(v.Note, "needs attention") {
		t.Errorf("automatic: %+v", v)
	}
	if v := judgeRun(run, goodFacts(), cleanByPerson); !v.Go {
		t.Errorf("a person, the VM confirmed on the source only: %+v", v)
	}
	run = leftRun(store.MigFailed)
	run.Unsure = true
	if v := judgeRun(run, goodFacts(), cleanAuto); v.Go {
		t.Errorf("unsure failed run, automatic: %+v", v)
	}
}

func TestLeftoverAlert(t *testing.T) {
	a := leftoverAlert(alertLeftover{Run: 7, Name: "web", DstName: "n4", State: store.MigFailed,
		Files: []store.MigFile{{Path: "/p/web.qcow2", Size: 3 << 30, Reason: "n4 cannot be checked: it is not connected"}}})
	if a.ID != "migleft:7" || a.Kind != "migration-leftover" || a.Title != "Files of a failed migration of web are left on n4" ||
		!strings.Contains(a.Message, "/p/web.qcow2 (3 GiB)") || a.Node != 0 {
		t.Errorf("%+v", a)
	}
	a = leftoverAlert(alertLeftover{Run: 8, Name: "web", DstName: "n4", State: store.MigAttention, Unsure: true, Note: "web is running on n4"})
	if a.Severity != "critical" || !strings.Contains(a.Title, "needs attention") || !strings.Contains(a.Message, "running on n4") {
		t.Errorf("%+v", a)
	}
	tr := newAlertTracker(alertConfig{})
	raised, _, _ := tr.step(alertInput{Fresh: map[int64]bool{}, Leftovers: []alertLeftover{{Run: 7, Name: "web", DstName: "n4", State: store.MigFailed}}}, time.Now())
	if len(raised) != 1 || raised[0].ID != "migleft:7" {
		t.Errorf("raised %+v", raised)
	}
	_, cleared, _ := tr.step(alertInput{Fresh: map[int64]bool{}}, time.Now())
	if len(cleared) != 1 {
		t.Errorf("not cleared when the files are gone: %+v", cleared)
	}
}

// A run that needs attention says how to settle it by hand: what to check on the target, then resume on the source.
func TestAttentionAlertHelp(t *testing.T) {
	a := leftoverAlert(alertLeftover{Run: 9, Name: "web", SrcName: "n1", DstName: "n2", State: store.MigAttention, Unsure: true, Waiting: true,
		Note: "the connection to n2 was lost at the end of the migration"})
	for _, w := range []string{"n1 keeps web paused until n2 answers", "on n2, run virsh list --all", "resume it on n1", "never resume that copy"} {
		if !strings.Contains(a.Help, w) {
			t.Errorf("help lacks %q: %s", w, a.Help)
		}
	}
	if a := leftoverAlert(alertLeftover{Run: 9, Name: "web", SrcName: "n1", DstName: "n2", State: store.MigFailed}); a.Help != "" {
		t.Errorf("a plain leftover has help: %s", a.Help)
	}
}
