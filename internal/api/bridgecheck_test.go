package api

import (
	"strings"
	"testing"
)

func TestCheckBridge(t *testing.T) {
	nics := []hostNIC{
		{Name: "eth0", Type: "physical"},
		{Name: "virbr0", Type: "bridge", Kind: "bridge"},
		{Name: "ovsbr0", Type: "other", Kind: "openvswitch"},
		{Name: "tap1", Type: "tap", Kind: "tun"},
	}
	for _, ok := range []string{"virbr0", "ovsbr0"} {
		if _, err := checkBridge(nics, ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	if _, err := checkBridge(nics, "eth0"); err == nil || !strings.Contains(err.Error(), "eth0 is a physical interface of this node, not a bridge") {
		t.Errorf("a physical interface: %v", err)
	}
	if _, err := checkBridge(nics, "tap1"); err == nil || !strings.Contains(err.Error(), "not a bridge") {
		t.Errorf("a tap: %v", err)
	}
	if _, err := checkBridge(nics, "br-nope"); err == nil || !strings.Contains(err.Error(), "no interface named br-nope") {
		t.Errorf("a missing name: %v", err)
	}
}

// The Open vSwitch datapath has the link kind of the bridges and is no bridge; a bridge says it is Open vSwitch.
func TestCheckBridgeOVS(t *testing.T) {
	nics := []hostNIC{
		{Name: "ovs-system", Type: "other", Kind: "openvswitch"},
		{Name: "ovsbr0", Type: "other", Kind: "openvswitch"},
		{Name: "br0", Type: "bridge", Kind: "bridge"},
	}
	if ovs, err := checkBridge(nics, "ovsbr0"); err != nil || !ovs {
		t.Errorf("ovsbr0: ovs=%v err=%v", ovs, err)
	}
	if ovs, err := checkBridge(nics, "br0"); err != nil || ovs {
		t.Errorf("br0: ovs=%v err=%v", ovs, err)
	}
	if _, err := checkBridge(nics, "ovs-system"); err == nil {
		t.Error("ovs-system is the datapath, not a bridge")
	}
	q := createVMRequest{Name: "x", VCPUs: 1, MemoryMiB: 128, Bridge: "ovsbr0", ISO: "/i.iso", Firmware: "bios", ovsBridge: true}
	if out := build(t, q); !strings.Contains(out, "<source bridge='ovsbr0'/>\n      <virtualport type='openvswitch'/>") {
		t.Errorf("new VM on an Open vSwitch bridge:\n%s", out)
	}
	q.Bridge, q.ovsBridge = "br0", false
	if out := build(t, q); strings.Contains(out, "virtualport") {
		t.Errorf("new VM on a Linux bridge:\n%s", out)
	}
}
