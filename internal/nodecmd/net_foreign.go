package nodecmd

import (
	"fmt"
	"regexp"
	"strings"

	"virticle/internal/nodeproto"
)

// Bridges that this feature did not create. Each one is read from the configuration system's saved
// definition and expressed as a bridgeSpec when every setting of the definition is one the model has (or is
// its default). Such a bridge is a virtual switch like one created here: the first change to it takes the
// definition over (net_nm_foreign.go, net_netplan.go), under the same rollback as every change, and once it
// is confirmed the bridge has a record in managed/ like any other.
//
// The check is an allow-list per backend: every key it understands is listed, any other key refuses. A
// wrong "yes" loses a setting of the administrator's or cuts the node off; a wrong "no" only leaves the
// bridge usable but not editable.

// foreignDef is what the recogniser found for one bridge.
type foreignDef struct {
	Spec *bridgeSpec // nil when the definition cannot be expressed
	// Short is the reason in a few words, Detail in a sentence (Spec == nil).
	Short, Detail string
	// Where names the saved definition for the review: profiles and files.
	Where []string

	// NetworkManager: the profiles of the definition (bridge first) and their keyfiles.
	nmProfiles []nmProfile
	// netplan: the files that hold the definition and what they become without it.
	npEdits []npEdit
}

func refuse(short, detail string, a ...any) *foreignDef {
	return &foreignDef{Short: short, Detail: fmt.Sprintf(detail, a...)}
}

// containerBridgeRE are names container engines give their bridges.
var containerBridgeRE = regexp.MustCompile(`^(docker[0-9]*|br-[0-9a-f]{12}|podman[0-9]*|cni[0-9]*|cni-podman[0-9]*|lxcbr[0-9]*|lxdbr[0-9]*|incusbr[0-9]*|cbr0|kube-bridge|flannel\..*|cilium_.*)$`)

// recognise reads the definition of a bridge that has no record in managed/.
func (c *Ctx) recognise(st stackInfo, snap *netSnap, br string) *foreignDef {
	l := snap.link(br)
	switch {
	case l == nil || l.Kind != "bridge":
		return refuse("Not a Linux bridge", "%s is not a Linux bridge.", br)
	case libvirtOwned(br):
		return refuse("Managed by libvirt", "libvirt creates and manages %s.", br)
	case containerBridgeRE.MatchString(br):
		return refuse("Container bridge", "%s looks like the bridge of a container engine, which manages it.", br)
	case !bridgeNameRE.MatchString(br):
		return refuse("Name not supported", "The name %s is not one virticle can manage (lower-case letters, digits, dash and underscore, starting with a letter).", br)
	}
	if fd := c.foreignPorts(snap, l); fd != nil {
		return fd
	}
	switch st.Name {
	case "networkmanager":
		return c.nmRecognise(snap, l)
	case "netplan-networkd":
		return c.npRecognise(snap, l)
	case "networkd":
		return c.ndForeign(l)
	case "ifupdown":
		return refuse("Defined in /etc/network/interfaces", "This node is configured with ifupdown, which virticle does not edit.")
	}
	return refuse("Network system not supported", "%s", st.Note)
}

// foreignPort is the bridge's port in the model: one card, or one bond of cards.
type foreignPort struct {
	Bond  string   // "" for a single card
	Cards []string // the physical cards, in name order
}

// foreignPorts checks the ports the kernel has now: virtual machine interfaces, and at most one card or bond
// of cards. It returns nil when they fit the model.
func (c *Ctx) foreignPorts(snap *netSnap, l *netLink) *foreignDef {
	_, fd := c.runtimePort(snap, l)
	return fd
}

func (c *Ctx) runtimePort(snap *netSnap, l *netLink) (*foreignPort, *foreignDef) {
	p := &foreignPort{}
	n := 0
	for _, name := range c.bridgePorts(l.Name) {
		if libvirtOwned(name) || strings.HasPrefix(name, "tap") {
			continue // a virtual machine
		}
		pl := snap.link(name)
		if pl == nil {
			return nil, refuse("Port "+name+" unknown", "The port %s of %s is not in the interface list.", name, l.Name)
		}
		n++
		if n > 1 {
			return nil, refuse("Several ports without a bond", "%s has more than one network card or bond as a port. A virtual switch has one: a card, or a bond of cards.", l.Name)
		}
		if len(pl.globalAddrs()) > 0 {
			return nil, refuse("Port "+name+" has an address", "The port %s of %s has an address of its own.", name, l.Name)
		}
		if pl.Kind == "bond" {
			p.Bond = name
			for _, m := range snap.Links {
				if m.Master != name {
					continue
				}
				if ok, _ := c.isPhysical(m); !ok {
					return nil, refuse("Port "+m.Name+" is not a network card", "The bond %s holds %s, which is not a physical network card.", name, m.Name)
				}
				p.Cards = append(p.Cards, m.Name)
			}
			continue
		}
		if ok, _ := c.isPhysical(*pl); !ok {
			return nil, refuse("Port "+name+" is not a network card or bond", "The port %s of %s is not a physical network card or a bond.", name, l.Name)
		}
		p.Cards = append(p.Cards, name)
	}
	return p, nil
}

// ndForeign: a bridge on a node with plain systemd-networkd (no netplan). Taking such files over is not
// done yet: the bridge is usable, not editable.
func (c *Ctx) ndForeign(l *netLink) *foreignDef {
	if f, _ := c.findNetworkFile(*l); f != nil {
		return refuse("Defined in "+f.Path, "%s is defined in the networkd file %s. virticle does not edit plain networkd files yet.", l.Name, f.Path)
	}
	return refuse("No saved definition (made with ip link)", "No networkd file defines %s, so it does not survive a restart.", l.Name)
}

// foreignSpec is the start of the spec of a recognised bridge: the cards, the bond and what the bridge has now.
func foreignSpec(in *netSnap, l *netLink, port *foreignPort, stack string) *bridgeSpec {
	s := &bridgeSpec{Bridge: l.Name, MAC: l.MAC, Stack: stack, Data: map[string]string{}, VLANAware: l.VLANFiltering}
	var cs []cardSpec
	for _, n := range port.Cards {
		cs = append(cs, cardSpec{Name: n, MTU: in.link(n).MTU})
	}
	s.setCards(cs)
	if port.Bond != "" {
		s.Bond = port.Bond
	}
	return s
}

// hasIPConfig: the bridge's own settings give it addresses (the card gets them when the switch is deleted).
func setMoved(s *bridgeSpec, l *netLink, has bool) {
	s.MoveAddresses = has && s.Uplink != ""
	if s.MoveAddresses {
		s.UplinkExpect = uplinkExpect(*l)
	}
}

// foreignWords is the first line of the review of a change that takes a definition over.
func foreignWords(br string) string {
	return fmt.Sprintf("%s was set up outside virticle. This change takes its saved definition over with the same settings; the original is saved first and comes back with a rollback.", br)
}

// foreignActionSummary adds what the change itself does to the bridge (after the takeover lines).
func foreignActionSummary(c *Ctx, pl *netPlanned, in *planInput) {
	f, n := in.Old, in.New
	switch in.Req.Action {
	case nodeproto.NetSetMTU:
		pl.Summary = append(pl.Summary, mtuSummary(n.MTUSet))
	case nodeproto.NetSetVLAN:
		pl.Summary = append(pl.Summary, vlanChangeSummary(f, n)...)
	case nodeproto.NetSetUplinks:
		describeUplinkChange(c, pl, in, diffCards(f, n))
	case nodeproto.NetAddVLANAddr:
		pl.Summary = append(pl.Summary, vlanAddrPlanSummary(f, n, *in.VLANAddr)...)
	}
}
