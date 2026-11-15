package api

// Verifying a backup: every file of the backup, and of every backup it builds on, is there, has the size the
// manifest records and, where the manifest records one, the same SHA-256. Encrypted files are checked as they are
// stored (the manifest records the checksum of the stored file), so no passphrase is needed. The work runs on the
// node that holds the backup, as a task that can be stopped; it changes nothing.

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"virticle/internal/hostconn"
)

// verifyFile is one file a verify checks.
type verifyFile struct {
	Backup string // directory name of the backup the file belongs to
	Name   string
	Size   int64  // -1: the manifest records no size (domain.xml, the manifest, plain UEFI variables)
	SHA    string // "": the manifest records no checksum
}

func (f verifyFile) String() string { return f.Backup + "/" + f.Name }

// verifyList lists the files of the chain (the full backup first, the verified one last), in the order they are
// checked.
func verifyList(chain []chainLink) []verifyFile {
	var out []verifyFile
	for _, l := range chain {
		m := l.Man
		add := func(name string, size int64, sum string) {
			out = append(out, verifyFile{Backup: l.Dir, Name: name, Size: size, SHA: sum})
		}
		add("manifest.json", -1, "")
		add("domain.xml", -1, "")
		if m.NVRAM != "" {
			if m.NVRAMEncrypted {
				add(m.NVRAM, int64(m.NVRAMBytes), m.NVRAMSHA256)
			} else {
				add(m.NVRAM, -1, "")
			}
		}
		if m.TPMState != "" {
			add(m.TPMState, int64(m.TPMBytes), m.TPMSHA256)
		}
		for _, d := range m.Disks {
			add(d.File, int64(d.Bytes), d.SHA256)
		}
	}
	return out
}

// verifySizes returns the first file that is missing or has another size than the manifest records. sizes holds
// what the node reported per file, in the order of files ("MISSING" for a missing one).
func verifySizes(files []verifyFile, sizes []string) error {
	for i, f := range files {
		switch {
		case i >= len(sizes) || sizes[i] == "MISSING":
			return fmt.Errorf("%s is missing", f)
		case f.Size >= 0 && sizes[i] != strconv.FormatInt(f.Size, 10):
			return fmt.Errorf("%s is damaged: it has %s bytes, the manifest says %d", f, sizes[i], f.Size)
		}
	}
	return nil
}

// verifyResult is the note of a verify that found nothing wrong.
func verifyResult(chain []chainLink, files []verifyFile) string {
	var unsummed int
	for _, f := range files {
		if f.Size >= 0 && f.SHA == "" {
			unsummed++
		}
	}
	s := fmt.Sprintf("%d %s intact", len(files), plural(len(files), "file", "files"))
	if len(chain) > 1 {
		s += fmt.Sprintf(" in %d backups", len(chain))
	}
	if unsummed > 0 {
		s += fmt.Sprintf("; %d without a checksum, size checked only", unsummed)
	}
	return s
}

// verifyChain finds the backup name and every backup it builds on in the scan of their pool, without checking the
// files (that is the verify's job). The links come full backup first.
func verifyChain(items []scanned, name string) ([]chainLink, error) {
	found := map[string]scanned{}
	for _, it := range items {
		found[it.Dir] = it
	}
	by := map[string]*backupManifest{}
	cur := name
	for {
		it, ok := found[cur]
		switch {
		case !ok && cur == name:
			return nil, fmt.Errorf("%s is not a backup in this pool", name)
		case !ok:
			return nil, fmt.Errorf("the earlier backup %s that this one builds on is missing", cur)
		case it.Err != "":
			return nil, fmt.Errorf("%s is unreadable: %s", cur, it.Err)
		case by[cur] != nil:
			return nil, fmt.Errorf("the chain of backups loops")
		case len(by) >= maxChainWalk:
			return nil, fmt.Errorf("the chain of backups is too long")
		}
		by[cur] = it.Manifest
		if it.Manifest.kindOf() != kindIncremental {
			break
		}
		cur = it.Manifest.Parent
	}
	order, err := chainOf(by, name)
	if err != nil {
		return nil, err
	}
	out := make([]chainLink, len(order))
	for i, d := range order {
		out[i] = chainLink{Dir: d, Man: by[d]}
	}
	return out, nil
}

// runVerify checks the chain's files on h: sizes first (quick, and a missing file is named before any long read),
// then the checksums, with progress by bytes read.
func runVerify(ctx context.Context, h *hostconn.Host, dir string, chain []chainLink, progress func(float64)) (string, error) {
	files := verifyList(chain)
	for _, l := range chain {
		var names []string
		var mine []verifyFile
		for _, f := range files {
			if f.Backup == l.Dir {
				names = append(names, f.Name)
				mine = append(mine, f)
			}
		}
		sizes, err := h.FilesSize(dir+"/"+l.Dir, names)
		if err != nil {
			return "", fmt.Errorf("cannot read %s: %w", l.Dir, err)
		}
		if err := verifySizes(mine, sizes); err != nil {
			return "", err
		}
	}
	progress(0.02)
	var total, done uint64
	for _, f := range files {
		if f.SHA != "" {
			total += uint64(f.Size)
		}
	}
	for _, f := range files {
		if f.SHA == "" {
			continue
		}
		base := done
		sum, err := h.SHA256Progress(ctx, dir+"/"+f.Backup+"/"+f.Name, func(n, _ uint64) {
			progress(spreadBytes(0.02, 1, base+n, total))
		})
		if err != nil {
			return "", err
		}
		if sum != f.SHA {
			return "", fmt.Errorf("%s is damaged: its SHA-256 differs from the one in the manifest", f)
		}
		done += uint64(f.Size)
		progress(spreadBytes(0.02, 1, done, total))
	}
	return verifyResult(chain, files), nil
}

// POST /api/nodes/{node}/backups/{pool}/{name}/verify
func (s *Server) verifyBackup(w http.ResponseWriter, r *http.Request) {
	src, err := s.backupFromURL(r)
	if err != nil {
		fail(w, err)
		return
	}
	// Retention keeps the backup, and what it builds on, while it is read. A missing or unreadable link fails the
	// task, like a damaged file.
	release := holdRead(src.pd.Dir, src.name)
	st := newStopper(context.Background())
	id, _, err := s.startBackgroundStop(r, "verify", src.name, src.h.Name, taskStop{operator, st.stop}, func(progress func(float64)) error {
		defer release()
		defer st.done()
		items, err := scanPool(src.h, src.pd)
		if err != nil {
			return err
		}
		chain, err := verifyChain(items, src.name)
		if err != nil {
			return err
		}
		note, err := runVerify(st.Ctx(), src.h, src.pd.Dir, chain, progress)
		if err != nil {
			return st.cancelled("verify", err)
		}
		return doneNote(note)
	})
	if err != nil {
		release()
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"task": id})
}
