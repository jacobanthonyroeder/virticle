package api

import (
	"reflect"
	"strings"
	"testing"
)

func TestVerifyList(t *testing.T) {
	ch := chainN(2)
	full, incr := ch[0].M, ch[1].M
	full.Disks[0].Bytes, full.Disks[0].SHA256 = 100, strings.Repeat("a", 64)
	full.Disks[1].Bytes = 50 // no checksum: a shut-off backup made by a schedule
	// The incremental one is encrypted, with UEFI variables and TPM state.
	incr.NVRAM, incr.NVRAMEncrypted, incr.NVRAMBytes, incr.NVRAMSHA256 = "nvram.fd.age", true, 7, strings.Repeat("b", 64)
	incr.TPMState, incr.TPMBytes, incr.TPMSHA256 = "tpm2-state.bin.age", 9, strings.Repeat("c", 64)
	for i := range incr.Disks {
		incr.Disks[i].File, incr.Disks[i].Bytes, incr.Disks[i].SHA256 = incr.Disks[i].Target+".qcow2.age", 30, strings.Repeat("d", 64)
	}
	chain := []chainLink{{Dir: ch[0].Dir, Man: full}, {Dir: ch[1].Dir, Man: incr}}
	got := verifyList(chain)
	var names []string
	for _, f := range got {
		names = append(names, f.String())
	}
	want := []string{
		"vm-a.vmbackup/manifest.json", "vm-a.vmbackup/domain.xml", "vm-a.vmbackup/vda.qcow2", "vm-a.vmbackup/vdb.qcow2",
		"vm-b.vmbackup/manifest.json", "vm-b.vmbackup/domain.xml", "vm-b.vmbackup/nvram.fd.age", "vm-b.vmbackup/tpm2-state.bin.age",
		"vm-b.vmbackup/vda.qcow2.age", "vm-b.vmbackup/vdb.qcow2.age",
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("files %v", names)
	}
	if got[0].Size != -1 || got[2].Size != 100 || got[2].SHA == "" || got[3].SHA != "" || got[6].Size != 7 || got[7].SHA != strings.Repeat("c", 64) {
		t.Fatalf("sizes and checksums %+v", got)
	}
	if n := verifyResult(chain, got); n != "10 files intact in 2 backups; 1 without a checksum, size checked only" {
		t.Errorf("note %q", n)
	}
	if n := verifyResult(chain[:1], got[:3]); n != "3 files intact" {
		t.Errorf("note %q", n)
	}
}

func TestVerifyChain(t *testing.T) {
	ch := chainN(3)
	scan := func(bs []chainBackup) []scanned {
		var out []scanned
		for _, b := range bs {
			out = append(out, scanned{Dir: b.Dir, Manifest: b.M})
		}
		return out
	}
	got, err := verifyChain(scan(ch), "vm-c.vmbackup")
	if err != nil || len(got) != 3 || got[0].Dir != "vm-a.vmbackup" || got[2].Dir != "vm-c.vmbackup" {
		t.Fatalf("whole chain: %+v %v", got, err)
	}
	if got, err := verifyChain(scan(ch), "vm-a.vmbackup"); err != nil || len(got) != 1 {
		t.Fatalf("full backup: %+v %v", got, err)
	}
	cases := []struct {
		items []scanned
		name  string
		want  string
	}{
		{scan([]chainBackup{ch[0], ch[2]}), "vm-c.vmbackup", "the earlier backup vm-b.vmbackup that this one builds on is missing"},
		{scan(ch[1:]), "vm-c.vmbackup", "the earlier backup vm-a.vmbackup that this one builds on is missing"},
		{scan(ch), "vm-x.vmbackup", "vm-x.vmbackup is not a backup in this pool"},
		{append(scan([]chainBackup{ch[0], ch[2]}), scanned{Dir: "vm-b.vmbackup", Err: "manifest.json is not valid"}), "vm-c.vmbackup", "vm-b.vmbackup is unreadable: manifest.json is not valid"},
	}
	for _, c := range cases {
		if _, err := verifyChain(c.items, c.name); err == nil || err.Error() != c.want {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	// A loop in bad data ends.
	loop := chainN(2)
	loop[0].M.Kind, loop[0].M.Format, loop[0].M.Parent = kindIncremental, 2, "vm-b.vmbackup"
	if _, err := verifyChain(scan(loop), "vm-b.vmbackup"); err == nil {
		t.Error("a loop must fail")
	}
}

func TestVerifySizes(t *testing.T) {
	files := []verifyFile{{Backup: "b", Name: "manifest.json", Size: -1}, {Backup: "b", Name: "domain.xml", Size: -1}, {Backup: "b", Name: "vda.qcow2", Size: 100}}
	cases := []struct {
		sizes []string
		want  string
	}{
		{[]string{"300", "2000", "100"}, ""},
		{[]string{"300", "MISSING", "100"}, "b/domain.xml is missing"},
		{[]string{"300", "2000", "99"}, "b/vda.qcow2 is damaged: it has 99 bytes, the manifest says 100"},
		{[]string{"300", "2000", "MISSING"}, "b/vda.qcow2 is missing"},
		{[]string{"300"}, "b/domain.xml is missing"},
	}
	for _, c := range cases {
		err := verifySizes(files, c.sizes)
		if got := ""; err != nil {
			got = err.Error()
			if got != c.want {
				t.Errorf("%v: %q, want %q", c.sizes, got, c.want)
			}
		} else if c.want != "" {
			t.Errorf("%v: no error, want %q", c.sizes, c.want)
		}
	}
}
