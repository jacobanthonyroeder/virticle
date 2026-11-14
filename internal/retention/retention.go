// Package retention decides which backups to keep: the keep-last, keep-hourly, keep-daily, keep-weekly and
// keep-monthly rules of restic, borg and Proxmox Backup. Scheduled VM backups and the controller's own backups
// both use it, and so does the projection the schedule forms show, so that what is shown and what is deleted come
// from the same code.
//
// The rules, walking the backups newest first:
//
//   - last N: the newest N backups.
//   - hourly N: the newest backup of each of the last N distinct hours that have a backup.
//   - daily N, weekly N, monthly N: the same for calendar days, ISO weeks (Monday to Sunday) and calendar months.
//
// A backup kept by any rule is kept; everything else goes. Periods are counted in the time zone given to Plan (the
// controller's local zone). A policy with every count at 0 keeps everything.
package retention

import (
	"fmt"
	"sort"
	"time"
)

// Policy is how many backups each rule keeps. The JSON names are the ones of the backup schedule API.
type Policy struct {
	KeepLast int `json:"keepLast"`
	Hourly   int `json:"keepHourly"`
	Daily    int `json:"keepDaily"`
	Weekly   int `json:"keepWeekly"`
	Monthly  int `json:"keepMonthly"`
}

// Max is the largest count of one rule.
const Max = 1000

// Reasons a backup is kept.
const (
	Last    = "last"
	Hourly  = "hourly"
	Daily   = "daily"
	Weekly  = "weekly"
	Monthly = "monthly"
	All     = "all"    // the policy keeps everything
	Needed  = "needed" // a kept backup builds on it (PlanChains)
	Held    = "in use" // the caller holds it (a restore or export reads it, or it was just written)
)

// Validate checks that every count is between 0 and Max.
func (p Policy) Validate() error {
	for _, c := range []struct {
		name string
		n    int
	}{{"keep-last", p.KeepLast}, {"hourly", p.Hourly}, {"daily", p.Daily}, {"weekly", p.Weekly}, {"monthly", p.Monthly}} {
		if c.n < 0 || c.n > Max {
			return fmt.Errorf("%s must be between 0 and %d", c.name, Max)
		}
	}
	return nil
}

// KeepsAll reports whether the policy keeps every backup (every count is 0).
func (p Policy) KeepsAll() bool { return p == Policy{} }

// Tiered reports whether a rule other than keep-last is set.
func (p Policy) Tiered() bool { return p.Hourly > 0 || p.Daily > 0 || p.Weekly > 0 || p.Monthly > 0 }

// String is a short description, for task notes and logs: "last 3, 24 hourly, 7 daily".
func (p Policy) String() string {
	if p.KeepsAll() {
		return "keep all"
	}
	s := ""
	add := func(n int, what string) {
		if n == 0 {
			return
		}
		if s != "" {
			s += ", "
		}
		s += fmt.Sprintf("%d %s", n, what)
	}
	if p.KeepLast > 0 {
		s = fmt.Sprintf("last %d", p.KeepLast)
	}
	add(p.Hourly, "hourly")
	add(p.Daily, "daily")
	add(p.Weekly, "weekly")
	add(p.Monthly, "monthly")
	return s
}

// Ineffective names the rules that cannot have an effect for backups made every step, with the reason: hourly
// for a schedule that runs at most once a day (daily keeps the same backups), daily for one that runs at most once
// a week, weekly for one that runs at most every four weeks. Monthly and last always count.
func Ineffective(step time.Duration) map[string]string {
	out := map[string]string{}
	if step >= 24*time.Hour {
		out[Hourly] = "Runs at most once a day: Daily keeps these"
	}
	if step >= 7*24*time.Hour {
		out[Daily] = "Runs at most once a week: Weekly keeps these"
	}
	if step >= 28*24*time.Hour {
		out[Weekly] = "Runs at most every four weeks: Monthly keeps these"
	}
	return out
}

// Normalize sets the rules that cannot have an effect for backups made every step to 0 (see Ineffective).
func (p Policy) Normalize(step time.Duration) Policy {
	off := Ineffective(step)
	if off[Hourly] != "" {
		p.Hourly = 0
	}
	if off[Daily] != "" {
		p.Daily = 0
	}
	if off[Weekly] != "" {
		p.Weekly = 0
	}
	return p
}

// period identifies the hour, day, ISO week or month of a time in loc.
type period struct{ a, b, c, d int }

func periodOf(rule string, t time.Time) period {
	switch rule {
	case Hourly:
		// The offset tells apart the two 01:00 hours of a night when the clocks go back.
		_, off := t.Zone()
		return period{t.Year(), t.YearDay(), t.Hour(), off}
	case Daily:
		return period{t.Year(), t.YearDay(), 0, 0}
	case Weekly:
		y, w := t.ISOWeek()
		return period{y, w, 0, 0}
	default:
		return period{t.Year(), int(t.Month()), 0, 0}
	}
}

// order returns the indexes of times, newest first. Equal times count the later index as newer.
func order(times []time.Time) []int {
	idx := make([]int, len(times))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(i, j int) bool {
		a, b := times[idx[i]], times[idx[j]]
		if !a.Equal(b) {
			return a.After(b)
		}
		return idx[i] > idx[j]
	})
	return idx
}

// Plan decides, for backups made at times (in any order), which ones the policy keeps. keep[i] and reason[i]
// belong to times[i]; reason is the rule that keeps it ("" for a backup that goes). When several rules keep a
// backup the one with the longest period is named (monthly, weekly, daily, hourly, last), since that is the rule
// it stays for longest. loc is the zone periods are counted in; nil means UTC.
func Plan(times []time.Time, p Policy, loc *time.Location) (keep []bool, reason []string) {
	rules := plan(times, p, loc)
	keep = make([]bool, len(times))
	reason = make([]string, len(times))
	for i, m := range rules {
		keep[i], reason[i] = m != 0, m.longest()
	}
	return
}

// ruleSet is the rules that keep one backup, a bit per rule in the order of ruleNames.
type ruleSet uint8

var ruleNames = []string{Last, Hourly, Daily, Weekly, Monthly, All}

func (m ruleSet) longest() string {
	for i := len(ruleNames) - 1; i >= 0; i-- {
		if m&(1<<i) != 0 {
			return ruleNames[i]
		}
	}
	return ""
}

// Names lists the rules in the set, shortest period first.
func (m ruleSet) Names() []string {
	var out []string
	for i, n := range ruleNames {
		if m&(1<<i) != 0 {
			out = append(out, n)
		}
	}
	return out
}

func plan(times []time.Time, p Policy, loc *time.Location) []ruleSet {
	out := make([]ruleSet, len(times))
	if p.KeepsAll() {
		for i := range out {
			out[i] = 1 << 5
		}
		return out
	}
	if loc == nil {
		loc = time.UTC
	}
	idx := order(times)
	for n, i := range idx {
		if n < p.KeepLast {
			out[i] |= 1
		}
	}
	for bit, r := range []struct {
		rule  string
		count int
	}{{Hourly, p.Hourly}, {Daily, p.Daily}, {Weekly, p.Weekly}, {Monthly, p.Monthly}} {
		left := r.count
		var last period
		first := true
		for _, i := range idx {
			if left == 0 {
				break
			}
			pd := periodOf(r.rule, times[i].In(loc))
			if !first && pd == last {
				continue
			}
			first, last = false, pd
			out[i] |= 1 << (bit + 1)
			left--
		}
	}
	return out
}

// Item is one backup for PlanChains: when it was made and the index of the backup it builds on (-1 for a
// backup that needs no other one).
type Item struct {
	Time   time.Time
	Parent int
	// Hold keeps the backup whatever the policy says (and with it every backup it builds on): it was just
	// written, or something reads it right now.
	Hold bool
}

// PlanChains is Plan for backups that may build on each other (incremental backups): a backup a kept one
// builds on is kept too, with reason Needed, and so are held ones and what they build on. The set of backups that
// go is therefore closed under "needed by": no kept backup ever loses a backup it needs. A parent index that is
// out of range is ignored (the parent is not in the list, so there is nothing to keep).
func PlanChains(items []Item, p Policy, loc *time.Location) (keep []bool, reason []string) {
	keep, reason, _ = planChains(items, p, loc)
	return
}

func planChains(items []Item, p Policy, loc *time.Location) (keep []bool, reason []string, rules []ruleSet) {
	times := make([]time.Time, len(items))
	for i, it := range items {
		times[i] = it.Time
	}
	rules = plan(times, p, loc)
	keep = make([]bool, len(items))
	reason = make([]string, len(items))
	for i, m := range rules {
		keep[i], reason[i] = m != 0, m.longest()
	}
	for i, it := range items {
		if it.Hold && !keep[i] {
			keep[i], reason[i] = true, Held
		}
	}
	for i := range items {
		if !keep[i] {
			continue
		}
		// Walk up the chain; a loop in bad data ends after len(items) steps.
		for k, n := items[i].Parent, 0; k >= 0 && k < len(items) && n < len(items); k, n = items[k].Parent, n+1 {
			if keep[k] {
				// Already kept: what it needs was or will be marked from it.
				if reason[k] != Needed {
					break
				}
				continue
			}
			keep[k], reason[k] = true, Needed
		}
	}
	return
}
