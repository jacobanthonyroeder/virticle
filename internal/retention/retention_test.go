package retention

import (
	"math/rand/v2"
	"sort"
	"testing"
	"time"
)

func berlin(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("no time zone data:", err)
	}
	return loc
}

// kept returns the local times (as "2006-01-02 15:04 MST") of the kept backups, newest first, with reasons.
func kept(times []time.Time, p Policy, loc *time.Location) []string {
	keep, reason := Plan(times, p, loc)
	var out []string
	for _, i := range order(times) {
		if keep[i] {
			out = append(out, times[i].In(loc).Format("2006-01-02 15:04 MST")+" "+reason[i])
		}
	}
	return out
}

func eq(t *testing.T, name string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d kept %q, want %d %q", name, len(got), got, len(want), want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: kept[%d] = %q, want %q (all: %q)", name, i, got[i], want[i], got)
		}
	}
}

func TestPlanBoundaries(t *testing.T) {
	loc := berlin(t)
	at := func(s string) time.Time {
		tm, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	ts := func(ss ...string) []time.Time {
		var out []time.Time
		for _, s := range ss {
			out = append(out, at(s))
		}
		return out
	}
	cases := []struct {
		name  string
		times []time.Time
		p     Policy
		want  []string
	}{
		{"many in one hour: hourly keeps the newest of each hour",
			ts("2026-06-01 10:05", "2026-06-01 10:35", "2026-06-01 10:55", "2026-06-01 11:10", "2026-06-01 11:40"),
			Policy{Hourly: 5},
			[]string{"2026-06-01 11:40 CEST hourly", "2026-06-01 10:55 CEST hourly"}},
		{"hour boundary at midnight is a day boundary too",
			ts("2026-06-01 23:50", "2026-06-02 00:10"),
			Policy{Hourly: 1, Daily: 2},
			[]string{"2026-06-02 00:10 CEST daily", "2026-06-01 23:50 CEST daily"}},
		{"local day, not UTC day: 00:30 Berlin is still the previous day in UTC",
			ts("2026-06-01 23:30", "2026-06-02 00:30", "2026-06-02 01:30"),
			Policy{Daily: 5},
			[]string{"2026-06-02 01:30 CEST daily", "2026-06-01 23:30 CEST daily"}},
		{"ISO week: Sunday and Monday are different weeks",
			ts("2026-06-07 12:00", "2026-06-08 12:00", "2026-06-09 12:00"),
			Policy{Weekly: 3},
			[]string{"2026-06-09 12:00 CEST weekly", "2026-06-07 12:00 CEST weekly"}},
		{"ISO week across the new year: 31 Dec 2026 and 1 Jan 2027 are both in 2026-W53",
			ts("2026-12-27 12:00", "2026-12-28 12:00", "2026-12-31 12:00", "2027-01-01 12:00"),
			Policy{Weekly: 5},
			[]string{"2027-01-01 12:00 CET weekly", "2026-12-27 12:00 CET weekly"}},
		{"month and year boundary",
			ts("2026-11-30 12:00", "2026-12-01 12:00", "2026-12-31 23:59", "2027-01-01 00:01"),
			Policy{Monthly: 3},
			[]string{"2027-01-01 00:01 CET monthly", "2026-12-31 23:59 CET monthly", "2026-11-30 12:00 CET monthly"}},
		{"gap: the VM was off for days; daily counts days that have a backup",
			ts("2026-06-01 03:00", "2026-06-02 03:00", "2026-06-03 03:00", "2026-06-10 03:00", "2026-06-11 03:00"),
			Policy{Daily: 4},
			[]string{"2026-06-11 03:00 CEST daily", "2026-06-10 03:00 CEST daily", "2026-06-03 03:00 CEST daily", "2026-06-02 03:00 CEST daily"}},
		{"keep-last and daily together; the longer rule names the reason",
			ts("2026-06-01 03:00", "2026-06-02 03:00", "2026-06-02 09:00", "2026-06-02 15:00"),
			Policy{KeepLast: 2, Daily: 2},
			[]string{"2026-06-02 15:00 CEST daily", "2026-06-02 09:00 CEST last", "2026-06-01 03:00 CEST daily"}},
		{"keep-last only",
			ts("2026-06-01 03:00", "2026-06-02 03:00", "2026-06-03 03:00"),
			Policy{KeepLast: 2},
			[]string{"2026-06-03 03:00 CEST last", "2026-06-02 03:00 CEST last"}},
		{"all zero keeps everything",
			ts("2026-06-01 03:00", "2026-06-01 03:01", "2026-06-02 03:00"),
			Policy{},
			[]string{"2026-06-02 03:00 CEST all", "2026-06-01 03:01 CEST all", "2026-06-01 03:00 CEST all"}},
		{"standard preset over a year keeps one per month",
			ts("2025-12-15 03:00", "2026-01-15 03:00", "2026-02-15 03:00", "2026-03-15 03:00"),
			Policy{Hourly: 24, Daily: 7, Weekly: 4, Monthly: 2},
			[]string{"2026-03-15 03:00 CET monthly", "2026-02-15 03:00 CET monthly", "2026-01-15 03:00 CET weekly", "2025-12-15 03:00 CET weekly"}},
	}
	for _, c := range cases {
		eq(t, c.name, kept(c.times, c.p, loc), c.want)
	}
}

func TestPlanDST(t *testing.T) {
	loc := berlin(t)
	// The clocks go back on 25 Oct 2026 at 03:00 CEST to 02:00 CET: the hour from 02:00 to 03:00 local happens twice.
	start := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC) // 02:00 CEST
	var times []time.Time
	for i := 0; i < 8; i++ { // every 30 minutes for four hours (absolute time)
		times = append(times, start.Add(time.Duration(i)*30*time.Minute))
	}
	got := kept(times, Policy{Hourly: 10}, loc)
	if len(got) != 4 {
		t.Fatalf("four absolute hours must give four hourly backups, got %q", got)
	}
	// Both 02:30 local times are kept: two distinct hours share the local hour 02.
	n := 0
	for _, s := range got {
		if len(s) >= 16 && s[11:16] == "02:30" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("the repeated local hour must count twice, got %q", got)
	}
	// Daily: all of it is one local day.
	if got := kept(times, Policy{Daily: 3}, loc); len(got) != 1 {
		t.Fatalf("one local day, got %q", got)
	}
	// Spring forward (29 Mar 2026, 02:00 CET to 03:00 CEST): the day has 23 hours and is still one day.
	sp := time.Date(2026, 3, 28, 23, 30, 0, 0, time.UTC) // 00:30 CET on the 29th
	times = nil
	for i := 0; i < 23; i++ {
		times = append(times, sp.Add(time.Duration(i)*time.Hour))
	}
	if got := kept(times, Policy{Daily: 5}, loc); len(got) != 1 {
		t.Fatalf("23 hourly backups of the spring-forward day are one day, got %q", got)
	}
	if got := kept(times, Policy{Hourly: 30}, loc); len(got) != 23 {
		t.Fatalf("23 distinct hours, got %d", len(got))
	}
}

func TestPlanClockBackwards(t *testing.T) {
	loc := time.UTC
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, loc)
	// Made in this order, but the clock was set back an hour before the third one.
	times := []time.Time{base, base.Add(time.Hour), base.Add(10 * time.Minute), base.Add(2 * time.Hour)}
	keep, reason := Plan(times, Policy{KeepLast: 2}, loc)
	// The newest two by time are index 3 and 1, whatever the order of the list.
	if !keep[3] || !keep[1] || keep[0] || keep[2] {
		t.Fatalf("keep = %v", keep)
	}
	if reason[3] != Last || reason[0] != "" {
		t.Fatalf("reason = %q", reason)
	}
	// Equal times: the later one in the list counts as newer, and both are distinct backups.
	same := []time.Time{base, base, base}
	keep, _ = Plan(same, Policy{KeepLast: 1}, loc)
	if !keep[2] || keep[0] || keep[1] {
		t.Fatalf("equal times: keep = %v", keep)
	}
	keep, _ = Plan(same, Policy{Hourly: 3}, loc)
	if n := count(keep); n != 1 {
		t.Fatalf("three backups in one hour, hourly keeps one, got %d", n)
	}
}

func count(b []bool) int {
	n := 0
	for _, v := range b {
		if v {
			n++
		}
	}
	return n
}

// The newest backup is kept by every policy that keeps anything.
func TestPlanAlwaysKeepsNewest(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for n := 0; n < 2000; n++ {
		times := make([]time.Time, 1+r.IntN(40))
		for i := range times {
			times[i] = base.Add(time.Duration(r.IntN(90*24*60)) * time.Minute)
		}
		p := Policy{KeepLast: r.IntN(3), Hourly: r.IntN(3), Daily: r.IntN(3), Weekly: r.IntN(3), Monthly: r.IntN(3)}
		keep, reason := Plan(times, p, time.UTC)
		if newest := order(times)[0]; !keep[newest] || reason[newest] == "" {
			t.Fatalf("policy %+v dropped the newest backup", p)
		}
		for i := range keep {
			if keep[i] != (reason[i] != "") {
				t.Fatalf("keep and reason disagree at %d", i)
			}
		}
	}
}

// Keep-last alone is the retention of before: the newest N by time.
func TestPlanKeepLastIsNewestN(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for n := 0; n < 1000; n++ {
		times := make([]time.Time, r.IntN(30))
		for i := range times {
			times[i] = base.Add(time.Duration(r.IntN(1e6)) * time.Second)
		}
		k := 1 + r.IntN(10)
		keep, _ := Plan(times, Policy{KeepLast: k}, time.UTC)
		sorted := append([]time.Time(nil), times...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].After(sorted[j]) })
		for i, tm := range times {
			// Strictly newer than the k-th newest: kept; strictly older: not (ties are decided by index).
			if len(sorted) > k && tm.Before(sorted[k-1]) && keep[i] {
				t.Fatalf("kept an old one")
			}
			if (len(sorted) <= k || tm.After(sorted[k])) && !keep[i] {
				t.Fatalf("dropped one of the newest %d", k)
			}
		}
		if want := min(k, len(times)); count(keep) != want {
			t.Fatalf("kept %d, want %d", count(keep), want)
		}
	}
}

func TestPolicyValidateNormalize(t *testing.T) {
	if err := (Policy{Daily: 1001}).Validate(); err == nil {
		t.Fatal("1001 accepted")
	}
	if err := (Policy{KeepLast: -1}).Validate(); err == nil {
		t.Fatal("-1 accepted")
	}
	if err := (Policy{KeepLast: 3, Hourly: 24, Monthly: 1000}).Validate(); err != nil {
		t.Fatal(err)
	}
	p := Policy{KeepLast: 1, Hourly: 24, Daily: 7, Weekly: 4, Monthly: 6}
	if got := p.Normalize(time.Hour); got != p {
		t.Fatalf("hourly runs keep everything: %+v", got)
	}
	if got := p.Normalize(24 * time.Hour); got.Hourly != 0 || got.Daily != 7 {
		t.Fatalf("daily runs: %+v", got)
	}
	if got := p.Normalize(7 * 24 * time.Hour); got.Hourly != 0 || got.Daily != 0 || got.Weekly != 4 {
		t.Fatalf("weekly runs: %+v", got)
	}
	if got := p.Normalize(30 * 24 * time.Hour); got.Weekly != 0 || got.Monthly != 6 || got.KeepLast != 1 {
		t.Fatalf("monthly runs: %+v", got)
	}
	if s := p.String(); s != "last 1, 24 hourly, 7 daily, 4 weekly, 6 monthly" {
		t.Fatalf("String = %q", s)
	}
	if s := (Policy{Daily: 7}).String(); s != "7 daily" {
		t.Fatalf("String = %q", s)
	}
}

// randomChains makes backups every hour, a full one followed by up to six incremental ones on the one before,
// with some parents missing (deleted by hand) and some out of range.
func randomChains(r *rand.Rand, n int) []Item {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	items := make([]Item, n)
	for i := range items {
		items[i] = Item{Time: base.Add(time.Duration(i*60+r.IntN(30)) * time.Minute), Parent: -1}
		if i > 0 && r.IntN(7) != 0 {
			items[i].Parent = i - 1
		}
		if r.IntN(50) == 0 {
			items[i].Parent = n + 5 // a parent that is not in the list
		}
		if r.IntN(40) == 0 {
			items[i].Hold = true
		}
	}
	r.Shuffle(len(items), func(i, j int) {
		items[i], items[j] = items[j], items[i]
		for k := range items {
			switch items[k].Parent {
			case i:
				items[k].Parent = j
			case j:
				items[k].Parent = i
			}
		}
	})
	return items
}

// What PlanChains deletes is closed under "needed by": no kept backup builds on one that goes.
func TestPlanChainsClosed(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	for n := 0; n < 1000; n++ {
		items := randomChains(r, 1+r.IntN(200))
		p := Policy{KeepLast: r.IntN(4), Hourly: r.IntN(10), Daily: r.IntN(4), Weekly: r.IntN(3), Monthly: r.IntN(2)}
		keep, reason := PlanChains(items, p, time.UTC)
		for i, it := range items {
			if it.Hold && !keep[i] {
				t.Fatal("a held backup goes")
			}
			if keep[i] && it.Parent >= 0 && it.Parent < len(items) && !keep[it.Parent] {
				t.Fatalf("policy %+v: kept backup %d (%s) needs %d, which goes", p, i, reason[i], it.Parent)
			}
			if keep[i] == (reason[i] == "") {
				t.Fatal("keep and reason disagree")
			}
		}
	}
}

func TestPlanChainsLoop(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	items := []Item{{Time: base, Parent: 2}, {Time: base.Add(time.Hour), Parent: 0}, {Time: base.Add(2 * time.Hour), Parent: 1}}
	keep, reason := PlanChains(items, Policy{KeepLast: 1}, time.UTC)
	if !keep[0] || !keep[1] || !keep[2] || reason[2] != Last || reason[0] != Needed {
		t.Fatalf("keep %v reason %q", keep, reason)
	}
}

func TestProjectDaily(t *testing.T) {
	loc := berlin(t)
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, loc)
	pr := Project(Sim{Runs: DailyRuns(3, 30, loc), Step: 24 * time.Hour, Policy: Policy{Daily: 7, Weekly: 4, Monthly: 6}, Loc: loc, Now: now})
	if !pr.Complete {
		t.Fatal("incomplete")
	}
	by := map[string]int{}
	for _, p := range pr.Points {
		for _, r := range p.Rules {
			by[r]++
		}
	}
	// 7 dailies (14 back to 8 Nov); 4 weeklies: 14 Nov (W46), the Sundays 8 Nov, 1 Nov and 25 Oct; 6 monthlies:
	// 14 Nov, 31 Oct, 30 Sep, 31 Aug, 31 Jul, 30 Jun.
	if len(pr.Points) == 0 || !pr.Points[0].Time.Equal(time.Date(2026, 11, 14, 3, 30, 0, 0, loc)) {
		t.Fatalf("newest point %v", pr.Points)
	}
	if by[Daily] != 7 || by[Weekly] != 4 || by[Monthly] != 6 {
		t.Fatalf("by rule %v", by)
	}
	oldest := pr.Points[len(pr.Points)-1].Time
	if oldest.Month() != time.June || oldest.Day() != 30 {
		t.Fatalf("oldest %v, points %v", oldest, pr.Points)
	}
	if len(pr.Points) != 7+2+5 { // the dailies, 1 and 25 Oct (weekly), 31 Oct back to 30 Jun (monthly)
		t.Fatalf("%d points: %v", len(pr.Points), by)
	}
}

// Pruning every so many runs keeps the same as pruning after every run, chains included.
func TestProjectPruneCadence(t *testing.T) {
	loc := berlin(t)
	now := time.Date(2026, 11, 14, 12, 7, 0, 0, loc)
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, p := range []Policy{{KeepLast: 3, Hourly: 5, Daily: 3}, {Hourly: 24, Daily: 7, Weekly: 4, Monthly: 6}, {KeepLast: 2}} {
		for _, step := range []time.Duration{10 * time.Minute, 3 * time.Hour} {
			s := Sim{Runs: IntervalRuns(anchor, step), Step: step, Chain: 7, Policy: p, Loc: loc, Now: now}
			got := Project(s)
			// The reference: prune after every run.
			runs := s.Runs(got.From, now, MaxSimRuns)
			var items []Item
			var full []bool
			for n, tm := range runs {
				it := Item{Time: tm, Parent: -1}
				f := n%7 == 0
				if !f && len(items) > 0 {
					it.Parent = len(items) - 1
				}
				items = append(items, it)
				full = append(full, f)
				keep, _ := PlanChains(items, p, loc)
				idx := make([]int, len(items))
				var ni []Item
				var nf []bool
				for i := range items {
					idx[i] = -1
					if keep[i] {
						idx[i] = len(ni)
						ni = append(ni, items[i])
						nf = append(nf, full[i])
					}
				}
				for i := range ni {
					if ni[i].Parent >= 0 {
						ni[i].Parent = idx[ni[i].Parent]
					}
				}
				items, full = ni, nf
			}
			if len(items) != len(got.Points) {
				t.Fatalf("policy %+v step %v: every run keeps %d, projection %d", p, step, len(items), len(got.Points))
			}
			for i := range items {
				pt := got.Points[len(items)-1-i]
				if !pt.Time.Equal(items[i].Time) || pt.Full != full[i] {
					t.Fatalf("policy %+v step %v: point %d differs", p, step, i)
				}
			}
			// Every incremental point has its parent: the run before it is kept too.
			for i, pt := range got.Points {
				if !pt.Full && (i+1 >= len(got.Points) || !got.Points[i+1].Time.Equal(pt.Time.Add(-step))) {
					t.Fatalf("policy %+v step %v: incremental point %v without its parent", p, step, pt.Time)
				}
			}
		}
	}
}

func TestProjectLimits(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	// Every minute with 120 monthlies: far more runs than the limit.
	pr := Project(Sim{Runs: IntervalRuns(now, time.Minute), Step: time.Minute, Policy: Policy{Monthly: 120}, Loc: time.UTC, Now: now})
	if pr.Complete {
		t.Fatal("a cut-short projection must say so")
	}
	pr = Project(Sim{Runs: IntervalRuns(now, time.Hour), Step: time.Hour, Policy: Policy{}, Loc: time.UTC, Now: now})
	if !pr.Unbounded || len(pr.Points) == 0 {
		t.Fatal("keep everything is unbounded")
	}
}

func TestRunGenerators(t *testing.T) {
	loc := berlin(t)
	from := time.Date(2026, 10, 24, 0, 0, 0, 0, loc)
	to := time.Date(2026, 10, 27, 0, 0, 0, 0, loc)
	d := DailyRuns(3, 30, loc)(from, to, 100)
	if len(d) != 3 || d[1].Hour() != 3 || d[1].Day() != 25 || d[2].Sub(d[1]) != 24*time.Hour || d[1].Sub(d[0]) != 25*time.Hour {
		t.Fatalf("daily runs across DST: %v", d)
	}
	anchor := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	iv := IntervalRuns(anchor, 6*time.Hour)(time.Date(2026, 3, 1, 1, 0, 0, 0, time.UTC), time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC), 100)
	if len(iv) != 3 || iv[0].Hour() != 6 || iv[0].Minute() != 5 {
		t.Fatalf("interval runs: %v", iv)
	}
	if got := IntervalRuns(anchor, time.Hour)(anchor, anchor.Add(100*time.Hour), 10); len(got) != 10 {
		t.Fatalf("max not honoured: %d", len(got))
	}
}
