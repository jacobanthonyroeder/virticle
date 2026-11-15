// Package backupkey is the one backup key of an installation: an age X25519 identity
// (https://age-encryption.org/v1) that VM backups and the controller's scheduled backups are encrypted to.
//
// The controller keeps the public recipient in clear, so that a scheduled backup can be encrypted with nobody
// present, and the private identity only inside a passphrase-encrypted age file (scrypt), the recovery key
// file. The passphrase is never stored: nothing on the controller can decrypt a backup without it.
//
// The recovery key file is a standard armored age file whose content is an ordinary age identity file, so the
// age tool opens a backup without Virticle:
//
//	age -d -i virticle-backup-key.age -o vda.qcow2 vda.qcow2.age
package backupkey

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"filippo.io/age"
	"filippo.io/age/armor"
)

// FileName is the recovery key file in the controller's data directory (a controller backup holds it).
const FileName = "backup-key.age"

// DownloadName is the name the recovery key file is downloaded under.
const DownloadName = "virticle-backup-key.age"

// MinPassphrase is the shortest passphrase accepted (the policy of the controller backup passphrase).
const MinPassphrase = 10

// workFactor is scrypt's log2(N) for the recovery key file: age's default, about a second on a server. Tests lower it.
var workFactor = 18

// ErrWrongPassphrase means the passphrase does not open the recovery key file.
var ErrWrongPassphrase = errors.New("wrong passphrase for the backup key")

// ErrNoKey means the data directory has no backup key.
var ErrNoKey = errors.New("this controller has no backup key")

// CheckPassphrase is the whole passphrase policy.
func CheckPassphrase(p string) error {
	if utf8.RuneCountInString(p) < MinPassphrase {
		return fmt.Errorf("the passphrase must be at least %d characters", MinPassphrase)
	}
	return nil
}

// New makes a new identity and returns its public recipient ("age1...") and the recovery key file that holds the
// identity, encrypted with pass.
func New(pass string, now time.Time) (string, []byte, error) {
	if err := CheckPassphrase(pass); err != nil {
		return "", nil, err
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return "", nil, err
	}
	rec := id.Recipient().String()
	text := fmt.Sprintf("# Virticle backup key, created %s\n# public key: %s\n%s\n", now.UTC().Format(time.RFC3339), rec, id)
	blob, err := wrap([]byte(text), pass)
	return rec, blob, err
}

func wrap(plain []byte, pass string) ([]byte, error) {
	r, err := age.NewScryptRecipient(pass)
	if err != nil {
		return nil, err
	}
	r.SetWorkFactor(workFactor)
	var b bytes.Buffer
	aw := armor.NewWriter(&b)
	w, err := age.Encrypt(aw, r)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(plain); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	if err := aw.Close(); err != nil {
		return nil, err
	}
	b.WriteString("\n")
	return b.Bytes(), nil
}

// unwrapText returns the identity file inside the recovery key file.
func unwrapText(blob []byte, pass string) ([]byte, error) {
	sid, err := age.NewScryptIdentity(pass)
	if err != nil {
		return nil, ErrWrongPassphrase
	}
	sid.SetMaxWorkFactor(22)
	var in io.Reader = bytes.NewReader(blob)
	if bytes.HasPrefix(bytes.TrimSpace(blob), []byte(armor.Header)) {
		in = armor.NewReader(bytes.NewReader(bytes.TrimSpace(blob)))
	}
	dr, err := age.Decrypt(in, sid)
	if err != nil {
		var nm *age.NoIdentityMatchError
		if errors.As(err, &nm) {
			return nil, ErrWrongPassphrase
		}
		return nil, fmt.Errorf("the recovery key file is not readable: %w", err)
	}
	text, err := io.ReadAll(io.LimitReader(dr, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("the recovery key file is damaged: %w", err)
	}
	return text, nil
}

// Unwrap opens the recovery key file with pass and returns the identity.
func Unwrap(blob []byte, pass string) (*age.X25519Identity, error) {
	text, err := unwrapText(blob, pass)
	if err != nil {
		return nil, err
	}
	ids, err := age.ParseIdentities(bytes.NewReader(text))
	if err != nil || len(ids) != 1 {
		return nil, errors.New("the recovery key file does not hold one age identity")
	}
	id, ok := ids[0].(*age.X25519Identity)
	if !ok {
		return nil, errors.New("the recovery key file does not hold an X25519 identity")
	}
	return id, nil
}

// Rewrap re-encrypts the recovery key file with a new passphrase; the identity stays the same, so every backup
// made with it stays readable.
func Rewrap(blob []byte, oldPass, newPass string) ([]byte, error) {
	if err := CheckPassphrase(newPass); err != nil {
		return nil, err
	}
	text, err := unwrapText(blob, oldPass)
	if err != nil {
		return nil, err
	}
	return wrap(text, newPass)
}

// ValidRecipient reports whether s is an age X25519 recipient ("age1...").
func ValidRecipient(s string) bool {
	_, err := age.ParseX25519Recipient(s)
	return err == nil
}

// Read returns the recovery key file of the data directory dir (ErrNoKey when there is none).
func Read(dir string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(dir, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoKey
	}
	if err != nil {
		return nil, err
	}
	if len(b) > 64<<10 {
		return nil, errors.New("the recovery key file is unexpectedly large")
	}
	return b, nil
}

// Write stores the recovery key file in dir (mode 0600, replaced by renaming).
func Write(dir string, blob []byte) error {
	tmp, err := os.CreateTemp(dir, ".backup-key-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(blob); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, FileName))
}

// Stanza tells how an age file is encrypted, from its first bytes: "X25519" (to a key such as the backup key),
// "scrypt" (a passphrase), "" when it is not an age file. Other recipient types are returned as named.
func Stanza(head []byte) string {
	if !bytes.HasPrefix(head, []byte("age-encryption.org/v1\n")) {
		return ""
	}
	sc := bufio.NewScanner(bytes.NewReader(head))
	sc.Scan()
	if sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) >= 2 && f[0] == "->" {
			return f[1]
		}
	}
	return "unknown"
}
