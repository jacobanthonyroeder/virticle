package api

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"virticle/internal/nodeproto"
	"virticle/internal/prepare"
	"virticle/internal/snapsched"
)

// The pure functions here build the Result text of tasks: terse facts, no sentence, no final period.

// vcpuNote is the Result of "set vCPUs": "4 vCPU", with the new maximum when it was given.
func vcpuNote(vcpus, maxVcpus int, res hwResult) string {
	max := ""
	if maxVcpus > 0 {
		max = fmt.Sprintf("max %d", maxVcpus)
	}
	return hwNote(joinFacts(fmt.Sprintf("%d vCPU", vcpus), max), res)
}

// memNote is the Result of "set memory": "4 GiB", with the new maximum when it was given.
func memNote(mib, maxMiB int, res hwResult) string {
	max := ""
	if maxMiB > 0 {
		max = "max " + mibText(uint64(maxMiB))
	}
	return hwNote(joinFacts(mibText(uint64(mib)), max), res)
}

// diskAddNote is the Result of "add disk": the target and the pool, with the size of a volume made for it or the
// name of the volume that was attached.
func diskAddNote(target, pool, volume string, newGiB int, res hwResult) string {
	if newGiB > 0 {
		return hwNote(joinFacts(target, gibText(newGiB), "pool "+pool), res)
	}
	return hwNote(joinFacts(target, volume, "pool "+pool), res)
}

// nicChangeNote is the Result of "change network interface": the card and what was set on it.
func nicChangeNote(mac, network, bridge, link string, limit bool, res hwResult) string {
	f := []string{mac}
	if network != "" {
		f = append(f, "network "+network)
	}
	if bridge != "" {
		f = append(f, "bridge "+bridge)
	}
	if link != "" {
		f = append(f, "link "+link)
	}
	if limit {
		f = append(f, "speed limit")
	}
	return hwNote(joinFacts(f...), res)
}

// newVolumeGiB is the size of the volume an "add disk" request makes, or 0 when it attaches an existing one.
func newVolumeGiB(nv *newVolumeReq) int {
	if nv == nil {
		return 0
	}
	return nv.SizeGiB
}

// volumeDeletedFact is the fact for a disk removal that also deleted its volume.
func volumeDeletedFact(deleted bool) string {
	if deleted {
		return "volume deleted"
	}
	return ""
}

// mediaFact is the fact for a CD-ROM medium change: the volume inserted, or "ejected".
func mediaFact(volume string) string {
	if volume == "" {
		return "ejected"
	}
	return volume
}

// nicSideFact says where a network card is attached.
func nicSideFact(network, bridge string) string {
	if network != "" {
		return "network " + network
	}
	return "bridge " + bridge
}

// powerNote is the Result of a power action: the state the VM is in now, or what was sent when the guest decides
// when it arrives there.
func powerNote(action string) string {
	switch action {
	case "start", "resume":
		return "running"
	case "pause":
		return "paused"
	case "poweroff":
		return "shut off, forced"
	case "shutdown":
		return "shutdown sent"
	case "reboot":
		return "reboot sent"
	case "reset":
		return "reset"
	}
	return action
}

// shutdownNote is the Result of a shutdown that waited for the guest.
func shutdownNote(o shutdownOutcome) string {
	switch {
	case o.Forced:
		return "shut off, forced after the wait"
	case o.Method == "agent":
		return "shut off, by the guest agent"
	case o.Method == "acpi":
		return "shut off, by ACPI"
	}
	return "already shut off"
}

// deleteNote is the Result of "delete": the VM, and how many disk files went with it.
func deleteNote(name string, disks int) string {
	switch disks {
	case 0:
		return name
	case 1:
		return name + ", 1 disk deleted"
	}
	return fmt.Sprintf("%s, %d disks deleted", name, disks)
}

// scheduleNote is the Result of setting or removing a snapshot schedule.
func scheduleNote(sch *snapsched.Schedule) string {
	if sch == nil {
		return "removed"
	}
	when := fmt.Sprintf("every %d min", sch.EveryMinutes)
	if sch.Mode == "daily" {
		when = "daily " + sch.At + " UTC"
	}
	return joinFacts(when, fmt.Sprintf("keep %d", sch.Keep), map[bool]string{true: "", false: "off"}[sch.Enabled])
}

// savedOrRemoved is the Result of changing a schedule kept in the VM's metadata: an empty element removes it.
func savedOrRemoved(elem string) string {
	if elem == "" {
		return "removed"
	}
	return "saved"
}

// clockChangeNote is the Result of "change clock": what was set.
func clockChangeNote(offset string, sync *bool, res hwResult) string {
	var f []string
	if offset != "" {
		f = append(f, "offset "+offset)
	}
	if sync != nil {
		f = append(f, map[bool]string{true: "sync on", false: "sync off"}[*sync])
	}
	return hwNote(joinFacts(f...), res)
}

// clockSyncNote is the Result of "sync guest clock": how far off the guest was. hwMissing says the guest could
// not set its hardware clock; why is "start" or "resume" for a sync the server started by itself.
func clockSyncNote(before time.Duration, known, hwMissing bool, why string) string {
	var off string
	switch {
	case !known:
		off = "clock set"
	case before > -time.Second && before < time.Second:
		off = "was in sync"
	case before < 0:
		off = "was " + durationText(-before) + " behind"
	default:
		off = "was " + durationText(before) + " ahead"
	}
	hw := ""
	if hwMissing {
		hw = "hardware clock not set"
	}
	if why != "" {
		why = "after " + why
	}
	return joinFacts(off, hw, why)
}

// durationText writes a duration in whole units: "45 s", "12 min", "3 h", "2 d".
func durationText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()+0.5))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()+0.5))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()+0.5))
	}
	return fmt.Sprintf("%d d", int(d.Hours()/24+0.5))
}

// fileFact is a file's name with its size.
func fileFact(name string, size uint64) string { return name + ", " + fmtBytes(size) }

// tlsNote is the Result of setting up or removing encrypted migration on a node: the folder and how many changes.
func tlsNote(info migTLSInfo, verb string) string {
	if len(info.Changes) == 0 {
		return joinFacts("nothing to change", info.Dir)
	}
	return joinFacts(verb, fmt.Sprintf("%d changes", len(info.Changes)), info.Dir)
}

// hostnetNote is the Result of a host network change that the node confirmed.
func hostnetNote(req nodeproto.NetRequest) string {
	return joinFacts(req.Action, req.Bridge, "confirmed")
}

// snapshotNote is the Result of "snapshot create": the name, and whether the memory was saved too.
func snapshotNote(name string, memory bool) string {
	if memory {
		return name + ", disk and memory"
	}
	return name + ", disk only"
}

// snapshotsDeletedNote is the Result of deleting snapshots: their names, or the count when there are many.
func snapshotsDeletedNote(names []string) string {
	if len(names) > 3 {
		return fmt.Sprintf("%d snapshots", len(names))
	}
	return strings.Join(names, ", ")
}

// createVMNote is the Result of "create": the size of the new VM, and "running" when it was started with it.
func createVMNote(vcpus, memMiB int, started bool) string {
	state := ""
	if started {
		state = "running"
	}
	return joinFacts(fmt.Sprintf("%d vCPU", vcpus), mibText(uint64(memMiB)), state)
}

// importOVANote is the Result of an OVA import: the first volume and the count when there are several.
func importOVANote(vols []string) string {
	if len(vols) == 1 {
		return vols[0]
	}
	return fmt.Sprintf("%s, %d disks", vols[0], len(vols))
}

// poolCreateNote is the Result of "create pool": its type and where it keeps its data.
func poolCreateNote(p poolRequest) string {
	if p.XML != "" {
		return "from XML"
	}
	where := p.Target
	switch {
	case p.SourceHost != "":
		where = p.SourceHost + ":" + p.SourcePath
	case p.SourceName != "":
		where = p.SourceName
	}
	return joinFacts(p.Type, where)
}

// poolActionNote is the Result of a pool action.
func poolActionNote(action string) string {
	switch action {
	case "start":
		return "active"
	case "stop":
		return "stopped"
	case "refresh":
		return "refreshed"
	case "autostart":
		return "autostart on"
	case "manual":
		return "autostart off"
	case "remove":
		return "removed, data kept"
	}
	return action
}

// volumeCreateNote is the Result of "create volume": "10 GiB qcow2".
func volumeCreateNote(capacity uint64, format string) string {
	if format == "" {
		return fmtBytes(capacity)
	}
	return fmtBytes(capacity) + " " + format
}

// volumeActionNote is the Result of a volume action: the new size, or the name of the copy.
func volumeActionNote(action, name string, capacity uint64) string {
	if action == "resize" {
		return fmtBytes(capacity)
	}
	return name
}

// networkCreateNote is the Result of "create network": the mode and the address it serves.
func networkCreateNote(req networkRequest) string {
	if req.XML != "" {
		return "from XML"
	}
	return joinFacts(req.Mode, req.Address)
}

// networkActionNote is the Result of a network action.
func networkActionNote(action string) string {
	switch action {
	case "start":
		return "active"
	case "stop":
		return "stopped"
	case "autostart":
		return "autostart on"
	case "noautostart":
		return "autostart off"
	case "delete":
		return "deleted"
	}
	return action
}

// reservationNote is the Result of a DHCP reservation change: the address, with the host name when it has one.
func reservationNote(e dhcpHost) string {
	return joinFacts(e.MAC, e.IP, e.Name)
}

// nodeCountNote is the Result of a change made on several nodes: "added on 3 nodes".
func nodeCountNote(verb string, n int) string {
	if n == 1 {
		return verb + " on 1 node"
	}
	return fmt.Sprintf("%s on %d nodes", verb, n)
}

// tuningNote is the Result of a tuning change: what was set. These changes are saved for the next start.
func tuningNote(fact string) string { return joinFacts(fact, "at next start") }

// cpuModelFact says what "set CPU model" sets.
func cpuModelFact(mode, model string, sockets, cores, threads int, noTopology bool) string {
	what := mode
	if mode == "custom" {
		what = model
	}
	topo := ""
	switch {
	case noTopology:
		topo = "no topology"
	case sockets != 0 || cores != 0 || threads != 0:
		topo = fmt.Sprintf("%d sockets, %d cores, %d threads", sockets, cores, threads)
	}
	return joinFacts(what, topo)
}

// hugepagesFact says what "set huge pages" sets.
func hugepagesFact(pageKiB uint64) string {
	if pageKiB == 0 {
		return "ordinary pages"
	}
	return fmtBytes(pageKiB*1024) + " pages"
}

// numaFact says what "set NUMA policy" sets.
func numaFact(mode, nodeset string) string {
	if mode == "" {
		return "policy removed"
	}
	return joinFacts(mode, nodeset)
}

// pinFact says what "set CPU pinning" did.
func pinFact(clear bool, vcpus int, emulator bool) string {
	var f []string
	if clear {
		f = append(f, "old pins cleared")
	}
	if vcpus > 0 {
		f = append(f, fmt.Sprintf("%d vCPU pinned", vcpus))
	}
	if emulator {
		f = append(f, "emulator pinned")
	}
	if len(f) == 0 {
		return "no pins"
	}
	return strings.Join(f, ", ")
}

// replStopNote is the Result of "replication stop".
func replStopNote(deletedCopy bool) string {
	if deletedCopy {
		return "stopped, copy deleted"
	}
	return "stopped, copy kept"
}

// cloneNote is the Result of "clone": the new VM, how its disks were made, and "running" when it was started.
func cloneNote(name string, linked, started bool) string {
	how := "full copy"
	if linked {
		how = "linked"
	}
	state := ""
	if started {
		state = "running"
	}
	return joinFacts(name, how, state)
}

// ctlBackupNote is the Result of the daily controller backup: the size of the copy.
func ctlBackupNote(size int64, encrypted bool) string {
	enc := ""
	if encrypted {
		enc = "encrypted"
	}
	return joinFacts(fmtBytes(uint64(size)), enc)
}

// prepareNote is the Result of "prepare node" and "check node". A check counts its warnings; a preparation counts
// the steps that changed something ("current" when the node had everything already).
func prepareNote(rows []prepare.Row, checkOnly bool) string {
	if checkOnly {
		n := 0
		for _, r := range rows {
			if r.Status == prepare.Warning {
				n++
			}
		}
		if n == 0 {
			return "no warnings"
		}
		return fmt.Sprintf("%d %s", n, plural(n, "warning", "warnings"))
	}
	n := 0
	for _, r := range rows {
		if strings.HasPrefix(r.ID, "do-") && r.Status == prepare.OK && !strings.HasPrefix(r.Detail, "Already in place") {
			n++
		}
	}
	if n == 0 {
		return "current"
	}
	return fmt.Sprintf("%d %s done", n, plural(n, "step", "steps"))
}

// moveNote is the Result of "move disk": the pool and whether the VM ran during the move.
func moveNote(pool string, live bool) string {
	if live {
		return "to pool " + pool + ", live"
	}
	return "to pool " + pool + ", offline"
}

// migrateNote is the Result of "migrate": the target, how the VM moved, and whether the memory stream was encrypted.
func migrateNote(dst, mode string, encrypted bool) string {
	enc := ""
	if encrypted {
		enc = "encrypted"
	}
	return joinFacts("to "+dst, mode, enc)
}

// evacuateNote is the Result of "evacuate" when every VM moved.
func evacuateNote(moved int) string {
	if moved == 0 {
		return "no VMs to move"
	}
	return fmt.Sprintf("%d %s moved", moved, plural(moved, "VM", "VMs"))
}

// rollNote is the Result of a finished rolling update: how many nodes were updated and how many were left alone.
func rollNote(r *rollRun) string {
	var done, current, skipped int
	for _, n := range r.Nodes {
		switch n.Status {
		case "done":
			done++
		case "current":
			current++
		case "skipped":
			skipped++
		}
	}
	var f []string
	f = append(f, fmt.Sprintf("%d %s updated", done, plural(done, "node", "nodes")))
	if current > 0 {
		f = append(f, fmt.Sprintf("%d already up to date", current))
	}
	if skipped > 0 {
		f = append(f, fmt.Sprintf("%d left out", skipped))
	}
	return strings.Join(f, ", ")
}

// replicateResult ends the replicate task. res is what finishFreeze made of the run: an error, a note
// (freeze, TPM) or nil. A good run shows what was sent and where, then those notes.
func replicateResult(r *replRun, res error) error {
	var dn doneNote
	if res != nil && !errors.As(res, &dn) {
		return res
	}
	fact := "up to date"
	if !r.Skipped {
		fact = joinFacts(r.Kind, fmtBytes(r.Sent), "to "+r.dst.Name)
	}
	if dn != "" {
		fact += "; " + string(dn)
	}
	return taskDone("%s", fact)
}

// restoreNote is the Result of "restore": the VM, the node, and whether it was started.
func restoreNote(p *restorePlan) string {
	state := ""
	if p.req.Start {
		state = "running"
	}
	return joinFacts(p.name, "on "+p.dst.Name, state)
}

// backupFact is what a finished backup is: "full, 3.2 GiB" (the bytes stored in the backup).
func backupFact(kind string, disks []bkDisk) string {
	var total uint64
	for _, d := range disks {
		total += d.Size
	}
	if total == 0 {
		return kind
	}
	return joinFacts(kind, fmtBytes(total))
}

// backupDeleteNote is the Result of "backup delete": the backup, and how many later ones went with it.
func backupDeleteNote(name string, later int) string {
	if later == 0 {
		return name
	}
	return fmt.Sprintf("%s, %d later %s", name, later, plural(later, "backup", "backups"))
}

// backupImportNote is the Result of "backup import": the name the backup was stored under.
func backupImportNote(res *map[string]any) string {
	if res == nil || *res == nil {
		return ""
	}
	return fmt.Sprint((*res)["dir"])
}

// templateNote is the Result of turning a VM into a template or back, or of sealing one: what it is now, and the
// guest family it was sealed for.
func templateNote(template bool, sealed string) string {
	if !template {
		return "VM"
	}
	if sealed == "" {
		return "template"
	}
	return "template, sealed for " + sealed
}

// tagsNote is the Result of "set tags".
func tagsNote(tags []string) string {
	if len(tags) == 0 {
		return "no tags"
	}
	return strings.Join(tags, ", ")
}
