package nodecmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"virticle/internal/nodeproto"
)

// Bridges set up outside virticle: recognition (allow-lists) and the takeover plans.

func (n *netNode) putAll(rel, content string) {
	n.t.Helper()
	if err := os.MkdirAll(filepath.Dir(n.abs(rel)), 0o755); err != nil {
		n.t.Fatal(err)
	}
	if err := os.WriteFile(n.abs(rel), []byte(content), 0o600); err != nil {
		n.t.Fatal(err)
	}
}

func (n *netNode) info() nodeproto.NetInfo {
	n.t.Helper()
	var info nodeproto.NetInfo
	jsonInto(n.t, n.ok("", "net-info"), &info)
	return info
}

func (n *netNode) bridge(name string) nodeproto.NetBridge {
	n.t.Helper()
	for _, b := range n.info().Bridges {
		if b.Name == name {
			return b
		}
	}
	n.t.Fatalf("no bridge %s in net-info", name)
	return nodeproto.NetBridge{}
}

// wantRefused: the bridge is shown, not editable, with this short reason.
func (n *netNode) wantRefused(why, br, short string) {
	n.t.Helper()
	b := n.bridge(br)
	if b.Managed || b.NotEditable == "" || !strings.Contains(b.NotEditable, short) {
		n.t.Errorf("%s: managed %v, reason %q (%s), want %q", why, b.Managed, b.NotEditable, b.NotEditableDetail, short)
	}
}

// --- NetworkManager ---

const (
	nmBrUUID   = "bf338861-d936-4fda-870d-8ae2a321efaf"
	nmPortUUID = "4feb7a3f-408b-460e-81b0-bbcc5ba0c872"
	nmKF       = "etc/NetworkManager/system-connections/"
)

const nmBrKeyfile = `[connection]
id=br0
uuid=` + nmBrUUID + `
type=bridge
interface-name=br0

[ethernet]

[bridge]
stp=false

[ipv4]
address1=10.99.6.1/24
method=manual

[ipv6]
addr-gen-mode=default
method=disabled

[proxy]
`

const nmPortKeyfile = `[connection]
id=br0-port
uuid=` + nmPortUUID + `
type=ethernet
controller=br0
interface-name=eth1
port-type=bridge

[ethernet]

[bridge-port]
`

// newNMForeign is a NetworkManager node with a bridge br0 that an administrator made with nmcli on eth1.
func newNMForeign(t *testing.T, brKF, portKF string) *netNode {
	n := newNMNode(t, nmDHCPProfile, true, "1.56.0")
	f := n.f
	f.links = append(f.links, fLink{name: "br0", kind: "bridge", mtu: 1500, mac: "52:54:00:aa:bb:dd", addrs: []fAddr{{"10.99.6.1/24", false}}})
	f.link("eth1").master = "br0"
	n.putAll("sys/class/net/br0/brif/eth1", "")
	n.putAll(nmKF+"br0.nmconnection", brKF)
	n.putAll(nmKF+"br0-port.nmconnection", portKF)
	n.setNMList()
	f.outputs["nmcli -t -f ipv4,ipv6 connection show uuid "+nmBrUUID] = "ipv4.method:manual\nipv4.addresses:10.99.6.1/24\nipv4.gateway:\nipv4.never-default:no\nipv6.method:disabled\n"
	return n
}

// setNMList lists every keyfile below etc/NetworkManager/system-connections as an active profile.
func (n *netNode) setNMList() {
	ents, _ := os.ReadDir(n.abs(nmKF))
	var lines []string
	lines = append(lines, "a41601f3-3acc-5f60-ac5f-9d9011ab7c25:802-3-ethernet:yes:eth0:/etc/NetworkManager/system-connections/Wired connection 1.nmconnection:Wired connection 1")
	for _, e := range ents {
		b, _ := os.ReadFile(n.abs(nmKF + e.Name()))
		p := nmProfile{secs: parseINI(string(b))}
		uuid, _ := p.get("connection", "uuid")
		typ, _ := p.get("connection", "type")
		dev, _ := p.get("connection", "interface-name")
		id, _ := p.get("connection", "id")
		if typ == "ethernet" {
			typ = "802-3-ethernet"
		}
		lines = append(lines, uuid+":"+typ+":yes:"+dev+":"+strings.ReplaceAll("/"+nmKF+e.Name(), ":", `\:`)+":"+id)
	}
	n.f.outputs["nmcli -t -f UUID,TYPE,ACTIVE,DEVICE,FILENAME,NAME connection show"] = strings.Join(lines, "\n") + "\n"
}

func TestNMForeignRecognisedAndTakenOver(t *testing.T) {
	n := newNMForeign(t, nmBrKeyfile, nmPortKeyfile)
	b := n.bridge("br0")
	if !b.Managed || b.Uplink != "eth1" || !b.MovedAddresses || b.NotEditable != "" {
		t.Fatalf("br0 %+v", b)
	}
	p := n.plan(nodeproto.NetRequest{Action: nodeproto.NetSetMTU, Bridge: "br0", MTU: 1400})
	cmds := strings.Join(p.Commands, "\n")
	for _, want := range []string{
		"nmcli connection add type bridge ifname br0 con-name virticle-br0 connection.autoconnect yes bridge.stp no bridge.mac-address 52:54:00:aa:bb:dd 802-3-ethernet.mtu 1400 ipv4.addresses 10.99.6.1/24",
		"nmcli connection add type ethernet ifname eth1 con-name virticle-port-eth1 controller br0 port-type bridge",
		"nmcli connection add type ethernet ifname eth1 con-name eth1 connection.uuid ",
		"nmcli --wait 0 connection up id virticle-br0",
		"nmcli connection delete uuid " + nmBrUUID + " uuid " + nmPortUUID,
		"ip link set dev br0 mtu 1400",
	} {
		has(t, "takeover", cmds, want)
	}
	// The originals go after virticle's profiles are active.
	if strings.Index(cmds, "connection delete uuid") < strings.Index(cmds, "connection up id virticle-port-eth1") {
		t.Error("the original profiles are deleted before virticle's are active")
	}
	has(t, "summary", strings.Join(p.Summary, " "), `"br0" (/etc/NetworkManager/system-connections/br0.nmconnection)`)

	// Applied and not confirmed: the timer restores the keyfiles exactly and activates them again.
	id := n.apply(nodeproto.NetRequest{Action: nodeproto.NetSetMTU, Bridge: "br0", MTU: 1400})
	n.f.onRun = func(f *fakeNet, argv []string) {
		if strings.Join(argv, " ") == "nmcli connection delete uuid "+nmBrUUID+" uuid "+nmPortUUID {
			os.Remove(n.abs(nmKF + "br0.nmconnection"))
			os.Remove(n.abs(nmKF + "br0-port.nmconnection"))
		}
	}
	n.ok("", "net-run", id)
	if n.exists(nmKF + "br0.nmconnection") {
		t.Fatal("the fake did not delete the profile")
	}
	n.ok("", "net-rollback", id, "timer")
	if readFile(n.abs(nmKF+"br0.nmconnection")) != nmBrKeyfile || readFile(n.abs(nmKF+"br0-port.nmconnection")) != nmPortKeyfile {
		t.Error("the keyfiles are not restored exactly")
	}
	for _, want := range []string{"nmcli connection reload", "nmcli --wait 0 connection up uuid " + nmBrUUID, "nmcli connection delete id virticle-br0"} {
		if !n.f.ran(want) {
			t.Errorf("rollback did not run %q", want)
		}
	}
	if n.f.index("run nmcli --wait 0 connection up uuid "+nmBrUUID) > n.f.index("run nmcli connection delete id virticle-br0") {
		t.Error("virticle's bridge profile is deleted before the original is active again")
	}
}

// A running VM's tap on the bridge has a profile NetworkManager made in memory; the bridge stays editable.
func TestNMForeignIgnoresVMTapProfile(t *testing.T) {
	n := newNMForeign(t, nmBrKeyfile, nmPortKeyfile)
	const key = "nmcli -t -f UUID,TYPE,ACTIVE,DEVICE,FILENAME,NAME connection show"
	n.putAll("run/NetworkManager/system-connections/vnet7.nmconnection",
		"[connection]\nid=vnet7\nuuid=dddddddd-0000-4000-8000-000000000007\ntype=tun\ncontroller="+nmBrUUID+"\ninterface-name=vnet7\nport-type=bridge\n\n[tun]\nmode=2\n\n[bridge-port]\n")
	n.f.outputs[key] += "dddddddd-0000-4000-8000-000000000007:tun:yes:vnet7:/run/NetworkManager/system-connections/vnet7.nmconnection:vnet7\n"
	if b := n.bridge("br0"); !b.Managed || b.NotEditable != "" {
		t.Fatalf("br0 %+v", b)
	}
	// saved on disk it is someone's own port definition: not ignored
	n.f.outputs[key] = strings.Replace(n.f.outputs[key], "/run/NetworkManager/system-connections/vnet7", "/etc/NetworkManager/system-connections/vnet7", 1)
	n.putAll(nmKF+"vnet7.nmconnection",
		"[connection]\nid=vnet7\nuuid=dddddddd-0000-4000-8000-000000000007\ntype=tun\ncontroller="+nmBrUUID+"\ninterface-name=vnet7\nport-type=bridge\n\n[tun]\nmode=2\n\n[bridge-port]\n")
	if b := n.bridge("br0"); b.NotEditable != "Port profile vnet7 not supported" {
		t.Fatalf("saved tap profile: %+v", b)
	}
}

func TestNMForeignConfirmedIsManaged(t *testing.T) {
	n := newNMForeign(t, nmBrKeyfile, nmPortKeyfile)
	n.f.onRun = nil
	id := n.apply(nodeproto.NetRequest{Action: nodeproto.NetSetVLAN, Bridge: "br0", VLANAware: true, VLANs: "10,20"})
	n.ok("", "net-run", id)
	n.ok("", "net-confirm", id)
	b := readFile(n.abs("var/lib/virticle-node/net/managed/br0.json"))
	for _, want := range []string{`"vlanAware": true`, `"vlans": "10,20"`, `"origID": "eth1"`, `"origAutoconnect": "yes"`, `"moveAddresses": true`} {
		has(t, "record", b, want)
	}
}

func TestNMForeignDelete(t *testing.T) {
	n := newNMForeign(t, nmBrKeyfile, nmPortKeyfile)
	p := n.plan(nodeproto.NetRequest{Action: nodeproto.NetDelete, Bridge: "br0"})
	cmds := strings.Join(p.Commands, "\n")
	has(t, "delete", cmds, "nmcli connection add type ethernet ifname eth1 con-name eth1 connection.uuid ")
	has(t, "delete", cmds, "connection.autoconnect yes ipv4.addresses 10.99.6.1/24")
	has(t, "delete", cmds, "nmcli connection delete uuid "+nmBrUUID+" uuid "+nmPortUUID)
	has(t, "delete", cmds, "wait up to 40 s until eth1 has 10.99.6.1/24")
}

func TestNMForeignRefusals(t *testing.T) {
	cases := []struct {
		why, short string
		br, port   func(string) string
		edit       func(n *netNode)
	}{
		{why: "STP default", short: "STP is on", br: func(s string) string { return strings.Replace(s, "stp=false\n", "", 1) }},
		{why: "STP on", short: "STP is on", br: func(s string) string { return strings.Replace(s, "stp=false", "stp=true", 1) }},
		{why: "forward delay", short: "bridge.forward-delay", br: func(s string) string { return strings.Replace(s, "stp=false", "stp=false\nforward-delay=4", 1) }},
		{why: "multicast", short: "bridge.multicast-snooping", br: func(s string) string {
			return strings.Replace(s, "stp=false", "stp=false\nmulticast-snooping=false", 1)
		}},
		{why: "unknown section", short: "user", br: func(s string) string { return s + "\n[user]\norg.freedesktop.NetworkManager.origin=cloud-init\n" }},
		{why: "autoconnect off", short: "does not start by itself", br: func(s string) string {
			return strings.Replace(s, "interface-name=br0", "interface-name=br0\nautoconnect=false", 1)
		}},
		{why: "autoconnect priority", short: "connection.autoconnect-priority", br: func(s string) string {
			return strings.Replace(s, "interface-name=br0", "interface-name=br0\nautoconnect-priority=5", 1)
		}},
		{why: "default pvid", short: "Default VLAN 5", br: func(s string) string {
			return strings.Replace(s, "stp=false", "stp=false\nvlan-filtering=true\nvlan-default-pvid=5", 1)
		}, edit: func(n *netNode) { n.f.link("br0").vlanFiltering = true }},
		{why: "vlan filtering differs", short: "VLAN filtering differs", br: func(s string) string { return strings.Replace(s, "stp=false", "stp=false\nvlan-filtering=true", 1) }},
		{why: "port vlans untagged", short: "VLAN list not supported", br: func(s string) string { return strings.Replace(s, "stp=false", "stp=false\nvlan-filtering=true", 1) },
			port: func(s string) string { return s + "vlans=1 pvid untagged,10 untagged\n" }, edit: func(n *netNode) { n.f.link("br0").vlanFiltering = true }},
		{why: "port has ip settings", short: "ipv4", port: func(s string) string { return s + "\n[ipv4]\nmethod=auto\n" }},
		{why: "port priority", short: "bridge-port.priority", port: func(s string) string { return s + "priority=10\n" }},
		{why: "port of another type", short: "Profile br0-port not supported", port: func(s string) string { return strings.Replace(s, "port-type=bridge", "port-type=ovs-port", 1) }},
		{why: "mtu on one layer", short: "MTU set on some layers only", br: func(s string) string { return strings.Replace(s, "[ethernet]\n", "[ethernet]\nmtu=9000\n", 1) }},
		{why: "mac without addresses", short: "MAC address set", br: func(s string) string {
			s = strings.Replace(s, "stp=false", "stp=false\nmac-address=52:54:00:AA:BB:DD", 1)
			return strings.Replace(strings.Replace(s, "address1=10.99.6.1/24\nmethod=manual", "method=disabled", 1), "", "", 0)
		}, edit: func(n *netNode) {
			n.f.outputs["nmcli -t -f ipv4,ipv6 connection show uuid "+nmBrUUID] = "ipv4.method:disabled\nipv6.method:disabled\n"
			n.f.link("br0").addrs = nil
		}},
		{why: "two bridge profiles", short: "Two profiles for br0", edit: func(n *netNode) {
			n.putAll(nmKF+"br0-copy.nmconnection", strings.Replace(nmBrKeyfile, nmBrUUID, "aaaaaaaa-d936-4fda-870d-8ae2a321efaf", 1))
			n.setNMList()
		}},
		{why: "vlan on the bridge", short: "VLAN interface on br0", edit: func(n *netNode) {
			n.putAll(nmKF+"v10.nmconnection", "[connection]\nid=v10\nuuid=cccccccc-0000-4000-8000-000000000010\ntype=vlan\ninterface-name=br0.10\n\n[vlan]\nid=10\nparent=br0\n")
			n.setNMList()
		}},
		{why: "inactive port profile", short: "Port profile br0-port not supported", edit: func(n *netNode) {
			out := n.f.outputs["nmcli -t -f UUID,TYPE,ACTIVE,DEVICE,FILENAME,NAME connection show"]
			n.f.outputs["nmcli -t -f UUID,TYPE,ACTIVE,DEVICE,FILENAME,NAME connection show"] = strings.Replace(out, nmPortUUID+":802-3-ethernet:yes:eth1", nmPortUUID+":802-3-ethernet:no:", 1)
		}},
		{why: "in memory only", short: "No saved definition", edit: func(n *netNode) {
			out := n.f.outputs["nmcli -t -f UUID,TYPE,ACTIVE,DEVICE,FILENAME,NAME connection show"]
			n.f.outputs["nmcli -t -f UUID,TYPE,ACTIVE,DEVICE,FILENAME,NAME connection show"] = strings.Replace(out, "/etc/NetworkManager/system-connections/br0.nmconnection", "/run/NetworkManager/system-connections/br0.nmconnection", 1)
		}},
		{why: "ifcfg", short: "Defined in /etc/sysconfig/network-scripts/ifcfg-br0", edit: func(n *netNode) {
			out := n.f.outputs["nmcli -t -f UUID,TYPE,ACTIVE,DEVICE,FILENAME,NAME connection show"]
			n.f.outputs["nmcli -t -f UUID,TYPE,ACTIVE,DEVICE,FILENAME,NAME connection show"] = strings.Replace(out, "/etc/NetworkManager/system-connections/br0.nmconnection", "/etc/sysconfig/network-scripts/ifcfg-br0", 1)
		}},
		{why: "made with ip link", short: "No saved definition (made with ip link)", edit: func(n *netNode) {
			out := n.f.outputs["nmcli -t -f UUID,TYPE,ACTIVE,DEVICE,FILENAME,NAME connection show"]
			n.f.outputs["nmcli -t -f UUID,TYPE,ACTIVE,DEVICE,FILENAME,NAME connection show"] = strings.Replace(out, ":bridge:yes:br0:", ":bridge:no::", 1)
		}},
		{why: "tap port", short: "Port dummy0 is not a network card or bond", edit: func(n *netNode) {
			n.f.links = append(n.f.links, fLink{name: "dummy0", kind: "dummy", mtu: 1500, master: "br0"})
			os.Remove(n.abs("sys/class/net/br0/brif/eth1"))
			n.putAll("sys/class/net/br0/brif/dummy0", "")
		}},
		{why: "two cards", short: "Several ports without a bond", edit: func(n *netNode) {
			n.f.links = append(n.f.links, fLink{name: "eth2", mtu: 1500, master: "br0", mac: "52:54:00:aa:bb:ee"})
			n.putAll("sys/class/net/eth2/device", "")
			n.putAll("sys/class/net/br0/brif/eth2", "")
		}},
		{why: "container bridge", short: "Container bridge", edit: func(n *netNode) {
			n.f.links = append(n.f.links, fLink{name: "docker0", kind: "bridge", mtu: 1500})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.why, func(t *testing.T) {
			br, port := nmBrKeyfile, nmPortKeyfile
			if tc.br != nil {
				br = tc.br(br)
			}
			if tc.port != nil {
				port = tc.port(port)
			}
			n := newNMForeign(t, br, port)
			if tc.edit != nil {
				tc.edit(n)
			}
			name := "br0"
			if tc.why == "container bridge" {
				name = "docker0"
			}
			n.wantRefused(tc.why, name, tc.short)
			if name == "br0" {
				n.refusedPlan(tc.why, nodeproto.NetRequest{Action: nodeproto.NetSetMTU, Bridge: "br0", MTU: 1400}, "cannot be changed here")
			}
		})
	}
}

func TestNMBondForeign(t *testing.T) {
	n := newNMForeign(t, nmBrKeyfile, nmPortKeyfile)
	f := n.f
	os.Remove(n.abs(nmKF + "br0-port.nmconnection"))
	os.Remove(n.abs("sys/class/net/br0/brif/eth1"))
	n.putAll("sys/class/net/br0/brif/bond0", "")
	n.putAll("sys/class/net/eth2/device", "")
	f.links = append(f.links, fLink{name: "bond0", kind: "bond", mtu: 1500, master: "br0", mac: "52:54:00:aa:bb:dd"},
		fLink{name: "eth2", mtu: 1500, mac: "52:54:00:aa:bb:ee", master: "bond0", slaveKind: "bond"})
	f.link("eth1").master, f.link("eth1").slaveKind = "bond0", "bond"
	bond := "[connection]\nid=bond0\nuuid=dddddddd-0000-4000-8000-000000000001\ntype=bond\ncontroller=br0\ninterface-name=bond0\nport-type=bridge\n\n[bond]\nmiimon=100\nmode=active-backup\n\n[bridge-port]\n"
	member := func(card, u string) string {
		return "[connection]\nid=bond0-" + card + "\nuuid=" + u + "\ntype=ethernet\ncontroller=bond0\ninterface-name=" + card + "\nport-type=bond\n\n[ethernet]\n\n[bond-port]\n"
	}
	n.putAll(nmKF+"bond0.nmconnection", bond)
	n.putAll(nmKF+"bond0-eth1.nmconnection", member("eth1", "eeeeeeee-0000-4000-8000-000000000001"))
	n.putAll(nmKF+"bond0-eth2.nmconnection", member("eth2", "eeeeeeee-0000-4000-8000-000000000002"))
	n.setNMList()
	b := n.bridge("br0")
	if !b.Managed || b.Bond != "bond0" || b.BondMode != nodeproto.BondFailover || strings.Join(b.Uplinks, ",") != "eth1,eth2" {
		t.Fatalf("br0 %+v", b)
	}
	p := n.plan(nodeproto.NetRequest{Action: nodeproto.NetSetMTU, Bridge: "br0", MTU: 9000})
	cmds := strings.Join(p.Commands, "\n")
	has(t, "bond", cmds, "con-name virticle-bond-bond0 controller br0 port-type bridge connection.autoconnect yes bond.options 'mode=active-backup,miimon=100'")
	has(t, "bond", cmds, "con-name virticle-port-eth2 controller bond0 port-type bond")
	// LACP with slow LACPDUs is not one of the two modes.
	n.putAll(nmKF+"bond0.nmconnection", strings.Replace(bond, "mode=active-backup", "mode=802.3ad\nlacp_rate=slow\nxmit_hash_policy=layer2+3", 1))
	n.wantRefused("lacp slow", "br0", "Bond options not supported")
}

// --- netplan ---

const npBridgeYAML = `# Lab bridge for VMs
network:
  version: 2
  ethernets:
    eth1:
      match:
        macaddress: "52:54:00:aa:bb:dd"
      set-name: eth1
      dhcp4: false   # port only
  bridges:
    # the VM bridge
    br0:
      interfaces: [eth1]
      addresses:
        - 10.99.9.1/24   # lab address
      routes:
        - to: 10.98.0.0/16
          via: 10.99.9.254
      parameters:
        stp: false
    # an unrelated bridge, must survive
    br9:
      addresses: [10.97.0.1/24]
      parameters:
        stp: false
`

const npBridgeNetwork = "[Match]\nName=br0\n\n[Network]\nLinkLocalAddressing=ipv6\nAddress=10.99.9.1/24\nConfigureWithoutCarrier=yes\n\n[Route]\nDestination=10.98.0.0/16\nGateway=10.99.9.254\n"

// newNPForeign is a netplan node with br0 on eth1, as netplan generated it.
func newNPForeign(t *testing.T, yamlText string) *netNode {
	n := newNetNode(t, dhcpNetwork, true)
	f := n.f
	f.links = append(f.links, fLink{name: "br0", kind: "bridge", mtu: 1500, mac: "c2:a9:34:6d:a6:82", addrs: []fAddr{{"10.99.9.1/24", false}}})
	f.link("eth1").master = "br0"
	n.putAll("sys/class/net/br0/brif/eth1", "")
	n.putAll("etc/netplan/60-bridge.yaml", yamlText)
	n.putAll("run/systemd/network/10-netplan-br0.network", npBridgeNetwork)
	n.putAll("run/systemd/network/10-netplan-br0.netdev", "[NetDev]\nName=br0\nKind=bridge\n\n[Bridge]\nSTP=false\n")
	n.putAll("run/systemd/network/10-netplan-eth1.network", "[Match]\nName=eth1\n\n[Network]\nLinkLocalAddressing=no\nBridge=br0\n")
	return n
}

func TestNetplanForeignRecognisedAndEdited(t *testing.T) {
	n := newNPForeign(t, npBridgeYAML)
	b := n.bridge("br0")
	if !b.Managed || b.Uplink != "eth1" || !b.MovedAddresses {
		t.Fatalf("br0 %+v", b)
	}
	p := n.plan(nodeproto.NetRequest{Action: nodeproto.NetSetMTU, Bridge: "br0", MTU: 1400})
	y := fileOf(p, "/etc/netplan/60-bridge.yaml")
	want := `# Lab bridge for VMs
network:
  version: 2
  ethernets:
    eth1:
      match:
        macaddress: "52:54:00:aa:bb:dd"
      set-name: eth1
      addresses:
        - 10.99.9.1/24   # lab address
      routes:
        - to: 10.98.0.0/16
          via: 10.99.9.254
  bridges:
    # an unrelated bridge, must survive
    br9:
      addresses: [10.97.0.1/24]
      parameters:
        stp: false
`
	if y != want {
		t.Errorf("edited YAML:\n%s\nwant:\n%s", y, want)
	}
	has(t, "bridge network", fileOf(p, "/etc/systemd/network/05-virticle-br0.network"), "Address=10.99.9.1/24\nConfigureWithoutCarrier=yes\n\n[Route]\nDestination=10.98.0.0/16")
	has(t, "netdev", fileOf(p, "/etc/systemd/network/05-virticle-br0.netdev"), "MACAddress=c2:a9:34:6d:a6:82")
	cmds := strings.Join(p.Commands, "\n")
	has(t, "commands", cmds, "netplan generate\nnetworkctl reload")
	hasNot(t, "commands", cmds, "netplan apply")

	// A rollback puts the YAML back byte for byte and generates again.
	id := n.apply(nodeproto.NetRequest{Action: nodeproto.NetSetMTU, Bridge: "br0", MTU: 1400})
	n.ok("", "net-run", id)
	if readFile(n.abs("etc/netplan/60-bridge.yaml")) != want {
		t.Fatal("the change did not write the YAML")
	}
	n.ok("", "net-rollback", id, "timer")
	if readFile(n.abs("etc/netplan/60-bridge.yaml")) != npBridgeYAML || n.exists("etc/systemd/network/05-virticle-br0.netdev") {
		t.Error("the rollback did not restore the files")
	}
	if fi, _ := os.Stat(n.abs("etc/netplan/60-bridge.yaml")); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	if n.f.index("run netplan generate") < 0 {
		t.Error("no netplan generate")
	}
}

func TestNetplanForeignDelete(t *testing.T) {
	n := newNPForeign(t, npBridgeYAML)
	p := n.plan(nodeproto.NetRequest{Action: nodeproto.NetDelete, Bridge: "br0"})
	has(t, "yaml", fileOf(p, "/etc/netplan/60-bridge.yaml"), "      set-name: eth1\n      addresses:\n        - 10.99.9.1/24")
	cmds := strings.Join(p.Commands, "\n")
	has(t, "delete", cmds, "ip link del br0")
	has(t, "delete", cmds, "wait up to 40 s until eth1 has 10.99.9.1/24")
	if fileOf(p, "/etc/systemd/network/05-virticle-br0.netdev") != "<no such file in the plan>" {
		t.Error("a delete writes virticle's files")
	}
}

func TestNetplanForeignRefusals(t *testing.T) {
	cases := []struct {
		why, short string
		yaml       func(string) string
		edit       func(n *netNode)
	}{
		{why: "STP default", short: "STP is on", yaml: func(s string) string {
			return strings.Replace(s, "          via: 10.99.9.254\n      parameters:\n        stp: false\n", "          via: 10.99.9.254\n", 1)
		}},
		{why: "forward delay", short: "parameters.forward-delay", yaml: func(s string) string {
			return strings.Replace(s, "          via: 10.99.9.254\n      parameters:\n        stp: false\n", "          via: 10.99.9.254\n      parameters:\n        stp: false\n        forward-delay: 4\n", 1)
		}},
		{why: "unknown bridge key", short: "Setting wakeonlan", yaml: func(s string) string {
			return strings.Replace(s, "      interfaces: [eth1]\n", "      interfaces: [eth1]\n      wakeonlan: true\n", 1)
		}},
		{why: "address with lifetime", short: "Address form", yaml: func(s string) string {
			return strings.Replace(s, "        - 10.99.9.1/24   # lab address\n", "        - 10.99.9.1/24:\n            lifetime: 0\n", 1)
		}},
		{why: "card has addresses", short: "Port eth1 has addresses", yaml: func(s string) string {
			return strings.Replace(s, "      dhcp4: false   # port only\n", "      dhcp4: true\n", 1)
		}},
		{why: "card key not supported", short: "Setting wakeonlan", yaml: func(s string) string {
			return strings.Replace(s, "      dhcp4: false   # port only\n", "      wakeonlan: true\n", 1)
		}},
		{why: "vlan on the bridge", short: "VLAN interface br0.10 on br0", yaml: func(s string) string {
			return s + "  vlans:\n    br0.10:\n      id: 10\n      link: br0\n"
		}},
		{why: "vlan on the card", short: "VLAN interface v10 on eth1", yaml: func(s string) string {
			return s + "  vlans:\n    v10:\n      id: 10\n      link: eth1\n"
		}},
		{why: "renderer", short: "Renderer NetworkManager", yaml: func(s string) string {
			return strings.Replace(s, "  version: 2\n", "  version: 2\n  renderer: NetworkManager\n", 1)
		}},
		{why: "card in two files", short: "eth1 defined in two netplan files", edit: func(n *netNode) {
			n.putAll("etc/netplan/70-more.yaml", "network:\n  version: 2\n  ethernets:\n    eth1:\n      optional: true\n")
		}},
		{why: "file in /lib", short: "Defined in /lib/netplan/60-bridge.yaml", edit: func(n *netNode) {
			n.putAll("lib/netplan/60-bridge.yaml", readFile(n.abs("etc/netplan/60-bridge.yaml")))
			os.Remove(n.abs("etc/netplan/60-bridge.yaml"))
		}},
		{why: "cloud-init", short: "Written by cloud-init", edit: func(n *netNode) {
			n.putAll("etc/netplan/50-cloud-init.yaml", readFile(n.abs("etc/netplan/60-bridge.yaml")))
			os.Remove(n.abs("etc/netplan/60-bridge.yaml"))
			n.putAll("usr/bin/cloud-init", "")
		}},
		{why: "flow style", short: "netplan file layout not supported", yaml: func(s string) string {
			return "network: {version: 2, ethernets: {eth1: {dhcp4: false}}, bridges: {br0: {interfaces: [eth1], addresses: [10.99.9.1/24], parameters: {stp: false}}, br9: {parameters: {stp: false}}}}\n"
		}},
		{why: "not generated", short: "netplan not applied", edit: func(n *netNode) {
			os.Remove(n.abs("run/systemd/network/10-netplan-br0.network"))
		}},
		{why: "admin file in front", short: "netplan not applied", edit: func(n *netNode) {
			n.putAll("etc/systemd/network/01-br0.network", "[Match]\nName=br0\n\n[Network]\nAddress=10.0.0.1/24\n")
		}},
		{why: "generated netdev setting", short: "Bridge.Priority", edit: func(n *netNode) {
			n.putAll("run/systemd/network/10-netplan-br0.netdev", "[NetDev]\nName=br0\nKind=bridge\n\n[Bridge]\nSTP=false\nPriority=10\n")
		}},
		{why: "vlan filtering at run time", short: "VLAN filtering set outside netplan", edit: func(n *netNode) { n.f.link("br0").vlanFiltering = true }},
		{why: "ports differ", short: "Ports differ from netplan", yaml: func(s string) string {
			return strings.Replace(s, "      interfaces: [eth1]\n", "      interfaces: []\n", 1)
		}},
		{why: "made with ip link", short: "No saved definition (made with ip link)", edit: func(n *netNode) {
			n.putAll("etc/netplan/60-bridge.yaml", "network:\n  version: 2\n")
			os.Remove(n.abs("run/systemd/network/10-netplan-br0.network"))
			os.Remove(n.abs("run/systemd/network/10-netplan-eth1.network"))
		}},
		{why: "mtu on one layer", short: "MTU set on some layers only", yaml: func(s string) string {
			return strings.Replace(s, "      interfaces: [eth1]\n", "      interfaces: [eth1]\n      mtu: 9000\n", 1)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.why, func(t *testing.T) {
			y := npBridgeYAML
			if tc.yaml != nil {
				y = tc.yaml(y)
			}
			n := newNPForeign(t, y)
			if tc.edit != nil {
				tc.edit(n)
			}
			n.wantRefused(tc.why, "br0", tc.short)
			n.refusedPlan(tc.why, nodeproto.NetRequest{Action: nodeproto.NetSetMTU, Bridge: "br0", MTU: 1400}, "cannot be changed here")
		})
	}
}

func TestNetplanBondForeign(t *testing.T) {
	y := `network:
  version: 2
  ethernets:
    eth1: {}
    eth2: {}
  bonds:
    bond0:
      interfaces: [eth1, eth2]
      parameters:
        mode: active-backup
        mii-monitor-interval: 100
  bridges:
    br0:
      interfaces: [bond0]
      dhcp4: true
      parameters:
        stp: false
`
	n := newNPForeign(t, y)
	f := n.f
	os.Remove(n.abs("sys/class/net/br0/brif/eth1"))
	n.putAll("sys/class/net/br0/brif/bond0", "")
	n.putAll("sys/class/net/eth2/device", "")
	f.links = append(f.links, fLink{name: "bond0", kind: "bond", mtu: 1500, master: "br0"}, fLink{name: "eth2", mtu: 1500, master: "bond0", slaveKind: "bond"})
	f.link("eth1").master, f.link("eth1").slaveKind = "bond0", "bond"
	f.link("br0").addrs = []fAddr{{"10.99.9.1/24", true}}
	b := n.bridge("br0")
	if !b.Managed || b.Bond != "bond0" || b.BondMode != nodeproto.BondFailover {
		t.Fatalf("br0 %+v", b)
	}
	p := n.plan(nodeproto.NetRequest{Action: nodeproto.NetSetMTU, Bridge: "br0", MTU: 9000})
	want := "network:\n  version: 2\n  ethernets:\n    eth1:\n      dhcp4: true\n    eth2: {}\n"
	if got := fileOf(p, "/etc/netplan/60-bridge.yaml"); got != want {
		t.Errorf("edited YAML:\n%s\nwant:\n%s", got, want)
	}
	has(t, "bond netdev", fileOf(p, "/etc/systemd/network/05-virticle-bond0.netdev"), "Mode=active-backup")
	// Another mode or option refuses.
	n.putAll("etc/netplan/60-bridge.yaml", strings.Replace(y, "        mii-monitor-interval: 100\n", "        mii-monitor-interval: 100\n        primary: eth1\n", 1))
	n.wantRefused("bond primary", "br0", "Bond options not supported")
}

func TestNPApplyKeepsOtherText(t *testing.T) {
	text := "a:\n  b: 1   # one\n  # about c\n  c:\n    d: 2\n\n    e: 3\n  f: 4\n"
	lines := strings.Split(text, "\n")
	if e := blockEnd(lines, 3, 2); e != 7 {
		t.Errorf("block end %d", e)
	}
	out, err := npApply(text, []npRange{{Start: 3, End: -3, comments: true, build: func([]string) ([]string, error) { return nil, nil }}})
	if err != nil || out != "a:\n  b: 1   # one\n  f: 4\n" {
		t.Errorf("%q %v", out, err)
	}
}
