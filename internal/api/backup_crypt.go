package api

// Encryption of backups with the installation's one backup key (internal/backupkey): the switch on the Backup
// encryption page, the recovery key file, and unlocking the key with its passphrase for one restore or export.
//
// The controller keeps the public recipient (in the web settings) and the recovery key file (backup-key.age in the
// data directory, so a controller backup holds both). While the switch is on, every new VM backup and every
// scheduled controller backup is encrypted to the recipient. Turning it off keeps the key: older backups need it.
// The passphrase is never stored; it comes with each request that decrypts, and the identity it opens lives in
// memory for that one operation.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"filippo.io/age"

	"virticle/internal/backupkey"
	"virticle/internal/cluster"
	"virticle/internal/ctlbackup"
)

const bkEncSettingKey = "backup-encryption"

// bkEncSettings is what the controller keeps of the backup key besides the recovery key file.
type bkEncSettings struct {
	On        bool   `json:"on"`
	Recipient string `json:"recipient,omitempty"`
	Since     int64  `json:"since,omitempty"` // when the key was made
}

// bkEncMu orders changes of the key (turning on twice at once must not make two keys).
var bkEncMu sync.Mutex

func (s *Server) bkEnc() bkEncSettings {
	var c bkEncSettings
	if s.Store == nil {
		return c
	}
	if v, ok, err := s.Store.WebSetting(bkEncSettingKey); err == nil && ok {
		json.Unmarshal([]byte(v), &c)
	}
	if !backupkey.ValidRecipient(c.Recipient) {
		c = bkEncSettings{}
	}
	return c
}

func (s *Server) setBkEnc(c bkEncSettings) error {
	b, _ := json.Marshal(c)
	if err := s.Store.SetWebSetting(bkEncSettingKey, string(b)); err != nil {
		return err
	}
	if s.Nodes != nil {
		s.Nodes.Publish(cluster.Event{Type: "backupkey"})
	}
	return nil
}

// backupRecipient is the key new backups are encrypted to, "" while encryption is off.
func (s *Server) backupRecipient() string {
	if c := s.bkEnc(); c.On {
		return c.Recipient
	}
	return ""
}

// errWrongBackupPass is what a restore, an export and the key pages say for a passphrase that does not fit.
var errWrongBackupPass = badRequest("wrong passphrase for the backup key")

// unlockBackupKey opens the recovery key file with the passphrase and returns the identity as text
// ("AGE-SECRET-KEY-1..."). Failures count against the user like wrong passwords. The caller clears the bytes when
// it is done with them.
func (s *Server) unlockBackupKey(r *http.Request, pass string) ([]byte, error) {
	key := "bkpass:" + userOf(r)
	if wait := s.limiter.locked(key); wait > 0 {
		return nil, badRequest("too many wrong passphrases; try again in " + strconv.Itoa(int(wait.Seconds())+1) + " seconds")
	}
	if pass == "" {
		return nil, badRequest("this backup is encrypted: the passphrase of the backup key is needed")
	}
	if s.DataDir == "" {
		return nil, badRequest("the controller has no data directory configured")
	}
	blob, err := backupkey.Read(s.DataDir)
	if errors.Is(err, backupkey.ErrNoKey) {
		return nil, badRequest("this controller has no backup key. Restore the controller backup that holds it, or decrypt the files by hand with the recovery key file (see the reference)")
	}
	if err != nil {
		return nil, err
	}
	id, err := backupkey.Unwrap(blob, pass)
	if errors.Is(err, backupkey.ErrWrongPassphrase) {
		s.limiter.fail(key, limPair)
		return nil, errWrongBackupPass
	}
	if err != nil {
		return nil, err
	}
	s.limiter.reset(key)
	return []byte(id.String()), nil
}

// checkRecipients refuses a backup whose files are encrypted to another key than this controller's.
func (s *Server) checkRecipients(links []chainLink) error {
	mine := s.bkEnc().Recipient
	for _, l := range links {
		if l.Man.encrypted() && l.Man.Recipient != mine {
			if mine == "" {
				return badRequest("this backup is encrypted, and this controller has no backup key. Restore the controller backup that holds the key, or decrypt the files by hand with the recovery key file")
			}
			return badRequest("the backup " + l.Dir + " is encrypted to another backup key (" + l.Man.Recipient + "); this controller's key is " + mine)
		}
	}
	return nil
}

// chainNeedsKey says whether reading the chain needs the backup key: one of its backups has an encrypted file.
func chainNeedsKey(chain []chainLink) bool {
	for _, l := range chain {
		if l.Man.encrypted() {
			return true
		}
	}
	return false
}

func wipe(b []byte) { clear(b) }

// --- the page ---

type bkEncStatus struct {
	On        bool   `json:"on"`
	HasKey    bool   `json:"hasKey"`
	Recipient string `json:"recipient,omitempty"`
	Since     int64  `json:"since,omitempty"`
	// Encrypted counts the backups that need the key (only asked for with ?count=1): VM backups with an
	// encrypted file, and scheduled controller backups encrypted to the key.
	VMBackups  *int `json:"vmBackups,omitempty"`
	CtlBackups *int `json:"controllerBackups,omitempty"`
}

func (s *Server) bkEncStatus(count bool) bkEncStatus {
	c := s.bkEnc()
	st := bkEncStatus{On: c.On, Recipient: c.Recipient, Since: c.Since}
	if s.DataDir != "" {
		if _, err := backupkey.Read(s.DataDir); err == nil {
			st.HasKey = true
		}
	}
	if !st.HasKey {
		st.On, st.Recipient, st.Since = false, "", 0
	}
	if count && st.HasKey {
		vm, ctl := 0, 0
		for _, e := range s.listBackups("") {
			if e.Manifest != nil && e.Manifest.encrypted() && e.Manifest.Recipient == c.Recipient {
				vm++
			}
		}
		dir := filepath.Join(s.DataDir, "backups")
		files, _ := ctlbackup.Files(dir)
		for _, f := range files {
			if ctlbackup.KeyEncrypted(filepath.Join(dir, f.Name)) {
				ctl++
			}
		}
		st.VMBackups, st.CtlBackups = &vm, &ctl
	}
	return st
}

// GET /api/backup-encryption
func (s *Server) getBackupEncryption(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.bkEncStatus(r.URL.Query().Get("count") == "1"))
}

// POST /api/backup-encryption/on {passphrase, again}: makes the key the first time (the answer then carries the
// recovery key file); later it needs the passphrase of the existing key, so that nobody turns on encryption with
// a key whose passphrase is forgotten.
func (s *Server) backupEncryptionOn(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Passphrase string `json:"passphrase"`
		Again      string `json:"again"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	if s.DataDir == "" {
		fail(w, badRequest("the controller has no data directory configured"))
		return
	}
	bkEncMu.Lock()
	defer bkEncMu.Unlock()
	c := s.bkEnc()
	var keyFile []byte
	if _, err := backupkey.Read(s.DataDir); err == nil {
		// The key exists (made earlier, or restored with the controller): never replace it, older backups need it.
		id, err := s.unlockBackupKey(r, req.Passphrase)
		if err != nil {
			fail(w, err)
			return
		}
		x, err := age.ParseX25519Identity(string(id))
		wipe(id)
		if err != nil {
			fail(w, err)
			return
		}
		if rec := x.Recipient().String(); rec != c.Recipient {
			c = bkEncSettings{Recipient: rec, Since: c.Since}
		}
	} else {
		if req.Passphrase != req.Again {
			fail(w, badRequest("the two passphrases differ"))
			return
		}
		if err := backupkey.CheckPassphrase(req.Passphrase); err != nil {
			fail(w, badRequest(err.Error()))
			return
		}
		rec, blob, err := backupkey.New(req.Passphrase, time.Now())
		if err != nil {
			fail(w, err)
			return
		}
		if err := backupkey.Write(s.DataDir, blob); err != nil {
			fail(w, err)
			return
		}
		c, keyFile = bkEncSettings{Recipient: rec, Since: time.Now().Unix()}, blob
		infoOf(r).detail = "backup key created"
	}
	c.On = true
	if err := s.setBkEnc(c); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"status": s.bkEncStatus(false), "keyFile": string(keyFile), "keyFileName": backupkey.DownloadName})
}

// POST /api/backup-encryption/off: new backups are plain again; the key stays for the encrypted ones.
func (s *Server) backupEncryptionOff(w http.ResponseWriter, r *http.Request) {
	bkEncMu.Lock()
	defer bkEncMu.Unlock()
	c := s.bkEnc()
	c.On = false
	if c.Recipient != "" {
		if err := s.setBkEnc(c); err != nil {
			fail(w, err)
			return
		}
	}
	writeJSON(w, s.bkEncStatus(true))
}

// POST /api/backup-encryption/passphrase {current, new}: the same key under a new passphrase.
func (s *Server) backupEncryptionPassphrase(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
		Again   string `json:"again"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	if req.New != req.Again {
		fail(w, badRequest("the two new passphrases differ"))
		return
	}
	if err := backupkey.CheckPassphrase(req.New); err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	bkEncMu.Lock()
	defer bkEncMu.Unlock()
	id, err := s.unlockBackupKey(r, req.Current)
	if err != nil {
		fail(w, err)
		return
	}
	wipe(id)
	blob, _ := backupkey.Read(s.DataDir)
	nb, err := backupkey.Rewrap(blob, req.Current, req.New)
	if err != nil {
		fail(w, err)
		return
	}
	if err := backupkey.Write(s.DataDir, nb); err != nil {
		fail(w, err)
		return
	}
	infoOf(r).detail = "backup key passphrase changed"
	writeJSON(w, map[string]any{"status": s.bkEncStatus(false), "keyFile": string(nb), "keyFileName": backupkey.DownloadName})
}

// POST /api/backup-encryption/key {password, code}: the recovery key file, with the administrator's password again
// (and a two-step code), as for the controller backup download.
func (s *Server) backupEncryptionKey(w http.ResponseWriter, r *http.Request) {
	var req cbDownloadReq
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	if s.DataDir == "" {
		fail(w, badRequest("the controller has no data directory configured"))
		return
	}
	blob, err := backupkey.Read(s.DataDir)
	if err != nil {
		fail(w, badRequest(err.Error()))
		return
	}
	if !s.recheckPassword(w, r, req.Password, req.Code) {
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", `attachment; filename="`+backupkey.DownloadName+`"`)
	h.Set("Cache-Control", "no-store")
	w.Write(blob)
}

// --- export tickets ---

// An export of an encrypted incremental backup is put together on the node, which needs the identity. The
// browser downloads with a plain link, so the passphrase is checked first (POST .../export-key) and the identity
// waits here, in memory, for the one download that follows: at most two minutes, used once, for the same user
// and backup.
type exportTicket struct {
	user, dir string
	id        []byte
	until     time.Time
}

var exportTickets = struct {
	sync.Mutex
	m map[string]*exportTicket
}{m: map[string]*exportTicket{}}

const exportTicketTTL = 2 * time.Minute

func newExportTicket(user, dir string, id []byte) string {
	b := make([]byte, 16)
	rand.Read(b)
	t := hex.EncodeToString(b)
	exportTickets.Lock()
	defer exportTickets.Unlock()
	now := time.Now()
	for k, v := range exportTickets.m {
		if now.After(v.until) {
			wipe(v.id)
			delete(exportTickets.m, k)
		}
	}
	exportTickets.m[t] = &exportTicket{user: user, dir: dir, id: id, until: now.Add(exportTicketTTL)}
	return t
}

// takeExportTicket returns the identity of a ticket once; the caller wipes it.
func takeExportTicket(ticket, user, dir string) []byte {
	exportTickets.Lock()
	defer exportTickets.Unlock()
	t := exportTickets.m[ticket]
	if t == nil {
		return nil
	}
	delete(exportTickets.m, ticket)
	if t.user != user || t.dir != dir || time.Now().After(t.until) {
		wipe(t.id)
		return nil
	}
	return t.id
}

// POST /api/nodes/{node}/backups/{pool}/{name}/export-key {passphrase}
func (s *Server) exportBackupKey(w http.ResponseWriter, r *http.Request) {
	b, err := s.backupFromURL(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		Passphrase string `json:"passphrase"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	m, err := b.load()
	if err != nil {
		fail(w, err)
		return
	}
	chain, err := b.loadChain(m)
	if err != nil {
		fail(w, err)
		return
	}
	if err := s.checkRecipients(chain); err != nil {
		fail(w, err)
		return
	}
	id, err := s.unlockBackupKey(r, req.Passphrase)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]string{"ticket": newExportTicket(userOf(r), b.dir, id)})
}
