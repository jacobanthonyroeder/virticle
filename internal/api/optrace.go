package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"virticle/internal/hostconn"
	"virticle/internal/optrace"
	"virticle/internal/store"
)

// Operation traces (internal/optrace): every request that changes something carries a trace in its context
// (wrap), the hosts it gets through s.host, s.domain and traceHost record into it, and a task started by the
// request shares it. The audit entry and the task keep the trace id; the operations are stored as they end.

// newTrace makes a trace whose operations go to the store.
func (s *Server) newTrace() *optrace.Trace {
	return optrace.New(func(id int64, op optrace.Op) {
		b, err := json.Marshal(op)
		if err == nil {
			err = s.Store.AddOp(id, b)
		}
		if err != nil {
			logf("operation trace: %v", err)
		}
	})
}

// traced gives a request the controller makes itself (a schedule, an evacuation) a trace of its own.
func (s *Server) traced(r *http.Request) *http.Request {
	if optrace.From(r.Context()) != nil {
		return r
	}
	return r.WithContext(optrace.With(r.Context(), s.newTrace()))
}

// traceHost is the view of h that records into the request's trace; h itself when the request has none.
func (s *Server) traceHost(r *http.Request, h *hostconn.Host) *hostconn.Host {
	if r == nil || h == nil {
		return h
	}
	tr := optrace.From(r.Context())
	if tr == nil || h.Trace() == tr {
		return h
	}
	return h.Traced(tr, s.hostLabel(h))
}

// hostLabel is the name a node is shown with: its host name, else its address.
func (s *Server) hostLabel(h *hostconn.Host) string {
	if cfg, ok := s.Nodes.ConfigOfHost(h); ok {
		return s.nodeLabel(cfg.ID, cfg.Addr)
	}
	return h.Name
}

func (s *Server) nodeLabel(id int64, fallback string) string {
	if n, ok := s.nodeStatus(id); ok && n.Hostname != "" {
		return n.Hostname
	}
	return fallback
}

// linkTask makes a task's operations those of the request's trace.
func (s *Server) linkTask(r *http.Request, t *store.Task) {
	if r == nil {
		return
	}
	if tr := optrace.From(r.Context()); tr != nil {
		s.Store.SetTaskTrace(t.ID, tr.ID())
	}
}

// auditOps answers GET /api/audit/{id}/ops: the operations of an audit entry.
func (s *Server) auditOps(w http.ResponseWriter, r *http.Request) {
	s.serveOps(w, r, s.Store.AuditTrace)
}

// taskOps answers GET /api/tasks/{id}/ops: the operations of a task.
func (s *Server) taskOps(w http.ResponseWriter, r *http.Request) {
	s.serveOps(w, r, s.Store.TaskTrace)
}

func (s *Server) serveOps(w http.ResponseWriter, r *http.Request, traceOf func(int64) (int64, error)) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, store.ErrNotFound)
		return
	}
	tr, err := traceOf(id)
	if err != nil {
		fail(w, err)
		return
	}
	ops, err := s.Store.Ops(tr)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"ops": ops})
}
