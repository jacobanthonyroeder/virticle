package main

import (
	"testing"

	"virticle/internal/store"
)

func TestListedIn(t *testing.T) {
	ns := []store.Node{{Addr: "10.0.0.5"}, {Addr: "10.0.0.6:2222"}, {Addr: "[fe80::1]:22"}, {Addr: "Node7.lan"}}
	for addr, want := range map[string]bool{
		"10.0.0.5": true, "10.0.0.6": true, "fe80::1": true, "node7.lan": true,
		"10.0.0.7": false, "10.0.0.5:22": true, "": false,
	} {
		if got := listedIn(ns, addr); got != want {
			t.Errorf("listedIn(%q) = %v, want %v", addr, got, want)
		}
	}
	if listedIn(nil, "10.0.0.5") {
		t.Error("an empty node list lists nothing")
	}
}

func TestNodeListedNoDatabase(t *testing.T) {
	if nodeListed(t.TempDir(), "10.0.0.5") {
		t.Error("no database: not listed")
	}
}
