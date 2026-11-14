package api

import (
	"testing"
	"time"

	"virticle/internal/store"
)

func TestJudgeWork(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	mine, theirs := "me", "earlier"
	stage := func(owner string, newest time.Duration) workCheck {
		return workCheck{rec: store.WorkDir{Path: "/p/.virticle-rp.AAAAAA", Kind: workKindStage, Owner: owner}, exists: true, newest: now.Add(-newest)}
	}
	replica := func(c workCheck) workCheck { c.rec.Kind = workKindReplica; return c }
	cases := []struct {
		name  string
		c     workCheck
		want  workVerdict
		retry bool
	}{
		{"held by a run of this process", func() workCheck { c := stage(mine, time.Hour); c.inUse = true; return c }(), workKeep, false},
		{"held, of an earlier process", func() workCheck { c := stage(theirs, time.Hour); c.inUse = true; return c }(), workKeep, false},
		{"gone", func() workCheck { c := stage(theirs, time.Hour); c.exists = false; return c }(), workForget, false},
		{"this process, run ended", stage(mine, time.Second), workRemove, false},
		{"earlier process, changed a minute ago", stage(theirs, time.Minute), workKeep, true},
		{"earlier process, quiet", stage(theirs, workQuiet+time.Second), workRemove, false},
		{"new replica with a copy in it", func() workCheck { c := replica(stage(mine, time.Hour)); c.holdsCopy, c.known = true, true; return c }(), workForget, false},
		{"new replica, a node did not answer", replica(stage(mine, time.Hour)), workKeep, true},
		{"new replica, named by a setting", func() workCheck { c := replica(stage(mine, time.Hour)); c.known, c.referenced = true, true; return c }(), workForget, false},
		{"new replica, named by nothing", func() workCheck { c := replica(stage(mine, time.Hour)); c.known = true; return c }(), workRemove, false},
		{"new replica of an earlier process, recent", func() workCheck { c := replica(stage(theirs, time.Minute)); c.known = true; return c }(), workKeep, true},
	}
	for _, tc := range cases {
		v, retry := judgeWork(tc.c, mine, now)
		if v != tc.want || (retry > 0) != tc.retry {
			t.Errorf("%s: got %v retry %v, want %v retry %v", tc.name, v, retry, tc.want, tc.retry)
		}
	}
	// The follow-up comes once the quiet time is over, not long after.
	if _, retry := judgeWork(stage(theirs, 4*time.Minute), mine, now); retry < workQuiet-4*time.Minute || retry > workQuiet-4*time.Minute+time.Minute {
		t.Errorf("retry %v", retry)
	}
}

func TestHoldAndReleaseWork(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/v.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &Server{Store: st}
	p := "/p/.virticle-bk.TEST01"
	s.holdWork(nil, p, workKindStage) // no cluster: held only
	if !workInUse(p) {
		t.Fatal("not held")
	}
	if err := st.AddWorkDir(store.WorkDir{Path: p, Node: 3, Kind: workKindStage, Owner: workOwner, Created: 1}); err != nil {
		t.Fatal(err)
	}
	s.releaseWork(p, false)
	if workInUse(p) {
		t.Fatal("still held")
	}
	if l, _ := st.WorkDirs(3); len(l) != 1 {
		t.Fatalf("a directory that was not removed keeps its record: %v", l)
	}
	s.holdWork(nil, p, "")
	s.releaseWork(p, true)
	if l, _ := st.WorkDirs(3); len(l) != 0 {
		t.Fatalf("a removed directory loses its record: %v", l)
	}
}
