package api

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

const tpmSum = "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"

func tpmManifest() backupManifest {
	m := goodManifest()
	m.TPM, m.TPMState, m.TPMBytes, m.TPMSHA256 = true, backupTPMFile, 6234, tpmSum
	m.Notes = append([]string{noteSnapshots}, tpmNotes(true, true)...)
	return m
}

func TestManifestWithTPMState(t *testing.T) {
	raw, _ := json.Marshal(tpmManifest())
	m, err := parseBackupManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(m.files(), backupTPMFile) {
		t.Errorf("the TPM state is not one of the files of the backup: %v", m.files())
	}
	if w := tpmRestoreWarning(m); w != "" {
		t.Errorf("a backup with the TPM state warns: %q", w)
	}
	for _, k := range []string{`"tpmState":"tpm2-state.bin"`, `"tpmBytes":6234`, `"tpmSha256":"` + tpmSum + `"`} {
		if !strings.Contains(string(raw), k) {
			t.Errorf("manifest lacks %s: %s", k, raw)
		}
	}
	cases := map[string]func(m *backupManifest){
		"name":          func(m *backupManifest) { m.TPMState = "../tpm2-00.permall" },
		"other name":    func(m *backupManifest) { m.TPMState = "nvram.fd" },
		"no TPM":        func(m *backupManifest) { m.TPM = false },
		"no checksum":   func(m *backupManifest) { m.TPMSHA256 = "" },
		"bad checksum":  func(m *backupManifest) { m.TPMSHA256 = "abc" },
		"empty":         func(m *backupManifest) { m.TPMBytes = 0 },
		"too large":     func(m *backupManifest) { m.TPMBytes = backupTPMMax + 1 },
		"sum, no file":  func(m *backupManifest) { m.TPMState, m.TPMBytes = "", 0 },
		"size, no file": func(m *backupManifest) { m.TPMState, m.TPMSHA256 = "", "" },
		"disk named so": func(m *backupManifest) { m.Disks[0].File = backupTPMFile },
		"disk, no state": func(m *backupManifest) {
			m.TPMState, m.TPMBytes, m.TPMSHA256, m.Disks[0].File = "", 0, "", backupTPMFile
		},
	}
	for name, mut := range cases {
		m := tpmManifest()
		mut(&m)
		raw, _ := json.Marshal(m)
		if _, err := parseBackupManifest(raw); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A backup from before TPM state was saved restores as it always did: no TPM file, and the warning.
func TestManifestFromBeforeTPMState(t *testing.T) {
	old := `{"format":2,"vm":"w11","uuid":"0dc0f2ac-2086-4f51-9633-9d6952479016","created":"2026-11-10T19:30:00Z","sourceNode":"n","appVersion":"virticle dev","live":true,` +
		`"disks":[{"target":"vda","file":"vda.qcow2","format":"qcow2","virtualSize":1,"bytes":1,"sha256":"` + tpmSum + `"}],"nvram":"nvram.fd","tpm":true,` +
		`"notes":["` + noteSnapshots + `","` + noteTPM + `"],"kind":"incremental","parent":"w11-20261110T183000Z.vmbackup"}`
	m, err := parseBackupManifest([]byte(old))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(m.files(), backupTPMFile) || len(m.files()) != 4 {
		t.Errorf("files of an old backup: %v", m.files())
	}
	if w := tpmRestoreWarning(m); !strings.Contains(w, "empty TPM") {
		t.Errorf("no warning for an old backup of a VM with a TPM: %q", w)
	}
	m.TPM = false
	if w := tpmRestoreWarning(m); w != "" {
		t.Errorf("a VM without a TPM warns: %q", w)
	}
}

func TestTPMNotes(t *testing.T) {
	if n := tpmNotes(false, false); n != nil {
		t.Errorf("no TPM: %v", n)
	}
	if n := tpmNotes(true, false); len(n) != 1 || n[0] != noteTPM {
		t.Errorf("TPM without state: %v", n)
	}
	if n := tpmNotes(true, true); len(n) != 1 || !strings.HasPrefix(n[0], "TPM state is included") {
		t.Errorf("TPM with state: %v", n)
	}
}

// The task of a backup without the TPM state says so, next to what it says about the freeze.
func TestFinishWithTPMNote(t *testing.T) {
	m := &bkRun{name: "vm1", fz: freezeOutcome{Frozen: true, Text: frozenText}, tpmNote: "TPM state not included: it is not an emulated TPM 2.0"}
	err := m.finishFreeze(nil)
	var dn doneNote
	if !errors.As(err, &dn) || !strings.Contains(string(dn), frozenText) || !strings.Contains(string(dn), "TPM state not included") {
		t.Errorf("note: %v", err)
	}
	if err := (&bkRun{tpmNote: "TPM state not included: x"}).finishFreeze(nil); err != doneNote("TPM state not included: x") {
		t.Errorf("offline run with a TPM note: %v", err)
	}
	boom := errors.New("boom")
	if err := m.finishFreeze(boom); err != boom {
		t.Errorf("a failed run keeps its error: %v", err)
	}
}

func TestVetTarTakesTPMState(t *testing.T) {
	m := tpmManifest()
	var hs []tar.Header
	for _, f := range m.files() {
		hs = append(hs, tar.Header{Name: f, Size: 3, Typeflag: tar.TypeReg, Mode: 0o600})
	}
	for i := 1; i < backupMaxDisks; i++ { // the most disks a manifest may list, with every other file
		hs = append(hs, tar.Header{Name: fmt.Sprintf("vd%d.qcow2", i), Size: 3, Typeflag: tar.TypeReg, Mode: 0o600})
	}
	if _, err := vetTar(bytes.NewReader(mkTar(t, hs...)), &bytes.Buffer{}, 1<<20); err != nil {
		t.Fatal(err)
	}
}
