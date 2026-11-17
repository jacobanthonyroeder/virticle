package api

// Restoring a backup as a new VM or as the original, on any node.
//
// The disks are staged in a work directory inside the target pool and only
// moved into place once all of them are there; the domain is defined last.
// A restore as the original replaces an existing VM of that name only when
// asked to, on the same node, with the VM shut off; the old VM is removed after
// the new disks are staged, never before.

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"regexp"
	"strings"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
)

type restoreReq struct {
	Mode       string `json:"mode"` // new | original
	Name       string `json:"name"` // the new VM's name (mode new)
	TargetNode int64  `json:"targetNode"`
	Pool       string `json:"pool"`
	Replace    bool   `json:"replace"` // original: replace an existing, shut-off VM of the same name on the target node
	Start      bool   `json:"start"`
	// Passphrase of the backup key, for a backup with encrypted files. It is used for this request only: never
	// stored, logged or put into the task.
	Passphrase string `json:"passphrase,omitempty"`
}

type restorePlan struct {
	s       *Server
	src     *backupRef
	man     *backupManifest
	chain   []chainLink // the backups this one rests on, the full one first; this backup is the last
	domXML  string
	req     restoreReq
	dst     *hostconn.Host
	dstID   int64
	remote  bool
	pd      poolDir
	pool    libvirt.StoragePool
	name    string
	vols    map[string]string // disk target -> volume file name
	oldPath map[string]string // disk target -> path in the backed-up definition
	media   []string          // CD-ROM / floppy images to eject
	nvram   string            // where the UEFI variables go
	old     *libvirt.Domain   // VM replaced by this restore
	ep      migEndpoint       // how the source reaches the target node
	st      *stopper          // a stop request from the task list (nil: the restore cannot be stopped)
	warn    []string
	needKey bool   // the chain has encrypted files
	id      []byte // the identity that decrypts them, for this restore only (wiped when it ends)
}

var reNvramPath = regexp.MustCompile(`<nvram\b[^>]*>([^<]*)</nvram>`)

func (s *Server) planRestore(src *backupRef, req restoreReq) (*restorePlan, error) {
	man, err := src.load()
	if err != nil {
		return nil, err
	}
	p := &restorePlan{s: s, src: src, man: man, req: req, vols: map[string]string{}, oldPath: map[string]string{}}
	if p.chain, err = src.loadChain(man); err != nil {
		return nil, err
	}
	raw, err := readLimited(src.h, src.dir+"/domain.xml", backupMaxXML)
	if err != nil {
		return nil, badRequest(err.Error())
	}
	p.domXML = string(raw)
	md, err := parseMigDomain(p.domXML)
	if err != nil {
		return nil, badRequest("domain.xml is not a valid domain definition: " + err.Error())
	}
	for _, d := range md.Disks {
		if d.Path != "" {
			p.oldPath[d.Target] = d.Path
			if d.Device == "cdrom" || d.Device == "floppy" {
				p.media = append(p.media, d.Path)
			}
		}
	}
	for _, d := range man.Disks {
		if p.oldPath[d.Target] == "" {
			return nil, badRequest("domain.xml has no disk " + d.Target + " that the manifest lists")
		}
		for _, l := range p.chain {
			ld, ok := l.diskOf(d.Target)
			if !ok {
				return nil, badRequest("the backup " + l.Dir + " that this one builds on has no disk " + d.Target)
			}
			if ld.Encrypted {
				continue // checked once decrypted
			}
			info, err := bkQemuInfo(src.h, l.path(src)+"/"+ld.File) // refuses backing files and the like
			if err != nil {
				return nil, fmt.Errorf("%s: %w", ld.File, err)
			}
			if info.Format != "qcow2" {
				return nil, badRequest(ld.File + " is not a qcow2 image")
			}
		}
	}

	// Target.
	p.dstID = req.TargetNode
	if p.dstID == 0 {
		p.dstID = src.id
	}
	if _, ok := s.nodeStatus(p.dstID); !ok {
		return nil, badRequest("no such target node")
	}
	if p.dst, err = s.Nodes.Host(p.dstID); err != nil {
		return nil, err
	}
	p.remote = p.dstID != src.id
	if p.pool, p.pd, err = destPool(p.dst, req.Pool); err != nil {
		return nil, err
	}
	if !safePathRE.MatchString(p.pd.Dir) {
		return nil, badRequest("unsupported characters in the pool directory " + p.pd.Dir)
	}

	// Identity.
	switch req.Mode {
	case "new":
		if !vmNameRE.MatchString(req.Name) {
			return nil, badRequest("name must start with a letter or digit and contain only letters, digits, dot, dash and underscore")
		}
		p.name = req.Name
		if _, err := p.dst.DomainLookupByName(p.name); err == nil {
			return nil, badRequest("a VM named " + p.name + " already exists on " + p.dst.Name)
		}
	case "original":
		p.name = man.VM
		if err := p.checkOriginal(); err != nil {
			return nil, err
		}
	default:
		return nil, badRequest(`mode must be "new" or "original"`)
	}

	// Volumes.
	var need uint64 = importFreeMargin
	for i, d := range man.Disks {
		v := p.name + ".qcow2"
		if i > 0 {
			v = p.name + "-" + d.Target + ".qcow2"
		}
		if !volNameRE.MatchString(v) {
			return nil, badRequest("volume name " + v + " is not valid")
		}
		p.vols[d.Target] = v
		need += p.chainBytes(d.Target)
		if p.old != nil {
			continue // the replaced VM's files go away first; checked against its disks below
		}
		if _, err := p.dst.StorageVolLookupByName(p.pool, v); err == nil {
			return nil, badRequest("a volume named " + v + " already exists in " + req.Pool + " on " + p.dst.Name)
		}
		if ex, err := p.dst.Exists(p.pd.Dir + "/" + v); err != nil || ex[p.pd.Dir+"/"+v] {
			return nil, badRequest("a file named " + v + " already exists in " + p.pd.Dir + " on " + p.dst.Name)
		}
	}
	if p.old != nil {
		if err := p.checkReplaceFiles(); err != nil {
			return nil, err
		}
	}
	var decrypted uint64 // the plain copies of encrypted files that are put together or sent on
	if p.needKey = chainNeedsKey(p.chain); p.needKey {
		if err := s.checkRecipients(p.chain); err != nil {
			return nil, err
		}
		if err := src.h.CanAge(); err != nil {
			return nil, badRequest("this backup is encrypted, but " + err.Error())
		}
		for _, l := range p.chain {
			for _, d := range l.Man.Disks {
				if d.Encrypted && (len(p.chain) > 1 || p.remote) {
					decrypted += d.PlainBytes
				}
			}
		}
		if !p.remote {
			need += decrypted
		}
	}
	if man.NVRAM != "" {
		if err := p.planNVRAM(md); err != nil {
			return nil, err
		}
	}
	free, err := bkFreeBytes(p.dst, p.pd.Dir)
	if err != nil {
		return nil, err
	}
	if uint64(free) < need {
		return nil, badRequest(fmt.Sprintf("not enough free space in %s on %s: %s available, about %s needed", p.pd.Dir, p.dst.Name, fmtBytes(uint64(free)), fmtBytes(need)))
	}
	if len(p.media) > 0 {
		p.warn = append(p.warn, "install media/CD-ROM images are ejected: "+strings.Join(p.media, ", "))
	}
	if w := tpmRestoreWarning(man); w != "" {
		p.warn = append(p.warn, w)
	}
	if p.remote {
		dn, _ := s.nodeStatus(p.dstID)
		if p.ep, err = s.checkBackupTransfer(src.h, p.dst, dn.Node); err != nil {
			return nil, err
		}
	}
	if len(p.chain) > 1 || (p.remote && decrypted > 0) {
		// An incremental backup is put together on the node that holds the backups first, and encrypted files
		// are decrypted there.
		var flat uint64 = importFreeMargin + decrypted
		for _, d := range man.Disks {
			if len(p.chain) > 1 {
				flat += p.chainBytes(d.Target)
			}
		}
		if sf, err := bkFreeBytes(src.h, src.pd.Dir); err != nil || uint64(sf) < flat {
			return nil, badRequest(fmt.Sprintf("not enough free space in %s on %s to put the incremental backup together: about %s needed", src.pd.Dir, src.h.Name, fmtBytes(flat)))
		}
	}
	return p, nil
}

// checkOriginal refuses to restore as the original while a domain with the
// same name or UUID exists on any connected node, except for a shut-off
// VM on the target node when replace was asked for.
func (p *restorePlan) checkOriginal() error {
	u, _ := cluster.ParseUUID(p.man.UUID)
	// The standby of the VM that is being replaced holds its replica: the same identity, but it is the VM's own copy,
	// which the next replication run brings up to date, so it is no obstacle.
	replTo := int64(0)
	if hd, err := p.s.Nodes.Host(p.dstID); err == nil && p.req.Replace {
		if d, err := hd.DomainLookupByUUID(u); err == nil {
			if m, _ := domainMeta(hd, d); parseReplication(m) != nil {
				replTo = parseReplication(m).TargetNode
			}
		}
	}
	for _, n := range p.s.Nodes.Status() {
		h, err := p.s.Nodes.Host(n.ID)
		if err != nil {
			continue
		}
		byName, errN := h.DomainLookupByName(p.man.VM)
		byUUID, errU := h.DomainLookupByUUID(u)
		if n.ID == replTo && n.ID != p.dstID && errU == nil && errN == nil && byName.UUID == byUUID.UUID && isReplicaDomain(h, byUUID) {
			continue
		}
		if errN != nil && errU != nil {
			continue
		}
		where := h.Name
		// A replica is never replaced by a restore (it would be removed and leave its files behind).
		replaceable := errN == nil && errU == nil && byName.UUID == byUUID.UUID && n.ID == p.dstID && p.req.Replace
		if replaceable && isReplicaDomain(h, byName) {
			replaceable = false
		}
		if replaceable {
			st, _, _, _, _, err := h.DomainGetInfo(byName)
			if err != nil {
				return err
			}
			if stateName(st) != "shutoff" {
				return badRequest("the VM " + p.man.VM + " on " + where + " is not shut off; shut it down before replacing it")
			}
			if replRunning(cluster.FormatUUID(byName.UUID)) {
				return badRequest("the VM " + p.man.VM + " is being copied to its standby node right now; replace it when the copy is done")
			}
			p.old = &byName
			continue
		}
		hint := ""
		var cm string
		if errU == nil {
			cm, _ = domainMeta(h, byUUID)
		}
		switch {
		case parseReplicaMeta(cm) != nil:
			hint = " It is the replica of this VM kept for a failover: open it and use \"Delete replica\" first, or restore as a new VM."
		case parseStale(cm) != nil:
			hint = " It is the old copy of this VM from before a failover: delete it first, or restore as a new VM."
		case n.ID != p.dstID:
			hint = " Delete or migrate it first (the original identity can exist only once), or restore as a new VM."
		case !p.req.Replace:
			hint = " Choose \"replace\" to remove it and restore the backup in its place (it must be shut off), or restore as a new VM."
		default:
			hint = " Name and UUID belong to different VMs there."
		}
		return badRequest("a VM with this name or UUID (" + p.man.VM + ", " + p.man.UUID + ") already exists on " + where + "." + hint)
	}
	return nil
}

// checkReplaceFiles makes sure the new volume names are free except for
// files of the VM that is being replaced.
func (p *restorePlan) checkReplaceFiles() error {
	mine := map[string]bool{}
	if x, err := p.dst.DomainGetXMLDesc(*p.old, libvirt.DomainXMLInactive); err == nil {
		var dx domainXML
		if xml.Unmarshal([]byte(x), &dx) == nil {
			for _, f := range diskPaths(&dx) {
				mine[path.Clean(f)] = true
			}
		}
	}
	for _, v := range p.vols {
		f := p.pd.Dir + "/" + v
		if ex, err := p.dst.Exists(f); (err != nil || ex[f]) && !mine[f] {
			return badRequest("a file " + f + " exists on " + p.dst.Name + " that does not belong to the VM being replaced")
		}
	}
	return nil
}

func (p *restorePlan) planNVRAM(md migDomain) error {
	// The saved file is placed byte for byte, so the definition's format must
	// be the one the manifest recorded (no entry: an old backup, raw).
	if (p.man.NVRAMFormat == "qcow2") != (md.NVRAMFormat == "qcow2") {
		return badRequest("this backup is inconsistent: the manifest and the VM definition disagree about the format of the UEFI variables")
	}
	if p.req.Mode == "original" {
		p.nvram = md.NVRAM
	} else {
		ext := ".fd"
		if md.NVRAMFormat == "qcow2" {
			ext = ".qcow2"
		}
		p.nvram = "/var/lib/libvirt/qemu/nvram/" + p.name + "_VARS" + ext
	}
	if !safePathRE.MatchString(p.nvram) {
		return badRequest("unsupported characters in the NVRAM path " + p.nvram)
	}
	if st, err := p.dst.Stat(path.Dir(p.nvram)); err != nil || !st[0].Exists || st[0].Kind != "dir" {
		return badRequest("the directory " + path.Dir(p.nvram) + " for the UEFI variables does not exist on " + p.dst.Name + " (is OVMF installed?)")
	}
	if p.old == nil {
		if ex, err := p.dst.Exists(p.nvram); err != nil || ex[p.nvram] {
			return badRequest("the UEFI variable file " + p.nvram + " already exists on " + p.dst.Name)
		}
	}
	return nil
}

// definition builds the XML of the restored VM from the backed-up one.
func (p *restorePlan) definition() (string, error) {
	x := stripBackingStore(p.domXML)
	m := map[string]string{}
	for _, d := range p.man.Disks {
		m[p.oldPath[d.Target]] = p.pd.Dir + "/" + p.vols[d.Target]
	}
	for _, f := range p.media {
		m[f] = ""
	}
	if p.req.Mode == "new" {
		out, err := cloneDomainXML(x, cloneEdit{Name: p.name, Disks: m})
		if err != nil {
			return "", err
		}
		if p.nvram != "" {
			// A store that names itself as its template (see withQcow2Nvram) keeps doing so at its new path.
			if n, ok := parseNvramEl(x); ok && n.Template != "" && n.Template == n.Path {
				out = withNvramTemplate(out, p.nvram)
			}
			out = reNvramEl.ReplaceAllString(out, "${1}"+p.nvram+"${2}")
		}
		return out, nil
	}
	return remapDisks(x, m), nil
}

// remapDisks changes the file behind disks in a definition, or empties the
// device when the new path is "".
func remapDisks(x string, m map[string]string) string {
	return reDiskEl.ReplaceAllStringFunc(x, func(d string) string {
		sm := reFileSrc.FindStringSubmatch(d)
		if sm == nil {
			return d
		}
		np, ok := m[xmlUnescape.Replace(sm[2])]
		switch {
		case !ok:
			return d
		case np == "":
			return strings.Replace(d, sm[0], "", 1)
		}
		return strings.Replace(d, sm[0], strings.Replace(sm[0], sm[1]+sm[2]+sm[3], "'"+esc(np)+"'", 1), 1)
	})
}

func (s *Server) restoreBackup(w http.ResponseWriter, r *http.Request) {
	src, err := s.backupFromURL(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req restoreReq
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	// Retention keeps the backup, and what it builds on, until the restore has read it.
	release := holdRead(src.pd.Dir, src.name)
	defer func() {
		if release != nil {
			release()
		}
	}()
	pass := req.Passphrase
	req.Passphrase = ""
	p, err := s.planRestore(src, req)
	if err != nil {
		fail(w, err)
		return
	}
	// Building the definition now catches problems before anything is copied.
	if _, err := p.definition(); err != nil {
		fail(w, badRequest("cannot prepare the VM definition: "+err.Error()))
		return
	}
	if p.needKey {
		// A wrong passphrase is refused here, before anything is changed.
		if p.id, err = s.unlockBackupKey(r, pass); err != nil {
			fail(w, err)
			return
		}
		infoOf(r).detail = "restore of an encrypted backup"
	}
	p.st = newStopper(context.Background())
	p.dst, p.src.h = s.traceHost(r, p.dst), s.traceHost(r, p.src.h)
	rel := release
	user := userOf(r)
	id, _, err := s.startBackgroundStop(r, "restore", p.name+" ← "+src.name, p.dst.Name, taskStop{operator, p.st.stop}, func(progress func(float64)) error {
		defer rel()
		defer wipe(p.id)
		_, err := p.run(progress)
		if err == nil && p.req.Start {
			if dom, lerr := p.dst.DomainLookupByName(p.name); lerr == nil {
				s.syncClockAfterStart(user, p.dst, dom)
			}
		}
		return doneOr(err, "%s", restoreNote(p))
	})
	if err == nil {
		release = nil // the task releases it
	}
	if err != nil {
		wipe(p.id)
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"task": id, "name": p.name, "warnings": append([]string{}, p.warn...)})
}

// run performs the restore and returns the UUID of the defined VM.
func (p *restorePlan) run(progress func(float64)) (uuid string, err error) {
	dst, src := p.dst, p.src.h
	st := p.st
	if st == nil {
		st = newStopper(context.Background())
	}
	defer st.done()
	progress(0.01)
	if st.Stopped() {
		return "", stoppedErr("restore")
	}
	wd, err := newWork(dst, p.pd.Dir, "rs")
	if err != nil {
		return "", err
	}
	// A stop ends qemu-img or the transfer and deletes the work directory, which holds everything staged so
	// far; the backup is only read. From the moment the disks are moved into place (commit) it changes nothing.
	wd.stop = st.Stopped
	defer st.onStop(func() { wd.remove() })()
	var moved []hostconn.FileID      // the disks this restore put into the pool, with their identity
	var nvramPlaced *hostconn.FileID // the UEFI variable store it made
	var defined *libvirt.Domain
	oldGone, kept := false, false
	defer func() {
		if err == nil || kept {
			wd.remove()
			return
		}
		if st.Stopped() {
			err = stoppedErr("restore") // what failed is only the consequence of the stop
		}
		var left []string
		if defined != nil {
			// The UEFI variable store is removed below, by its identity, not by libvirt by name.
			f := libvirt.DomainUndefineKeepNvram | libvirt.DomainUndefineManagedSave | libvirt.DomainUndefineSnapshotsMetadata | libvirt.DomainUndefineTpm
			if uerr := dst.DomainUndefineFlags(*defined, f); uerr != nil {
				left = append(left, "VM definition "+p.name+": "+uerr.Error())
			}
		}
		// Only what this restore made, and only while it is still that file and nothing holds it open.
		for _, f := range moved {
			if why := removeOwnFile(dst, f); why != "" {
				left = append(left, f.Path+" ("+why+")")
			}
		}
		if nvramPlaced != nil {
			if why := removeOwnFile(dst, *nvramPlaced); why != "" {
				left = append(left, nvramPlaced.Path+" ("+why+")")
			}
		}
		if rerr := wd.remove(); rerr != nil {
			left = append(left, wd.path)
		}
		poolsByPath(dst)
		if oldGone {
			left = append(left, "NOTE: the VM that was to be replaced had already been removed; restore the backup again")
		}
		note := "rolled back"
		if len(left) > 0 {
			note += "; left behind: " + strings.Join(left, ", ")
		}
		err = withNote(err, note)
	}()

	// Stage the disks.
	var total uint64
	for _, d := range p.man.Disks {
		total += p.chainBytes(d.Target) + 1
	}
	var done uint64
	staged := map[string]string{}
	// A restore of an incremental backup puts the chain together on the node
	// that holds the backups, in a work directory of its own.
	var srcWd *bkWork
	if (len(p.chain) > 1 || p.needKey) && p.remote {
		if srcWd, err = newWork(src, p.src.pd.Dir, "rs"); err != nil {
			return "", err
		}
		defer srcWd.remove()
		srcWd.stop = st.Stopped
		defer st.onStop(func() { srcWd.remove() })()
	}
	// Encrypted files are decrypted on the node that holds the backup: into the restore's work directory there.
	decWd := wd
	if p.remote {
		decWd = srcWd
	}
	for _, d := range p.man.Disks {
		if st.Stopped() {
			return "", stoppedErr("restore")
		}
		stage := wd.file(p.vols[d.Target])
		base, span := done, p.chainBytes(d.Target)+1
		prog := func(f float64) { progress(0.02 + 0.8*(float64(base)+f*float64(span))/float64(total)) }
		var files, temps []string
		// A single encrypted backup restored on its own node is decrypted straight into place; otherwise the
		// decryption takes the first half of the disk's share of the bar.
		direct := len(p.chain) == 1 && !p.remote && p.chain[0].hasEncryptedDisk(d.Target)
		decShare := 0.5
		if direct {
			decShare = 1
		}
		var read uint64
		for i, l := range p.chain {
			ld, _ := l.diskOf(d.Target)
			f := l.path(p.src) + "/" + ld.File
			if ld.Encrypted {
				out := stage
				if !direct {
					out = decWd.file(fmt.Sprintf("dec%d-%s", i, strings.TrimSuffix(ld.File, ageSuffix)))
					temps = append(temps, out)
				}
				at := read
				if derr := decryptFile(st, src, f, out, diskSums(ld), p.id, func(n uint64) {
					prog(decShare * float64(at+n) / float64(span))
				}); derr != nil {
					return "", st.cancelled("restore", fmt.Errorf("decrypting %s of the backup %s: %w", ld.File, l.Dir, derr))
				}
				if info, ierr := bkQemuInfo(src, out); ierr != nil || info.Format != "qcow2" { // refuses backing files and the like
					return "", fmt.Errorf("%s of the backup %s does not decrypt to a qcow2 image", ld.File, l.Dir)
				}
				f = out
			} else if ld.SHA256 != "" {
				sum, serr := src.SHA256Ctx(st.Ctx(), f)
				if serr != nil {
					return "", serr
				}
				if sum != ld.SHA256 {
					return "", fmt.Errorf("%s of the backup %s is damaged: its SHA-256 is %s, the manifest says %s", ld.File, l.Dir, sum, ld.SHA256)
				}
			}
			read += ld.Bytes
			files = append(files, f)
		}
		if len(temps) > 0 {
			// What follows (putting together, sending) moves the bar from the end of the decryption.
			at := prog
			prog = func(f float64) { at(0.5 + 0.5*f) }
		}
		switch {
		case direct:
			// decrypted into place
		case p.remote && len(p.chain) == 1:
			if cerr := copyToNode(st.Ctx(), src, dst, p.ep, files[0], stage, "600", func(sent uint64) {
				prog(float64(sent) / float64(span))
			}); cerr != nil {
				return "", fmt.Errorf("copying %s to %s failed: %w", d.File, dst.Name, cerr)
			}
		case p.remote:
			flat := srcWd.file(p.vols[d.Target])
			if cerr := srcWd.convertChain(files, flat, func(f float64) { prog(f * 0.5) }); cerr != nil {
				return "", fmt.Errorf("putting %s together: %w", d.File, cerr)
			}
			if cerr := copyToNode(st.Ctx(), src, dst, p.ep, flat, stage, "600", func(sent uint64) {
				prog(0.5 + 0.5*float64(sent)/float64(span))
			}); cerr != nil {
				return "", fmt.Errorf("copying %s to %s failed: %w", d.File, dst.Name, cerr)
			}
			_ = src.Remove(flat)
		default:
			if cerr := wd.convertChain(files, stage, prog); cerr != nil {
				return "", fmt.Errorf("copying %s: %w", d.File, cerr)
			}
		}
		for _, t := range temps {
			_ = src.Remove(t) // the plain copies go as soon as the disk is staged (the work directory goes anyway)
		}
		if info, ierr := bkQemuInfo(dst, stage); ierr != nil || info.Format != "qcow2" {
			return "", fmt.Errorf("the copy of %s is not a valid qcow2 image on %s", d.File, dst.Name)
		}
		done += p.chainBytes(d.Target) + 1
		staged[d.Target] = stage
		progress(0.02 + 0.8*float64(done)/float64(total))
	}
	nvramStage := ""
	if p.man.NVRAM != "" {
		nvramStage = wd.file(backupNVRAMFile)
		srcFile := p.src.dir + "/" + p.man.NVRAM
		var cerr error
		switch {
		case p.man.NVRAMEncrypted:
			cerr = p.stageEncrypted(st, srcFile, nvramStage, srcWd, fileSums{Bytes: p.man.NVRAMBytes, SHA: p.man.NVRAMSHA256, PlainBytes: p.man.NVRAMPlainBytes, PlainSHA: p.man.NVRAMPlainSHA256})
		case p.remote:
			cerr = copyToNode(st.Ctx(), src, dst, p.ep, srcFile, nvramStage, "600", nil)
		default:
			cerr = dst.Copy(srcFile, nvramStage)
		}
		if cerr != nil {
			return "", st.cancelled("restore", fmt.Errorf("copying the UEFI variables: %s", impFirstLine(cerr.Error())))
		}
	}
	tpmStage, err := p.stageTPM(st, wd, srcWd)
	if err != nil {
		return "", st.cancelled("restore", err)
	}
	xmlText, err := p.definition()
	if err != nil {
		return "", err
	}
	// Point of no return: from here on the VM that is replaced goes away and the disks are moved into place.
	if !st.commit() {
		return "", stoppedErr("restore")
	}

	// Replace: only now, with everything staged, remove the old VM. Its replication setting is carried over (the
	// next run copies the restored disks whole), because the backup's own metadata is a picture of the past.
	oldMeta := ""
	if p.old != nil {
		oldMeta, _ = domainMeta(dst, *p.old)
	}
	if p.old != nil {
		oldGone = true
		if err := p.removeOld(); err != nil {
			if _, lerr := dst.DomainLookupByUUID(p.old.UUID); lerr == nil {
				oldGone = false // refused before anything was removed: the VM is there as it was
			}
			return "", err
		}
	}
	progress(0.88)

	// The name may have been taken since the plan (another restore, a clone, a migration): never define over it.
	if why := restoreNameTaken(dst, p.name, xmlText); why != "" {
		return "", errors.New(why)
	}
	for _, d := range p.man.Disks {
		f := p.pd.Dir + "/" + p.vols[d.Target]
		// The identity of the staged file, which is this restore's own (in its work directory); the move keeps it.
		// A file is recorded as the restore's only once the move, which never replaces anything, has put it there.
		ids, ierr := dst.FileIDs(staged[d.Target])
		if ierr != nil || len(ids) != 1 || !ids[0].Regular {
			return "", fmt.Errorf("cannot identify the staged copy of %s: %v", f, ierr)
		}
		if merr := dst.Move(staged[d.Target], f); merr != nil {
			return "", fmt.Errorf("moving %s into the pool: %s", f, impFirstLine(merr.Error()))
		}
		id := ids[0]
		id.Path = f
		moved = append(moved, id)
	}
	if nvramStage != "" {
		if cerr := dst.Copy(nvramStage, p.nvram); cerr != nil {
			return "", fmt.Errorf("placing the UEFI variables at %s: %s", p.nvram, impFirstLine(cerr.Error()))
		}
		// The copy was made exclusively just now; its identity proves it later.
		if ids, ierr := dst.FileIDs(p.nvram); ierr == nil && len(ids) == 1 && ids[0].Exists {
			nvramPlaced = &ids[0]
		} else {
			nvramPlaced = &hostconn.FileID{Path: p.nvram, Exists: true}
		}
	}
	dst.StoragePoolRefresh(p.pool, 0)
	dom, derr := dst.DomainDefineXMLFlags(xmlText, libvirt.DomainDefineValidate)
	if derr != nil {
		return "", fmt.Errorf("defining the VM on %s: %w", dst.Name, derr)
	}
	defined = &dom
	uuid = cluster.FormatUUID(dom.UUID)
	if tpmStage != "" {
		// The state belongs to the VM's UUID, which a restore as a new VM has only now.
		if terr := dst.TPMPlace(tpmStage, uuid); terr != nil {
			return "", fmt.Errorf("placing the TPM state of the VM on %s: %w", dst.Name, terr)
		}
	}
	if perr := restorePlacement(dst, dom, oldMeta); perr != nil {
		log.Printf("restore %s: replication settings: %v", p.name, perr)
	}
	if p.req.Mode == "new" {
		// A new VM's definition drops the app metadata (cloneDomainXML); a hardware clock switch that waited when
		// the backup was made still waits, as for a clone (winclock.go).
		if sw := defClockSwitch(p.domXML); sw != "" {
			if serr := setClockSwitch(dst, dom, sw); serr != nil {
				log.Printf("restore %s: hardware clock switch: %v", p.name, serr)
			}
		}
	}
	progress(0.97)
	if p.req.Start {
		if serr := dst.DomainCreate(dom); serr != nil {
			// The restore itself worked; keep it.
			kept = true
			return uuid, fmt.Errorf("the VM was restored but did not start: %w", serr)
		}
	}
	return uuid, nil
}

// removeOld undefines the VM this restore replaces and deletes its disks.
func (p *restorePlan) removeOld() error { return removeVMAndDisks(p.dst, *p.old) }

// removeVMAndDisks undefines a shut-off VM with its UEFI variables, snapshot and checkpoint records and TPM state,
// then deletes its disks, by the rules of a normal delete (vmDelete): writable disks that are pool volumes, not
// shareable ones, and the cloud-init seed; a disk that linked clones are built on stops it. The state is checked
// again here, at the moment of removal, not only when the work was planned: a VM started meanwhile, or one being
// copied to its standby node, is left alone. Each disk is removed only while it is the file it was when the VM
// was looked at, and while no process (QEMU) holds it open; what stays is named in the error.
func removeVMAndDisks(dst *hostconn.Host, dom libvirt.Domain) error {
	if st, _, err := dst.DomainGetState(dom, 0); err != nil {
		return fmt.Errorf("cannot check the existing VM %s: %w", dom.Name, err)
	} else if stateName(uint8(st)) != "shutoff" {
		return fmt.Errorf("the existing VM %s is not shut off any more; it was not removed", dom.Name)
	}
	if replRunning(cluster.FormatUUID(dom.UUID)) {
		return fmt.Errorf("the existing VM %s is being copied to its standby node; it was not removed", dom.Name)
	}
	x, err := dst.DomainGetXMLDesc(dom, libvirt.DomainXMLInactive)
	if err != nil {
		return err
	}
	var dx domainXML
	if err := xml.Unmarshal([]byte(x), &dx); err != nil {
		return err
	}
	var paths []string
	for _, f := range ownDiskFiles(&dx, dom.Name) {
		if _, err := lookupVolByPath(dst, f); err != nil {
			continue // not a pool volume: a normal delete leaves it too
		}
		paths = append(paths, f)
	}
	if deps, err := dependentVolumes(dst, paths, paths); err != nil {
		return fmt.Errorf("cannot check what is built on the disks of %s: %w", dom.Name, err)
	} else if len(deps) > 0 {
		return fmt.Errorf("the existing VM %s was not removed: %s", dom.Name, dependentsError(deps).Error())
	}
	ids, err := dst.FileIDs(paths...)
	if err != nil {
		return fmt.Errorf("cannot identify the disks of %s: %w", dom.Name, err)
	}
	f := libvirt.DomainUndefineManagedSave | libvirt.DomainUndefineSnapshotsMetadata | libvirt.DomainUndefineNvram |
		libvirt.DomainUndefineCheckpointsMetadata | libvirt.DomainUndefineTpm
	if err := dst.DomainUndefineFlags(dom, f); err != nil {
		return fmt.Errorf("removing the existing VM: %w", err)
	}
	var left []string
	for _, id := range ids {
		if !id.Exists {
			continue
		}
		if why := removeOwnFile(dst, id); why != "" {
			left = append(left, id.Path+" ("+why+")")
		}
	}
	refreshLive(dst) // libvirt forgets the removed volumes
	if len(left) > 0 {
		return fmt.Errorf("the existing VM was removed but these of its disks were kept: %s", strings.Join(left, ", "))
	}
	return nil
}

// ownDiskFiles are the files of a VM's definition that go with it when it is deleted: its writable, not shareable
// disks, and its cloud-init seed (the CD-ROM named after it). Read-only and shareable disks are shared images.
func ownDiskFiles(dx *domainXML, name string) []string {
	var out []string
	for _, k := range dx.Devices.Disks {
		f := k.Source.File
		if f == "" {
			continue
		}
		if k.Device == "disk" && k.ReadOnly == nil && k.Shareable == nil || k.Device == "cdrom" && path.Base(f) == seedVolumeName(name) {
			out = append(out, f)
		}
	}
	return out
}

// restoreNameTaken says why the VM must not be defined now: a VM with its name, or with the UUID the definition
// carries, exists on the node (defining would change that VM instead). "" when neither does.
func restoreNameTaken(h *hostconn.Host, name, def string) string {
	if _, err := h.DomainLookupByName(name); err == nil {
		return "a VM named " + name + " appeared on " + h.Name + " meanwhile; nothing was replaced"
	} else if !isNoDomain(err) {
		return "cannot check whether a VM named " + name + " exists on " + h.Name + ": " + err.Error()
	}
	var x struct {
		UUID string `xml:"uuid"`
	}
	if xml.Unmarshal([]byte(def), &x) == nil && strings.TrimSpace(x.UUID) != "" {
		u, err := parseUUID(strings.TrimSpace(x.UUID))
		if err != nil {
			return "the definition has an invalid UUID"
		}
		if d, err := h.DomainLookupByUUID(u); err == nil {
			return "the VM " + d.Name + " with the UUID of the restored VM appeared on " + h.Name + " meanwhile; nothing was replaced"
		} else if !isNoDomain(err) {
			return "cannot check the UUID of the restored VM on " + h.Name + ": " + err.Error()
		}
	}
	return ""
}

// removeOwnFile removes the file id names, only while it is still that file and no process holds it open. The
// answer is "" when it is gone, or why it stays.
func removeOwnFile(h *hostconn.Host, id hostconn.FileID) string {
	if !id.Regular || id.ID == "" {
		return "its file system gives no identity that proves it is still the same file"
	}
	ans, err := h.RemoveIfID(id.Path, id.ID)
	return keptBecause(h.Name, ans, err)
}

// keptBecause reads the answer of remove-id: "" when the file is gone (removed, or nothing was there), otherwise
// why it stays.
func keptBecause(node, ans string, err error) string {
	switch {
	case err != nil:
		return err.Error()
	case ans == hostconn.RemovedFile, ans == hostconn.MissingFile:
		return ""
	case ans == hostconn.InUseFile:
		return "a process on " + node + " has it open"
	case ans == hostconn.OtherFile:
		return "another file has its name now"
	}
	return "unexpected answer " + ans
}

// restorePlacement removes what a backup remembers of where the VM was placed (replication, replica, old-copy and
// failed-over records are runtime state of the cluster, not part of the VM) and puts back the replication setting
// of the VM that was replaced, if it had one.
func restorePlacement(h *hostconn.Host, dom libvirt.Domain, replacedMeta string) error {
	meta, err := domainMeta(h, dom)
	if err != nil {
		return err
	}
	changes := map[string]string{replChild: "", replicaChild: "", staleChild: "", failedOverChd: ""}
	if cfg := parseReplication(replacedMeta); cfg != nil {
		cfg.PausedCopy = false
		changes[replChild] = cfg.element()
	}
	nm, err := setMetaChildren(meta, changes)
	if err != nil {
		return err
	}
	if nm == meta {
		return nil
	}
	return setDomainMeta(h, dom, nm)
}
