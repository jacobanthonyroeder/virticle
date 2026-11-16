package api

import (
	"encoding/xml"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/hostconn"
)

// Rollbacks and cleanups remove a file only through remove-id; every other answer keeps it and says why.
func TestKeptBecause(t *testing.T) {
	for _, c := range []struct {
		ans  string
		err  error
		gone bool
		want string
	}{
		{hostconn.RemovedFile, nil, true, ""},
		{hostconn.MissingFile, nil, true, ""},
		{hostconn.InUseFile, nil, false, "has it open"},
		{hostconn.OtherFile, nil, false, "another file"},
		{"", errors.New("no answer"), false, "no answer"},
		{"what", nil, false, "unexpected"},
	} {
		got := keptBecause("n1", c.ans, c.err)
		if (got == "") != c.gone || !strings.Contains(got, c.want) {
			t.Errorf("%q %v: %q", c.ans, c.err, got)
		}
	}
	// A file without an identity is never removed: nothing is even asked.
	if why := removeOwnFile(nil, hostconn.FileID{Path: "/p/a", Exists: true, Regular: true}); why == "" {
		t.Error("a file without identity was not kept")
	}
}

// Replacing a VM by a restore deletes what a normal delete would: writable, not shareable disks and the seed.
func TestOwnDiskFiles(t *testing.T) {
	raw := `<domain><name>web</name><devices>
	<disk type='file' device='disk'><source file='/p/web.qcow2'/><target dev='vda'/></disk>
	<disk type='file' device='disk'><source file='/p/base.iso.img'/><target dev='vdb'/><readonly/></disk>
	<disk type='file' device='disk'><source file='/p/shared.img'/><target dev='vdc'/><shareable/></disk>
	<disk type='file' device='cdrom'><source file='/p/` + seedVolumeName("web") + `'/><target dev='sda'/><readonly/></disk>
	<disk type='file' device='cdrom'><source file='/p/debian.iso'/><target dev='sdb'/><readonly/></disk>
	<disk type='block' device='disk'><source dev='/dev/vg/x'/><target dev='vdd'/></disk>
	</devices></domain>`
	var dx domainXML
	if err := xml.Unmarshal([]byte(raw), &dx); err != nil {
		t.Fatal(err)
	}
	got := ownDiskFiles(&dx, "web")
	want := []string{"/p/web.qcow2", "/p/" + seedVolumeName("web")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A cleanup touches nothing on a share that does not answer.
func TestDownIn(t *testing.T) {
	pools := map[string]dstPool{
		"/mnt/nfs":  {pool: libvirt.StoragePool{Name: "nfs"}, down: true},
		"/var/pool": {pool: libvirt.StoragePool{Name: "default"}, active: true},
	}
	if got := downIn(pools, []string{"/var/pool"}); got != "" {
		t.Errorf("a local pool: %q", got)
	}
	if got := downIn(pools, []string{"/var/pool", "/mnt/nfs/"}); !strings.Contains(got, "nfs") || !strings.Contains(got, "does not answer") {
		t.Errorf("a dead share: %q", got)
	}
	if got := downIn(pools, []string{"/elsewhere"}); got != "" {
		t.Errorf("no pool: %q", got)
	}
}

// The private name of a copy is a valid, distinct volume name next to the real one.
func TestMoveTempName(t *testing.T) {
	a, b := moveTempName("disk.qcow2"), moveTempName("disk.qcow2")
	if a == b || !strings.HasPrefix(a, "disk.qcow2.virticle-move-") || !moveNameRE.MatchString(a) || !safePathRE.MatchString("/p/"+a) {
		t.Errorf("%q %q", a, b)
	}
}
