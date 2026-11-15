package api

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	testRecipient = "age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p"
	sumA          = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sumB          = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// encManifest is a valid backup of a UEFI VM with a TPM, every data file encrypted.
func encManifest() backupManifest {
	m := goodManifest()
	m.Recipient = testRecipient
	m.Disks[0] = backupDiskRec{Target: "vda", File: "vda.qcow2.age", Format: "qcow2", VirtualSize: 1 << 30, Bytes: 1200, SHA256: sumA, Encrypted: true, PlainBytes: 1000, PlainSHA256: sumB}
	m.NVRAM, m.NVRAMFormat, m.NVRAMEncrypted = "nvram.fd.age", "qcow2", true
	m.NVRAMBytes, m.NVRAMSHA256, m.NVRAMPlainBytes, m.NVRAMPlainSHA256 = 900, sumA, 800, sumB
	m.TPM, m.TPMState, m.TPMEncrypted = true, "tpm2-state.bin.age", true
	m.TPMBytes, m.TPMSHA256, m.TPMPlainBytes, m.TPMPlainSHA256 = 6500, sumA, 6234, sumB
	return m
}

func TestManifestEncrypted(t *testing.T) {
	ok := encManifest()
	raw, _ := json.Marshal(ok)
	m, err := parseBackupManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !m.encrypted() {
		t.Fatal("not seen as encrypted")
	}
	files := strings.Join(m.files(), " ")
	if files != "manifest.json domain.xml nvram.fd.age tpm2-state.bin.age vda.qcow2.age" {
		t.Fatalf("files %s", files)
	}
	if !strings.Contains(string(raw), `"recipient":"age1`) || !strings.Contains(string(raw), `"plainSha256"`) {
		t.Fatalf("fields: %s", raw)
	}
	// Only some files encrypted is also a valid backup: each file says what it is.
	part := encManifest()
	part.NVRAM, part.NVRAMEncrypted, part.NVRAMBytes, part.NVRAMSHA256, part.NVRAMPlainBytes, part.NVRAMPlainSHA256 = "nvram.fd", false, 0, "", 0, ""
	raw, _ = json.Marshal(part)
	if _, err := parseBackupManifest(raw); err != nil {
		t.Fatalf("partly encrypted: %v", err)
	}
	cases := map[string]func(m *backupManifest){
		"no recipient":     func(m *backupManifest) { m.Recipient = "" },
		"bad recipient":    func(m *backupManifest) { m.Recipient = "age1nope" },
		"ssh recipient":    func(m *backupManifest) { m.Recipient = "ssh-ed25519 AAAA" },
		"recipient, plain": func(m *backupManifest) { *m = goodManifest(); m.Recipient = testRecipient },
		"disk not .age":    func(m *backupManifest) { m.Disks[0].File = "vda.qcow2" },
		"plain disk named .age": func(m *backupManifest) {
			m.Disks[0].Encrypted = false
			m.Disks[0].PlainBytes, m.Disks[0].PlainSHA256 = 0, ""
		},
		"plain fields, plain":  func(m *backupManifest) { m.Disks[0].Encrypted, m.Disks[0].File = false, "vda.qcow2" },
		"no stored sha":        func(m *backupManifest) { m.Disks[0].SHA256 = "" },
		"no plain sha":         func(m *backupManifest) { m.Disks[0].PlainSHA256 = "" },
		"bad plain sha":        func(m *backupManifest) { m.Disks[0].PlainSHA256 = "xyz" },
		"no plain size":        func(m *backupManifest) { m.Disks[0].PlainBytes = 0 },
		"smaller than plain":   func(m *backupManifest) { m.Disks[0].Bytes = 10 },
		"reserved disk name":   func(m *backupManifest) { m.Disks[0].File = "nvram.fd.age" },
		"reserved disk name 2": func(m *backupManifest) { m.Disks[0].File = "manifest.json.age" },
		"nvram plain name":     func(m *backupManifest) { m.NVRAM = "nvram.fd" },
		"nvram other name":     func(m *backupManifest) { m.NVRAM = "x.age" },
		"nvram no sha":         func(m *backupManifest) { m.NVRAMSHA256 = "" },
		"nvram no plain sha":   func(m *backupManifest) { m.NVRAMPlainSHA256 = "" },
		"nvram sizes, plain":   func(m *backupManifest) { m.NVRAMEncrypted, m.NVRAM = false, "nvram.fd" },
		"tpm plain name":       func(m *backupManifest) { m.TPMState = "tpm2-state.bin" },
		"tpm no plain":         func(m *backupManifest) { m.TPMPlainSHA256 = "" },
		"tpm too large":        func(m *backupManifest) { m.TPMPlainBytes = backupTPMMax + 1 },
		"tpm flag, no file": func(m *backupManifest) {
			m.TPMState, m.TPMBytes, m.TPMSHA256, m.TPMPlainBytes, m.TPMPlainSHA256 = "", 0, "", 0, ""
		},
		"tpm plain fields, plain": func(m *backupManifest) { m.TPMEncrypted, m.TPMState, m.TPMBytes = false, "tpm2-state.bin", 6234 },
		"tpm plain fields, none": func(m *backupManifest) {
			m.TPMEncrypted, m.TPMState, m.TPMBytes, m.TPMSHA256 = false, "", 0, ""
		},
	}
	for name, mut := range cases {
		m := encManifest()
		mut(&m)
		raw, _ := json.Marshal(m)
		if _, err := parseBackupManifest(raw); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A manifest written before encryption reads as before: no new field, nothing encrypted.
func TestManifestOldIsPlain(t *testing.T) {
	old := `{"format":1,"vm":"a","uuid":"0dc0f2ac-2086-4f51-9633-9d6952479016","created":"2026-11-10T19:30:00Z","sourceNode":"n","appVersion":"x","live":true,` +
		`"disks":[{"target":"vda","file":"vda.qcow2","format":"qcow2","virtualSize":1,"bytes":1,"sha256":"` + sumA + `"}],"nvram":"nvram.fd","tpm":true,` +
		`"tpmState":"tpm2-state.bin","tpmBytes":5,"tpmSha256":"` + sumB + `","notes":[]}`
	m, err := parseBackupManifest([]byte(old))
	if err != nil {
		t.Fatal(err)
	}
	if m.encrypted() || m.Recipient != "" || strings.Join(m.files(), " ") != "manifest.json domain.xml nvram.fd tpm2-state.bin vda.qcow2" {
		t.Fatalf("%+v", m)
	}
}

// A chain may mix plain and encrypted backups: the list says a restore needs the key when any link has an
// encrypted file, and an export of it needs the passphrase only for an incremental one.
func TestChainKeyNeeds(t *testing.T) {
	full := goodManifest()
	full.Checkpoint = "virticle-a"
	inc := encManifest()
	inc.Format, inc.Kind, inc.Parent, inc.Created = backupFormatIncremental, kindIncremental, "web-1-20261110T193000Z.vmbackup", "2026-11-11T19:30:00Z"
	list := []backupEntry{
		{Node: 1, Pool: "default", Dir: "web-1-20261110T193000Z.vmbackup", Manifest: &full},
		{Node: 1, Pool: "default", Dir: "web-1-20261111T193000Z.vmbackup", Manifest: &inc},
	}
	annotateChains(list)
	if list[0].NeedsKey || !list[1].NeedsKey || list[1].Chain != 2 {
		t.Fatalf("%+v %+v", list[0], list[1])
	}
	// Plain incremental on an encrypted full: the restore needs the key too.
	full2, inc2 := encManifest(), goodManifest()
	inc2.Format, inc2.Kind, inc2.Parent, inc2.Created = backupFormatIncremental, kindIncremental, "web-1-20261110T193000Z.vmbackup", "2026-11-11T19:30:00Z"
	list = []backupEntry{
		{Node: 1, Pool: "default", Dir: "web-1-20261110T193000Z.vmbackup", Manifest: &full2},
		{Node: 1, Pool: "default", Dir: "web-1-20261111T193000Z.vmbackup", Manifest: &inc2},
	}
	annotateChains(list)
	if !list[0].NeedsKey || !list[1].NeedsKey {
		t.Fatal("a plain backup on an encrypted one needs the key")
	}
	if !chainNeedsKey([]chainLink{{Man: &full2}, {Man: &inc2}}) || chainNeedsKey([]chainLink{{Man: &inc2}}) {
		t.Fatal("chainNeedsKey")
	}
}
