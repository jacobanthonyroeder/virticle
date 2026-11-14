package store

// workSchema is applied on every open, like tagSchema. A row is a work directory this controller made on a node
// for a replication run or a backup and has not removed yet (see internal/api/workdirs.go). owner is the random
// token of the controller process that made it, so that a later process can tell its own leftovers from the
// ones of a process that is gone.
const workSchema = `
CREATE TABLE IF NOT EXISTS work_dirs (
  path TEXT PRIMARY KEY, node INTEGER NOT NULL, kind TEXT NOT NULL, owner TEXT NOT NULL, created INTEGER NOT NULL);
`

// WorkDir is one row of work_dirs.
type WorkDir struct {
	Path    string
	Node    int64
	Kind    string
	Owner   string
	Created int64
}

// AddWorkDir records a work directory; an existing row for the path is replaced.
func (s *Store) AddWorkDir(w WorkDir) error {
	_, err := s.db.Exec(`INSERT INTO work_dirs(path, node, kind, owner, created) VALUES(?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET node=excluded.node, kind=excluded.kind, owner=excluded.owner, created=excluded.created`,
		w.Path, w.Node, w.Kind, w.Owner, w.Created)
	return err
}

// DeleteWorkDir forgets a work directory; forgetting one that is not recorded is not an error.
func (s *Store) DeleteWorkDir(path string) error {
	_, err := s.db.Exec(`DELETE FROM work_dirs WHERE path=?`, path)
	return err
}

// WorkDirs lists the recorded work directories of a node, oldest first.
func (s *Store) WorkDirs(node int64) ([]WorkDir, error) {
	rows, err := s.db.Query(`SELECT path, node, kind, owner, created FROM work_dirs WHERE node=? ORDER BY created, path`, node)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkDir
	for rows.Next() {
		var w WorkDir
		if err := rows.Scan(&w.Path, &w.Node, &w.Kind, &w.Owner, &w.Created); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
