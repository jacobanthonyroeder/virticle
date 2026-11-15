package api

// Pure logic of VM backups: the manifest, names, the libvirt backup XML,
// scanning a pool for backups, retention and vetting an uploaded archive.
//
// A backup is a self-contained directory "<vm>-<UTC timestamp>.vmbackup" in
// a directory-backed storage pool. The manifest is written last, so a
// directory without one is a partial backup that is never listed.

import (
	"archive/tar"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"virticle/internal/buildinfo"

	"virticle/internal/backupkey"
	"virticle/internal/cluster"
)

// backupAppVersion is what a new manifest records as its writer: "virticle 0.4.1", or "virticle dev" for a plain build.
func backupAppVersion() string { return "virticle " + buildinfo.Version }

const (
	backupFormat      = 1
	backupSuffix      = ".vmbackup"
	backupTimeLayout  = "20060102T150405Z"
	backupMaxManifest = 64 << 10
	backupMaxXML      = 1 << 20
	backupMaxDisks    = 32
	backupMaxScan     = 500 // backups listed per pool
	backupNVRAMFile   = "nvram.fd"
	// backupTPMFile holds the state of an emulated TPM 2.0 (swtpm's tpm2-00.permall, a few KiB).
	backupTPMFile = "tpm2-state.bin"
	backupTPMMax  = 1 << 20 // the node command copies no larger state
	// ageSuffix ends the name of a file that is stored encrypted with age; the rest of the name is the plain file's.
	ageSuffix = ".age"
	// ageOverheadMax bounds what age adds to a small file (header and one tag per 64 KiB chunk).
	ageOverheadMax = 1 << 12
)

var (
	backupDirRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,200}\.vmbackup$`)
	targetDevRE  = regexp.MustCompile(`^[a-z][a-z0-9]{1,7}$`)
	sha256RE     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	checkpointRE = regexp.MustCompile(`^virticle-[A-Za-z0-9._-]{1,80}$`)
)

const (
	noteSnapshots = "Snapshots are not included: a backup holds the current state of the VM only."
	noteTPM       = "TPM state is not included: a restored VM starts with an empty TPM (BitLocker keys or measured-boot secrets sealed to it will not unseal)."
	// noteTPMIncluded is the note of a backup that holds the TPM state.
	noteTPMIncluded = "TPM state is included: the backup holds the keys the TPM keeps (BitLocker, for instance), so protect it like the VM itself."
)

type backupDiskRec struct {
	Target      string `json:"target"`
	File        string `json:"file"`
	Format      string `json:"format"`
	VirtualSize uint64 `json:"virtualSize"`
	Bytes       uint64 `json:"bytes"`
	SHA256      string `json:"sha256,omitempty"`
	// Source is the disk's file in the VM when the backup was taken; with the
	// size it tells whether the next backup can build on this one.
	Source string `json:"source,omitempty"`
	// Encrypted says File is stored encrypted with age to the manifest's Recipient ("<target>.qcow2.age"). Bytes
	// and SHA256 are then those of the stored, encrypted file (so the backup can be checked without the
	// passphrase), PlainBytes and PlainSHA256 those of the qcow2 image inside.
	Encrypted   bool   `json:"encrypted,omitempty"`
	PlainBytes  uint64 `json:"plainBytes,omitempty"`
	PlainSHA256 string `json:"plainSha256,omitempty"`
}

type backupManifest struct {
	Format     int             `json:"format"`
	VM         string          `json:"vm"`
	UUID       string          `json:"uuid"`
	Created    string          `json:"created"` // RFC 3339, UTC
	SourceNode string          `json:"sourceNode"`
	AppVersion string          `json:"appVersion"`
	Live       bool            `json:"live"` // taken while the VM was running
	Disks      []backupDiskRec `json:"disks"`
	NVRAM      string          `json:"nvram,omitempty"` // file name of the saved UEFI variables
	// NVRAMFormat is the on-disk format of the saved UEFI variables: "qcow2"
	// or "raw". Backups from before qcow2 stores have none, which means raw.
	NVRAMFormat string `json:"nvramFormat,omitempty"`
	TPM         bool   `json:"tpm"` // the VM had a TPM
	// TPMState is the file with the state of the VM's emulated TPM 2.0, with its size and SHA-256. Backups made
	// before TPM state was saved, and backups of a VM whose TPM state could not be read, have none: the restored
	// VM starts with an empty TPM. A program that does not know the field restores the backup the same way.
	TPMState  string   `json:"tpmState,omitempty"`
	TPMBytes  uint64   `json:"tpmBytes,omitempty"`
	TPMSHA256 string   `json:"tpmSha256,omitempty"`
	Notes     []string `json:"notes"`
	// Kind is "full" or "incremental"; a backup from before incremental
	// backups has none and is full. An incremental backup (format 2) holds
	// only the blocks that changed since its Parent, a backup in the same pool.
	Kind   string `json:"kind,omitempty"`
	Parent string `json:"parent,omitempty"` // directory name of the backup this one builds on
	// Checkpoint is the libvirt checkpoint made with this backup, the point the
	// next incremental backup counts changes from.
	Checkpoint string `json:"checkpoint,omitempty"`
	// FullReason says in words why a backup is full when an incremental one
	// might have been expected.
	FullReason string `json:"fullReason,omitempty"`
	// Frozen says whether the guest's file systems were frozen while the backup
	// began (a live backup only; absent for older backups and shut-off VMs).
	Frozen *bool `json:"frozen,omitempty"`
	// Recipient is the public age key ("age1...") the encrypted files of this backup are encrypted to: the backup
	// key whose recovery key file and passphrase open them. Empty when no file is encrypted. The manifest itself
	// and domain.xml are never encrypted, so a backup is listed and checked without the passphrase.
	Recipient string `json:"recipient,omitempty"`
	// NVRAMEncrypted: the UEFI variables are stored encrypted ("nvram.fd.age"), with the size and SHA-256 of the
	// stored file and of the plain one.
	NVRAMEncrypted   bool   `json:"nvramEncrypted,omitempty"`
	NVRAMBytes       uint64 `json:"nvramBytes,omitempty"`
	NVRAMSHA256      string `json:"nvramSha256,omitempty"`
	NVRAMPlainBytes  uint64 `json:"nvramPlainBytes,omitempty"`
	NVRAMPlainSHA256 string `json:"nvramPlainSha256,omitempty"`
	// TPMEncrypted: the TPM state is stored encrypted ("tpm2-state.bin.age"); TPMBytes and TPMSHA256 are then
	// those of the stored file, TPMPlainBytes and TPMPlainSHA256 those of the state.
	TPMEncrypted   bool   `json:"tpmEncrypted,omitempty"`
	TPMPlainBytes  uint64 `json:"tpmPlainBytes,omitempty"`
	TPMPlainSHA256 string `json:"tpmPlainSha256,omitempty"`
}

// encrypted says whether any file of the backup is stored encrypted.
func (m *backupManifest) encrypted() bool {
	if m.NVRAMEncrypted || m.TPMEncrypted {
		return true
	}
	for _, d := range m.Disks {
		if d.Encrypted {
			return true
		}
	}
	return false
}

// checkEncryptedFile vets the fields of one file that may be stored encrypted: its name (plain names never end in
// .age, encrypted ones always do) and, when it is encrypted, the stored and the plain size and checksum.
func checkEncryptedFile(what, name string, enc bool, bytes uint64, sum string, plainBytes uint64, plainSum string) error {
	switch {
	case !enc && strings.HasSuffix(name, ageSuffix):
		return fmt.Errorf("manifest: %s is named like an encrypted file but not marked encrypted", what)
	case !enc && (plainBytes != 0 || plainSum != ""):
		return fmt.Errorf("manifest: %s has plain-file fields but is not encrypted", what)
	case !enc:
		return nil
	case !strings.HasSuffix(name, ageSuffix):
		return fmt.Errorf("manifest: the encrypted %s must be named .age", what)
	case bytes == 0 || !sha256RE.MatchString(sum):
		return fmt.Errorf("manifest: the encrypted %s needs the size and SHA-256 of the stored file", what)
	case plainBytes == 0 || !sha256RE.MatchString(plainSum):
		return fmt.Errorf("manifest: the encrypted %s needs the size and SHA-256 of the plain file", what)
	case bytes < plainBytes:
		return fmt.Errorf("manifest: the encrypted %s is smaller than its plain content", what)
	}
	return nil
}

func (m *backupManifest) createdTime() time.Time {
	t, _ := time.Parse(time.RFC3339, m.Created)
	return t
}

// backupDirName is the directory name for a backup of vm taken at t.
func backupDirName(vm string, t time.Time) string {
	return vm + "-" + t.UTC().Format(backupTimeLayout) + backupSuffix
}

// parseBackupManifest reads and vets a manifest. Everything in it ends up in
// file names and commands, so only plain names are accepted.
func parseBackupManifest(raw []byte) (*backupManifest, error) {
	if len(raw) > backupMaxManifest {
		return nil, fmt.Errorf("manifest.json is larger than %d KiB", backupMaxManifest>>10)
	}
	var m backupManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("manifest.json is not valid: %w", err)
	}
	switch {
	case m.Format != backupFormat && m.Format != backupFormatIncremental:
		return nil, fmt.Errorf("unsupported backup format %d (this version reads formats %d and %d)", m.Format, backupFormat, backupFormatIncremental)
	case (m.Format == backupFormatIncremental) != (m.Kind == kindIncremental):
		return nil, errors.New("manifest: the format and the kind of backup do not fit together")
	case m.Kind != "" && m.Kind != kindFull && m.Kind != kindIncremental:
		return nil, fmt.Errorf("manifest: unknown kind of backup %q", m.Kind)
	case !vmNameRE.MatchString(m.VM):
		return nil, errors.New("manifest: the VM name is not valid")
	case len(m.Disks) == 0 || len(m.Disks) > backupMaxDisks:
		return nil, errors.New("manifest: a backup needs between 1 and 32 disks")
	}
	if _, err := cluster.ParseUUID(m.UUID); err != nil {
		return nil, errors.New("manifest: the VM UUID is not valid")
	}
	if _, err := time.Parse(time.RFC3339, m.Created); err != nil {
		return nil, errors.New("manifest: the creation time is not valid")
	}
	seenFile, seenDev := map[string]bool{}, map[string]bool{}
	for _, d := range m.Disks {
		switch {
		case !targetDevRE.MatchString(d.Target) || seenDev[d.Target]:
			return nil, fmt.Errorf("manifest: bad or duplicate disk target %q", d.Target)
		case !plainNameRE.MatchString(d.File) || seenFile[d.File] || reservedBackupFile(d.File):
			return nil, fmt.Errorf("manifest: bad or duplicate file name %q", d.File)
		case d.Format != "qcow2":
			return nil, fmt.Errorf("manifest: disk %s has format %q, only qcow2 is supported", d.Target, d.Format)
		case d.SHA256 != "" && !sha256RE.MatchString(d.SHA256):
			return nil, fmt.Errorf("manifest: disk %s has a malformed checksum", d.Target)
		case d.Encrypted && reservedBackupFile(strings.TrimSuffix(d.File, ageSuffix)):
			return nil, fmt.Errorf("manifest: bad or duplicate file name %q", d.File)
		}
		if err := checkEncryptedFile("disk "+d.Target, d.File, d.Encrypted, d.Bytes, d.SHA256, d.PlainBytes, d.PlainSHA256); err != nil {
			return nil, err
		}
		seenFile[d.File], seenDev[d.Target] = true, true
	}
	if m.Kind == kindIncremental {
		if !backupDirRE.MatchString(m.Parent) {
			return nil, errors.New("manifest: an incremental backup needs the name of the backup it builds on")
		}
	} else if m.Parent != "" {
		return nil, errors.New("manifest: only an incremental backup has a parent")
	}
	if m.Checkpoint != "" && !checkpointRE.MatchString(m.Checkpoint) {
		return nil, fmt.Errorf("manifest: bad checkpoint name %q", m.Checkpoint)
	}
	if len(m.FullReason) > 300 {
		return nil, errors.New("manifest: the reason is too long")
	}
	switch {
	case m.NVRAMEncrypted && m.NVRAM != backupNVRAMFile+ageSuffix, !m.NVRAMEncrypted && m.NVRAM != "" && m.NVRAM != backupNVRAMFile:
		return nil, fmt.Errorf("manifest: bad NVRAM file name %q", m.NVRAM)
	case !m.NVRAMEncrypted && (m.NVRAMBytes != 0 || m.NVRAMSHA256 != ""):
		return nil, errors.New("manifest: a size or checksum for plain UEFI variables")
	}
	if err := checkEncryptedFile("UEFI variable file", m.NVRAM, m.NVRAMEncrypted, m.NVRAMBytes, m.NVRAMSHA256, m.NVRAMPlainBytes, m.NVRAMPlainSHA256); err != nil {
		return nil, err
	}
	if m.NVRAMFormat != "" && m.NVRAMFormat != "raw" && m.NVRAMFormat != "qcow2" {
		return nil, fmt.Errorf("manifest: bad NVRAM format %q", m.NVRAMFormat)
	}
	tpmFile, tpmMax := backupTPMFile, uint64(backupTPMMax)
	if m.TPMEncrypted {
		tpmFile, tpmMax = backupTPMFile+ageSuffix, backupTPMMax+ageOverheadMax
	}
	switch {
	case m.TPMState == "" && (m.TPMBytes != 0 || m.TPMSHA256 != "" || m.TPMEncrypted):
		return nil, errors.New("manifest: a size or checksum for TPM state, but no file")
	case m.TPMState == "":
	case m.TPMState != tpmFile:
		return nil, fmt.Errorf("manifest: bad TPM state file name %q", m.TPMState)
	case !m.TPM:
		return nil, errors.New("manifest: TPM state, but the VM has no TPM")
	case m.TPMBytes == 0 || m.TPMBytes > tpmMax || m.TPMPlainBytes > backupTPMMax:
		return nil, errors.New("manifest: the TPM state has an impossible size")
	case !sha256RE.MatchString(m.TPMSHA256):
		return nil, errors.New("manifest: the TPM state needs its SHA-256")
	}
	if m.TPMState != "" {
		if err := checkEncryptedFile("TPM state", m.TPMState, m.TPMEncrypted, m.TPMBytes, m.TPMSHA256, m.TPMPlainBytes, m.TPMPlainSHA256); err != nil {
			return nil, err
		}
	} else if m.TPMPlainBytes != 0 || m.TPMPlainSHA256 != "" {
		return nil, errors.New("manifest: plain-file fields for TPM state, but no file")
	}
	switch {
	case m.encrypted() && !backupkey.ValidRecipient(m.Recipient):
		return nil, errors.New("manifest: encrypted files need the public key they are encrypted to")
	case !m.encrypted() && m.Recipient != "":
		return nil, errors.New("manifest: a key, but no encrypted file")
	}
	return &m, nil
}

func reservedBackupFile(n string) bool {
	return n == "manifest.json" || n == "domain.xml" || n == backupNVRAMFile || n == backupTPMFile ||
		n == backupNVRAMFile+ageSuffix || n == backupTPMFile+ageSuffix
}

// files lists every file a backup directory consists of.
func (m *backupManifest) files() []string {
	out := []string{"manifest.json", "domain.xml"}
	if m.NVRAM != "" {
		out = append(out, m.NVRAM)
	}
	if m.TPMState != "" {
		out = append(out, m.TPMState)
	}
	for _, d := range m.Disks {
		out = append(out, d.File)
	}
	return out
}

// tpmNotes is what the notes of a backup say about the TPM: that its state is included, or for a VM with a TPM
// whose state is not (TPM 1.2, a passthrough TPM, a state that could not be read), that it is not.
func tpmNotes(hasTPM, included bool) []string {
	switch {
	case included:
		return []string{noteTPMIncluded}
	case hasTPM:
		return []string{noteTPM}
	}
	return nil
}

// --- libvirt backup XML ---

// backupXML is the <domainbackup> document for a push-mode backup of the
// given disks into dir: full, or with incremental set to the name of the
// checkpoint to count changes from (the file then holds only those blocks).
// Other disks (read-only, CD-ROMs) are left out explicitly. The checkpoint, if
// any, is a separate document given to the same call (checkpointXML).
func backupXML(dir string, disks []migDisk, files map[string]string, skip []string, incremental string) string {
	var b strings.Builder
	b.WriteString("<domainbackup mode='push'>")
	if incremental != "" {
		b.WriteString("<incremental>" + esc(incremental) + "</incremental>")
	}
	b.WriteString("<disks>")
	for _, d := range disks {
		fmt.Fprintf(&b, "<disk name='%s' backup='yes' type='file'><target file='%s'/><driver type='qcow2'/></disk>",
			esc(d.Target), esc(dir+"/"+files[d.Target]))
	}
	for _, t := range skip {
		fmt.Fprintf(&b, "<disk name='%s' backup='no'/>", esc(t))
	}
	b.WriteString("</disks></domainbackup>")
	return b.String()
}

var (
	reBackingOpen  = regexp.MustCompile(`<backingStore\b[^>]*>`)
	reBackingClose = regexp.MustCompile(`</backingStore>`)
)

// stripBackingStore removes <backingStore> elements, which describe the
// backing chain of the disks as they were on the source. A restored disk is a
// single flat image; leaving the element in would make libvirt open a
// backing file that does not exist. Nested chains are removed whole.
func stripBackingStore(x string) string {
	var out strings.Builder
	depth := 0
	for len(x) > 0 {
		o, c := reBackingOpen.FindStringIndex(x), reBackingClose.FindStringIndex(x)
		next := -1
		var open bool
		switch {
		case o != nil && (c == nil || o[0] < c[0]):
			next, open = o[0], true
		case c != nil:
			next = c[0]
		}
		if next < 0 {
			if depth == 0 {
				out.WriteString(x)
			}
			break
		}
		if depth == 0 {
			out.WriteString(x[:next])
		}
		if open {
			end := o[1]
			if !strings.HasSuffix(x[o[0]:end], "/>") {
				depth++
			}
			x = x[end:]
		} else {
			if depth > 0 {
				depth--
			}
			x = x[c[1]:]
		}
		if depth == 0 {
			// Drop the indentation and newline left behind by a removed element.
			x = strings.TrimLeft(x, " \t")
			x = strings.TrimPrefix(x, "\n")
			s := out.String()
			out.Reset()
			out.WriteString(strings.TrimRight(s, " \t"))
		}
	}
	return out.String()
}

// --- scanning pools for backups ---

// scanItem is one backup found in a pool: its directory, size on disk and the
// manifest text.
type scanItem struct {
	Dir      string
	Size     uint64
	Manifest []byte
}

func newScanMarker() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "@@@" + hex.EncodeToString(b)
}

// parseScan splits the output of the node command's backup-scan verb: per
// backup "<marker> <dir>", the size in bytes, the manifest, a newline. Directories outside poolDir or with
// unexpected names are dropped.
func parseScan(out, marker, poolDir string) []scanItem {
	var items []scanItem
	for _, sec := range strings.Split(out, marker+" ")[1:] {
		head, rest, _ := strings.Cut(sec, "\n")
		sz, body, _ := strings.Cut(rest, "\n")
		dir := strings.TrimSpace(head)
		size, _ := strconv.ParseUint(strings.TrimSpace(sz), 10, 64)
		if path.Clean(dir) != dir || path.Dir(dir) != strings.TrimRight(poolDir, "/") || !backupDirRE.MatchString(path.Base(dir)) {
			continue
		}
		items = append(items, scanItem{Dir: dir, Size: size, Manifest: []byte(strings.TrimSpace(body))})
	}
	return items
}

// --- retention ---

type retained struct {
	Key     string // what identifies the backup to the caller (its directory name)
	UUID    string
	Created time.Time
	Parent  string // the backup this one builds on ("" for a full backup)
}

// --- vetting an uploaded archive ---

// vetTar copies a tar stream from in to out, refusing anything but a small
// set of regular files with plain names. The archive is rewritten, not
// passed through: headers are rebuilt from the vetted fields, so extensions
// (links, long names, sparse maps) never reach the node's tar. budget is the
// most data accepted (free space on the node minus a margin).
func vetTar(in io.Reader, out io.Writer, budget int64) ([]string, error) {
	tr := tar.NewReader(in)
	tw := tar.NewWriter(out)
	var names []string
	seen := map[string]bool{}
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, badRequest("refusing archive: not a readable tar file: " + err.Error())
		}
		if h.Typeflag != tar.TypeReg {
			return nil, badRequest("refusing archive: " + quoteTag(h.Name) + " is not a regular file (directories, links and devices are not allowed)")
		}
		if !plainNameRE.MatchString(h.Name) {
			return nil, badRequest("refusing archive: member " + quoteTag(h.Name) + " is not a plain file name (no slashes, no ..)")
		}
		if seen[h.Name] {
			return nil, badRequest("refusing archive: duplicate member " + h.Name)
		}
		seen[h.Name] = true
		if len(names) >= backupMaxDisks+4 {
			return nil, badRequest("refusing archive: too many members")
		}
		if h.Size < 0 {
			return nil, badRequest("refusing archive: bad size for " + h.Name)
		}
		if total += h.Size; total > budget {
			return nil, badRequest(fmt.Sprintf("refusing archive: its contents (%d MiB so far) do not fit the free space of the pool", total>>20))
		}
		names = append(names, h.Name)
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: h.Name, Size: h.Size, Mode: 0o600, ModTime: time.Unix(0, 0)}); err != nil {
			return nil, err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return nil, err
		}
	}
	if len(names) == 0 {
		return nil, badRequest("the archive is empty")
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return names, nil
}
