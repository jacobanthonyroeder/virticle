package api

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"virticle/internal/ctlbackup"
	"virticle/internal/retention"
)

// oldPruneChains is pruneChains as it was before the retention tiers, kept to check that keep-last alone still
// deletes exactly the same backups.
func oldPruneChains(all []retained, uuid string, keep int, just string) []string {
	if keep < 1 {
		return nil
	}
	var mine []retained
	by := map[string]retained{}
	for _, b := range all {
		if b.UUID == uuid {
			mine = append(mine, b)
			by[b.Key] = b
		}
	}
	sort.SliceStable(mine, func(i, j int) bool { return mine[i].Created.After(mine[j].Created) })
	if len(mine) <= keep {
		return nil
	}
	need := map[string]bool{}
	mark := func(k string) {
		for n := 0; k != "" && n < maxChainWalk; n++ {
			if need[k] {
				return
			}
			b, ok := by[k]
			if !ok {
				return
			}
			need[k] = true
			k = b.Parent
		}
	}
	for _, b := range mine[:keep] {
		mark(b.Key)
	}
	mark(just)
	var out []string
	for _, b := range mine[keep:] {
		if !need[b.Key] {
			out = append(out, b.Key)
		}
	}
	return out
}

// randomBackups is a list of backups of two VMs in one pool: chains of a full backup and up to six incremental
// ones, some made by hand in between, some parents deleted by hand, distinct times.
func randomBackups(r *rand.Rand) []retained {
	var all []retained
	t := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	n := 1 + r.IntN(60)
	prev := map[string]string{}
	for i := 0; i < n; i++ {
		uuid := "u1"
		if r.IntN(5) == 0 {
			uuid = "u2"
		}
		t = t.Add(time.Duration(1+r.IntN(600)) * time.Minute)
		key := fmt.Sprintf("b%03d", i)
		b := retained{Key: key, UUID: uuid, Created: t}
		if p := prev[uuid]; p != "" && r.IntN(7) != 0 {
			b.Parent = p
		}
		if r.IntN(30) == 0 {
			b.Parent = "gone"
		}
		all = append(all, b)
		prev[uuid] = key
	}
	r.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	return all
}

func TestPrunePolicyKeepLastIsTheOldRule(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 8))
	for n := 0; n < 3000; n++ {
		all := randomBackups(r)
		keep := r.IntN(12)
		just := ""
		if r.IntN(3) == 0 {
			just = all[r.IntN(len(all))].Key
		}
		old := oldPruneChains(all, "u1", keep, just)
		got := pruneChains(all, "u1", keep, just)
		if len(old) == 0 && len(got) == 0 {
			continue
		}
		if !reflect.DeepEqual(old, got) {
			t.Fatalf("keep %d just %q:\nold %v\nnew %v", keep, just, old, got)
		}
	}
}

func TestPrunePolicyTiersAndChains(t *testing.T) {
	r := rand.New(rand.NewPCG(9, 10))
	for n := 0; n < 3000; n++ {
		all := randomBackups(r)
		pol := retention.Policy{KeepLast: r.IntN(3), Hourly: r.IntN(6), Daily: r.IntN(4), Weekly: r.IntN(3), Monthly: r.IntN(2)}
		held := map[string]bool{}
		if r.IntN(4) == 0 {
			held[all[r.IntN(len(all))].Key] = true
		}
		del := map[string]bool{}
		got := prunePolicy(all, "u1", pol, "", held, time.UTC)
		for _, k := range got {
			del[k] = true
		}
		var newest retained
		for _, b := range all {
			if b.UUID == "u1" && b.Created.After(newest.Created) {
				newest = b
			}
			if del[b.Key] && b.UUID != "u1" {
				t.Fatal("another VM's backup deleted")
			}
			// Closed under "needed by": nothing that stays needs a deleted one.
			if b.UUID == "u1" && !del[b.Key] && b.Parent != "" && del[b.Parent] {
				t.Fatalf("policy %+v: %s stays without its parent %s", pol, b.Key, b.Parent)
			}
			if held[b.Key] && del[b.Key] {
				t.Fatal("a backup that is being read was deleted")
			}
		}
		if del[newest.Key] {
			t.Fatalf("policy %+v deleted the newest backup", pol)
		}
		for i := 1; i < len(got); i++ {
			if got[i-1] < got[i] { // keys grow with time
				t.Fatalf("not newest first: %v", got)
			}
		}
	}
	if got := prunePolicy(randomBackups(r), "u1", retention.Policy{}, "", nil, time.UTC); got != nil {
		t.Fatalf("an empty policy deletes nothing: %v", got)
	}
}

// A schedule of before the tiers reads as it was and is written back the same way; a tiered one is refused by
// the parser of before (which needs keep-last 1 or more) instead of being pruned down to keep-last alone.
func TestBackupScheduleMetadataOldAndNew(t *testing.T) {
	old := `<virticle:meta xmlns:virticle="x"><virticle:backup-schedule enabled="yes" mode="daily" at="01:00" target-node="0" pool="p" keep-last="7"/></virticle:meta>`
	b, err := parseBackupSchedule(old)
	if err != nil || b == nil || b.KeepLast != 7 || b.policy() != (retention.Policy{KeepLast: 7}) {
		t.Fatalf("old element: %+v %v", b, err)
	}
	if el := b.element(); el != `<backup-schedule enabled="yes" mode="daily" at="01:00" target-node="0" pool="p" keep-last="7"/>` {
		t.Fatalf("an untiered schedule is written as before: %s", el)
	}
	tiered := backupSchedule{Enabled: true, Mode: "interval", EveryMinutes: 60, Pool: "p", KeepLast: 2, KeepHourly: 24, KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6}
	meta, err := metaSetChild(`<virticle:meta xmlns:virticle="x"/>`, backupSchedChild, tiered.element())
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseBackupSchedule(meta)
	if err != nil || got == nil || *got != tiered {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	// The parser of before: encoding/xml ignores the new attributes, and its validation needs keep-last >= 1.
	var root struct {
		B *struct {
			Keep int `xml:"keep-last,attr"`
		} `xml:"backup-schedule"`
	}
	if err := xml.Unmarshal([]byte(meta), &root); err != nil || root.B == nil {
		t.Fatalf("the old parser cannot read it: %v", err)
	}
	if root.B.Keep >= 1 {
		t.Fatalf("an old controller would prune a tiered schedule to keep-last %d", root.B.Keep)
	}
	// Tiers without keep-last.
	only := tiered
	only.KeepLast = 0
	meta, _ = metaSetChild(`<virticle:meta xmlns:virticle="x"/>`, backupSchedChild, only.element())
	if got, err := parseBackupSchedule(meta); err != nil || *got != only {
		t.Fatalf("tiers only: %+v %v", got, err)
	}
}

func TestBackupScheduleNormalizeAndValidate(t *testing.T) {
	b := backupSchedule{Enabled: true, Mode: "daily", At: "02:00", EveryMinutes: 30, Pool: "p", KeepHourly: 24, KeepDaily: 7}
	b.normalize()
	if b.KeepHourly != 0 || b.KeepDaily != 7 || b.EveryMinutes != 0 {
		t.Fatalf("daily schedule: %+v", b)
	}
	if err := b.validate(); err != nil {
		t.Fatal(err)
	}
	b = backupSchedule{Enabled: true, Mode: "daily", At: "02:00", Pool: "p", KeepHourly: 24}
	b.normalize()
	if err := b.validate(); err == nil {
		t.Fatal("a daily schedule with hourly retention alone keeps nothing and must be refused")
	}
	b = backupSchedule{Enabled: true, Mode: "interval", EveryMinutes: 10, Pool: "p", KeepHourly: 1001}
	if err := b.validate(); err == nil {
		t.Fatal("1001 accepted")
	}
}

func TestHoldRead(t *testing.T) {
	r1 := holdRead("/pool", "a")
	r2 := holdRead("/pool", "a")
	if !readingIn("n", "/pool")["a"] {
		t.Fatal("not held")
	}
	r1()
	r1() // twice is once
	if !readingIn("n", "/pool")["a"] {
		t.Fatal("released by the first holder twice")
	}
	r2()
	if readingIn("n", "/pool")["a"] || len(bkReading.m) != 0 {
		t.Fatal("still held")
	}
}

func TestProjectionEstimate(t *testing.T) {
	p := projection{Points: []projPoint{{Full: true}, {}, {}, {Full: true}}}
	p.estimate(100, 10, 3, "backups")
	if p.Size.Fulls != 2 || p.Size.Incrs != 2 || p.Size.Low != 176 || p.Size.High != 264 {
		t.Fatalf("%+v", p.Size)
	}
	p.estimate(1000, 0, 0, "disks")
	if p.Size.Low != 2000+40 || p.Size.High != 2000+400 {
		t.Fatalf("unknown incremental size: %+v", p.Size)
	}
	if got := nextRuns(time.Unix(1000, 0), time.Hour, false, 3); !reflect.DeepEqual(got, []int64{1000, 4600, 8200}) {
		t.Fatalf("next runs %v", got)
	}
}

// --- controller ---

func TestControllerBackupSettingsDefaults(t *testing.T) {
	e := backupEnv(t)
	// As stored before the schedule could be set.
	if err := e.srv.Store.SetWebSetting(cbSettingKey, `{"daily":true}`); err != nil {
		t.Fatal(err)
	}
	c := e.srv.cbSettings()
	if !c.Daily || c.Mode != "daily" || c.At != "03:30" || c.Policy != (retention.Policy{Daily: 7}) {
		t.Fatalf("defaults: %+v", c)
	}
	// {"daily": false} alone switches off and keeps the rest.
	put := func(body map[string]any) int {
		return e.do("PUT", "/api/controller-backup", body, cookieOf(e, "ann")).Code
	}
	if code := put(map[string]any{"mode": "interval", "everyHours": 6, "keepHourly": 4, "keepDaily": 7, "keepWeekly": 4, "keepMonthly": 0}); code != 200 {
		t.Fatalf("put: %d", code)
	}
	c = e.srv.cbSettings()
	if c.Mode != "interval" || c.EveryHours != 6 || c.At != "" || c.Policy != (retention.Policy{Hourly: 4, Daily: 7, Weekly: 4}) || !c.Daily {
		t.Fatalf("saved: %+v", c)
	}
	if code := put(map[string]any{"mode": "daily", "at": "25:00"}); code != 400 {
		t.Fatalf("bad time: %d", code)
	}
	if code := put(map[string]any{"mode": "daily", "at": "04:00", "keepHourly": 24, "keepDaily": 0, "keepWeekly": 0}); code != 400 {
		t.Fatalf("hourly alone with a daily copy keeps nothing: %d", code)
	}
	if code := put(map[string]any{"mode": "interval", "everyHours": 0}); code != 400 {
		t.Fatalf("every 0 hours: %d", code)
	}
	var st cbStatus
	decode(t, e.do("GET", "/api/controller-backup", nil, cookieOf(e, "ann")), &st)
	if st.Mode != "interval" || st.EveryHours != 6 || st.Policy.Daily != 7 || st.Zone == "" {
		t.Fatalf("status: %+v", st)
	}
	if code := e.do("POST", "/api/controller-backup/preview", map[string]any{}, cookieOf(e, "bob")).Code; code != 403 {
		t.Fatalf("operator preview: %d", code)
	}
	w := e.do("POST", "/api/controller-backup/preview", map[string]any{"mode": "daily", "at": "03:30", "keepDaily": 7, "keepWeekly": 4, "keepMonthly": 6, "keepHourly": 24}, cookieOf(e, "ann"))
	var pr projection
	decode(t, w, &pr)
	if w.Code != 200 || pr.Policy.Hourly != 0 || pr.Ineffective["hourly"] == "" || len(pr.NextRuns) != 3 || pr.Kept < 13 || pr.Kept > 17 || !pr.Complete {
		t.Fatalf("preview %d: %+v", w.Code, pr)
	}
}

func TestControllerBackupPrunesWithThePolicy(t *testing.T) {
	e := backupEnv(t)
	dir := filepath.Join(e.srv.DataDir, "backups")
	os.MkdirAll(dir, 0o700)
	// Back-dated copies every 6 hours for 30 days.
	start := time.Now().Add(-30 * 24 * time.Hour)
	for i := 0; i < 30*4; i++ {
		name := ctlbackup.FileName(thisHost(), start.Add(time.Duration(i)*6*time.Hour), false)
		os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600)
	}
	b, _ := json.Marshal(cbSettings{Daily: true, Mode: "interval", EveryHours: 6, Policy: retention.Policy{Hourly: 4, Daily: 3}})
	e.srv.Store.SetWebSetting(cbSettingKey, string(b))
	if err := e.srv.dailyBackupRun(); err != nil {
		t.Fatal(err)
	}
	files, _ := ctlbackup.Files(dir)
	// 4 hourly (the new one and three back-dated, 6 hours apart) plus dailies of the days they do not cover.
	if len(files) < 4 || len(files) > 7 {
		t.Fatalf("kept %d", len(files))
	}
	tasks, _ := e.srv.Store.TasksBy("scheduler", 0)
	if len(tasks) != 1 || !strings.Contains(tasks[0].Note, "older backups deleted (keep 4 hourly, 3 daily)") {
		t.Fatalf("task note: %+v", tasks)
	}
}

func TestControllerBackupIntervalPlan(t *testing.T) {
	c := cbSettings{Mode: "interval", EveryHours: 6}
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	if got := c.next(now, now.Add(-2*time.Hour), time.Minute); !got.Equal(now.Add(4 * time.Hour)) {
		t.Fatalf("next %v", got)
	}
	if got := c.next(now, time.Time{}, time.Minute); !got.Equal(now.Add(time.Minute)) {
		t.Fatalf("first %v", got)
	}
	if got := c.next(now, now.Add(-7*time.Hour), time.Minute); !got.Equal(now.Add(time.Minute)) {
		t.Fatalf("overdue %v", got)
	}
	if got := c.next(now, now.Add(time.Hour), 0); !got.Equal(now) {
		t.Fatalf("last in the future (clock set back): %v", got)
	}
	d := cbSettings{Mode: "daily", At: "22:15"}
	if got := d.next(now, time.Time{}, 0); !got.Equal(time.Date(2026, 6, 10, 22, 15, 0, 0, time.UTC)) {
		t.Fatalf("daily %v", got)
	}
}
