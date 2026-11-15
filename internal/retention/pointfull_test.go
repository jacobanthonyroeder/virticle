package retention

import (
	"testing"
	"time"
)

func TestPointFull(t *testing.T) {
	loc := berlin(t)
	at := func(m time.Month, d, h int) time.Time { return time.Date(2026, m, d, h, 30, 0, 0, loc) }
	wm := Policy{Daily: 7, Weekly: 4, Monthly: 6}
	cases := []struct {
		name          string
		p             Policy
		prev, t, next time.Time
		want          bool
	}{
		// 8 Nov 2026 is a Sunday, 31 Oct a Saturday.
		{"last run of the week", wm, at(11, 7, 3), at(11, 8, 3), at(11, 9, 3), true},
		{"mid week", wm, at(11, 9, 3), at(11, 10, 3), at(11, 11, 3), false},
		{"first run of the week", wm, at(11, 8, 3), at(11, 9, 3), at(11, 10, 3), false},
		{"last run of the month", Policy{Monthly: 6}, at(10, 30, 3), at(10, 31, 3), at(11, 1, 3), true},
		{"last run of the week, no long rule", Policy{KeepLast: 3, Hourly: 24, Daily: 7}, at(11, 7, 3), at(11, 8, 3), at(11, 9, 3), false},
		{"last run of the week, monthly only", Policy{Monthly: 6}, at(11, 7, 3), at(11, 8, 3), at(11, 9, 3), false},
		{"hourly, last hour of the week", wm, at(11, 8, 22), at(11, 8, 23), time.Date(2026, 11, 9, 0, 30, 0, 0, loc), true},
		{"hourly, not the last hour", wm, at(11, 8, 21), at(11, 8, 22), at(11, 8, 23), false},
		// A weekly schedule: every run is alone in its week and builds on the previous weekly point.
		{"alone in its week", Policy{Weekly: 8}, at(11, 1, 3), at(11, 8, 3), at(11, 15, 3), false},
		{"alone in its week, last of the month", Policy{Weekly: 8, Monthly: 6}, at(10, 18, 3), at(10, 25, 3), at(11, 1, 3), true},
		{"first backup", wm, time.Time{}, at(11, 8, 3), at(11, 9, 3), false},
		{"no next run", wm, at(11, 7, 3), at(11, 8, 3), time.Time{}, false},
	}
	for _, c := range cases {
		if got := PointFull(c.p, c.prev, c.t, c.next, loc); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

// counts are what a projection keeps: all points, those kept only for a chain, and the full ones.
type counts struct{ kept, needed, fulls int }

func countOf(pr Projection) counts {
	var c counts
	for _, p := range pr.Points {
		c.kept++
		if p.Reason == Needed {
			c.needed++
		}
		if p.Full {
			c.fulls++
		}
	}
	return c
}

// Schedules run on paper over several months, with and without full weekly and monthly points: the rule drops the
// backups kept only for the chain of a weekly or monthly point, and every such point of a finished period is a full
// backup. What remains of "needed" are the chains of daily and hourly points, as before.
func TestProjectPointFulls(t *testing.T) {
	loc := berlin(t)
	now := time.Date(2026, 11, 14, 12, 7, 0, 0, loc)
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		runs       func(from, to time.Time, max int) []time.Time
		step       time.Duration
		p          Policy
		with, plan counts // with the rule, and with the rotation alone
	}{
		{"daily 03:30, 7 daily 4 weekly 6 monthly", DailyRuns(3, 30, loc), 24 * time.Hour, Policy{Daily: 7, Weekly: 4, Monthly: 6},
			counts{14, 0, 8}, counts{31, 17, 8}},
		{"hourly, 24 hourly 7 daily 4 weekly 6 monthly", IntervalRuns(anchor, time.Hour), time.Hour, Policy{Hourly: 24, Daily: 7, Weekly: 4, Monthly: 6},
			counts{57, 21, 17}, counts{69, 33, 17}},
		{"every 6 hours, 14 daily 8 weekly 12 monthly", IntervalRuns(anchor, 6*time.Hour), 6 * time.Hour, Policy{Daily: 14, Weekly: 8, Monthly: 12},
			counts{58, 28, 24}, counts{100, 70, 24}},
		{"daily, 7 daily only", DailyRuns(3, 30, loc), 24 * time.Hour, Policy{Daily: 7},
			counts{9, 2, 2}, counts{9, 2, 2}},
	}
	for _, c := range cases {
		s := Sim{Runs: c.runs, Step: c.step, Chain: 7, Policy: c.p, Loc: loc, Now: now}
		with := Project(s)
		s.rotationOnly = true
		without := Project(s)
		cw, co := countOf(with), countOf(without)
		t.Logf("%s: with the rule %+v, rotation alone %+v", c.name, cw, co)
		if cw != c.with || co != c.plan {
			t.Errorf("%s: with the rule %+v (want %+v), rotation alone %+v (want %+v)", c.name, cw, c.with, co, c.plan)
		}
		// Every weekly or monthly point of a period that has ended is full.
		y, w := now.ISOWeek()
		for _, pt := range with.Points {
			for _, r := range pt.Rules {
				py, pw := pt.Time.In(loc).ISOWeek()
				ended := r == Weekly && (py != y || pw != w) || r == Monthly && (pt.Time.In(loc).Month() != now.Month() || pt.Time.In(loc).Year() != now.Year())
				if ended && !pt.Full {
					t.Errorf("%s: the %s point %v is incremental", c.name, r, pt.Time)
				}
			}
		}
	}
}
