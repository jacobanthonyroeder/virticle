package api

import (
	"fmt"
	"log"
	"path"
	"strings"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/hostconn"
	"virticle/internal/store"
)

// The two ways a VM moves (live/suspended and offline) and the removal of its files from the source. Every
// file made on the target is in the journal (migjournal.go); a failure removes them only through its rule.

func (m *migRun) runOnline() error {
	p := m.plan
	if err := m.checkCancel(); err != nil {
		return err
	}
	autostart, _ := m.src.DomainGetAutostart(m.dom)
	if err := m.journalStart(); err != nil {
		return err
	}
	failed := func(err error, notes ...string) error {
		return withNote(err, append(notes, m.failedConfirmed(err))...)
	}

	if err := m.copyFiles(0.02, 0.25); err != nil {
		return failed(err)
	}
	if err := m.createActive(); err != nil {
		return failed(err)
	}
	if err := m.createNVRAM(); err != nil {
		return failed(err)
	}
	if err := m.checkCancel(); err != nil {
		return failed(err)
	}

	// libvirt refuses to migrate a domain that has snapshot metadata. Take
	// the records off the source for the duration; the files stay.
	var removed []migSnap
	restoreSnaps := func() string {
		if len(removed) == 0 {
			return ""
		}
		if rerr := redefineSnapshots(m.src, m.dom, removed); rerr != nil {
			for _, sn := range removed {
				log.Printf("migrate %s: snapshot %s XML (restore manually with virsh snapshot-create --redefine):\n%s", p.Name, sn.Name, sn.XML)
			}
			return "restoring snapshot metadata on the source failed: " + rerr.Error()
		}
		return ""
	}
	if len(p.Snaps) > 0 {
		var err error
		if removed, err = deleteSnapshotMetadata(m.src, m.dom, p.Snaps); err != nil {
			if rerr := redefineSnapshots(m.src, m.dom, removed); rerr != nil {
				err = fmt.Errorf("%w (restoring snapshots on the source failed: %v)", err, rerr)
			}
			return failed(err)
		}
	}

	// From here the VM may change hands. That is recorded first: a run found in this state later, by a process
	// that did not see the outcome, removes nothing by itself.
	if err := m.setState(store.MigSwitching, false, ""); err != nil {
		return failed(err, restoreSnaps())
	}
	flags := libvirt.MigratePersistDest | libvirt.MigrateUndefineSource | p.mirrorFlag()
	if p.Mode == "live" {
		flags |= libvirt.MigrateLive | libvirt.MigrateAutoConverge
	}
	if p.TLS {
		flags |= libvirt.MigrateTLS
	}
	hand, err := m.perform(flags, 0.25, 0.95)
	if err != nil {
		switch hand {
		case handNone:
			if m.finishLost.Load() {
				// The target said later that the VM is not there. Between the lost answer and that, the VM may have
				// run there and been removed by hand, so nothing is removed without a person.
				return withNote(err, restoreSnaps(),
					m.attention(fmt.Sprintf("the answer of %s at the end of the migration was lost; it said later that %s is not there, and %s resumed on %s", m.dst.Name, p.Name, p.Name, m.src.Name)))
			}
			return failed(err, restoreSnaps())
		case handDone:
			// The end of the migration failed on the way back, but the VM runs on the target.
			log.Printf("migrate %s: error %v but the VM runs on %s", p.Name, err, m.dst.Name)
			note := ""
			if moved, lerr := m.dst.DomainLookupByUUID(m.dom.UUID); lerr == nil {
				if ferr := m.finishOnTarget(moved, removed, autostart); ferr != nil {
					note = ferr.Error()
				}
			}
			return withNote(fmt.Errorf("the migration reported an error (%v), but %s runs on %s now", err, p.Name, m.dst.Name),
				note, m.attention(fmt.Sprintf("%s runs on %s (the connection was lost at the end of the migration); its files on %s were kept", p.Name, m.dst.Name, m.src.Name)))
		default:
			return withNote(fmt.Errorf("the migration reported an error (%v), and %s cannot be asked whether %s runs there", err, m.dst.Name, p.Name),
				fmt.Sprintf("%s stays paused on %s until %s answers", p.Name, m.src.Name, m.dst.Name),
				m.attention(fmt.Sprintf("the connection to %s was lost at the end of the migration: it is not known whether %s runs there; it is paused on %s", m.dst.Name, p.Name, m.src.Name)))
		}
	}
	m.moved()

	moved, err := m.dst.DomainLookupByUUID(m.dom.UUID)
	if err != nil {
		return fmt.Errorf("migration finished but the VM was not found on %s: %w", m.dst.Name, err)
	}
	if err := m.finishOnTarget(moved, removed, autostart); err != nil {
		return err
	}
	m.progress(0.97)
	m.s.autoSyncClock("system", "migration", m.dst, moved)
	return m.cleanSource()
}

// finishOnTarget restores what the VM had on the source: autostart and the snapshot records.
func (m *migRun) finishOnTarget(moved libvirt.Domain, removed []migSnap, autostart int32) error {
	if autostart != 0 {
		if err := m.dst.DomainSetAutostart(moved, 1); err != nil {
			log.Printf("migrate %s: could not restore autostart on %s: %v", m.plan.Name, m.dst.Name, err)
		}
	}
	if len(removed) > 0 {
		if err := redefineSnapshots(m.dst, moved, removed); err != nil {
			for _, sn := range removed {
				log.Printf("migrate %s: snapshot %s XML:\n%s", m.plan.Name, sn.Name, sn.XML)
			}
			return fmt.Errorf("the VM moved to %s but its snapshots could not be recreated there: %w; the snapshot files were kept on the source", m.dst.Name, err)
		}
	}
	return nil
}

// runOffline moves a shut-off VM: copy every file, define it on the target,
// recreate snapshots there, and only then take it off the source.
func (m *migRun) runOffline() error {
	p := m.plan
	autostart, _ := m.src.DomainGetAutostart(m.dom)
	if err := m.journalStart(); err != nil {
		return err
	}
	failed := func(err error, notes ...string) error {
		return withNote(err, append(notes, m.failedConfirmed(err))...)
	}
	if err := m.copyFiles(0.02, 0.80); err != nil {
		return failed(err)
	}
	if err := m.checkCancel(); err != nil {
		return failed(err)
	}
	if err := m.setState(store.MigSwitching, false, ""); err != nil {
		return failed(err)
	}
	moved, err := m.dst.DomainDefineXMLFlags(p.PersistXML, 0)
	if err != nil {
		err = fmt.Errorf("defining the VM on %s: %w", m.dst.Name, err)
		if libvirtAnswered(err) {
			return failed(err)
		}
		return withNote(err, m.attention(fmt.Sprintf("the answer of %s to defining %s was lost: it may be defined there", m.dst.Name, p.Name)))
	}
	// undoDst takes the definition off the target again. The UEFI variable store stays for the journal's rule.
	undoDst := func(err error) error {
		uf := libvirt.DomainUndefineSnapshotsMetadata | libvirt.DomainUndefineKeepNvram | libvirt.DomainUndefineManagedSave
		if uerr := m.dst.DomainUndefineFlags(moved, uf); uerr != nil {
			return withNote(err, "could not undefine the VM on "+m.dst.Name+": "+uerr.Error(),
				m.attention(fmt.Sprintf("%s is defined on %s and on %s", p.Name, m.dst.Name, m.src.Name)))
		}
		return failed(err)
	}
	m.progress(0.85)
	if err := redefineSnapshots(m.dst, moved, p.Snaps); err != nil {
		return undoDst(err)
	}
	// The source's UEFI variable store is kept by the undefine: cleanSource removes it with the other files, once
	// the VM is confirmed on the target.
	uf := libvirt.DomainUndefineSnapshotsMetadata | libvirt.DomainUndefineManagedSave | libvirt.DomainUndefineCheckpointsMetadata
	if p.NVRAM != nil {
		uf |= libvirt.DomainUndefineKeepNvram
	}
	if err := m.src.DomainUndefineFlags(m.dom, uf); err != nil {
		err = fmt.Errorf("removing the VM from %s: %w", m.src.Name, err)
		where, _, _, werr := whereVM(m.src, m.dom.UUID)
		switch {
		case werr == nil && (where == vmDefined || where == vmRunning):
			return undoDst(err)
		case werr == nil && where == vmAbsent:
			log.Printf("migrate %s: %v, but the VM is no longer defined there: the move is complete", p.Name, err)
		default:
			return withNote(err, m.attention(fmt.Sprintf("%s is defined on %s; whether it is still defined on %s is not known", p.Name, m.dst.Name, m.src.Name)))
		}
	}
	m.moved()
	if autostart != 0 {
		if err := m.dst.DomainSetAutostart(moved, 1); err != nil {
			log.Printf("migrate %s: could not restore autostart on %s: %v", p.Name, m.dst.Name, err)
		}
	}
	m.progress(0.95)
	return m.cleanSource()
}

// sourceProof says why the VM's files on the source must not be removed, or "" when they may: the VM is defined
// on the target with every file of its definition there, and it is no longer defined on the source.
func (m *migRun) sourceProof() string {
	where, _, d, err := whereVM(m.dst, m.dom.UUID)
	switch {
	case err != nil:
		return "cannot confirm the VM on " + m.dst.Name + ": " + err.Error()
	case where != vmRunning && where != vmDefined:
		return "the VM is not defined on " + m.dst.Name
	}
	raw, err := m.dst.DomainGetXMLDesc(d, libvirt.DomainXMLInactive)
	if err != nil {
		return "cannot read the VM's definition on " + m.dst.Name + ": " + err.Error()
	}
	df, err := parseDomFiles(raw)
	if err != nil {
		return "cannot read the VM's definition on " + m.dst.Name + ": " + err.Error()
	}
	var need []string
	for _, k := range df.Disks {
		if k.Type == "file" && k.Path != "" && (k.Device == "" || k.Device == "disk") {
			need = append(need, k.Path)
		}
	}
	if df.NVRAM != "" {
		need = append(need, df.NVRAM)
	}
	if len(need) > 0 {
		st, err := m.dst.Stat(need...)
		if err != nil {
			return "cannot check the VM's files on " + m.dst.Name + ": " + err.Error()
		}
		for _, x := range st {
			if !x.Exists {
				return x.Path + " does not exist on " + m.dst.Name
			}
		}
	}
	sw, _, _, err := whereVM(m.src, m.dom.UUID)
	switch {
	case err != nil:
		return "cannot check " + m.src.Name + ": " + err.Error()
	case sw != vmAbsent:
		return "the VM is still defined on " + m.src.Name
	}
	return ""
}

// cleanSource deletes the source files that belonged to this VM, only once sourceProof holds. Read-only media and
// shared base images are never touched, and neither is a file that anything on the source still refers to (a
// domain, its backing chain or snapshots, a volume built on it), nor one that is the same file as on the target
// (storage both nodes see).
func (m *migRun) cleanSource() error {
	if m.req.KeepSourceDisks {
		return nil
	}
	keptAll := func(why string) error {
		log.Printf("migrate %s: no source file removed: %s", m.plan.Name, why)
		return fmt.Errorf("the VM was moved to %s, but its files on %s were left in place: %s", m.dst.Name, m.src.Name, why)
	}
	if why := m.sourceProof(); why != "" {
		return keptAll(why)
	}
	var cand []*migFile
	for _, f := range m.plan.Files.list {
		if f.Owned && !f.Shared {
			cand = append(cand, f)
		}
	}
	// The UEFI variable store: a live migration recreates it on the target, an offline one copies it; the
	// undefine leaves the source's file behind either way.
	if nv := path.Clean(m.plan.Dom.NVRAM); m.plan.Dom.NVRAM != "" && strings.HasPrefix(nv, nvramDir+"/") && safePathRE.MatchString(nv) {
		cand = append(cand, &migFile{Path: nv, Owned: true})
	}
	if len(cand) == 0 {
		return nil
	}
	refs, err := nodeRefs(m.src)
	if err != nil {
		return keptAll("cannot check what uses them on " + m.src.Name + ": " + err.Error())
	}
	paths := make([]string, len(cand))
	for i, f := range cand {
		paths[i] = f.Path
	}
	sids, err := m.src.FileIDs(paths...)
	if err != nil {
		return keptAll(err.Error())
	}
	dids, err := m.dst.FileIDs(paths...)
	if err != nil {
		return keptAll(err.Error())
	}
	var left []string
	removed := false
	for i, f := range cand {
		if by, ok := refs[path.Clean(f.Path)]; ok {
			left = append(left, f.Path+" (in use by "+by+")")
			continue
		}
		sid, did := sids[i], dids[i]
		if !sid.Exists {
			continue
		}
		if !sid.Regular || sid.ID == "" || (did.Exists && did.ID == sid.ID) {
			left = append(left, f.Path+" (it may be the file the VM uses on "+m.dst.Name+")")
			continue
		}
		if err := m.removeSourceFile(f.Path, sid); err != nil {
			left = append(left, f.Path+" ("+err.Error()+")")
			continue
		}
		removed = true
	}
	if removed {
		refreshLive(m.src) // libvirt forgets the removed volumes
	}
	if len(left) > 0 {
		return fmt.Errorf("the VM was moved to %s, but these source files were left in place: %s", m.dst.Name, strings.Join(left, ", "))
	}
	return nil
}

// removeSourceFile removes one file of the source, only if it is still the file that was checked.
func (m *migRun) removeSourceFile(p string, id hostconn.FileID) error {
	ans, err := m.src.RemoveIfID(p, id.ID)
	if err != nil {
		return err
	}
	switch ans {
	case hostconn.RemovedFile, hostconn.MissingFile:
		return nil
	case hostconn.InUseFile:
		return fmt.Errorf("a process on %s has it open", m.src.Name)
	}
	return fmt.Errorf("another file has this name now")
}
