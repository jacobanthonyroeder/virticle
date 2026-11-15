package api

import "testing"

func TestPoolsOfShare(t *testing.T) {
	dirs := []poolDir{{"default", "/var/lib/libvirt/images"}, {"shared", "/mnt/shared"}, {"other", "/mnt/other"}, {"plain", "/mnt/plain"}}
	descs := map[string]poolDesc{
		"/var/lib/libvirt/images": {Name: "default", Type: "dir", Target: "/var/lib/libvirt/images"},
		"/mnt/shared":             {Name: "shared", Type: "netfs", Server: "NAS", Export: "/export/vm", Target: "/mnt/shared"},
		"/mnt/other":              {Name: "other", Type: "netfs", Server: "nas", Export: "/export/other", Target: "/mnt/other"},
		// A directory pool is never the share, whatever its path.
		"/mnt/plain": {Name: "plain", Type: "dir", Target: "/mnt/plain"},
	}
	id := func(p poolDesc) string { return p.shareKey() }
	got := poolsOfShare("nas:/export/vm@/mnt/shared", dirs, descs, id)
	if len(got) != 1 || got[0] != "shared" {
		t.Fatalf("got %v", got)
	}
	if got := poolsOfShare("", dirs, descs, id); len(got) != 0 {
		t.Fatalf("an empty share id matched %v", got)
	}
}
