package api

// The journal of a migration and the cleanup after a failed one.
//
// A migration makes files on its target: copies of backing images, snapshot files and the cloud-init seed, the
// empty images libvirt mirrors the disks into, and a UEFI variable store. When the migration fails these copies
// must go, and nothing else may. The rules:
//
//   - A file is made only where nothing exists (the node command creates exclusively and never replaces), and it
//     is recorded as the run's only after it was made, with the identity the node command reported for the file
//     it made (internal/nodecmd/verbs_fileid.go). A file whose making cannot be confirmed is recorded without an
//     identity, and such a file is never removed.
//   - The run is written to the controller's database before the first file is made (store.MigRun), and the
//     hand-over to the target is recorded before it starts (MigSwitching). Cleanup works from these records only,
//     so it also runs after a restart of the controller and when the target comes back.
//   - A recorded file is removed only when every clause of judgeRun and judgeFile holds at that moment, each
//     checked on the nodes, each failing closed: what cannot be checked is kept. The removal itself is atomic on
//     the node (remove-id): the file is renamed away, its identity checked on what was renamed, and it is removed
//     or put back; a file that a process (QEMU) holds a lock on is put back too.
//   - A run whose hand-over outcome the controller did not see (unsure), or where the VM was found on the target,
//     needs attention: nothing is removed by itself, and an administrator decides (Remove applies the same rule,
//     Keep forgets the records and leaves the files as ordinary volumes).
//
// Cleanup is tried when the run fails, when either node comes online, and on the alert pass (at most every
// migRetryGap per run). Every attempt that removes a file, or keeps one for a new reason, runs as a task whose
// operation trace holds the node calls and whose Result lists what was removed and what was kept and why.

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
	"virticle/internal/store"
)

// migRetryGap is the shortest time between two automatic attempts for one run from the alert pass. A node that
// comes online is tried at once.
const migRetryGap = 5 * time.Minute

// nvramDir is where libvirt keeps UEFI variable stores; it is not a storage pool but it is local to the node.
const nvramDir = "/var/lib/libvirt/qemu/nvram"

// migJournal is what this process knows about the runs it is working on and the attempts it made.
var migJournal = struct {
	sync.Mutex
	held  map[int64]bool      // runs a migration of this process is still working on
	busy  map[int64]bool      // runs a cleanup is being tried for now
	tried map[int64]time.Time // the last automatic attempt per run
	// confirm holds the source's end of a hand-over whose outcome was not known: called with true once the VM is
	// confirmed running on the target, with false once it is confirmed not there (the source resumes it).
	confirm map[int64]func(moved bool) error
}{held: map[int64]bool{}, busy: map[int64]bool{}, tried: map[int64]time.Time{}, confirm: map[int64]func(bool) error{}}

func migHeld(id int64) bool {
	migJournal.Lock()
	defer migJournal.Unlock()
	return migJournal.held[id]
}

// --- recording, by the migration itself ---

// journalStart records the run before it makes anything. Without the record the migration does not start: a file
// it made could not be cleaned up after a restart.
func (m *migRun) journalStart() error {
	if m.s == nil || m.s.Store == nil {
		return nil
	}
	var src []string
	for _, f := range m.plan.Files.list {
		src = append(src, f.Path)
	}
	if m.plan.Dom.NVRAM != "" {
		src = append(src, m.plan.Dom.NVRAM)
	}
	id, err := m.s.Store.AddMigRun(store.MigRun{UUID: m.plan.UUID, Name: m.plan.Name, Src: m.srcID, Dst: m.req.Target,
		SrcName: m.src.Name, DstName: m.dst.Name, Mode: m.plan.Mode, State: store.MigPrepare, SrcFiles: src, Owner: workOwner})
	if err != nil {
		return fmt.Errorf("cannot record the migration in the database, so it is not started: %w", err)
	}
	m.jid = id
	migJournal.Lock()
	migJournal.held[id] = true
	migJournal.Unlock()
	return nil
}

// journalEnd lets other cleanups look at the run again.
func (m *migRun) journalEnd() {
	if m.jid == 0 {
		return
	}
	migJournal.Lock()
	delete(migJournal.held, m.jid)
	migJournal.Unlock()
}

// recordFile adds a file the run made to the journal. id is the identity the node gave for it ("" when its file
// system gives none: the file is then never removed by the controller). An error stops the migration.
func (m *migRun) recordFile(p, id string, size uint64) error {
	m.created = append(m.created, store.MigFile{Path: p, Ident: id, Size: size, State: store.MigFilePresent})
	if m.jid == 0 {
		return nil
	}
	if err := m.s.Store.AddMigFile(m.jid, store.MigFile{Path: p, Ident: id, Size: size}); err != nil {
		return fmt.Errorf("cannot record %s on %s in the database: %w", p, m.dst.Name, err)
	}
	return nil
}

// recordPending records, before a file is made, that it is about to be made, without an identity. If the
// controller stops before the answer comes, the entry stays: it is never removed by the rule (no identity), but it
// is reported, and it is forgotten when no file of that name turns up.
func (m *migRun) recordPending(p string) error {
	if m.jid == 0 {
		return nil
	}
	if err := m.s.Store.AddMigFile(m.jid, store.MigFile{Path: p, Reason: "being made"}); err != nil {
		return fmt.Errorf("cannot record %s on %s in the database: %w", p, m.dst.Name, err)
	}
	return nil
}

// recordUnsure settles the entry of a file whose making failed. A clear answer that the name was taken already
// (by another file), or no file under the name, forgets the entry. Otherwise the file may exist because of this run
// although its making was not confirmed (the answer was lost): it stays recorded without an identity, so it is
// never removed, only reported.
func (m *migRun) recordUnsure(p string, cause error) {
	forget := cause != nil && (strings.Contains(cause.Error(), "already exists") || strings.Contains(cause.Error(), "appeared meanwhile"))
	if !forget {
		if st, err := m.dst.Stat(p); err == nil && len(st) == 1 && !st[0].Exists {
			forget = true // nothing was made
		}
	}
	if forget {
		if m.jid != 0 {
			m.s.Store.DeleteMigFile(m.jid, p)
		}
		return
	}
	m.created = append(m.created, store.MigFile{Path: p, State: store.MigFilePresent})
	if m.jid != 0 {
		if err := m.s.Store.AddMigFile(m.jid, store.MigFile{Path: p, Reason: "not confirmed as made by this migration"}); err != nil {
			log.Printf("migrate %s: recording %s: %v", m.plan.Name, p, err)
		}
	}
}

// setState records the state of the run. Before the hand-over a failure to record stops the migration.
func (m *migRun) setState(state string, unsure bool, note string) error {
	if m.jid == 0 {
		return nil
	}
	if err := m.s.Store.SetMigState(m.jid, state, unsure, note); err != nil {
		log.Printf("migrate %s: recording the state %s: %v", m.plan.Name, state, err)
		return fmt.Errorf("cannot record the migration's state in the database: %w", err)
	}
	return nil
}

// moved ends the journal of a run that succeeded: its files on the target are the VM's now.
func (m *migRun) moved() {
	if m.jid == 0 {
		return
	}
	if err := m.s.Store.DeleteMigRun(m.jid); err != nil {
		log.Printf("migrate %s: forgetting the journal: %v", m.plan.Name, err)
	}
}

// failedConfirmed records that the VM stayed on the source and removes what the rule allows now. The note says
// what was left on the target and why.
func (m *migRun) failedConfirmed(why error) string {
	note := ""
	if why != nil {
		note = firstLine(why.Error())
	}
	if m.jid == 0 {
		return "files left on " + m.dst.Name + ": " + strings.Join(pathsOf(m.created), ", ")
	}
	m.setState(store.MigFailed, false, note)
	res := m.s.cleanMigRun(m.jid, cleanByRun, m.src, m.dst, false)
	return res.leftNote()
}

// attention records a run whose outcome is not known or whose VM is not where it should be. Nothing is removed.
func (m *migRun) attention(note string) string {
	m.setState(store.MigAttention, true, note)
	if m.jid != 0 {
		m.s.pokeAlerts()
	}
	if len(m.created) == 0 {
		return ""
	}
	return "nothing was removed on either node; the files on " + m.dst.Name + " are kept until an administrator decides (Alerts)"
}

func pathsOf(fs []store.MigFile) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Path
	}
	return out
}

// --- the facts and the rule ---

// vmPresence is what a node says about a VM.
type vmPresence int

const (
	vmUnknown   vmPresence = iota // the node could not be asked, or did not answer clearly
	vmAbsent                      // no domain with this UUID (a replica of it does not count)
	vmDefined                     // defined, not running
	vmRunning                     // defined and active (running or paused)
	vmTransient                   // active but not defined: an incoming migration that never finished
)

func (p vmPresence) String() string {
	return [...]string{"unknown", "absent", "defined", "running", "transient"}[p]
}

// runFacts is what one attempt found out on both nodes. An error text means that part could not be checked.
type runFacts struct {
	Held      bool   // a migration of this process is working on the run
	Other     string // another migration of the same VM is running now
	TargetErr string
	SourceErr string
	OnTarget  vmPresence
	OnSource  vmPresence
	SrcState  string   // the VM's state on the source, for messages
	SrcMiss   []string // disk files of the VM's definition on the source that do not exist
	Files     map[string]fileFacts
}

// fileFacts is what was found for one journalled path.
type fileFacts struct {
	Now    hostconn.FileID // the file at the path on the target now
	SrcNow hostconn.FileID // the file at the same path on the source
	UsedBy string          // a domain, snapshot or volume on the target that refers to the path
	Claim  string          // another journalled run or a migration in flight that claims the path
	Shared string          // why the storage at the path may be shared with the source ("": it is not)
}

// runVerdict decides for the whole run.
type runVerdict struct {
	Go     bool   // the file rule may remove files
	State  string // the run's new state, "" for unchanged
	Unsure bool
	Note   string // why nothing is removed, or where the VM is
}

// Cleanup modes: who asks.
const (
	cleanByRun    = "run"    // the migration that just failed, with a confirmed outcome
	cleanAuto     = "auto"   // a node came online, or the alert pass
	cleanByPerson = "manual" // an administrator pressed Remove
)

// judgeRun is the part of the rule that holds for the run as a whole. Every clause fails closed.
func judgeRun(run store.MigRun, f runFacts, mode string) runVerdict {
	v := runVerdict{}
	state, unsure := run.State, run.Unsure
	if f.Held && mode != cleanByRun {
		v.Note = "the migration is still running"
		return v
	}
	if f.Other != "" {
		v.Note = f.Other
		return v
	}
	if state == store.MigSwitching && mode != cleanByRun {
		// The process that started the hand-over is gone: its outcome was never seen.
		state, unsure = store.MigAttention, true
		v.State, v.Unsure = state, true
		v.Note = "the hand-over to " + run.DstName + " was cut short (the controller stopped); its outcome was not seen"
	}
	if f.TargetErr != "" {
		v.Note = joinNote(v.Note, run.DstName+" cannot be checked: "+f.TargetErr)
		return v
	}
	if f.SourceErr != "" {
		v.Note = joinNote(v.Note, run.SrcName+" cannot be checked: "+f.SourceErr)
		return v
	}
	switch f.OnTarget {
	case vmRunning, vmDefined:
		v.State, v.Unsure = store.MigAttention, unsure
		where := "running"
		if f.OnTarget == vmDefined {
			where = "defined"
		}
		v.Note = fmt.Sprintf("%s is %s on %s", run.Name, where, run.DstName)
		if f.OnSource != vmAbsent {
			v.Note += fmt.Sprintf(" and %s on %s", orUnknown(f.SrcState), run.SrcName)
		}
		return v
	case vmTransient:
		v.Note = joinNote(v.Note, "an unfinished incoming copy of "+run.Name+" is on "+run.DstName)
		return v
	case vmUnknown:
		v.Note = joinNote(v.Note, "cannot tell whether "+run.Name+" is on "+run.DstName)
		return v
	}
	switch f.OnSource {
	case vmAbsent:
		v.State, v.Unsure = store.MigAttention, unsure
		v.Note = fmt.Sprintf("%s exists on neither %s nor %s", run.Name, run.SrcName, run.DstName)
		return v
	case vmTransient:
		v.State, v.Unsure = store.MigAttention, unsure
		v.Note = fmt.Sprintf("%s runs on %s but is not defined there", run.Name, run.SrcName)
		return v
	case vmUnknown:
		v.Note = joinNote(v.Note, "cannot tell whether "+run.Name+" is on "+run.SrcName)
		return v
	}
	if len(f.SrcMiss) > 0 {
		v.State, v.Unsure = store.MigAttention, unsure
		v.Note = fmt.Sprintf("%s is on %s, but these of its disk files are missing there: %s", run.Name, run.SrcName, strings.Join(f.SrcMiss, ", "))
		return v
	}
	here := fmt.Sprintf("%s is %s on %s and not on %s", run.Name, orUnknown(f.SrcState), run.SrcName, run.DstName)
	if state == store.MigAttention || unsure {
		if mode != cleanByPerson {
			v.Note = joinNote(v.Note, "needs attention: "+here)
			return v
		}
		v.Go, v.Note = true, here
		return v
	}
	if state == store.MigPrepare || state == store.MigFailed {
		v.Go, v.Note = true, here
		return v
	}
	v.Note = "unknown state " + state
	return v
}

func orUnknown(s string) string {
	if s == "" {
		return "present"
	}
	return s
}

func joinNote(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}

// File actions.
const (
	fileKeep    = "keep"    // left in place, look again later
	fileKeepFor = "keepfor" // left in place for good: it is not the run's file (any more)
	fileRemove  = "remove"  // remove it (remove-id checks the identity again, atomically)
	fileGone    = "gone"    // nothing is there any more: forget it
)

// judgeFile is the part of the rule for one file, once judgeRun allowed removals. Every clause fails closed.
func judgeFile(f store.MigFile, ff fileFacts) (action, reason string) {
	switch {
	case f.State == store.MigFileKept:
		return fileKeepFor, f.Reason
	case !ff.Now.Exists:
		return fileGone, ""
	case f.Ident == "":
		return fileKeepFor, "not confirmed as made by this migration"
	case !ff.Now.Regular:
		return fileKeepFor, "another kind of file has this name now"
	case ff.Now.ID == "" || ff.Now.ID != f.Ident:
		return fileKeepFor, "another file has this name now (replaced since the migration made it)"
	case ff.UsedBy != "":
		return fileKeep, "used by " + ff.UsedBy
	case ff.Claim != "":
		return fileKeep, "claimed by " + ff.Claim
	case ff.Shared != "":
		return fileKeep, ff.Shared
	case ff.SrcNow.Exists && (ff.SrcNow.ID == "" || ff.SrcNow.ID == ff.Now.ID):
		// The same file seen from both nodes (a share the planner did not know of), or a source file whose
		// identity cannot be compared.
		if ff.SrcNow.ID == "" {
			return fileKeep, "the file of the same name on the source cannot be told apart from it"
		}
		return fileKeep, "it is the source's own file, seen through shared storage"
	}
	return fileRemove, ""
}

// --- gathering the facts ---

func isNoDomain(err error) bool {
	var lv libvirt.Error
	return errors.As(err, &lv) && lv.Code == uint32(libvirt.ErrNoDomain)
}

// libvirtAnswered says that err is an answer of libvirt (it refused or failed the call), as opposed to a lost
// connection, after which nobody knows what the node did.
func libvirtAnswered(err error) bool {
	var lv libvirt.Error
	return errors.As(err, &lv)
}

// whereVM says whether the VM with this UUID is on the node. A replica of it is not the VM.
func whereVM(h *hostconn.Host, uuid libvirt.UUID) (vmPresence, string, libvirt.Domain, error) {
	d, err := h.DomainLookupByUUID(uuid)
	if err != nil {
		if isNoDomain(err) {
			return vmAbsent, "", d, nil
		}
		return vmUnknown, "", d, err
	}
	if isReplicaDomain(h, d) {
		return vmAbsent, "", d, nil
	}
	state, _, err := h.DomainGetState(d, 0)
	if err != nil {
		if isNoDomain(err) {
			return vmAbsent, "", d, nil
		}
		return vmUnknown, "", d, err
	}
	pers, err := h.DomainIsPersistent(d)
	if err != nil {
		return vmUnknown, "", d, err
	}
	name := stateName(uint8(state))
	switch {
	case libvirt.DomainState(state) == libvirt.DomainShutoff:
		return vmDefined, name, d, nil
	case pers == 0:
		return vmTransient, name, d, nil
	}
	return vmRunning, name, d, nil
}

// domFiles are the files a domain definition names: every disk source, the backing chain libvirt shows, the UEFI
// variable store and the TPM state directory is left out (it is not a pool file).
type domFiles struct {
	Disks []domDisk
	NVRAM string
}

type domDisk struct {
	Type, Device, Format, Path string
	Pool, Volume               string // type volume
	Backing                    []string
}

func parseDomFiles(raw string) (domFiles, error) {
	type src struct {
		File   string `xml:"file,attr"`
		Dev    string `xml:"dev,attr"`
		Pool   string `xml:"pool,attr"`
		Volume string `xml:"volume,attr"`
	}
	type backing struct {
		Source  src      `xml:"source"`
		Backing *backing `xml:"backingStore"`
	}
	var x struct {
		OS struct {
			NVRAM string `xml:"nvram"`
		} `xml:"os"`
		Devices struct {
			Disks []struct {
				Type   string `xml:"type,attr"`
				Device string `xml:"device,attr"`
				Driver struct {
					Type string `xml:"type,attr"`
				} `xml:"driver"`
				Source  src      `xml:"source"`
				Backing *backing `xml:"backingStore"`
			} `xml:"disk"`
		} `xml:"devices"`
	}
	if err := xml.Unmarshal([]byte(raw), &x); err != nil {
		return domFiles{}, err
	}
	out := domFiles{NVRAM: strings.TrimSpace(x.OS.NVRAM)}
	for _, d := range x.Devices.Disks {
		dd := domDisk{Type: d.Type, Device: d.Device, Format: d.Driver.Type, Path: d.Source.File, Pool: d.Source.Pool, Volume: d.Source.Volume}
		if dd.Path == "" {
			dd.Path = d.Source.Dev
		}
		for b := d.Backing; b != nil; b = b.Backing {
			if p := b.Source.File; p != "" {
				dd.Backing = append(dd.Backing, p)
			} else if p := b.Source.Dev; p != "" {
				dd.Backing = append(dd.Backing, p)
			}
		}
		out.Disks = append(out.Disks, dd)
	}
	return out, nil
}

// nodeRefs lists every file that something on the node refers to, with what refers to it: each domain's disks
// and their whole backing chains (from libvirt, and from qemu-img for images whose chain the definition does not
// show), its UEFI variable store, the files of its snapshots, and the backing file of every volume in the active
// pools. Anything that cannot be read is an error: the caller then removes nothing.
func nodeRefs(h *hostconn.Host) (map[string]string, error) {
	refs := map[string]string{}
	add := func(p, by string) {
		if p == "" {
			return
		}
		p = path.Clean(p)
		if _, ok := refs[p]; !ok {
			refs[p] = by
		}
	}
	doms, _, err := h.ConnectListAllDomains(1, 0)
	if err != nil {
		return nil, fmt.Errorf("cannot list the VMs: %w", err)
	}
	for _, d := range doms {
		by := "VM " + d.Name
		xmls := []uint32{uint32(libvirt.DomainXMLInactive)}
		if act, err := h.DomainIsActive(d); err != nil {
			if isNoDomain(err) {
				continue // gone meanwhile
			}
			return nil, fmt.Errorf("cannot read VM %s: %w", d.Name, err)
		} else if act != 0 {
			xmls = append(xmls, 0)
		}
		for _, fl := range xmls {
			raw, err := h.DomainGetXMLDesc(d, libvirt.DomainXMLFlags(fl))
			if err != nil {
				if isNoDomain(err) {
					break
				}
				return nil, fmt.Errorf("cannot read the definition of VM %s: %w", d.Name, err)
			}
			df, err := parseDomFiles(raw)
			if err != nil {
				return nil, fmt.Errorf("cannot parse the definition of VM %s: %w", d.Name, err)
			}
			add(df.NVRAM, by)
			for _, k := range df.Disks {
				p := k.Path
				if k.Type == "volume" && k.Pool != "" && k.Volume != "" {
					pl, err := h.StoragePoolLookupByName(k.Pool)
					if err != nil {
						return nil, fmt.Errorf("cannot resolve the disk %s/%s of VM %s: %w", k.Pool, k.Volume, d.Name, err)
					}
					vol, err := h.StorageVolLookupByName(pl, k.Volume)
					if err != nil {
						return nil, fmt.Errorf("cannot resolve the disk %s/%s of VM %s: %w", k.Pool, k.Volume, d.Name, err)
					}
					if p, err = h.StorageVolGetPath(vol); err != nil {
						return nil, fmt.Errorf("cannot resolve the disk %s/%s of VM %s: %w", k.Pool, k.Volume, d.Name, err)
					}
				}
				add(p, by)
				for _, b := range k.Backing {
					add(b, by+" (backing chain)")
				}
				if p == "" || k.Type == "block" || k.Type == "network" || k.Format == "raw" || len(k.Backing) > 0 || !safePathRE.MatchString(p) {
					continue
				}
				st, err := h.Stat(p)
				if err != nil {
					return nil, fmt.Errorf("cannot inspect %s of VM %s: %w", p, d.Name, err)
				}
				if len(st) != 1 || !st[0].Exists {
					continue // a missing image has no chain
				}
				out, err := h.QemuInfo(p, true)
				if err != nil {
					return nil, fmt.Errorf("cannot read the backing chain of %s (VM %s): %s", p, d.Name, firstLine(out))
				}
				chain, err := parseChain(out)
				if err != nil {
					return nil, fmt.Errorf("cannot read the backing chain of %s (VM %s): %w", p, d.Name, err)
				}
				for _, img := range chain[1:] {
					add(img.Path, by+" (backing chain)")
				}
			}
		}
		snaps, _, err := h.DomainListAllSnapshots(d, 1, 0)
		if err != nil {
			if isNoDomain(err) {
				continue
			}
			return nil, fmt.Errorf("cannot list the snapshots of VM %s: %w", d.Name, err)
		}
		for _, sn := range snaps {
			raw, err := h.DomainSnapshotGetXMLDesc(sn, 0)
			if err != nil {
				return nil, fmt.Errorf("cannot read snapshot %s of VM %s: %w", sn.Name, d.Name, err)
			}
			sf, err := parseSnapshotFiles(raw)
			if err != nil {
				return nil, fmt.Errorf("cannot read snapshot %s of VM %s: %w", sn.Name, d.Name, err)
			}
			sby := by + " (snapshot " + sn.Name + ")"
			add(sf.Memory, sby)
			for _, p := range sf.Disks {
				add(p, sby)
			}
			for _, p := range sf.Domain {
				add(p, sby)
			}
		}
	}
	pools, err := poolsByPathOpt(h, false)
	if err != nil {
		return nil, fmt.Errorf("cannot list the storage pools: %w", err)
	}
	for dir, pl := range pools {
		if pl.down {
			return nil, fmt.Errorf("the storage %s (%s) does not answer", pl.pool.Name, dir)
		}
		if !pl.active {
			continue
		}
		vols, _, err := h.StoragePoolListAllVolumes(pl.pool, 1, 0)
		if err != nil {
			return nil, fmt.Errorf("cannot list the volumes of pool %s: %w", pl.pool.Name, err)
		}
		for _, v := range vols {
			raw, err := h.StorageVolGetXMLDesc(v, 0)
			if err != nil {
				continue // removed meanwhile
			}
			var x struct {
				Backing struct {
					Path string `xml:"path"`
				} `xml:"backingStore"`
			}
			if xml.Unmarshal([]byte(raw), &x) == nil && x.Backing.Path != "" {
				add(x.Backing.Path, "volume "+pl.pool.Name+"/"+v.Name+" (built on it)")
			}
		}
	}
	return refs, nil
}

// sharedDirs says, for each directory, whether its storage may be shared between the two nodes. A directory that
// is an NFS pool on the target and also an NFS pool on the source counts as shared, whatever the servers are
// called (keeping is the safe side). A directory that is no pool on the target and not the UEFI directory cannot
// be judged and counts as shared too.
func sharedDirs(src, dst *hostconn.Host, dirs []string) (map[string]string, error) {
	sp, err := poolDescs(src)
	if err != nil {
		return nil, fmt.Errorf("cannot list the pools of %s: %w", src.Name, err)
	}
	dp, err := poolDescs(dst)
	if err != nil {
		return nil, fmt.Errorf("cannot list the pools of %s: %w", dst.Name, err)
	}
	return sharedReasons(src.Name, dst.Name, sp, dp, dirs), nil
}

func sharedReasons(srcName, dstName string, sp, dp map[string]poolDesc, dirs []string) map[string]string {
	out := map[string]string{}
	for _, d := range dirs {
		t, ok := dp[d]
		switch {
		case !ok && d == nvramDir:
		case !ok:
			out[d] = "outside every storage pool of " + dstName
		case t.Type == "netfs" || t.isNFS():
			if s, ok := sp[d]; ok && (s.Type == "netfs" || s.isNFS()) {
				out[d] = "on network storage (pool " + t.Name + ") that " + srcName + " mounts at the same place"
			}
		}
	}
	return out
}

// gatherFacts asks both nodes about the run. The source is asked only when the target could be read.
func (s *Server) gatherFacts(run store.MigRun, src, dst *hostconn.Host) runFacts {
	f := runFacts{Held: migHeld(run.ID), Files: map[string]fileFacts{}}
	migRuns.Lock()
	for _, m := range migRuns.m {
		if m.jid == run.ID {
			continue
		}
		if m.plan.UUID == run.UUID {
			f.Other = "another migration of " + run.Name + " is running"
		}
		if m.req.Target == run.Dst {
			for _, p := range m.plan.Files.list {
				ff := f.Files[p.Path]
				ff.Claim = "the migration of " + m.plan.Name + " that is running"
				f.Files[p.Path] = ff
			}
		}
	}
	migRuns.Unlock()
	if others, err := s.Store.MigRuns(); err == nil {
		for _, o := range others {
			if o.ID == run.ID || o.Dst != run.Dst {
				continue
			}
			for _, of := range o.Files {
				if of.State == store.MigFilePresent {
					ff := f.Files[of.Path]
					if ff.Claim == "" {
						ff.Claim = fmt.Sprintf("the journal of another migration of %s (%d)", o.Name, o.ID)
					}
					f.Files[of.Path] = ff
				}
			}
		}
	} else {
		f.TargetErr = "cannot read the journal: " + err.Error()
		return f
	}
	if dst == nil {
		f.TargetErr = "it is not connected"
		return f
	}
	if src == nil {
		f.SourceErr = "it is not connected"
		return f
	}
	var paths []string
	dirs := map[string]bool{}
	for _, jf := range run.Files {
		if jf.State == store.MigFilePresent {
			paths = append(paths, jf.Path)
			dirs[path.Dir(jf.Path)] = true
		}
	}
	uuid, err := parseUUID(run.UUID)
	if err != nil {
		f.TargetErr = "the journal names no valid VM"
		return f
	}

	// The target.
	revalidateNFSDirs(dst, paths)
	ids, err := dst.FileIDs(paths...)
	if err != nil {
		f.TargetErr = err.Error()
		return f
	}
	for _, id := range ids {
		ff := f.Files[id.Path]
		ff.Now = id
		f.Files[id.Path] = ff
	}
	if f.OnTarget, _, _, err = whereVM(dst, uuid); err != nil {
		f.TargetErr = "cannot look the VM up: " + err.Error()
		return f
	}
	refs, err := nodeRefs(dst)
	if err != nil {
		f.TargetErr = err.Error()
		return f
	}
	for _, p := range paths {
		if by, ok := refs[path.Clean(p)]; ok {
			ff := f.Files[p]
			ff.UsedBy = by
			f.Files[p] = ff
		}
	}

	// The source.
	var dl []string
	for d := range dirs {
		dl = append(dl, d)
	}
	sort.Strings(dl)
	shared, err := sharedDirs(src, dst, dl)
	if err != nil {
		f.SourceErr = err.Error()
		return f
	}
	for _, p := range paths {
		if sh := shared[path.Dir(p)]; sh != "" {
			ff := f.Files[p]
			ff.Shared = sh
			f.Files[p] = ff
		}
	}
	revalidateNFSDirs(src, paths)
	sids, err := src.FileIDs(paths...)
	if err != nil {
		f.SourceErr = err.Error()
		return f
	}
	for _, id := range sids {
		ff := f.Files[id.Path]
		ff.SrcNow = id
		f.Files[id.Path] = ff
	}
	var sdom libvirt.Domain
	if f.OnSource, f.SrcState, sdom, err = whereVM(src, uuid); err != nil {
		f.SourceErr = "cannot look the VM up: " + err.Error()
		return f
	}
	if f.OnSource == vmDefined || f.OnSource == vmRunning {
		raw, err := src.DomainGetXMLDesc(sdom, libvirt.DomainXMLInactive)
		if err != nil {
			f.SourceErr = "cannot read the VM's definition: " + err.Error()
			return f
		}
		df, err := parseDomFiles(raw)
		if err != nil {
			f.SourceErr = "cannot read the VM's definition: " + err.Error()
			return f
		}
		var need []string
		for _, k := range df.Disks {
			if k.Type == "file" && k.Path != "" && (k.Device == "" || k.Device == "disk") {
				need = append(need, k.Path)
			}
		}
		if df.NVRAM != "" && f.OnSource == vmDefined {
			need = append(need, df.NVRAM) // a running VM's store may be made at its next start
		}
		if len(need) > 0 {
			st, err := src.Stat(need...)
			if err != nil {
				f.SourceErr = "cannot check the VM's disk files: " + err.Error()
				return f
			}
			for _, x := range st {
				if !x.Exists {
					f.SrcMiss = append(f.SrcMiss, x.Path)
				}
			}
		}
	}
	return f
}

func parseUUID(s string) (libvirt.UUID, error) {
	var u libvirt.UUID
	h := strings.ReplaceAll(s, "-", "")
	if len(h) != 32 {
		return u, fmt.Errorf("bad uuid")
	}
	for i := 0; i < 16; i++ {
		b, err := strconv.ParseUint(h[2*i:2*i+2], 16, 8)
		if err != nil {
			return u, err
		}
		u[i] = byte(b)
	}
	return u, nil
}

// --- the cleanup ---

// cleanResult is what one attempt did.
type cleanResult struct {
	Run     int64      `json:"run"`
	Name    string     `json:"name"`
	Node    string     `json:"node"`
	Removed []cleanHit `json:"removed"`
	Kept    []cleanHit `json:"kept"`
	Note    string     `json:"note,omitempty"` // why nothing was removed, or where the VM is
	State   string     `json:"state"`          // the run's state afterwards; "" when the journal entry is gone
	changed bool       // something was removed, or a reason is new
}

type cleanHit struct {
	Path   string `json:"path"`
	Size   uint64 `json:"size"`
	Reason string `json:"reason,omitempty"`
}

// leftNote is the remark for the task of the failed migration.
func (r cleanResult) leftNote() string {
	if len(r.Kept) == 0 {
		return ""
	}
	var l []string
	for _, k := range r.Kept {
		l = append(l, k.Path+" ("+k.Reason+")")
	}
	return "files left on " + r.Node + ": " + strings.Join(l, ", ")
}

// summary is the Result of a cleanup task.
func (r cleanResult) summary() string {
	var parts []string
	if n := len(r.Removed); n > 0 {
		var sz uint64
		for _, h := range r.Removed {
			sz += h.Size
		}
		parts = append(parts, fmt.Sprintf("removed %d %s (%s)", n, plural(n, "file", "files"), fmtBytes(sz)))
	}
	for _, k := range r.Kept {
		parts = append(parts, "kept "+k.Path+": "+k.Reason)
	}
	if len(parts) == 0 {
		parts = append(parts, "nothing to remove")
	}
	if r.Note != "" && len(r.Removed) == 0 {
		parts = append(parts, r.Note)
	}
	return strings.Join(parts, "; ")
}

// cleanMigRun applies the rule to one journalled run now and removes what it allows. src and dst may be given (by
// the migration itself); otherwise the current connections are used. dry only looks: it changes nothing anywhere
// and says whether a real attempt would (changed).
func (s *Server) cleanMigRun(id int64, mode string, src, dst *hostconn.Host, dry bool) cleanResult {
	migJournal.Lock()
	if migJournal.busy[id] {
		migJournal.Unlock()
		return cleanResult{Run: id, Note: "a cleanup of this migration is already running"}
	}
	migJournal.busy[id] = true
	if mode == cleanAuto && !dry {
		migJournal.tried[id] = time.Now()
	}
	migJournal.Unlock()
	defer func() {
		migJournal.Lock()
		delete(migJournal.busy, id)
		migJournal.Unlock()
	}()
	run, err := s.Store.MigRun(id)
	if err != nil {
		return cleanResult{Run: id, Note: "no such journal entry"}
	}
	res := cleanResult{Run: id, Name: run.Name, Node: run.DstName, State: run.State}
	if src == nil {
		src, _ = s.Nodes.Host(run.Src)
	}
	if dst == nil {
		dst, _ = s.Nodes.Host(run.Dst)
	}

	// A hand-over whose outcome was not known while the source still waits for the answer: settle it first.
	if fn := pendingConfirm(id); fn != nil && dst != nil && dry {
		return cleanResult{Run: id, changed: true}
	} else if fn != nil && dst != nil {
		if uuid, err := parseUUID(run.UUID); err == nil {
			if where, _, _, err := whereVM(dst, uuid); err == nil {
				switch where {
				case vmRunning:
					if cerr := fn(true); cerr == nil {
						s.Store.SetMigState(id, store.MigAttention, true, fmt.Sprintf("%s runs on %s (the connection was lost at the end of the migration); its files on %s were kept", run.Name, run.DstName, run.SrcName))
					}
				case vmAbsent:
					fn(false)
				}
			}
		}
		if run, err = s.Store.MigRun(id); err != nil {
			return cleanResult{Run: id, Note: "no such journal entry"}
		}
	}

	facts := s.gatherFacts(run, src, dst)
	if !dry {
		for i, jf := range run.Files {
			if now := facts.Files[jf.Path].Now; now.Exists && now.Regular && now.ID == jf.Ident && now.Blocks != jf.Size {
				s.Store.SetMigFileSize(id, jf.Path, now.Blocks)
				run.Files[i].Size = now.Blocks
			}
		}
	}
	v := judgeRun(run, facts, mode)
	if v.State != "" && (v.State != run.State || v.Unsure && !run.Unsure || v.Note != run.Note) {
		res.changed = true
		if dry {
			return res
		}
		if err := s.Store.SetMigState(id, v.State, v.Unsure, v.Note); err == nil {
			run.State = v.State
		}
	}
	res.State, res.Note = run.State, v.Note
	if !v.Go {
		for _, jf := range run.Files {
			reason := jf.Reason
			if jf.State == store.MigFilePresent && jf.Ident != "" {
				reason = v.Note
			}
			if reason != jf.Reason {
				res.changed = true
				if !dry {
					s.Store.SetMigFile(id, jf.Path, jf.State, reason)
				}
			}
			res.Kept = append(res.Kept, cleanHit{jf.Path, jf.Size, reason})
		}
		return res
	}
	if dry {
		for _, jf := range run.Files {
			action, reason := judgeFile(jf, facts.Files[jf.Path])
			if action == fileRemove || action == fileGone || reason != jf.Reason {
				res.changed = true
			}
		}
		return res
	}
	removedAny := false
	for _, jf := range run.Files {
		action, reason := judgeFile(jf, facts.Files[jf.Path])
		switch action {
		case fileGone:
			s.Store.DeleteMigFile(id, jf.Path)
			res.changed = true
			continue
		case fileRemove:
			ans, err := dst.RemoveIfID(jf.Path, jf.Ident)
			switch {
			case err != nil:
				action, reason = fileKeep, "removing it failed: "+err.Error()
			case ans == hostconn.RemovedFile:
				log.Printf("migration cleanup: removed %s on %s (left by the failed migration of %s)", jf.Path, run.DstName, run.Name)
				s.Store.DeleteMigFile(id, jf.Path)
				res.Removed = append(res.Removed, cleanHit{jf.Path, jf.Size, ""})
				res.changed, removedAny = true, true
				continue
			case ans == hostconn.MissingFile:
				s.Store.DeleteMigFile(id, jf.Path)
				res.changed = true
				continue
			case ans == hostconn.InUseFile:
				action, reason = fileKeep, "a process on "+run.DstName+" has it open"
			default:
				action, reason = fileKeepFor, "another file has this name now (replaced since the migration made it)"
			}
		}
		state := store.MigFilePresent
		if action == fileKeepFor {
			state = store.MigFileKept
		}
		if state != jf.State || reason != jf.Reason {
			res.changed = true
			s.Store.SetMigFile(id, jf.Path, state, reason)
			if reason != jf.Reason {
				log.Printf("migration cleanup: kept %s on %s: %s", jf.Path, run.DstName, reason)
			}
		}
		res.Kept = append(res.Kept, cleanHit{jf.Path, jf.Size, reason})
	}
	if removedAny {
		poolsByPath(dst) // refreshes the active pools: libvirt forgets the removed volumes
	}
	if len(res.Kept) == 0 {
		s.Store.DeleteMigRun(id)
		res.State = ""
		res.changed = true
	}
	return res
}

// --- retries ---

// retryMigLeftovers tries the journalled runs again: those that touch node (0: every run, at most every
// migRetryGap each). Each attempt that changes something runs as a task.
func (s *Server) retryMigLeftovers(node int64) {
	if s.Store == nil || s.Nodes == nil {
		return
	}
	runs, err := s.Store.MigRuns()
	if err != nil {
		return
	}
	for _, run := range runs {
		if migHeld(run.ID) {
			continue
		}
		if node != 0 && run.Src != node && run.Dst != node {
			continue
		}
		if node == 0 {
			migJournal.Lock()
			last := migJournal.tried[run.ID]
			migJournal.Unlock()
			if time.Since(last) < migRetryGap {
				continue
			}
		}
		if _, err := s.Nodes.Host(run.Dst); err != nil && pendingConfirm(run.ID) == nil {
			continue // tried when it comes online
		}
		s.cleanAsTask(run, cleanAuto)
	}
}

// cleanAsTask runs one attempt. An automatic attempt that changes nothing leaves no task behind: it is looked at
// first with plain connections, and only when it would remove something or has news is it run again as a task
// with an operation trace.
func (s *Server) cleanAsTask(run store.MigRun, mode string) cleanResult {
	if mode == cleanAuto {
		if look := s.cleanMigRun(run.ID, mode, nil, nil, true); !look.changed {
			return look
		}
	}
	r, _ := http.NewRequestWithContext(context.WithValue(context.Background(), ctxKey{}, "scheduler"), http.MethodPost, "/", nil)
	return s.cleanTask(s.traced(r), run, mode)
}

func (s *Server) cleanTask(r *http.Request, run store.MigRun, mode string) cleanResult {
	var res cleanResult
	target := run.Name + " on " + run.DstName
	err := s.task(r, "migration cleanup", target, run.DstName, func(func(float64)) error {
		var src, dst *hostconn.Host
		if h, err := s.Nodes.Host(run.Src); err == nil {
			src = s.traceHost(r, h)
		}
		if h, err := s.Nodes.Host(run.Dst); err == nil {
			dst = s.traceHost(r, h)
		}
		res = s.cleanMigRun(run.ID, mode, src, dst, false)
		return taskDone("%s", res.summary())
	})
	if err != nil {
		log.Printf("migration cleanup of %s: %v", target, err)
	}
	if res.changed {
		s.pokeAlerts()
	}
	return res
}

// startMigCleanup tries the runs of a node as soon as its connection comes (back) up.
func (s *Server) startMigCleanup() {
	if s.Nodes == nil || s.Store == nil {
		return
	}
	s.migCleanOnce.Do(func() {
		events, _ := s.Nodes.Subscribe()
		go func() {
			for ev := range events {
				if ev.Type == "node" && ev.Detail == "online" {
					go s.retryMigLeftovers(ev.Node)
				}
			}
		}()
	})
}

// pendingConfirm is the source's end of a hand-over of run id that waits for the outcome, or nil.
func pendingConfirm(id int64) func(bool) error {
	migJournal.Lock()
	defer migJournal.Unlock()
	return migJournal.confirm[id]
}

// leftoverOf names the failed migration whose journal holds path on node, or "".
func (s *Server) leftoverOf(node int64, p string) string {
	if s == nil || s.Store == nil {
		return ""
	}
	runs, err := s.Store.MigRuns()
	if err != nil {
		return ""
	}
	for _, r := range runs {
		if r.Dst != node {
			continue
		}
		for _, f := range r.Files {
			if f.Path == p {
				return "the failed migration of " + r.Name
			}
		}
	}
	return ""
}

// --- alerts ---

type alertLeftover struct {
	Run                    int64
	Name, SrcName, DstName string
	Dst                    int64
	State, Note            string
	Unsure                 bool
	Files                  []store.MigFile
}

func (s *Server) gatherLeftovers() []alertLeftover {
	if s.Store == nil {
		return nil
	}
	runs, err := s.Store.MigRuns()
	if err != nil {
		return nil
	}
	var out []alertLeftover
	for _, r := range runs {
		if migHeld(r.ID) {
			continue
		}
		out = append(out, alertLeftover{Run: r.ID, Name: r.Name, SrcName: r.SrcName, DstName: r.DstName, Dst: r.Dst, State: r.State, Note: r.Note, Unsure: r.Unsure, Files: r.Files})
	}
	return out
}

// leftoverAlert is the alert of one journalled run. It names no node: it is read from the journal on every pass,
// so it is never kept for a node that could not be read.
func leftoverAlert(l alertLeftover) Alert {
	var lines []string
	for _, f := range l.Files {
		t := f.Path
		if f.Size > 0 {
			t += " (" + fmtBytes(f.Size) + ")"
		}
		if f.Reason != "" && f.Reason != l.Note && !strings.HasSuffix(l.Note, f.Reason) {
			t += ": " + f.Reason
		}
		lines = append(lines, t)
	}
	a := Alert{ID: "migleft:" + strconv.FormatInt(l.Run, 10), Kind: "migration-leftover", Severity: "warning", Link: "/tasks",
		Title: fmt.Sprintf("Files of a failed migration of %s are left on %s", l.Name, l.DstName)}
	if l.State == store.MigAttention || l.Unsure {
		a.Severity = "critical"
		a.Title = fmt.Sprintf("The migration of %s needs attention", l.Name)
		a.Message = l.Note + "."
		if len(lines) > 0 {
			a.Message += " Kept on " + l.DstName + ": " + strings.Join(lines, "; ") + "."
		}
		return a
	}
	a.Message = "On " + l.DstName + ": " + strings.Join(lines, "; ") + "."
	if len(lines) == 0 {
		a.Message = l.Note
	}
	return a
}

// --- HTTP ---

// migLeftovers answers GET /api/migration-leftovers: the journal.
func (s *Server) migLeftovers(w http.ResponseWriter, r *http.Request) {
	runs, err := s.Store.MigRuns()
	if err != nil {
		fail(w, err)
		return
	}
	if runs == nil {
		runs = []store.MigRun{}
	}
	writeJSON(w, map[string]any{"runs": runs})
}

func (s *Server) leftoverRun(r *http.Request) (store.MigRun, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return store.MigRun{}, store.ErrNotFound
	}
	return s.Store.MigRun(id)
}

// migLeftoverRemove answers POST /api/migration-leftovers/{id}/remove: the rule, now, as a task. What was removed
// and what was kept, with the reasons, is the answer.
func (s *Server) migLeftoverRemove(w http.ResponseWriter, r *http.Request) {
	run, err := s.leftoverRun(r)
	if err != nil {
		fail(w, err)
		return
	}
	if migHeld(run.ID) {
		fail(w, badRequest("the migration is still running"))
		return
	}
	res := s.cleanTask(r, run, cleanByPerson)
	infoOf(r).detail = "migration cleanup of " + run.Name + " on " + run.DstName + ": " + res.summary()
	if res.Removed == nil {
		res.Removed = []cleanHit{}
	}
	if res.Kept == nil {
		res.Kept = []cleanHit{}
	}
	writeJSON(w, res)
}

// migLeftoverKeep answers POST /api/migration-leftovers/{id}/keep: the journal entry goes, the files stay as
// ordinary volumes.
func (s *Server) migLeftoverKeep(w http.ResponseWriter, r *http.Request) {
	run, err := s.leftoverRun(r)
	if err != nil {
		fail(w, err)
		return
	}
	if migHeld(run.ID) {
		fail(w, badRequest("the migration is still running"))
		return
	}
	if err := s.Store.DeleteMigRun(run.ID); err != nil {
		fail(w, err)
		return
	}
	infoOf(r).detail = fmt.Sprintf("kept the files of the failed migration of %s on %s: %s", run.Name, run.DstName, strings.Join(pathsOf(run.Files), ", "))
	log.Printf("migration cleanup: %s", infoOf(r).detail)
	s.pokeAlerts()
	s.Nodes.Publish(cluster.Event{Type: "alert"})
	writeJSON(w, map[string]bool{"ok": true})
}
