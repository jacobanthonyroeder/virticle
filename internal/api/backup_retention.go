package api

// Retention of VM backups and the schedule assistant's projection.
//
// Pruning (pruneBackups, prunePolicy) and the projection (projectSchedule) both run internal/retention, so that
// what the form shows is what the pruning does. Backups a restore or an export reads right now are held here and
// never pruned.

import (
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
	"virticle/internal/retention"
)

// bkReading counts the restores and exports that read a backup, by pool directory and backup name. The host is
// left out on purpose: an NFS pool shows the same backups on every node that mounts it, and holding one too many
// only keeps a backup longer.
var bkReading = struct {
	sync.Mutex
	m map[string]map[string]int
}{m: map[string]map[string]int{}}

// holdRead marks a backup as read; the function it returns ends that. Holding the backup holds what it builds on
// as well (prunePolicy keeps the chain of a held backup).
func holdRead(poolDir, name string) func() {
	bkReading.Lock()
	if bkReading.m[poolDir] == nil {
		bkReading.m[poolDir] = map[string]int{}
	}
	bkReading.m[poolDir][name]++
	bkReading.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			bkReading.Lock()
			defer bkReading.Unlock()
			if bkReading.m[poolDir][name]--; bkReading.m[poolDir][name] <= 0 {
				delete(bkReading.m[poolDir], name)
			}
			if len(bkReading.m[poolDir]) == 0 {
				delete(bkReading.m, poolDir)
			}
		})
	}
}

// readingIn is the set of backups in a pool directory that are being read.
func readingIn(_ string, poolDir string) map[string]bool {
	bkReading.Lock()
	defer bkReading.Unlock()
	out := map[string]bool{}
	for n := range bkReading.m[poolDir] {
		out[n] = true
	}
	return out
}

// schedulePolicyFor returns the retention of the VM's backup schedule when its target is the given one (target
// node 0 = the VM's node).
func (s *Server) schedulePolicyFor(h *hostconn.Host, dom libvirt.Domain, srcID, targetNode int64, pool string) (retention.Policy, bool) {
	meta, err := domainMeta(h, dom)
	if err != nil {
		return retention.Policy{}, false
	}
	bs, err := parseBackupSchedule(meta)
	if err != nil || bs == nil {
		return retention.Policy{}, false
	}
	a, b := bs.TargetNode, targetNode
	if a == 0 {
		a = srcID
	}
	if b == 0 {
		b = srcID
	}
	if a != b || bs.Pool != pool {
		return retention.Policy{}, false
	}
	return bs.policy(), true
}

// --- projection ---

type projPoint struct {
	Time   int64    `json:"time"`
	Reason string   `json:"reason"`
	Rules  []string `json:"rules,omitempty"`
	Full   bool     `json:"full"`
}

// projSize is the estimated space the kept backups take.
type projSize struct {
	Low  uint64 `json:"low"`
	High uint64 `json:"high"`
	// Basis is "backups" (measured from backups in the target), "disks" (no backup yet: the allocated size of
	// the VM's disks) or "last" (the controller: the last backup's size).
	Basis string `json:"basis"`
	Full  uint64 `json:"full"`
	// Incremental is the average size of the recent incremental backups that were measured (Measured of them);
	// 0 when there are none, and the range then covers 2 to 20% of a full backup per incremental one.
	Incremental uint64 `json:"incremental,omitempty"`
	Measured    int    `json:"measured,omitempty"`
	Fulls       int    `json:"fulls"`
	Incrs       int    `json:"incrementals"`
}

type projection struct {
	// Policy is what would be stored: the rules that cannot have an effect at this frequency are 0.
	Policy      retention.Policy  `json:"policy"`
	Ineffective map[string]string `json:"ineffective"`
	NextRuns    []int64           `json:"nextRuns"`
	Points      []projPoint       `json:"points"`
	Kept        int               `json:"kept"`
	Oldest      int64             `json:"oldest,omitempty"`
	// Complete is false when the run limit cut the projection short (very frequent runs kept for very long).
	Complete bool `json:"complete"`
	// Unbounded: the policy keeps everything, the count only grows.
	Unbounded bool      `json:"unbounded,omitempty"`
	Zone      string    `json:"zone"` // the controller's zone, in which hours, days, weeks and months are counted
	Size      *projSize `json:"size,omitempty"`
	Free      *int64    `json:"free,omitempty"` // free bytes in the target
	// Used is what the backups there take now (the VM's backups in the target; the controller's files). Retention
	// replaces them over time, so the estimate is to be held against Free + Used.
	Used   uint64 `json:"used"`
	Chains bool   `json:"chains"` // incremental backups expected (a running VM with qcow2 disks)
	Note   string `json:"note,omitempty"`
}

// projectSchedule runs the policy over the schedule's runs up to now.
func projectSchedule(runs func(from, to time.Time, max int) []time.Time, step time.Duration, chain int, pol retention.Policy, now time.Time) projection {
	pr := retention.Project(retention.Sim{Runs: runs, Step: step, Chain: chain, Policy: pol, Loc: time.Local, Now: now})
	out := projection{Policy: pol, Ineffective: retention.Ineffective(step), Complete: pr.Complete, Unbounded: pr.Unbounded,
		Zone: now.In(time.Local).Format("MST (-07:00)"), Points: []projPoint{}, Chains: chain > 1}
	for _, p := range pr.Points {
		out.Points = append(out.Points, projPoint{Time: p.Time.Unix(), Reason: p.Reason, Rules: p.Rules, Full: p.Full})
	}
	out.Kept = len(out.Points)
	if out.Kept > 0 {
		out.Oldest = out.Points[out.Kept-1].Time
	}
	return out
}

// estimate fills Size from a full size and an average incremental size (0 = not measured).
func (p *projection) estimate(full, incr uint64, measured int, basis string) {
	sz := &projSize{Basis: basis, Full: full, Incremental: incr, Measured: measured}
	for _, pt := range p.Points {
		if pt.Full {
			sz.Fulls++
		} else {
			sz.Incrs++
		}
	}
	base := uint64(sz.Fulls) * full
	if incr > 0 {
		mid := base + uint64(sz.Incrs)*incr
		sz.Low, sz.High = mid-mid/5, mid+mid/5
	} else {
		sz.Low, sz.High = base+uint64(sz.Incrs)*full/50, base+uint64(sz.Incrs)*full/5
	}
	p.Size = sz
}

// nextRuns is the next few run times: the first from the scheduler's own decision, the rest a step apart.
func nextRuns(first time.Time, step time.Duration, daily bool, n int) []int64 {
	out := []int64{first.Unix()}
	t := first
	for len(out) < n {
		if daily {
			t = t.AddDate(0, 0, 1)
		} else {
			t = t.Add(step)
		}
		out = append(out, t.Unix())
	}
	return out
}

// POST /api/nodes/{node}/vms/{uuid}/backup-schedule/preview: what the schedule in the body (not saved) would
// keep and how much space that takes.
func (s *Server) previewBackupSchedule(w http.ResponseWriter, r *http.Request) {
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
	bs.Enabled = true
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
	_, pd, err := destPool(dst, bs.Pool)
	if err != nil {
		fail(w, err)
		return
	}
	uuid := cluster.FormatUUID(dom.UUID)
	items, err := scanPool(dst, pd)
	if err != nil {
		fail(w, err)
		return
	}
	// The VM's backups there, newest first: the last run, the size of the newest full one, the average of recent
	// incremental ones.
	var mine []scanned
	for _, it := range items {
		if it.Err == "" && it.Manifest.UUID == uuid {
			mine = append(mine, it)
		}
	}
	sortScannedNewest(mine)
	var last time.Time
	var full, incrSum uint64
	measured := 0
	for _, it := range mine {
		if last.IsZero() {
			last = it.Manifest.createdTime()
		}
		if it.Manifest.kindOf() == kindIncremental {
			if measured < chainBackups-1 {
				incrSum += it.Size
				measured++
			}
		} else if full == 0 {
			full = it.Size
		}
	}
	state, _, err := h.DomainGetState(dom, 0)
	if err != nil {
		fail(w, err)
		return
	}
	live := stateName(uint8(state)) != "shutoff"
	chains := live && measured > 0
	basis := "backups"
	note := ""
	if full == 0 || (live && measured == 0) {
		// No full backup to measure, or no incremental one yet: the disks tell the size and whether chains form.
		x, err := h.DomainGetXMLDesc(dom, libvirt.DomainXMLInactive)
		if err != nil {
			fail(w, err)
			return
		}
		md, err := parseMigDomain(x)
		if err != nil {
			fail(w, badRequest("cannot read the domain definition: "+err.Error()))
			return
		}
		disks, _, allQcow2, _, err := inspectBackupDisks(h, md, "backed up")
		if err != nil {
			fail(w, err)
			return
		}
		chains = live && allQcow2
		if full == 0 {
			for _, d := range disks {
				full += d.Actual
			}
			basis = "disks"
		}
	}
	if !live {
		note = "VM shut off: every backup is full"
	}
	chain := 1
	if chains {
		chain = chainBackups
	}
	now := time.Now()
	var runs func(from, to time.Time, max int) []time.Time
	if bs.Mode == "interval" {
		anchor := last
		if anchor.IsZero() {
			anchor = now
		}
		runs = retention.IntervalRuns(anchor, bs.step())
	} else {
		hh, _ := strconv.Atoi(bs.At[:2])
		mm, _ := strconv.Atoi(bs.At[3:])
		runs = retention.DailyRuns(hh, mm, time.UTC)
	}
	out := projectSchedule(runs, bs.step(), chain, bs.policy(), now)
	out.Note = note
	if next, ok := bs.timing().NextRun(now, last); ok {
		out.NextRuns = nextRuns(next, bs.step(), bs.Mode == "daily", 3)
	}
	var incr uint64
	if measured > 0 {
		incr = incrSum / uint64(measured)
	}
	out.estimate(full, incr, measured, basis)
	for _, it := range mine {
		out.Used += it.Size
	}
	if free, err := bkFreeBytes(dst, pd.Dir); err == nil {
		out.Free = &free
	}
	writeJSON(w, out)
}

func sortScannedNewest(l []scanned) {
	sort.SliceStable(l, func(i, j int) bool { return l[i].Manifest.createdTime().After(l[j].Manifest.createdTime()) })
}
