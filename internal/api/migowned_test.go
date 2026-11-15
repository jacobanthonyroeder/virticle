package api

import "testing"

func TestOwnedIface(t *testing.T) {
	nics := []hostNIC{
		{Name: "eth0", State: "up", Addresses: []string{"192.0.2.5/24"}},
		{Name: "virbr0", State: "up", Type: "bridge", Addresses: []string{"192.168.122.1/24"}},
		{Name: "labnat", State: "up", Type: "bridge", Addresses: []string{"10.9.0.1/24"}},
		{Name: "br0", State: "up", Type: "bridge", Addresses: []string{"198.51.100.5/24"}},
	}
	own := map[string]bool{"labnat": true}
	for addr, want := range map[string]string{
		"192.168.122.1": "virbr0", // by its name
		"10.9.0.1":      "labnat", // a libvirt network with a name of its own
		"10.9.0.1:2222": "labnat",
		"192.0.2.5":     "",
		"198.51.100.5":  "", // a bridge of the host itself
		"203.0.113.9":   "",
		"node1.lan":     "",
		"[2001:db8::1]": "",
	} {
		if got := ownedIface(nics, own, addr); got != want {
			t.Errorf("%s: %q, want %q", addr, got, want)
		}
	}
}
