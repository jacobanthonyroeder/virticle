package api

// Work directories that replication runs and backups leave on the nodes when they are cut short.
//
// What a run makes on a node, and when it normally goes:
//
//	.virticle-rp.XXXXXX on the source   staging of a replication run      removed at the end of the run
//	.virticle-rp.XXXXXX on the standby  the replica directory (the copy)  stays; only "stop replication" removes it
//	.virticle-bk.XXXXXX on the source   staging of a backup               removed after the copy, or at the end
//	.virticle-bk.XXXXXX on the target   the backup being written          renamed to the backup when complete
//	in<S>-... inside a replica          a point being received            settle() of the next run removes it
//
// A run that dies with its node, or with the controller, cannot remove its directories. Every one of them is
// recorded in the store when it is made (holdWork) and the record goes when the directory goes. A sweep
// (sweepWorkDirs) runs when a node comes online and before a replication run or a backup is planned on it, and
// removes what the records name, under these rules:
//
//   - A directory a run of this process holds right now is never touched (workHeld, in memory).
//   - Only recorded directories are removed: a directory this controller did not make (another controller's, or
//     anything else) is never touched by this sweep. The older, slow sweeps (replica_sweep.go, sweepStaleWork)
//     stay as they were for directories nobody recorded.
//   - A recorded directory of this process that no run holds belongs to a run that has ended: it goes at once.
//   - One recorded by an earlier process (the controller was restarted, or this data directory is a copy) goes
//     only after nothing in it has changed for workQuiet. A backup job that libvirt keeps running after the
//     controller is gone still writes there, and a copy of the data directory may belong to a controller that
//     still runs.
//   - A new replica directory is recorded only until the VM's replication setting names it. One that is still
//     recorded goes only when no replica domain and no replication setting on any node names it (every node must
//     answer) and it holds no point manifest and no base image: a directory with a copy in it is never garbage,
//     whatever the records say (the merge of a replica is finished or rolled back from its directory).

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"path"
	"strings"
	"sync"
	"time"

	"virticle/internal/hostconn"
	"virticle/internal/store"
)

const (
	workKindStage   = "stage"   // staging of a replication run, or a backup's work directory
	workKindReplica = "replica" // a new replica directory, until the replication setting names it

	workQuiet = 10 * time.Minute
)

// workOwner tells the records of this process from the ones of earlier processes.
var workOwner = func() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}()

// workHeld are the work directories runs of this process are using, by path.
var workHeld = struct {
	sync.Mutex
	m map[string]bool
}{m: map[string]bool{}}

func workInUse(p string) bool {
	workHeld.Lock()
	defer workHeld.Unlock()
	return workHeld.m[p]
}

// holdWork marks a work directory as used by a run of this process. With a kind it is also recorded in the store,
// so that it can be found again when the run cannot remove it.
func (s *Server) holdWork(h *hostconn.Host, p, kind string) {
	workHeld.Lock()
	workHeld.m[p] = true
	workHeld.Unlock()
	if kind == "" || s == nil || s.Store == nil || s.Nodes == nil {
		return
	}
	cfg, ok := s.Nodes.ConfigOfHost(h)
	if !ok {
		return
	}
	if err := s.Store.AddWorkDir(store.WorkDir{Path: p, Node: cfg.ID, Kind: kind, Owner: workOwner, Created: time.Now().Unix()}); err != nil {
		log.Printf("work directories: recording %s: %v", p, err)
	}
}

// releaseWork ends a run's use of a work directory; gone says it no longer exists (removed, or renamed into a
// backup), so its record goes too.
func (s *Server) releaseWork(p string, gone bool) {
	workHeld.Lock()
	delete(workHeld.m, p)
	workHeld.Unlock()
	if gone {
		s.forgetWork(p)
	}
}

// forgetWork drops the record of a work directory and leaves the directory alone.
func (s *Server) forgetWork(p string) {
	if s == nil || s.Store == nil {
		return
	}
	if err := s.Store.DeleteWorkDir(p); err != nil {
		log.Printf("work directories: forgetting %s: %v", p, err)
	}
}

// workCheck is what the sweep found out about one recorded directory.
type workCheck struct {
	rec        store.WorkDir
	inUse      bool      // a run of this process holds it
	exists     bool      // it is still a directory
	holdsCopy  bool      // a point manifest or a base image is in it
	known      bool      // the answer to referenced is complete (every node answered)
	referenced bool      // a replica domain or a replication setting names it
	newest     time.Time // the newest change: the record, and every file in it
}

type workVerdict int

const (
	workKeep   workVerdict = iota // leave it and its record, look again later
	workForget                    // not garbage, or not there: drop the record, leave the directory
	workRemove                    // garbage: remove it, then the record
)

// judgeWork decides about one recorded directory. retry is set when the directory may become garbage later
// without anything else happening (an earlier process's directory that changed too recently).
func judgeWork(c workCheck, owner string, now time.Time) (v workVerdict, retry time.Duration) {
	switch {
	case c.inUse:
		return workKeep, 0
	case !c.exists:
		return workForget, 0
	}
	if c.rec.Kind == workKindReplica {
		switch {
		case c.holdsCopy:
			return workForget, 0
		case !c.known:
			return workKeep, workQuiet
		case c.referenced:
			return workForget, 0
		}
	}
	if c.rec.Owner != owner {
		if age := now.Sub(c.newest); age < workQuiet {
			return workKeep, workQuiet - age + 30*time.Second
		}
	}
	return workRemove, 0
}

// workSweeps keeps one sweep per node at a time and at most one follow-up timer.
var workSweeps = struct {
	sync.Mutex
	busy  map[int64]bool
	timer map[int64]bool
}{busy: map[int64]bool{}, timer: map[int64]bool{}}

// sweepWorkDirs removes the recorded work directories of a node that are garbage (see the top of this file).
func (s *Server) sweepWorkDirs(id int64) {
	if s.Store == nil || s.Nodes == nil {
		return
	}
	workSweeps.Lock()
	if workSweeps.busy[id] {
		workSweeps.Unlock()
		return
	}
	workSweeps.busy[id] = true
	workSweeps.Unlock()
	defer func() {
		workSweeps.Lock()
		delete(workSweeps.busy, id)
		workSweeps.Unlock()
	}()
	recs, err := s.Store.WorkDirs(id)
	if err != nil || len(recs) == 0 {
		return
	}
	h, err := s.Nodes.Host(id)
	if err != nil {
		return // swept when it comes online
	}
	var known map[string]bool
	askedKnown := false
	var retry time.Duration
	for _, rec := range recs {
		c := workCheck{rec: rec, inUse: workInUse(rec.Path), newest: time.Unix(rec.Created, 0)}
		var size uint64
		count := 0
		if !c.inUse {
			st, err := h.Stat(rec.Path)
			if err != nil || len(st) != 1 {
				continue
			}
			c.exists = st[0].Exists && st[0].Kind == "dir"
		}
		if c.exists {
			names, err := h.ReplList(rec.Path)
			if err != nil {
				continue
			}
			var files []string
			count = len(names)
			for n, sz := range names {
				size += sz
				files = append(files, rec.Path+"/"+n)
				if rePointJSON.MatchString(n) || reBaseFile.MatchString(n) {
					c.holdsCopy = true
				}
			}
			if rec.Kind == workKindReplica && !c.holdsCopy {
				if !askedKnown {
					known, askedKnown = s.knownReplicaDirs(id, h), true
				}
				c.known, c.referenced = known != nil, known[rec.Path]
			}
			if rec.Owner != workOwner && len(files) > 0 {
				if len(files) > 200 {
					continue // not what a run makes: leave it to the slow sweeps
				}
				stamps, err := h.Stamps(files...)
				if err != nil {
					continue
				}
				for _, st := range stamps {
					if t := time.Unix(0, st.Mtime); st.Exists && t.After(c.newest) {
						c.newest = t
					}
				}
			}
		}
		v, again := judgeWork(c, workOwner, time.Now())
		if again > 0 && (retry == 0 || again < retry) {
			retry = again
		}
		switch v {
		case workForget:
			s.forgetWork(rec.Path)
		case workRemove:
			if workInUse(rec.Path) { // a run cannot take a recorded directory over, but be sure
				continue
			}
			if err := h.RemoveWork(rec.Path); err != nil {
				log.Printf("work directories: cannot remove the leftover %s on %s: %v", rec.Path, h.Name, err)
				continue
			}
			what := "a replication run"
			if strings.HasPrefix(path.Base(rec.Path), ".virticle-bk.") {
				what = "a backup"
			}
			log.Printf("work directories: removed the leftover %s on %s (%s in %d files, left by %s that was cut short)", rec.Path, h.Name, fmtBytes(size), count, what)
			s.forgetWork(rec.Path)
		}
	}
	if retry > 0 {
		workSweeps.Lock()
		if !workSweeps.timer[id] {
			workSweeps.timer[id] = true
			time.AfterFunc(retry, func() {
				workSweeps.Lock()
				delete(workSweeps.timer, id)
				workSweeps.Unlock()
				s.sweepWorkDirs(id)
			})
		}
		workSweeps.Unlock()
	}
}

// startWorkSweep sweeps a node's work directories whenever its connection comes (back) up.
func (s *Server) startWorkSweep() {
	if s.Nodes == nil || s.Store == nil {
		return
	}
	s.workSweepOnce.Do(func() {
		events, _ := s.Nodes.Subscribe()
		go func() {
			for ev := range events {
				if ev.Type == "node" && ev.Detail == "online" {
					go s.sweepWorkDirs(ev.Node)
				}
			}
		}()
	})
}
