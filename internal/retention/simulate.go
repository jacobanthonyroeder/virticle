package retention

import "time"

// Projection: what a schedule keeps once it has run for a while. The schedule is run on paper from far enough
// back to fill every rule up to now, pruning with PlanChains as the real runs do, and what remains is the steady
// state. The times are real past times, as if the schedule had been running all along.

// Sim describes the schedule to project.
type Sim struct {
	// Runs returns the run times of the schedule from from to to, oldest first.
	Runs func(from, to time.Time, max int) []time.Time
	// Step is the usual time between two runs (an interval, or a day).
	Step time.Duration
	// Chain is how many backups a chain holds: a full one, then Chain-1 that build each on the one before
	// (1 or less: every backup stands alone).
	Chain  int
	Policy Policy
	Loc    *time.Location
	Now    time.Time
}

// Point is one backup of the steady state.
type Point struct {
	Time   time.Time
	Reason string
	// Rules are all the rules that keep it, shortest period first (empty for a backup kept only because a kept one
	// builds on it).
	Rules []string
	Full  bool
}

// Projection is the result of Project.
type Projection struct {
	Points []Point // newest first
	// From is when the simulated runs start; Complete is false when the run count limit cut the simulation short,
	// so that the longest rules are not filled.
	From     time.Time
	Complete bool
	// Unbounded is set for a policy that keeps everything: the count only grows.
	Unbounded bool
}

// MaxSimRuns bounds the work of one projection.
const MaxSimRuns = 600000

// pruneEvery is how many simulated runs pass between two prunes. Pruning less often than every run gives the same
// result: a backup that a later prune keeps was kept by every prune before it.
const pruneEvery = 2048

// reach is how far back the policy can keep backups made every step, with one period to spare.
func (p Policy) reach(step time.Duration) time.Duration {
	if step <= 0 {
		step = time.Hour
	}
	longest := time.Duration(p.KeepLast+1) * step
	for _, r := range []struct {
		n   int
		per time.Duration
	}{{p.Hourly, time.Hour}, {p.Daily, 24 * time.Hour}, {p.Weekly, 7 * 24 * time.Hour}, {p.Monthly, 31 * 24 * time.Hour}} {
		if r.n == 0 {
			continue
		}
		per := r.per
		if step > per {
			per = step
		}
		if d := time.Duration(r.n+1) * per; d > longest {
			longest = d
		}
	}
	return longest + step
}

// Project runs the schedule on paper and returns what it keeps at Now.
func Project(s Sim) Projection {
	const maxSpan = 11 * 366 * 24 * time.Hour
	span := s.Policy.reach(s.Step)
	pr := Projection{Complete: true}
	if s.Policy.KeepsAll() {
		// Everything stays; show one step's worth of the newest ones.
		pr.Unbounded = true
		span = 32 * s.Step
	}
	if span > maxSpan {
		span, pr.Complete = maxSpan, false
	}
	if s.Step > 0 && span/s.Step > MaxSimRuns {
		span, pr.Complete = time.Duration(MaxSimRuns)*s.Step, false
	}
	pr.From = s.Now.Add(-span)
	runs := s.Runs(pr.From, s.Now, MaxSimRuns+1)
	if len(runs) > MaxSimRuns {
		runs = runs[len(runs)-MaxSimRuns:]
		pr.Complete = false
	}
	var items []Item
	full := make([]bool, 0, pruneEvery*2)
	prune := func() {
		keep, _ := PlanChains(items, s.Policy, s.Loc)
		newIdx := make([]int, len(items))
		var ni []Item
		var nf []bool
		for i, it := range items {
			newIdx[i] = -1
			if keep[i] {
				newIdx[i] = len(ni)
				ni = append(ni, it)
				nf = append(nf, full[i])
			}
		}
		for i := range ni {
			if ni[i].Parent >= 0 {
				ni[i].Parent = newIdx[ni[i].Parent]
			}
		}
		items, full = ni, nf
	}
	for n, t := range runs {
		it := Item{Time: t, Parent: -1}
		isFull := s.Chain <= 1 || n%s.Chain == 0
		if !isFull && len(items) > 0 {
			it.Parent = len(items) - 1 // the newest backup is always kept, so the chain goes on from it
		}
		items = append(items, it)
		full = append(full, isFull)
		if len(items) >= pruneEvery && !pr.Unbounded {
			prune()
		}
	}
	keep, reason, rules := planChains(items, s.Policy, s.Loc)
	for i := len(items) - 1; i >= 0; i-- {
		if keep[i] {
			pr.Points = append(pr.Points, Point{Time: items[i].Time, Reason: reason[i], Rules: rules[i].Names(), Full: full[i]})
		}
	}
	return pr
}

// IntervalRuns are the run times of a schedule that runs every step, aligned so that one run is at anchor.
func IntervalRuns(anchor time.Time, step time.Duration) func(from, to time.Time, max int) []time.Time {
	return func(from, to time.Time, max int) []time.Time {
		if step <= 0 {
			return nil
		}
		// The first run at or after from.
		k := from.Sub(anchor) / step
		t := anchor.Add(k * step)
		for t.Before(from) {
			t = t.Add(step)
		}
		var out []time.Time
		for ; !t.After(to) && len(out) < max; t = t.Add(step) {
			out = append(out, t)
		}
		return out
	}
}

// DailyRuns are the run times of a schedule that runs once a day at hour:minute in loc.
func DailyRuns(hour, minute int, loc *time.Location) func(from, to time.Time, max int) []time.Time {
	return func(from, to time.Time, max int) []time.Time {
		f := from.In(loc)
		var out []time.Time
		for d := time.Date(f.Year(), f.Month(), f.Day(), hour, minute, 0, 0, loc); !d.After(to) && len(out) < max; d = time.Date(d.Year(), d.Month(), d.Day()+1, hour, minute, 0, 0, loc) {
			if !d.Before(from) {
				out = append(out, d)
			}
		}
		return out
	}
}
