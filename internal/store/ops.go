package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Operation traces: what a request or task did on the nodes (see internal/optrace). The store keeps each
// operation as the JSON the API serves, keyed by the trace id that the audit entry and the task carry. The
// data has passed the redaction of internal/optrace before it gets here.

const opsSchema = `
CREATE TABLE IF NOT EXISTS ops (
  id INTEGER PRIMARY KEY, trace INTEGER NOT NULL, time INTEGER NOT NULL, size INTEGER NOT NULL, data TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS ops_trace ON ops(trace);
`

// Bounds of the stored operations: the audit log's age, and a total size.
const (
	opsMaxBytes = 64 << 20
	opsPruneGap = 200 // prune once per this many inserts
)

// AddOp stores one operation of a trace; data is its JSON.
func (s *Store) AddOp(trace int64, data []byte) error {
	res, err := s.db.Exec(`INSERT INTO ops(trace, time, size, data) VALUES(?,?,?,?)`, trace, time.Now().Unix(), len(data), string(data))
	if err != nil {
		return err
	}
	if id, _ := res.LastInsertId(); id%opsPruneGap == 0 {
		return s.PruneOps()
	}
	return nil
}

// PruneOps drops operations older than the audit log keeps entries, then the oldest beyond the size limit.
func (s *Store) PruneOps() error { return s.pruneOps(auditMaxAge, opsMaxBytes) }

func (s *Store) pruneOps(age time.Duration, maxBytes int64) error {
	if _, err := s.db.Exec(`DELETE FROM ops WHERE time<?`, time.Now().Add(-age).Unix()); err != nil {
		return err
	}
	var cut sql.NullInt64
	err := s.db.QueryRow(`SELECT max(id) FROM (SELECT id, sum(size) OVER (ORDER BY id DESC) AS run FROM ops) WHERE run>?`, maxBytes).Scan(&cut)
	if err != nil || !cut.Valid {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM ops WHERE id<=?`, cut.Int64)
	return err
}

// Ops returns the operations of a trace in the order they were stored, as JSON.
func (s *Store) Ops(trace int64) ([]json.RawMessage, error) {
	out := []json.RawMessage{}
	if trace == 0 {
		return out, nil
	}
	rows, err := s.db.Query(`SELECT data FROM ops WHERE trace=? ORDER BY id`, trace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, json.RawMessage(d))
	}
	return out, rows.Err()
}

// AuditTrace is the trace id of an audit entry (0 when it has none).
func (s *Store) AuditTrace(id int64) (int64, error) {
	var t int64
	err := s.db.QueryRow(`SELECT trace FROM audit WHERE id=?`, id).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return t, err
}

// TaskTrace is the trace id of a task (0 when it has none).
func (s *Store) TaskTrace(id int64) (int64, error) {
	var t int64
	err := s.db.QueryRow(`SELECT trace FROM tasks WHERE id=?`, id).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return t, err
}

// SetTaskTrace records the trace a task's operations go to.
func (s *Store) SetTaskTrace(id, trace int64) {
	s.db.Exec(`UPDATE tasks SET trace=? WHERE id=?`, trace, id)
}

// opCount is the SQL for the number of operations of the row's trace.
const opCount = `(SELECT count(*) FROM ops WHERE ops.trace=%s.trace AND %s.trace<>0)`
