package api

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
	"virticle/internal/retention"
	"virticle/internal/snapsched"
)

// Scheduled backups. Like scheduled snapshots they keep no state of their
// own: the schedule is the "backup-schedule" child of the VM's shared
// metadata root, "last run" is the creation time in the manifest of the
// newest backup of the VM in the target pool, and retention is the pruning the
// backup itself does at its end, with the schedule's policy (internal/retention).
// Timing (interval or daily slot) is snapsched's.

const (
	backupSchedChild = "backup-schedule"
	// backupCheckEvery bounds how often the target pool is listed to find the
	// newest backup; it is a scan over SSH, not a libvirt call.
	backupCheckEvery = time.Minute
)

type backupSchedule struct {
	Enabled bool `json:"enabled"`
	// Mode is "interval" (every EveryMinutes) or "daily" (at At, UTC).
	Mode         string `json:"mode"`
	EveryMinutes int    `json:"everyMinutes,omitempty"`
	At           string `json:"at,omitempty"`
	TargetNode   int64  `json:"targetNode"` // 0 = the node the VM is on
	Pool         string `json:"pool"`
	// Retention (internal/retention): keep the newest KeepLast, and the newest backup of each of the last
	// KeepHourly hours, KeepDaily days, KeepWeekly ISO weeks and KeepMonthly months, in the controller's time zone.
	// A schedule from before the tiers has only KeepLast and behaves as it did.
	KeepLast    int `json:"keepLast"`
	KeepHourly  int `json:"keepHourly"`
	KeepDaily   int `json:"keepDaily"`
	KeepWeekly  int `json:"keepWeekly"`
	KeepMonthly int `json:"keepMonthly"`
}

func (b backupSchedule) policy() retention.Policy {
	return retention.Policy{KeepLast: b.KeepLast, Hourly: b.KeepHourly, Daily: b.KeepDaily, Weekly: b.KeepWeekly, Monthly: b.KeepMonthly}
}

func (b *backupSchedule) setPolicy(p retention.Policy) {
	b.KeepLast, b.KeepHourly, b.KeepDaily, b.KeepWeekly, b.KeepMonthly = p.KeepLast, p.Hourly, p.Daily, p.Weekly, p.Monthly
}

// step is the usual time between two runs.
func (b backupSchedule) step() time.Duration {
	if b.Mode == "interval" {
		return time.Duration(b.EveryMinutes) * time.Minute
	}
	return 24 * time.Hour
}

// normalize clears the rules that cannot have an effect at the schedule's frequency (hourly for a daily schedule).
func (b *backupSchedule) normalize() {
	if b.Mode == "interval" {
		b.At = ""
	} else {
		b.EveryMinutes = 0
	}
	b.setPolicy(b.policy().Normalize(b.step()))
}

var poolNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func (b backupSchedule) timing() snapsched.Schedule {
	return snapsched.Schedule{Enabled: b.Enabled, Mode: b.Mode, EveryMinutes: b.EveryMinutes, At: b.At, Keep: 1, Prefix: "x"}
}

func (b backupSchedule) validate() error {
	if err := b.timing().Validate(); err != nil {
		return err
	}
	switch {
	case b.TargetNode < 0:
		return fmt.Errorf("bad target node")
	case !poolNameRE.MatchString(b.Pool):
		return fmt.Errorf("a target pool is required")
	}
	p := b.policy()
	if err := p.Validate(); err != nil {
		return err
	}
	if p.KeepsAll() {
		return fmt.Errorf("set at least one keep value (a schedule without retention would fill the pool)")
	}
	return nil
}

// pointAt is what decideBackup needs to make a run at now full when the schedule keeps it as a weekly or monthly
// point: the schedule's next run, if this backup were the newest one (a manual backup counts as a run too, since
// the schedule times its next run from the newest backup). nil for a paused schedule.
func (b backupSchedule) pointAt(now time.Time) *pointSched {
	next, ok := b.timing().NextRun(now, now)
	if !ok {
		return nil
	}
	return &pointSched{Policy: b.policy(), Now: now, Next: next, Loc: time.Local}
}

// due reports whether a backup should start now, given the creation time of
// the newest backup of the VM in the target (zero if none).
func (b backupSchedule) due(now, last time.Time) bool { return b.timing().Due(now, last) }

func (b backupSchedule) element() string {
	el := fmt.Sprintf(`<%s enabled=%q mode=%q`, backupSchedChild, yesNo(b.Enabled), b.Mode)
	if b.Mode == "interval" {
		el += fmt.Sprintf(` every-minutes="%d"`, b.EveryMinutes)
	} else {
		el += fmt.Sprintf(` at=%q`, b.At)
	}
	el += fmt.Sprintf(` target-node="%d" pool=%q`, b.TargetNode, b.Pool)
	if !b.policy().Tiered() {
		return el + fmt.Sprintf(` keep-last="%d"/>`, b.KeepLast)
	}
	// keep-last="0" makes a controller from before the tiers refuse the schedule (it needs 1 or more) instead of
	// pruning it down to keep-last alone; this one reads the count from keep-newest.
	return el + fmt.Sprintf(` keep-last="0" keep-newest="%d" keep-hourly="%d" keep-daily="%d" keep-weekly="%d" keep-monthly="%d"/>`,
		b.KeepLast, b.KeepHourly, b.KeepDaily, b.KeepWeekly, b.KeepMonthly)
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// parseBackupSchedule reads the schedule from the application metadata; nil
// when there is none.
func parseBackupSchedule(meta string) (*backupSchedule, error) {
	if meta == "" {
		return nil, nil
	}
	var root struct {
		B *struct {
			Enabled string `xml:"enabled,attr"`
			Mode    string `xml:"mode,attr"`
			Every   int    `xml:"every-minutes,attr"`
			At      string `xml:"at,attr"`
			Node    int64  `xml:"target-node,attr"`
			Pool    string `xml:"pool,attr"`
			Keep    int    `xml:"keep-last,attr"`
			Newest  *int   `xml:"keep-newest,attr"`
			Hourly  int    `xml:"keep-hourly,attr"`
			Daily   int    `xml:"keep-daily,attr"`
			Weekly  int    `xml:"keep-weekly,attr"`
			Monthly int    `xml:"keep-monthly,attr"`
		} `xml:"backup-schedule"`
	}
	if err := xml.Unmarshal([]byte(meta), &root); err != nil {
		return nil, fmt.Errorf("metadata is not well-formed XML: %w", err)
	}
	if root.B == nil {
		return nil, nil
	}
	b := &backupSchedule{Enabled: root.B.Enabled == "yes", Mode: root.B.Mode, EveryMinutes: root.B.Every, At: root.B.At,
		TargetNode: root.B.Node, Pool: root.B.Pool, KeepLast: root.B.Keep,
		KeepHourly: root.B.Hourly, KeepDaily: root.B.Daily, KeepWeekly: root.B.Weekly, KeepMonthly: root.B.Monthly}
	if root.B.Newest != nil {
		b.KeepLast = *root.B.Newest
	}
	return b, b.validate()
}

// newestBackup is the creation time of the newest backup of VM uuid in all.
func newestBackup(all []retained, uuid string) time.Time {
	var last time.Time
	for _, b := range all {
		if b.UUID == uuid && b.Created.After(last) {
			last = b.Created
		}
	}
	return last
}

// lastBackupTime lists the target pool and returns the newest backup of the
// VM there (zero if none).
func lastBackupTime(dst *hostconn.Host, pool, uuid string) (time.Time, error) {
	_, pd, err := destPool(dst, pool)
	if err != nil {
		return time.Time{}, err
	}
	items, err := scanPool(dst, pd)
	if err != nil {
		return time.Time{}, err
	}
	var all []retained
	for _, it := range items {
		if it.Err == "" {
			all = append(all, it.retained())
		}
	}
	return newestBackup(all, uuid), nil
}

// bkSchedState is the scheduler's in-memory pacing; losing it on restart
// only means one extra look at the target.
type bkSchedState struct {
	mu      sync.Mutex
	checked map[string]time.Time // last time the target was listed
	failed  map[string]time.Time // last failed attempt
}

func (st *bkSchedState) get(m *map[string]time.Time, key string) time.Time {
	st.mu.Lock()
	defer st.mu.Unlock()
	return (*m)[key]
}

func (st *bkSchedState) set(m *map[string]time.Time, key string, t time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if *m == nil {
		*m = map[string]time.Time{}
	}
	if t.IsZero() {
		delete(*m, key)
	} else {
		(*m)[key] = t
	}
}

// runBackupSchedule starts a backup of dom if its schedule says one is due.
// The backup runs in the background; this only plans and launches it.
func (s *Server) runBackupSchedule(srcID int64, h *hostconn.Host, dom libvirt.Domain, bs backupSchedule, key string) {
	uuid := cluster.FormatUUID(dom.UUID)
	bkRuns.Lock()
	_, busy := bkRuns.m[uuid]
	bkRuns.Unlock()
	if busy {
		return
	}
	now := time.Now()
	if now.Sub(s.bk.get(&s.bk.failed, key)) < schedRetry || now.Sub(s.bk.get(&s.bk.checked, key)) < backupCheckEvery {
		return
	}
	s.bk.set(&s.bk.checked, key, now)
	dstID := bs.TargetNode
	if dstID == 0 {
		dstID = srcID
	}
	no := false // a scheduled backup checksums only what may become part of a chain (see measure); the freeze stays automatic
	r := s.traced(schedRequest())
	h = s.traceHost(r, h)
	target := backupTarget(dom.Name, fmt.Sprintf("node %d", dstID), bs.Pool)
	failed := func(err error) {
		s.bk.set(&s.bk.failed, key, now)
		s.recordFailure(r, "backup", target, h.Name, err)
		logf("scheduled backup of %s on %s failed: %v", dom.Name, h.Name, err)
	}
	dst, err := s.Nodes.Host(dstID)
	if err != nil {
		failed(err)
		return
	}
	target = backupTarget(dom.Name, dst.Name, bs.Pool) // the name a real run's task carries, so a success clears the alert
	last, err := lastBackupTime(dst, bs.Pool, uuid)
	if err != nil {
		failed(err)
		return
	}
	if !bs.due(now, last) {
		return
	}
	pol := bs.policy()
	m, err := s.planBackup(srcID, h, dom, backupReq{TargetNode: bs.TargetNode, Pool: bs.Pool, KeepLast: bs.KeepLast, SHA256: &no, policy: &pol, point: bs.pointAt(now)})
	if err != nil {
		failed(err)
		return
	}
	_, done, err := s.launchBackup(r, h, m)
	if err != nil {
		if _, isBusy := err.(badRequest); !isBusy {
			failed(err)
		}
		return
	}
	go func() {
		if err := <-done; err != nil {
			s.bk.set(&s.bk.failed, key, time.Now())
		} else {
			s.bk.set(&s.bk.failed, key, time.Time{})
		}
	}()
}

// --- API ---

func (s *Server) getBackupSchedule(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	meta, err := domainMeta(h, dom)
	if err != nil {
		fail(w, err)
		return
	}
	bs, err := parseBackupSchedule(meta)
	if err != nil {
		fail(w, err)
		return
	}
	resp := map[string]any{"schedule": bs}
	if bs != nil {
		var lastT time.Time
		srcID, _, _ := s.host(r)
		dstID := bs.TargetNode
		if dstID == 0 {
			dstID = srcID
		}
		if dst, err := s.Nodes.Host(dstID); err != nil {
			resp["lastError"] = err.Error()
		} else if last, err := lastBackupTime(dst, bs.Pool, cluster.FormatUUID(dom.UUID)); err != nil {
			resp["lastError"] = err.Error()
		} else if !last.IsZero() {
			resp["last"] = last.Unix()
			lastT = last
		}
		// Backups run for shut-off VMs too; the timing is the same snapsched code the scheduler decides with.
		resp["runner"] = "this"
		if s.NoScheduler {
			resp["runner"] = "disabled"
		}
		if next, ok := bs.timing().NextRun(time.Now(), lastT); ok {
			resp["nextRun"] = next.Unix()
		}
		resp["tick"] = int(schedTick.Seconds())
	}
	writeJSON(w, resp)
}

// defaultBackupSchedule holds what a saved schedule gets for fields the client leaves out: a saved schedule is enabled.
func defaultBackupSchedule() backupSchedule { return backupSchedule{Enabled: true, KeepLast: 7} }

func (s *Server) putBackupSchedule(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	bs := defaultBackupSchedule()
	if err := readJSON(r, &bs); err != nil {
		fail(w, err)
		return
	}
	bs.normalize()
	if err := bs.validate(); err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	srcID, _, _ := s.host(r)
	dstID := bs.TargetNode
	if dstID == 0 {
		dstID = srcID
	}
	dst, err := s.Nodes.Host(dstID)
	if err != nil {
		fail(w, err)
		return
	}
	if _, _, err := destPool(dst, bs.Pool); err != nil {
		fail(w, err)
		return
	}
	s.updateBackupSchedule(w, r, h, dom, bs.element(), "backup schedule set")
}

func (s *Server) deleteBackupSchedule(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	s.updateBackupSchedule(w, r, h, dom, "", "backup schedule remove")
}

func (s *Server) updateBackupSchedule(w http.ResponseWriter, r *http.Request, h *hostconn.Host, dom libvirt.Domain, elem, kind string) {
	err := s.task(r, kind, dom.Name, h.Name, func(func(float64)) error {
		old, err := domainMeta(h, dom)
		if err != nil {
			return err
		}
		meta, err := metaSetChild(old, backupSchedChild, elem)
		if err != nil {
			return badRequest(err.Error())
		}
		return setDomainMeta(h, dom, meta)
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
