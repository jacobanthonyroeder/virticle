package nodecmd

import (
	"strings"
	"testing"

	"virticle/internal/nodeproto"
)

// The first change to a bridge set up by hand can be any change: here it adds a card (a bond is made) or an
// address on a VLAN.

func TestNMForeignFirstChangeSetUplinks(t *testing.T) {
	n := newNMForeign(t, nmBrKeyfile, nmPortKeyfile)
	f := n.f
	f.links = append(f.links, fLink{name: "eth2", mac: "52:54:00:aa:bb:ee", mtu: 1500})
	n.putAll("sys/class/net/eth2/device", "")
	f.addAddr("eth2", "198.51.100.20/24", true)
	f.outputs["nmcli -t -g GENERAL.CON-UUID device show eth2"] = nm2UUID + "\n"
	f.outputs["nmcli -t -f connection.id,connection.type,connection.autoconnect,802-3-ethernet.mtu,ipv4,ipv6 connection show uuid "+nm2UUID] =
		"connection.id:Wired connection 3\nconnection.type:802-3-ethernet\nconnection.autoconnect:yes\n802-3-ethernet.mtu:auto\nipv4.method:auto\n"
	req := nodeproto.NetRequest{Action: nodeproto.NetSetUplinks, Bridge: "br0", Uplinks: []string{"eth1", "eth2"}, BondMode: nodeproto.BondFailover, ControllerAddrs: []string{"192.0.2.10"}}
	p := n.plan(req)
	cmds := strings.Join(p.Commands, "\n")
	for _, want := range []string{
		"nmcli connection add type bond ifname b-br0 con-name virticle-bond-b-br0 controller br0 port-type bridge",
		"nmcli connection add type ethernet ifname eth1 con-name virticle-port-eth1 controller b-br0 port-type bond",
		"nmcli connection add type ethernet ifname eth2 con-name virticle-port-eth2 controller b-br0 port-type bond",
		"nmcli connection modify uuid " + nm2UUID + " connection.autoconnect no",
		"nmcli connection delete uuid " + nmBrUUID + " uuid " + nmPortUUID,
		"ip addr flush dev eth2",
	} {
		has(t, "set-uplinks takeover", cmds, want)
	}
	sum := strings.Join(p.Summary, " ")
	has(t, "summary", sum, "A bond b-br0 is created")
	has(t, "summary", sum, "eth2 joins br0")
	// The rollback gives eth2 its own profile back and removes the new bond.
	n.f.onRun = nil
	id := n.apply(req)
	n.ok("", "net-run", id)
	n.ok("", "net-rollback", id, "timer")
	for _, want := range []string{"nmcli connection modify uuid " + nm2UUID + " connection.autoconnect yes", "ip link del b-br0", "nmcli --wait 0 connection up uuid " + nmPortUUID} {
		if !n.f.ran(want) {
			t.Errorf("rollback did not run %q", want)
		}
	}
	// Confirmed: the record has both cards, the bond and eth2's own profile.
	id = n.apply(req)
	n.ok("", "net-run", id)
	n.ok("", "net-confirm", id)
	rec := readFile(n.abs("var/lib/virticle-node/net/managed/br0.json"))
	for _, want := range []string{`"bond": "b-br0"`, `"bondMode": "failover"`, `"name": "eth2"`, `"uuid": "` + nm2UUID + `"`, `"origID": "eth1"`} {
		has(t, "record", rec, want)
	}
}

func TestNMForeignFirstChangeAddVLANAddress(t *testing.T) {
	br := strings.Replace(nmBrKeyfile, "stp=false", "stp=false\nvlan-filtering=true", 1)
	n := newNMForeign(t, br, nmPortKeyfile+"vlans=1 pvid untagged,10,20\n")
	n.f.link("br0").vlanFiltering = true
	req := nodeproto.NetRequest{Action: nodeproto.NetAddVLANAddr, Bridge: "br0", VLANID: 10, AddressMethod: nodeproto.AddrStatic, Address: "10.30.0.5/24"}
	p := n.plan(req)
	cmds := strings.Join(p.Commands, "\n")
	for _, want := range []string{
		"bridge.vlan-filtering yes bridge.vlan-default-pvid 1 bridge.vlans 10",
		"bridge-port.vlans '1 pvid untagged,10,20'",
		"nmcli connection add type vlan ifname br0.10 con-name virticle-vlan-br0.10 dev br0 id 10",
		"nmcli --wait 0 connection up id virticle-vlan-br0.10",
		"wait up to 20 s until br0.10 has 10.30.0.5/24",
	} {
		has(t, "add-vlan-address takeover", cmds, want)
	}
	has(t, "summary", strings.Join(p.Summary, " "), "The node gets the address 10.30.0.5/24 on VLAN 10 of br0")
	// A VLAN id the port does not allow yet is added to the list.
	req.VLANID, req.Address = 30, "10.31.0.5/24"
	has(t, "new id", strings.Join(n.plan(req).Commands, "\n"), "bridge-port.vlans '1 pvid untagged,10,20,30'")
	// Without VLAN filtering the address is refused, as on a switch virticle created.
	m := newNMForeign(t, nmBrKeyfile, nmPortKeyfile)
	m.refusedPlan("no filtering", req, "does not filter VLANs")
}

func TestNetplanForeignFirstChangeSetUplinks(t *testing.T) {
	n := newNPForeign(t, npBridgeYAML)
	f := n.f
	f.links = append(f.links, fLink{name: "eth2", mac: "52:54:00:aa:bb:ee", mtu: 1500})
	n.putAll("sys/class/net/eth2/device", "")
	req := nodeproto.NetRequest{Action: nodeproto.NetSetUplinks, Bridge: "br0", Uplinks: []string{"eth1", "eth2"}}
	p := n.plan(req)
	has(t, "bond netdev", fileOf(p, "/etc/systemd/network/05-virticle-b-br0.netdev"), "Kind=bond")
	has(t, "card", fileOf(p, "/etc/systemd/network/05-virticle-br0-eth2.network"), "Bond=b-br0")
	cmds := strings.Join(p.Commands, "\n")
	has(t, "commands", cmds, "wait up to 20 s for the interface b-br0")
	has(t, "commands", cmds, "networkctl reconfigure br0 b-br0 eth1 eth2")
	has(t, "yaml", fileOf(p, "/etc/netplan/60-bridge.yaml"), "      set-name: eth1\n      addresses:")
	has(t, "summary", strings.Join(p.Summary, " "), "A bond b-br0 is created")
}
