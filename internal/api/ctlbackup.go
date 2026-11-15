package api

// Backup of the controller itself (see internal/ctlbackup): the Controller backup page's
// status and the download, and the optional scheduled copy on this server (daily at a time or every N hours,
// pruned with internal/retention).
//
// The download hands out the controller's private keys, so it needs a web
// session (not an API token) and the administrator's password again; a user
// with two-step sign-in on also gives a current code (or a recovery code).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"virticle/internal/buildinfo"
	"virticle/internal/ctlbackup"
	"virticle/internal/retention"
)

const (
	cbSettingKey = "controller-backup"
	// ctlBackupKeep is how many daily copies stay on the server when nothing else is set (the retention of
	// before the schedule could be set: the newest 7, which is 7 dailies for a daily copy).
	ctlBackupKeep = 7
	cbTaskKind    = "controller backup"
	cbMaxHours    = 168
)

// cbSettings is the scheduled copy on this server. Settings stored before the schedule could be set hold only
// Daily; the rest then has its defaults (withDefaults).
type cbSettings struct {
	Daily bool `json:"daily"` // on; the name is from when it could only run daily
	// Mode is "daily" (at At, the controller's local time) or "interval" (every EveryHours).
	Mode       string `json:"mode"`
	At         string `json:"at,omitempty"`
	EveryHours int    `json:"everyHours,omitempty"`
	// Dir is the folder the scheduled copies are written to; empty is "backups" in the data directory.
	Dir string `json:"dir,omitempty"`
	retention.Policy
}

// cbDir is the folder of the scheduled copies.
func (s *Server) cbDir() string {
	if d := s.cbSettings().Dir; d != "" {
		return d
	}
	return filepath.Join(s.DataDir, "backups")
}

// cbCheckDir makes sure the controller can write scheduled copies to dir: it creates the folder and writes and
// removes a file in it. A folder inside the data directory other than the default is refused, since a backup holds
// the data directory.
func (s *Server) cbCheckDir(dir string) error {
	def := filepath.Join(s.DataDir, "backups")
	if dir == def {
		return nil
	}
	if !filepath.IsAbs(dir) || dir == "/" {
		return errors.New("the folder must be a full path, for example /mnt/backup/virticle")
	}
	if data, err := filepath.Abs(s.DataDir); err == nil && (dir == data || strings.HasPrefix(dir, data+string(filepath.Separator))) {
		return fmt.Errorf("choose a folder outside the data directory %s (or leave the default %s)", data, def)
	}
	why := func(err error) error {
		msg := err.Error()
		if errors.Is(err, unix.EROFS) || errors.Is(err, os.ErrPermission) {
			msg += ". The folder must be writable by the account the controller runs as; an installed controller may write below /var/lib/virticle, /mnt, /media and /srv"
		}
		return errors.New(msg)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return why(err)
	}
	f, err := os.CreateTemp(dir, ".virticle-write-test-")
	if err != nil {
		return why(err)
	}
	f.Close()
	return os.Remove(f.Name())
}

var cbAtRE = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

func (c cbSettings) withDefaults() cbSettings {
	if c.Mode == "" {
		c.Mode = "daily"
	}
	if c.Mode == "daily" && c.At == "" {
		c.At = fmt.Sprintf("%02d:%02d", ctlbackup.DailyHour, ctlbackup.DailyMinute)
	}
	if c.Policy.KeepsAll() {
		c.Policy = retention.Policy{Daily: ctlBackupKeep}
	}
	return c
}

func (c cbSettings) step() time.Duration {
	if c.Mode == "interval" {
		return time.Duration(c.EveryHours) * time.Hour
	}
	return 24 * time.Hour
}

// normalize drops what the mode does not use and the rules that cannot have an effect at its frequency.
func (c *cbSettings) normalize() {
	if c.Mode == "interval" {
		c.At = ""
	} else {
		c.EveryHours = 0
	}
	c.Policy = c.Policy.Normalize(c.step())
}

func (c cbSettings) validate() error {
	switch {
	case c.Mode != "daily" && c.Mode != "interval":
		return errors.New(`mode must be "daily" or "interval"`)
	case c.Mode == "daily" && !cbAtRE.MatchString(c.At):
		return errors.New("the time must look like 03:30")
	case c.Mode == "interval" && (c.EveryHours < 1 || c.EveryHours > cbMaxHours):
		return fmt.Errorf("every must be between 1 and %d hours", cbMaxHours)
	}
	if err := c.Policy.Validate(); err != nil {
		return err
	}
	if c.Policy.KeepsAll() {
		return errors.New("set at least one keep value (backups without retention would fill the disk)")
	}
	return nil
}

func (c cbSettings) hourMinute() (int, int) {
	h, _ := strconv.Atoi(c.At[:2])
	m, _ := strconv.Atoi(c.At[3:])
	return h, m
}

// next is when the copy runs after now, given the time of the last scheduled copy (zero if none).
func (c cbSettings) next(now, last time.Time, jitter time.Duration) time.Time {
	if c.Mode == "interval" {
		if last.IsZero() || !last.Before(now) {
			return now.Add(jitter)
		}
		t := last.Add(c.step())
		if t.Before(now) {
			return now.Add(jitter)
		}
		return t
	}
	h, m := c.hourMinute()
	return ctlbackup.NextAt(now, h, m, jitter)
}

func (s *Server) cbSettings() cbSettings {
	var c cbSettings
	if v, ok, err := s.Store.WebSetting(cbSettingKey); err == nil && ok {
		json.Unmarshal([]byte(v), &c)
	}
	return c.withDefaults()
}

func (s *Server) cbSource() (ctlbackup.Source, error) {
	if s.DataDir == "" {
		return ctlbackup.Source{}, errors.New("the controller has no data directory configured")
	}
	host, _ := os.Hostname()
	return ctlbackup.Source{DataDir: s.DataDir, Snapshot: s.Store.SnapshotTo, App: buildinfo.Version, Host: host}, nil
}

type cbRecordView struct {
	Time      int64  `json:"time"`
	Size      int64  `json:"size,omitempty"`
	Encrypted bool   `json:"encrypted,omitempty"`
	Via       string `json:"via"` // cli | download | daily
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
}

type cbStatus struct {
	Daily      bool   `json:"daily"`
	Mode       string `json:"mode"`
	At         string `json:"at,omitempty"`
	EveryHours int    `json:"everyHours,omitempty"`
	retention.Policy
	Zone      string        `json:"zone"` // the controller's time zone: At, and the periods of retention
	NextRun   int64         `json:"nextRun,omitempty"`
	Dir       string        `json:"dir"`
	Last      *cbRecordView `json:"last,omitempty"`      // newest successful backup of any kind
	LastDaily *cbRecordView `json:"lastDaily,omitempty"` // newest daily attempt, also when it failed
	Command   string        `json:"restoreCommand"`
}

func recView(kind string, r ctlbackup.Record) *cbRecordView {
	return &cbRecordView{Time: r.Time.Unix(), Size: r.Size, Encrypted: r.Encrypted, Via: kind, OK: r.OK, Error: r.Error}
}

func (s *Server) ctlBackupStatus(w http.ResponseWriter, r *http.Request) {
	c := s.cbSettings()
	st := cbStatus{Daily: c.Daily, Mode: c.Mode, At: c.At, EveryHours: c.EveryHours, Policy: c.Policy, Zone: zoneName(time.Now()),
		Dir: s.cbDir(), Command: "sudo systemctl stop virticle && sudo virticle restore FILE"}
	if c.Daily {
		cbSched.Lock()
		if !cbSched.next.IsZero() {
			st.NextRun = cbSched.next.Unix()
		}
		cbSched.Unlock()
	}
	recs := ctlbackup.ReadRecords(s.DataDir)
	if k, rec, ok := recs.Newest(); ok {
		st.Last = recView(k, rec)
	}
	if d, ok := recs["daily"]; ok {
		st.LastDaily = recView("daily", d)
	}
	writeJSON(w, st)
}

func (s *Server) putCtlBackup(w http.ResponseWriter, r *http.Request) {
	cur := s.cbSettings()
	req := cur // fields left out keep their values: {"daily": true} alone only switches it on
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	if s.DataDir == "" {
		fail(w, badRequest("the controller has no data directory configured"))
		return
	}
	req.normalize()
	if err := req.validate(); err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	if req.Dir = strings.TrimSpace(req.Dir); req.Dir != "" {
		req.Dir = filepath.Clean(req.Dir)
		if req.Dir == filepath.Join(s.DataDir, "backups") {
			req.Dir = "" // the default is stored as "not set", so it follows the data directory
		} else if err := s.cbCheckDir(req.Dir); err != nil {
			fail(w, badRequest(err.Error()))
			return
		}
	}
	was := cur.Daily
	b, _ := json.Marshal(req)
	if err := s.Store.SetWebSetting(cbSettingKey, string(b)); err != nil {
		fail(w, err)
		return
	}
	if req.Daily && !was {
		go s.dailyBackupRun() // the first copy at once, so that "on" can be seen to work
	}
	cbSched.Lock()
	cbSched.changed = true // the loop plans the next run again with the new settings
	cbSched.Unlock()
	s.ctlBackupStatus(w, r)
}

type cbDownloadReq struct {
	Password   string `json:"password"`
	Code       string `json:"code"` // current two-step code or recovery code, when the user has two-step sign-in on
	Passphrase string `json:"passphrase"`
}

// recheckPassword asks for the signed-in user's password again (and a two-step code when the user has two-step
// sign-in on), for what hands out keys. It needs a web session, not an API token. On false the answer is written.
func (s *Server) recheckPassword(w http.ResponseWriter, r *http.Request, password, code string) bool {
	p, _, ok := accountSession(w, r)
	if !ok {
		return false
	}
	key := "pw:" + p.User.Username
	if wait := s.limiter.locked(key); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		deny(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return false
	}
	if !s.verifyPassword(p.User, password) {
		s.limiter.fail(key, limPair)
		deny(w, http.StatusForbidden, "the password is not right")
		return false
	}
	if p.User.TwoStep {
		good, _, err := s.checkSecondStep(p.User.ID, code)
		if err != nil {
			fail(w, err)
			return false
		}
		if !good {
			s.limiter.fail(key, limPair)
			deny(w, http.StatusForbidden, "the two-step code is not right")
			return false
		}
	}
	s.limiter.reset(key)
	return true
}

func (s *Server) ctlBackupDownload(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := accountSession(w, r); !ok {
		return
	}
	var req cbDownloadReq
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	src, err := s.cbSource()
	if err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	if req.Passphrase != "" {
		if err := ctlbackup.CheckPassphrase(req.Passphrase); err != nil {
			fail(w, badRequest(err.Error()))
			return
		}
	}
	if !s.recheckPassword(w, r, req.Password, req.Code) {
		return
	}

	src.Now = time.Now
	name := ctlbackup.FileName(src.Host, src.Now(), req.Passphrase != "")
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	h.Set("Cache-Control", "no-store")
	cw := &countWriter{w: w}
	var out io.Writer = cw
	var enc io.Closer
	if req.Passphrase != "" {
		e, err := ctlbackup.Encrypt(cw, req.Passphrase)
		if err != nil {
			fail(w, err)
			return
		}
		out, enc = e, e
	}
	_, err = ctlbackup.Write(out, src)
	if err == nil && enc != nil {
		err = enc.Close()
	}
	if err != nil {
		logf("controller backup download failed: %v", err)
		if cw.n == 0 {
			h.Del("Content-Disposition")
			fail(w, err)
			return
		}
		panic(http.ErrAbortHandler) // part of the file is out: cut the connection so that it cannot pass as complete
	}
	rec := ctlbackup.Record{Time: time.Now(), Size: cw.n, Encrypted: req.Passphrase != "", OK: true}
	if err := ctlbackup.Remember(s.DataDir, "download", rec, nil, nil); err != nil {
		logf("controller backup: recording the download: %v", err)
	}
}

type countWriter struct {
	w http.ResponseWriter
	n int64
}

func (c *countWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n += int64(n)
	return n, err
}

// --- the scheduled copy ---

var cbOnce sync.Once

// cbSched is what the status shows of the loop: the planned run, and whether the settings changed since.
var cbSched struct {
	sync.Mutex
	next    time.Time
	changed bool
}

// startControllerBackup runs the copy loop (once per process).
func (s *Server) startControllerBackup() {
	if s.DataDir == "" || s.Store == nil {
		return
	}
	cbOnce.Do(func() {
		d := &dailyBackup{now: time.Now, enabled: func() bool { return s.cbSettings().Daily }, run: s.dailyBackupRun,
			jitter: func() time.Duration { return randomJitter(20 * time.Minute) },
			plan: func(now time.Time, jitter time.Duration) time.Time {
				return s.cbSettings().next(now, ctlbackup.ReadRecords(s.DataDir)["daily"].Time, jitter)
			}}
		go func() {
			for range time.Tick(time.Minute) {
				cbSched.Lock()
				if cbSched.changed {
					d.next, cbSched.changed = time.Time{}, false
				}
				cbSched.Unlock()
				d.tick()
				if d.next.IsZero() {
					d.tick() // plan at once, so that the status can show the next run
				}
				cbSched.Lock()
				cbSched.next = d.next
				cbSched.Unlock()
			}
		}()
	})
}

// dailyBackup decides when the scheduled copy runs. tick is called every minute.
type dailyBackup struct {
	now     func() time.Time
	enabled func() bool
	run     func() error
	jitter  func() time.Duration
	// plan is the next run after now (nil: daily at 03:30).
	plan func(now time.Time, jitter time.Duration) time.Time
	next time.Time
}

func (d *dailyBackup) planNext(now time.Time) time.Time {
	if d.plan != nil {
		return d.plan(now, d.jitter())
	}
	return ctlbackup.NextDaily(now, d.jitter())
}

// retryAfter is the wait after a failed copy.
const cbRetry = 30 * time.Minute

// tick runs the copy when it is due and reports whether it did.
func (d *dailyBackup) tick() bool {
	now := d.now()
	if !d.enabled() {
		d.next = time.Time{}
		return false
	}
	if d.next.IsZero() {
		d.next = d.planNext(now)
		return false
	}
	if now.Before(d.next) {
		return false
	}
	if err := d.run(); err != nil {
		d.next = now.Add(cbRetry)
	} else {
		d.next = d.planNext(now)
	}
	return true
}

var cbRunning sync.Mutex

// dailyBackupRun writes one copy into <data>/backups, prunes the directory with
// the schedule's retention, records the result and leaves a task the alerts page
// turns into an alert when the copy failed.
func (s *Server) dailyBackupRun() error {
	cbRunning.Lock()
	defer cbRunning.Unlock()
	t, terr := s.Store.StartTask(cbTaskKind, "controller", "", "scheduler")
	size, gone, err := s.writeDailyBackup()
	if terr == nil {
		note := ""
		if gone > 0 {
			note = fmt.Sprintf("%d older %s deleted (keep %s)", gone, plural(gone, "backup", "backups"), s.cbSettings().Policy)
		}
		s.Store.FinishTaskNote(&t, err, note)
	}
	rec := ctlbackup.Record{Time: time.Now(), OK: err == nil, Size: size, Encrypted: err == nil && s.backupRecipient() != ""}
	if err != nil {
		rec.Error = err.Error()
		logf("daily controller backup failed: %v", err)
	}
	if e := ctlbackup.Remember(s.DataDir, "daily", rec, nil, nil); e != nil {
		logf("controller backup: recording the result: %v", e)
	}
	return err
}

// writeDailyBackup writes the copy and prunes; it returns the size of the copy and how many older ones went.
func (s *Server) writeDailyBackup() (int64, int, error) {
	src, err := s.cbSource()
	if err != nil {
		return 0, 0, err
	}
	src.Now = time.Now
	dir := s.cbDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, 0, err
	}
	// With backup encryption on, the copy is encrypted to the backup key: written with nobody present, opened
	// only with the recovery key file and its passphrase (virticle restore --key).
	rec := s.backupRecipient()
	name := ctlbackup.FileName(src.Host, src.Now(), rec != "")
	_, size, err := ctlbackup.WriteFileTo(filepath.Join(dir, name), src, rec)
	if err != nil {
		return 0, 0, err
	}
	// WriteFile has checked every file into the archive and only then given it its name: prune after that.
	gone, err := ctlbackup.PrunePolicy(dir, s.cbSettings().Policy, time.Local, name)
	return size, len(gone), err
}

// zoneName is the controller's zone as "CET (+01:00)".
func zoneName(t time.Time) string { return t.In(time.Local).Format("MST (-07:00)") }

// POST /api/controller-backup/preview: what the schedule in the body (not saved) would keep, and the space.
func (s *Server) previewCtlBackup(w http.ResponseWriter, r *http.Request) {
	c := s.cbSettings()
	if err := readJSON(r, &c); err != nil {
		fail(w, err)
		return
	}
	c.normalize()
	if err := c.validate(); err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	now := time.Now()
	last := ctlbackup.ReadRecords(s.DataDir)["daily"].Time
	var runs func(from, to time.Time, max int) []time.Time
	if c.Mode == "interval" {
		anchor := last
		if anchor.IsZero() {
			anchor = now
		}
		runs = retention.IntervalRuns(anchor, c.step())
	} else {
		h, m := c.hourMinute()
		runs = retention.DailyRuns(h, m, time.Local)
	}
	out := projectSchedule(runs, c.step(), 1, c.Policy, now)
	out.NextRuns = nextRuns(c.next(now, last, 0), c.step(), c.Mode == "daily", 3)
	dir := s.cbDir()
	if d := strings.TrimSpace(c.Dir); filepath.IsAbs(d) { // the folder in the form, saved or not
		dir = filepath.Clean(d)
	}
	files, _ := ctlbackup.Files(dir)
	var size uint64
	for _, f := range files {
		out.Used += uint64(f.Size)
	}
	if len(files) > 0 {
		size = uint64(files[0].Size)
	} else if k, rec, ok := ctlbackup.ReadRecords(s.DataDir).Newest(); ok && k != "" {
		size = uint64(rec.Size)
	}
	if size > 0 {
		out.Size = &projSize{Basis: "last", Full: size, Low: size * uint64(out.Kept), High: size * uint64(out.Kept), Fulls: out.Kept}
	}
	at := dir // a folder that does not exist yet is made on the file system of its nearest existing parent
	for at != "/" {
		if _, err := os.Stat(at); err == nil {
			break
		}
		at = filepath.Dir(at)
	}
	if free, err := freeBytesLocal(at); err == nil {
		out.Free = &free
	}
	writeJSON(w, out)
}

// randomJitter is a random wait between 0 and max.
func randomJitter(max time.Duration) time.Duration { return time.Duration(rand.Int64N(int64(max))) }

// freeBytesLocal is the space left for an unprivileged writer on the file system of dir.
func freeBytesLocal(dir string) (int64, error) {
	var sf unix.Statfs_t
	if err := unix.Statfs(dir, &sf); err != nil {
		return 0, err
	}
	return int64(sf.Bavail) * int64(sf.Bsize), nil
}
