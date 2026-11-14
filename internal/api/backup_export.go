package api

// Export and import of a backup as one file: an uncompressed tar of the
// backup directory. The tar is made on the node and streamed through this
// server to the browser; an uploaded tar is vetted on the way through (see
// vetTar) and unpacked on the node into a work directory inside a pool, so
// nothing is held in memory or on the controller.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"virticle/internal/hostconn"
)

// streamOut sends the first write's headers lazily, so a failure before any
// data can still be reported as an ordinary error.
type streamOut struct {
	w        http.ResponseWriter
	filename string
	total    uint64
	n        atomic.Uint64
	progress func(float64)
	started  bool
}

func (o *streamOut) Write(p []byte) (int, error) {
	if !o.started {
		o.started = true
		h := o.w.Header()
		h.Set("Content-Type", "application/x-tar")
		h.Set("Content-Disposition", `attachment; filename="`+o.filename+`"`)
		o.w.WriteHeader(http.StatusOK)
	}
	n, err := o.w.Write(p)
	done := o.n.Add(uint64(n))
	if o.total > 0 && o.progress != nil {
		o.progress(float64(done) / float64(o.total))
	}
	return n, err
}

func (s *Server) exportBackup(w http.ResponseWriter, r *http.Request) {
	b, err := s.backupFromURL(r)
	if err != nil {
		fail(w, err)
		return
	}
	m, err := b.load()
	if err != nil {
		fail(w, err)
		return
	}
	var chain []chainLink
	if m.kindOf() == kindIncremental {
		if chain, err = b.loadChain(m); err != nil {
			fail(w, err)
			return
		}
	}
	var total uint64
	names := m.files()
	for _, d := range m.Disks {
		total += d.Bytes
	}
	// Explicit plain member names, so the archive can be vetted on import.
	out := &streamOut{w: w, filename: m.VM + ".vmexport.tar", total: total}
	st := newStopper(r.Context())
	defer st.done()
	err = s.taskStop(r, "backup export", b.name, b.h.Name, taskStop{operator, st.stop}, func(progress func(float64)) (rerr error) {
		defer func() { rerr = st.cancelled("backup export", rerr) }()
		dir := b.dir
		out.progress = progress
		if chain != nil {
			// An export is one self-contained file, so an incremental backup is put
			// together with the ones it builds on first (a full backup in the export).
			flat, fm, err := b.flatten(m, chain, st, func(f float64) { progress(f * 0.5) })
			if err != nil {
				return err
			}
			defer flat.remove()
			dir, names, out.total = flat.path, fm.files(), 0
			for _, d := range fm.Disks {
				out.total += d.Bytes
			}
			out.progress = func(f float64) { progress(0.5 + f*0.5) }
		}
		return b.h.TarPack(st.Ctx(), dir, names, out)
	})
	if err != nil {
		if !out.started {
			fail(w, err)
		} else {
			logf("export of %s failed after %d bytes: %v", b.dir, out.n.Load(), err)
		}
	}
}

// flatten puts the disks of an incremental backup together with the backups it
// builds on, into a work directory in the same pool, with a manifest that
// describes a full backup. The caller removes the directory.
func (b *backupRef) flatten(m *backupManifest, chain []chainLink, st *stopper, progress func(float64)) (wd *bkWork, fm *backupManifest, err error) {
	// w, not wd: a failing return sets the named result wd to nil before the deferred functions run.
	w, err := newWork(b.h, b.pd.Dir, "ex")
	if err != nil {
		return nil, nil, err
	}
	wd = w
	defer func() {
		if err != nil {
			w.remove()
		}
	}()
	// A stop ends qemu-img and deletes the work directory.
	w.stop = st.Stopped
	defer st.onStop(func() { w.remove() })()
	cp := *m
	cp.Format, cp.Kind, cp.Parent, cp.Checkpoint, cp.Disks = backupFormat, kindFull, "", "", nil
	cp.FullReason = fmt.Sprintf("Put together from %d backups when exported.", len(chain))
	for i, d := range m.Disks {
		var files []string
		for _, l := range chain {
			ld, ok := l.diskOf(d.Target)
			if !ok {
				return nil, nil, badRequest("the backup " + l.Dir + " has no disk " + d.Target)
			}
			files = append(files, l.path(b)+"/"+ld.File)
		}
		lo, n := float64(i), float64(len(m.Disks))
		if err = wd.convertChain(files, wd.file(d.File), func(f float64) { progress((lo + f) / n) }); err != nil {
			return nil, nil, fmt.Errorf("putting %s together: %w", d.File, err)
		}
		info, err := bkQemuInfo(b.h, wd.file(d.File))
		if err != nil {
			return nil, nil, err
		}
		st, err := b.h.Stat(wd.file(d.File))
		if err != nil || !st[0].Exists {
			return nil, nil, fmt.Errorf("cannot read %s: %v", d.File, err)
		}
		cp.Disks = append(cp.Disks, backupDiskRec{Target: d.Target, File: d.File, Format: "qcow2", VirtualSize: uint64(info.VirtualSize), Bytes: st[0].Size, Source: d.Source})
	}
	if err = b.h.Copy(b.dir+"/domain.xml", wd.file("domain.xml")); err != nil {
		return nil, nil, err
	}
	if m.NVRAM != "" {
		if err = b.h.Copy(b.dir+"/"+m.NVRAM, wd.file(m.NVRAM)); err != nil {
			return nil, nil, err
		}
	}
	mj, _ := json.MarshalIndent(&cp, "", "  ")
	if err = writeNodeFile(b.h, wd.file("manifest.json"), mj); err != nil {
		return nil, nil, err
	}
	return wd, &cp, nil
}

// importBackup stores an uploaded export in a pool as a backup directory.
// Restoring from it is a separate step.
func (s *Server) importBackup(w http.ResponseWriter, r *http.Request) {
	_, h, err := s.host(r)
	if err != nil {
		fail(w, err)
		return
	}
	_, pd, err := destPool(h, r.URL.Query().Get("pool"))
	if err != nil {
		fail(w, err)
		return
	}
	if !safePathRE.MatchString(pd.Dir) {
		fail(w, badRequest("unsupported characters in the pool directory "+pd.Dir))
		return
	}
	free, err := bkFreeBytes(h, pd.Dir)
	if err != nil {
		fail(w, err)
		return
	}
	budget := free - importFreeMargin
	if budget <= 0 || (r.ContentLength > 0 && r.ContentLength > budget) {
		fail(w, badRequest(fmt.Sprintf("not enough free space in %s: %d MiB available, the upload is %d MiB", pd.Dir, free>>20, r.ContentLength>>20)))
		return
	}
	var res map[string]any
	// A stop ends the unpacking on the node (the session is hung up), cuts the read of the upload short and
	// deletes the work directory; nothing reaches the pool before the archive is complete and checked.
	st := newStopper(r.Context())
	defer st.done()
	rc := http.NewResponseController(w)
	st.onStop(func() { rc.SetReadDeadline(time.Now()) })
	err = s.taskStop(r, "backup import", r.URL.Query().Get("pool"), h.Name, taskStop{operator, st.stop}, func(progress func(float64)) (rerr error) {
		defer func() { rerr = st.cancelled("backup import", rerr) }()
		wd, err := newWork(h, pd.Dir, "imp")
		if err != nil {
			return err
		}
		defer func() {
			if rerr != nil {
				cerr := wd.remove()
				if cerr != nil && st.Stopped() {
					// The unpacking on the node may still write for a moment.
					time.Sleep(time.Second)
					cerr = wd.remove()
				}
				if cerr != nil {
					rerr = withNote(rerr, "work directory left behind: "+wd.path)
				}
			}
		}()
		pr, pw := io.Pipe()
		type vetResult struct {
			names []string
			err   error
		}
		vetCh := make(chan vetResult, 1)
		go func() {
			names, err := vetTar(r.Body, pw, budget)
			pw.CloseWithError(err)
			vetCh <- vetResult{names, err}
		}()
		perr := h.TarUnpack(st.Ctx(), wd.path, pr)
		pr.CloseWithError(errors.New("extraction ended"))
		v := <-vetCh
		var br badRequest
		switch {
		case errors.As(v.err, &br):
			return v.err
		case perr != nil:
			return fmt.Errorf("unpacking the archive on %s: %w", h.Name, perr)
		case v.err != nil:
			return v.err
		}
		progress(0.8)
		if !st.commit() { // the checks and the move into place are short: a stop from here on changes nothing
			return stoppedErr("backup import")
		}
		return finishImport(h, pd, wd, v.names, &res)
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, res)
}

// finishImport checks the unpacked directory like a backup and moves it to
// its final name.
func finishImport(h *hostconn.Host, pd poolDir, wd *bkWork, got []string, res *map[string]any) error {
	ref := &backupRef{h: h, pd: pd, dir: wd.path}
	m, err := ref.load()
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, f := range m.files() {
		want[f] = true
	}
	for _, f := range got {
		if !want[f] {
			return badRequest("refusing archive: " + quoteTag(f) + " is not part of a backup (the manifest does not list it)")
		}
		delete(want, f)
	}
	if len(want) > 0 {
		return badRequest("the archive is not a complete backup: files missing")
	}
	if x, err := readLimited(h, wd.path+"/domain.xml", backupMaxXML); err != nil {
		return badRequest(err.Error())
	} else if _, err := parseMigDomain(string(x)); err != nil {
		return badRequest("domain.xml is not a valid domain definition: " + err.Error())
	}
	for _, d := range m.Disks {
		info, err := bkQemuInfo(h, wd.path+"/"+d.File)
		if err != nil {
			return fmt.Errorf("%s: %w", d.File, err)
		}
		if info.Format != "qcow2" {
			return badRequest(d.File + " is not a qcow2 image")
		}
	}
	name := backupDirName(m.VM, m.createdTime())
	final := pd.Dir + "/" + name
	if err := h.ChmodWork(wd.path, "700"); err != nil {
		return err
	}
	if err := h.Move(wd.path, final); err != nil {
		return badRequest("a backup named " + name + " already exists in this pool, or it cannot be moved into place: " + impFirstLine(err.Error()))
	}
	*res = map[string]any{"pool": pd.Name, "dir": name, "vm": m.VM, "uuid": m.UUID}
	return nil
}
