package store

import (
	"encoding/json"
	"time"
)

// migSchema is applied on every open. A mig_runs row is a migration that has made, or is about to make, files on
// its target node; each file it made is a mig_files row with the identity the node reported when it made it
// (internal/nodecmd/verbs_fileid.go). A run's rows go when the migration succeeded, when its files are gone, or
// when an administrator keeps the files. Cleanup after a failed migration works only from these rows (see
// internal/api/migjournal.go), so it also works after a restart of the controller.
const migSchema = `
CREATE TABLE IF NOT EXISTS mig_runs (
  id INTEGER PRIMARY KEY, vm_uuid TEXT NOT NULL, vm_name TEXT NOT NULL, src_node INTEGER NOT NULL, dst_node INTEGER NOT NULL,
  src_name TEXT NOT NULL DEFAULT '', dst_name TEXT NOT NULL DEFAULT '', mode TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL, unsure INTEGER NOT NULL DEFAULT 0, note TEXT NOT NULL DEFAULT '',
  src_files TEXT NOT NULL DEFAULT '[]', owner TEXT NOT NULL DEFAULT '', started INTEGER NOT NULL, updated INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS mig_files (
  run_id INTEGER NOT NULL REFERENCES mig_runs(id) ON DELETE CASCADE, path TEXT NOT NULL, ident TEXT NOT NULL DEFAULT '',
  size INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL DEFAULT 'present', reason TEXT NOT NULL DEFAULT '',
  added INTEGER NOT NULL, PRIMARY KEY(run_id, path));
`

// States of a migration run in the journal.
const (
	MigPrepare   = "prepare"   // files are being made on the target; nothing has been handed over
	MigSwitching = "switching" // the hand-over to the target has started; its outcome is not known yet
	MigFailed    = "failed"    // the migration failed and the VM was confirmed to have stayed on the source
	MigAttention = "attention" // the outcome or the VM's whereabouts could not be confirmed; nothing is removed by itself
)

// States of a journalled file.
const (
	MigFilePresent = "present" // made by the run and not removed yet
	MigFileKept    = "kept"    // another file has its name now, or it is not ours to remove: left alone for good
)

// MigRun is one row of mig_runs with its files.
type MigRun struct {
	ID       int64     `json:"id"`
	UUID     string    `json:"uuid"`
	Name     string    `json:"name"`
	Src      int64     `json:"src"`
	Dst      int64     `json:"dst"`
	SrcName  string    `json:"srcName"`
	DstName  string    `json:"dstName"`
	Mode     string    `json:"mode"`
	State    string    `json:"state"`
	Unsure   bool      `json:"unsure"` // the hand-over's outcome was not seen by the controller
	Note     string    `json:"note,omitempty"`
	SrcFiles []string  `json:"srcFiles"` // the VM's disk files on the source when the run started
	Owner    string    `json:"-"`
	Started  int64     `json:"started"`
	Updated  int64     `json:"updated"`
	Files    []MigFile `json:"files"`
}

// MigFile is one row of mig_files.
type MigFile struct {
	Path string `json:"path"`
	// Ident is the identity the node gave when it made the file; empty when it gave none or the answer was lost,
	// and such a file is never removed by the controller.
	Ident  string `json:"ident,omitempty"`
	Size   uint64 `json:"size"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"` // why it was kept, at the last attempt
}

// AddMigRun records a run before it makes anything and returns its id.
func (s *Store) AddMigRun(r MigRun) (int64, error) {
	src, _ := json.Marshal(r.SrcFiles)
	now := time.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO mig_runs(vm_uuid, vm_name, src_node, dst_node, src_name, dst_name, mode, state, unsure, note, src_files, owner, started, updated)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.UUID, r.Name, r.Src, r.Dst, r.SrcName, r.DstName, r.Mode, r.State, r.Unsure, r.Note, string(src), r.Owner, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// AddMigFile records a file the run made. A row for the same path is replaced.
func (s *Store) AddMigFile(run int64, f MigFile) error {
	if f.State == "" {
		f.State = MigFilePresent
	}
	_, err := s.db.Exec(`INSERT INTO mig_files(run_id, path, ident, size, state, reason, added) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(run_id, path) DO UPDATE SET ident=excluded.ident, size=excluded.size, state=excluded.state, reason=excluded.reason`,
		run, f.Path, f.Ident, f.Size, f.State, f.Reason, time.Now().Unix())
	return err
}

// SetMigState sets a run's state; unsure is only ever raised, never cleared, and note replaces the old one.
func (s *Store) SetMigState(run int64, state string, unsure bool, note string) error {
	_, err := s.db.Exec(`UPDATE mig_runs SET state=?, unsure=MAX(unsure, ?), note=?, updated=? WHERE id=?`, state, unsure, note, time.Now().Unix(), run)
	return err
}

// SetMigFile records the outcome of an attempt for one file: its state and the reason it is kept.
func (s *Store) SetMigFile(run int64, path, state, reason string) error {
	_, err := s.db.Exec(`UPDATE mig_files SET state=?, reason=? WHERE run_id=? AND path=?`, state, reason, run, path)
	return err
}

// SetMigFileSize records the space a journalled file takes now (a mirrored image grows after it was made).
func (s *Store) SetMigFileSize(run int64, path string, size uint64) error {
	_, err := s.db.Exec(`UPDATE mig_files SET size=? WHERE run_id=? AND path=?`, size, run, path)
	return err
}

// DeleteMigFile forgets one file of a run (it is gone).
func (s *Store) DeleteMigFile(run int64, path string) error {
	_, err := s.db.Exec(`DELETE FROM mig_files WHERE run_id=? AND path=?`, run, path)
	return err
}

// DeleteMigRun forgets a run and its files; the files themselves are not touched.
func (s *Store) DeleteMigRun(run int64) error {
	_, err := s.db.Exec(`DELETE FROM mig_runs WHERE id=?`, run)
	return err
}

// MigRuns lists every journalled run with its files, oldest first.
func (s *Store) MigRuns() ([]MigRun, error) {
	rows, err := s.db.Query(`SELECT id, vm_uuid, vm_name, src_node, dst_node, src_name, dst_name, mode, state, unsure, note, src_files, owner, started, updated FROM mig_runs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var out []MigRun
	for rows.Next() {
		var r MigRun
		var src string
		if err := rows.Scan(&r.ID, &r.UUID, &r.Name, &r.Src, &r.Dst, &r.SrcName, &r.DstName, &r.Mode, &r.State, &r.Unsure, &r.Note, &src, &r.Owner, &r.Started, &r.Updated); err != nil {
			rows.Close()
			return nil, err
		}
		json.Unmarshal([]byte(src), &r.SrcFiles)
		r.Files = []MigFile{}
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	idx := map[int64]int{}
	for i, r := range out {
		idx[r.ID] = i
	}
	frows, err := s.db.Query(`SELECT run_id, path, ident, size, state, reason FROM mig_files ORDER BY run_id, rowid`)
	if err != nil {
		return nil, err
	}
	defer frows.Close()
	for frows.Next() {
		var id int64
		var f MigFile
		if err := frows.Scan(&id, &f.Path, &f.Ident, &f.Size, &f.State, &f.Reason); err != nil {
			return nil, err
		}
		if i, ok := idx[id]; ok {
			out[i].Files = append(out[i].Files, f)
		}
	}
	return out, frows.Err()
}

// MigRun returns one run with its files.
func (s *Store) MigRun(id int64) (MigRun, error) {
	all, err := s.MigRuns()
	if err != nil {
		return MigRun{}, err
	}
	for _, r := range all {
		if r.ID == id {
			return r, nil
		}
	}
	return MigRun{}, ErrNotFound
}
