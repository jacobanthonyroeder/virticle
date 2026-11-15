package nodecmd

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"virticle/internal/nodeproto"
)

// netplan (with networkd): a bridge defined in a netplan YAML file.
//
// Recognition parses every netplan file and accepts a bridge only when its entry, the entry of its port (one
// card, or one bond and its cards) use nothing but the keys listed below, with values the model has. netplan
// merges entries of the same name from several files; an entry that is in more than one file refuses.
//
// Taking it over edits the YAML: the bridge's entry (and the bond's) is removed, and when the bridge has
// addresses its address settings move to the first card's entry, which is what the card gets back when the
// switch is deleted (the same as the uplink's own file of a switch that virticle created). Only those lines
// change; comments and every other entry stay as they are. virticle's networkd files (05-virticle-*) take the
// bridge's place and sort before netplan's generated files, so the card's moved settings stay unused while it
// belongs to the switch. `netplan generate` then rewrites /run/systemd/network; nothing is deleted (netplan
// apply would remove the bridge, as it is no longer in the YAML). The original files are saved with the change
// and a rollback puts them back and generates again. A later netplan apply, or netplan generate at boot, finds
// one definition of the bridge only: virticle's.
//
// cloud-init writes 50-cloud-init.yaml again at boot unless its network configuration is disabled; a bridge in
// that file is refused while cloud-init is installed and its network configuration is not disabled.

var netplanDirs = []string{"/lib/netplan", "/etc/netplan", "/run/netplan"}

const netplanEtc = "/etc/netplan"

type npFile struct {
	Path string
	Text string
	Mode uint32
	Doc  *yaml.Node // the document
	Net  *yaml.Node // the mapping under network:, nil when there is none
}

type npDef struct {
	File    *npFile
	Section *yaml.Node // the mapping of ethernets:, bonds:, bridges:
	SecKey  *yaml.Node // the key ethernets:
	Key     *yaml.Node
	Val     *yaml.Node
}

// npEdit is one netplan file as a change writes it.
type npEdit struct {
	Path    string
	Content string
	Mode    uint32
}

func mapGet(m *yaml.Node, key string) (*yaml.Node, *yaml.Node) {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i], m.Content[i+1]
		}
	}
	return nil, nil
}

// npLoad reads the netplan files the way netplan does: a file in /run hides one of the same name in /etc,
// which hides one in /lib; the files are taken in the order of their names.
func (c *Ctx) npLoad() ([]*npFile, error) {
	byName := map[string]string{}
	for _, d := range netplanDirs {
		m, _ := filepath.Glob(c.hostPath(d + "/*.yaml"))
		for _, p := range m {
			byName[filepath.Base(p)] = d + "/" + filepath.Base(p)
		}
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	var files []*npFile
	for _, n := range names {
		p := byName[n]
		b, err := os.ReadFile(c.hostPath(p))
		if err != nil {
			return nil, fmt.Errorf("%s: %v", p, err)
		}
		f := &npFile{Path: p, Text: string(b), Mode: 0o600}
		if fi, err := os.Stat(c.hostPath(p)); err == nil {
			f.Mode = uint32(fi.Mode().Perm())
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(b, &doc); err != nil {
			return nil, fmt.Errorf("%s: %v", p, err)
		}
		f.Doc = &doc
		if len(doc.Content) > 0 {
			_, f.Net = mapGet(doc.Content[0], "network")
		}
		files = append(files, f)
	}
	return files, nil
}

// npDefs are the entries of a name in a section (ethernets, bonds, bridges, vlans) across the files.
func npDefs(files []*npFile, section, name string) []npDef {
	var r []npDef
	for _, f := range files {
		sk, sec := mapGet(f.Net, section)
		if sec == nil || sec.Kind != yaml.MappingNode {
			continue
		}
		for i := 0; i+1 < len(sec.Content); i += 2 {
			if sec.Content[i].Value == name {
				r = append(r, npDef{File: f, Section: sec, SecKey: sk, Key: sec.Content[i], Val: sec.Content[i+1]})
			}
		}
	}
	return r
}

func npBool(n *yaml.Node) (bool, bool) {
	var b bool
	if n == nil || n.Kind != yaml.ScalarNode || n.Decode(&b) != nil {
		return false, false
	}
	return b, true
}

func npStrings(n *yaml.Node) ([]string, bool) {
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil, false
	}
	var r []string
	for _, x := range n.Content {
		if x.Kind != yaml.ScalarNode {
			return nil, false
		}
		r = append(r, x.Value)
	}
	return r, true
}

// The keys of a bridge entry. The address keys are common to every device type and move to the card.
var (
	npBridgeOwn  = map[string]bool{"interfaces": true, "parameters": true, "macaddress": true, "mtu": true, "renderer": true}
	npAddressKey = map[string]bool{"addresses": true, "dhcp4": true, "dhcp6": true, "routes": true, "nameservers": true,
		"gateway4": true, "gateway6": true, "link-local": true, "accept-ra": true, "optional": true, "critical": true,
		"dhcp4-overrides": true, "dhcp6-overrides": true, "ipv6-privacy": true, "dhcp-identifier": true}
	// a port: a card or a bond, without addresses of its own
	npPortOnly = map[string]bool{"dhcp4": true, "dhcp6": true, "link-local": true, "accept-ra": true}
	npCardKeys = map[string]bool{"match": true, "set-name": true, "optional": true, "mtu": true, "renderer": true}
	npBondKeys = map[string]bool{"interfaces": true, "parameters": true, "optional": true, "mtu": true, "renderer": true}
)

func npRefuse(d npDef, key string) *foreignDef {
	return refuse("Setting "+key+" not supported", "The netplan entry %s in %s has the setting %s, which virticle does not have.", d.Key.Value, d.File.Path, key)
}

// npPortOnlyOK: a port-only key with the value that means "no addresses".
func npPortOnlyOK(k string, v *yaml.Node) bool {
	switch k {
	case "dhcp4", "dhcp6", "accept-ra":
		b, ok := npBool(v)
		return ok && !b
	case "link-local":
		l, ok := npStrings(v)
		return ok && len(l) == 0
	}
	return false
}

func npRenderer(v *yaml.Node) bool { return v.Kind == yaml.ScalarNode && v.Value == "networkd" }

func npMTU(v *yaml.Node) (int, bool) {
	n, err := strconv.Atoi(v.Value)
	return n, v.Kind == yaml.ScalarNode && err == nil && n >= 1280 && n <= 9000
}

// npOne is the single editable entry of a name, or the refusal.
func npOne(files []*npFile, section, name string) (*npDef, *foreignDef) {
	defs := npDefs(files, section, name)
	switch {
	case len(defs) == 0:
		return nil, refuse("No netplan entry for "+name, "netplan has no %s entry %s.", strings.TrimSuffix(section, "s"), name)
	case len(defs) > 1:
		return nil, refuse(name+" defined in two netplan files", "netplan merges the entries for %s from %s and %s; virticle edits one entry in one file.", name, defs[0].File.Path, defs[1].File.Path)
	case path.Dir(defs[0].File.Path) != netplanEtc:
		return nil, refuse("Defined in "+defs[0].File.Path, "%s is defined in %s; virticle edits only files in %s.", name, defs[0].File.Path, netplanEtc)
	case defs[0].Val.Kind != yaml.MappingNode:
		return nil, refuse("Entry "+name+" not supported", "The netplan entry %s in %s is not a plain mapping.", name, defs[0].File.Path)
	}
	return &defs[0], nil
}

// cloudInitRewrites: cloud-init is installed and its network configuration is not disabled.
func (c *Ctx) cloudInitRewrites() bool {
	installed := false
	for _, p := range []string{"/usr/bin/cloud-init", "/usr/local/bin/cloud-init"} {
		if _, err := os.Stat(c.hostPath(p)); err == nil {
			installed = true
		}
	}
	if !installed {
		return false
	}
	if _, err := os.Stat(c.hostPath("/etc/cloud/cloud-init.disabled")); err == nil {
		return false
	}
	cfgs, _ := filepath.Glob(c.hostPath("/etc/cloud/cloud.cfg.d/*.cfg"))
	cfgs = append([]string{c.hostPath("/etc/cloud/cloud.cfg")}, cfgs...)
	off := false
	for _, p := range cfgs {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var x struct {
			Network struct {
				Config any `yaml:"config"`
			} `yaml:"network"`
		}
		if yaml.Unmarshal(b, &x) == nil && x.Network.Config != nil {
			off = fmt.Sprint(x.Network.Config) == "disabled"
		}
	}
	return !off
}

func (c *Ctx) npRecognise(snap *netSnap, l *netLink) *foreignDef {
	br := l.Name
	port, fd := c.runtimePort(snap, l)
	if fd != nil {
		return fd
	}
	files, err := c.npLoad()
	if err != nil {
		return refuse("netplan file not readable", "A netplan file cannot be read: %v.", err)
	}
	if len(npDefs(files, "bridges", br)) == 0 {
		return c.ndForeign(l)
	}
	bd, bad := npOne(files, "bridges", br)
	if bad != nil {
		return bad
	}
	if _, r := mapGet(bd.File.Net, "renderer"); r != nil && !npRenderer(r) {
		return refuse("Renderer "+r.Value, "%s sets the renderer %s.", bd.File.Path, r.Value)
	}
	if l.VLANFiltering {
		return refuse("VLAN filtering set outside netplan", "%s filters VLANs, which its netplan entry cannot say.", br)
	}
	s := foreignSpec(snap, l, port, "netplan-networkd")
	fd = &foreignDef{Spec: s}
	mtus := map[string]int{br: 0}
	var ifaces []string
	stp := true // netplan's default
	has := false
	var moved []*yaml.Node // key nodes of the bridge's address settings
	for i := 0; i+1 < len(bd.Val.Content); i += 2 {
		k, v := bd.Val.Content[i], bd.Val.Content[i+1]
		switch {
		case k.Value == "interfaces":
			var ok bool
			if ifaces, ok = npStrings(v); !ok {
				return npRefuse(*bd, "interfaces")
			}
		case k.Value == "parameters":
			if v.Kind != yaml.MappingNode {
				return npRefuse(*bd, "parameters")
			}
			for j := 0; j+1 < len(v.Content); j += 2 {
				if v.Content[j].Value != "stp" {
					return npRefuse(*bd, "parameters."+v.Content[j].Value)
				}
				b, ok := npBool(v.Content[j+1])
				if !ok {
					return npRefuse(*bd, "parameters.stp")
				}
				stp = b
			}
		case k.Value == "macaddress":
			if v.Kind != yaml.ScalarNode || !strings.EqualFold(v.Value, l.MAC) {
				return refuse("MAC address set", "The netplan entry %s sets the MAC address %s, which the bridge does not have now.", br, v.Value)
			}
			s.Data["macSet"] = "yes"
		case k.Value == "mtu":
			var ok bool
			if mtus[br], ok = npMTU(v); !ok {
				return npRefuse(*bd, "mtu")
			}
		case k.Value == "renderer":
			if !npRenderer(v) {
				return npRefuse(*bd, "renderer")
			}
		case npAddressKey[k.Value]:
			moved = append(moved, k)
			switch k.Value {
			case "addresses":
				a, ok := npStrings(v)
				if !ok {
					return refuse("Address form not supported", "The netplan entry %s lists an address with options (lifetime or label).", br)
				}
				has = has || len(a) > 0
			case "dhcp4", "dhcp6":
				b, _ := npBool(v)
				has = has || b
			}
		default:
			return npRefuse(*bd, k.Value)
		}
	}
	if stp {
		return refuse("STP is on", "The netplan entry %s in %s has STP on (netplan's default when parameters.stp is not false); virticle's switches have it off.", br, bd.File.Path)
	}
	if len(ifaces) > 1 {
		return refuse("Several ports without a bond", "The netplan entry %s lists %s; a virtual switch has one port, a card or a bond of cards.", br, strings.Join(ifaces, ", "))
	}
	// The YAML must name the ports the kernel has.
	want := append([]string(nil), port.Cards...)
	if port.Bond != "" {
		want = []string{port.Bond}
	}
	if strings.Join(ifaces, ",") != strings.Join(want, ",") {
		return refuse("Ports differ from netplan", "%s has the ports %s now, and its netplan entry lists %s.", br, orDash(want), orDash(ifaces))
	}
	edits := map[*npFile][]npRange{}
	cardDef := func(name string) (*npDef, *foreignDef) {
		d, bad := npOne(files, "ethernets", name)
		if bad != nil {
			return nil, bad
		}
		for i := 0; i+1 < len(d.Val.Content); i += 2 {
			k, v := d.Val.Content[i], d.Val.Content[i+1]
			switch {
			case npPortOnly[k.Value]:
				if !npPortOnlyOK(k.Value, v) {
					return nil, refuse("Port "+name+" has addresses", "The netplan entry of the port %s gives it addresses or address settings (%s).", name, k.Value)
				}
			case !npCardKeys[k.Value]:
				return nil, npRefuse(*d, k.Value)
			case k.Value == "match":
				if v.Kind != yaml.MappingNode {
					return nil, npRefuse(*d, "match")
				}
				for j := 0; j+1 < len(v.Content); j += 2 {
					mk, mv := v.Content[j].Value, v.Content[j+1]
					ok := mv.Kind == yaml.ScalarNode && (mk == "macaddress" && strings.EqualFold(mv.Value, snap.link(name).MAC) || mk == "name" && mv.Value == name)
					if !ok {
						return nil, npRefuse(*d, "match."+mk)
					}
				}
			case k.Value == "set-name":
				if v.Value != name {
					return nil, npRefuse(*d, "set-name")
				}
			case k.Value == "mtu":
				var ok bool
				if mtus[name], ok = npMTU(v); !ok {
					return nil, npRefuse(*d, "mtu")
				}
			case k.Value == "renderer":
				if !npRenderer(v) {
					return nil, npRefuse(*d, "renderer")
				}
			}
		}
		if _, ok := mtus[name]; !ok {
			mtus[name] = 0
		}
		return d, nil
	}
	var cards []*npDef
	if port.Bond != "" {
		bo, bad := npOne(files, "bonds", port.Bond)
		if bad != nil {
			return bad
		}
		mtus[port.Bond] = 0
		params := map[string]string{}
		var members []string
		for i := 0; i+1 < len(bo.Val.Content); i += 2 {
			k, v := bo.Val.Content[i], bo.Val.Content[i+1]
			switch {
			case npPortOnly[k.Value]:
				if !npPortOnlyOK(k.Value, v) {
					return refuse("Bond "+port.Bond+" has addresses", "The netplan entry of the bond %s gives it address settings (%s).", port.Bond, k.Value)
				}
			case !npBondKeys[k.Value]:
				return npRefuse(*bo, k.Value)
			case k.Value == "interfaces":
				var ok bool
				if members, ok = npStrings(v); !ok {
					return npRefuse(*bo, "interfaces")
				}
			case k.Value == "parameters":
				if v.Kind != yaml.MappingNode {
					return npRefuse(*bo, "parameters")
				}
				for j := 0; j+1 < len(v.Content); j += 2 {
					params[v.Content[j].Value] = v.Content[j+1].Value
				}
			case k.Value == "mtu":
				var ok bool
				if mtus[port.Bond], ok = npMTU(v); !ok {
					return npRefuse(*bo, "mtu")
				}
			case k.Value == "renderer":
				if !npRenderer(v) {
					return npRefuse(*bo, "renderer")
				}
			}
		}
		mode, bad := npBondMode(params, port.Bond)
		if bad != nil {
			return bad
		}
		s.BondMode = mode
		sorted := append([]string(nil), members...)
		sort.Strings(sorted)
		if strings.Join(sorted, ",") != strings.Join(port.Cards, ",") {
			return refuse("Bond members differ from netplan", "The bond %s has the cards %s now, and its netplan entry lists %s.", port.Bond, orDash(port.Cards), orDash(members))
		}
		// The first card in the YAML's order is the switch's first card.
		var cs []cardSpec
		for _, m := range members {
			cs = append(cs, cardSpec{Name: m, MTU: snap.link(m).MTU})
		}
		s.setCards(cs)
		edits[bo.File] = append(edits[bo.File], npRemoveEntry(bo))
		fd.Where = append(fd.Where, fmt.Sprintf("netplan bond %s in %s", port.Bond, bo.File.Path))
	}
	for _, n := range s.cardNames() {
		d, bad := cardDef(n)
		if bad != nil {
			return bad
		}
		cards = append(cards, d)
	}
	// A VLAN on the bridge, the bond or a card is not part of the model.
	for _, f := range files {
		_, vl := mapGet(f.Net, "vlans")
		if vl == nil || vl.Kind != yaml.MappingNode {
			continue
		}
		for i := 0; i+1 < len(vl.Content); i += 2 {
			_, link := mapGet(vl.Content[i+1], "link")
			if link != nil && (link.Value == br || link.Value == port.Bond || containsStr(port.Cards, link.Value)) {
				return refuse("VLAN interface "+vl.Content[i].Value+" on "+link.Value, "The netplan VLAN %s in %s is on %s; virticle does not take those over.", vl.Content[i].Value, f.Path, link.Value)
			}
		}
	}
	mtu, bad := sameMTU(mtus)
	if bad != nil {
		return bad
	}
	s.MTUSet = mtu
	setMoved(s, l, has)
	if s.Data["macSet"] != "" && !s.MoveAddresses {
		return refuse("MAC address set", "The netplan entry %s sets a MAC address but no addresses; virticle sets the MAC address only on a bridge with addresses.", br)
	}
	delete(s.Data, "macSet")
	// What networkd uses now: netplan's generated files, no file of the administrator's in front of them.
	gen, err := c.findNetworkFile(*l)
	want1 := "/run/systemd/network/10-netplan-" + br + ".network"
	if err != nil || gen == nil || gen.Path != want1 {
		return refuse("netplan not applied", "networkd does not use netplan's file %s for %s (netplan generate has not run, or another file comes first).", want1, br)
	}
	if b, err := os.ReadFile(c.hostPath("/run/systemd/network/10-netplan-" + br + ".netdev")); err != nil {
		return refuse("netplan not applied", "netplan has not generated %s.", "/run/systemd/network/10-netplan-"+br+".netdev")
	} else if k := ndNetdevAllowed(string(b)); k != "" {
		return refuse("Setting "+k+" not supported", "netplan generates the networkd setting %s for %s, which virticle does not have.", k, br)
	}
	text, _, _ := movedConfig(parseINI(gen.Content))
	s.Moved = text
	for _, cd := range cards {
		if bad := c.ndForeignOrder(s, cd.Key.Value); bad != nil {
			return bad
		}
	}
	// The files as a change writes them: without the bridge (and the bond), with the address settings on the first card.
	edits[bd.File] = append(edits[bd.File], npRemoveEntry(bd))
	if s.MoveAddresses {
		edits[cards[0].File] = append(edits[cards[0].File], npMoveTo(cards[0], bd, moved))
	}
	for f, rs := range edits {
		if filepath.Base(f.Path) == "50-cloud-init.yaml" && c.cloudInitRewrites() {
			return refuse("Written by cloud-init", "%s is in %s, which cloud-init writes again at boot. Disable cloud-init's network configuration (network: {config: disabled}) to edit it here.", br, f.Path)
		}
		out, err := npApply(f.Text, rs)
		if err != nil {
			return refuse("netplan file layout not supported", "%s cannot be edited safely: %v.", f.Path, err)
		}
		fd.npEdits = append(fd.npEdits, npEdit{Path: f.Path, Content: out, Mode: f.Mode})
	}
	sort.Slice(fd.npEdits, func(i, j int) bool { return fd.npEdits[i].Path < fd.npEdits[j].Path })
	if bad := npCheckEdits(fd.npEdits, br, port.Bond); bad != nil {
		return bad
	}
	fd.Where = append([]string{fmt.Sprintf("netplan bridge %s in %s", br, bd.File.Path)}, fd.Where...)
	return fd
}

func orDash(l []string) string {
	if len(l) == 0 {
		return "none"
	}
	return strings.Join(l, ", ")
}

func npBondMode(p map[string]string, bond string) (string, *foreignDef) {
	mii := p["mii-monitor-interval"]
	miiOK := mii == "100" || mii == "100ms"
	switch {
	case p["mode"] == "active-backup" && miiOK && len(p) == 2:
		return nodeproto.BondFailover, nil
	case p["mode"] == "802.3ad" && miiOK && p["lacp-rate"] == "fast" && p["transmit-hash-policy"] == "layer2+3" && len(p) == 4:
		return nodeproto.BondLACP, nil
	}
	var w []string
	for k, v := range p {
		w = append(w, k+": "+v)
	}
	sort.Strings(w)
	return "", refuse("Bond options not supported", "The bond %s has the parameters %q, which are not one of virticle's two modes (active-backup with mii-monitor-interval 100, or 802.3ad with lacp-rate fast, transmit-hash-policy layer2+3 and mii-monitor-interval 100).", bond, strings.Join(w, ", "))
}

// ndNetdevAllowed finds a setting of a generated bridge .netdev beyond what virticle writes ("" when there is none).
func ndNetdevAllowed(text string) string {
	for _, s := range parseINI(text) {
		for _, l := range s.Lines {
			k, v, _ := strings.Cut(l, "=")
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			switch {
			case s.Name == "NetDev" && (k == "Name" || k == "Kind" && v == "bridge" || k == "MACAddress" || k == "MTUBytes"):
			case s.Name == "Bridge" && k == "STP" && (v == "false" || v == "no" || v == "0"):
			default:
				return s.Name + "." + k
			}
		}
	}
	return ""
}

// ndForeignOrder: no networkd file of the administrator's that sorts before virticle's file matches the card.
func (c *Ctx) ndForeignOrder(s *bridgeSpec, card string) *foreignDef {
	l := netLink{Name: card}
	f, err := c.findNetworkFile(l)
	if err != nil {
		return refuse("networkd match not supported", "It cannot be told which networkd file configures %s: %v.", card, err)
	}
	if f != nil && path.Base(f.Path) <= path.Base(ndCardPath(s, card)) {
		return refuse("Defined in "+f.Path, "%s configures %s and sorts before the file virticle would write.", f.Path, card)
	}
	return nil
}

// --- editing the YAML text ---

// npRange replaces the lines [Start, End) of a file with Lines.
type npRange struct {
	// Start is the first line (0-based); End the line after the last, or minus the key's column: the end of its block.
	Start, End int
	// build makes the new lines from the file's lines.
	build func(lines []string) ([]string, error)
	// comments: the comment lines right above the key, at its indentation, go with it.
	comments bool
}

func indentOf(l string) int { return len(l) - len(strings.TrimLeft(l, " ")) }

// blockEnd is the end of the block of a key that starts at line start (0-based) with the given indentation:
// the lines after it that are blank or indented deeper. Trailing blank lines stay outside.
func blockEnd(lines []string, start, ind int) int {
	end := start + 1
	for j := start + 1; j < len(lines); j++ {
		t := strings.TrimSpace(lines[j])
		if t == "" {
			continue
		}
		if indentOf(lines[j]) <= ind {
			break
		}
		end = j + 1
	}
	return end
}

// npRemoveEntry removes an entry; when it is the only one of its section, the section goes too.
func npRemoveEntry(d *npDef) npRange {
	k := d.Key
	if len(d.Section.Content) == 2 && d.SecKey != nil {
		k = d.SecKey
	}
	flow := d.Section.Style&yaml.FlowStyle != 0 && k == d.Key
	// End: minus the column of the key; npApply finds the end of its block.
	return npRange{Start: k.Line - 1, End: -k.Column, comments: true, build: func([]string) ([]string, error) {
		if flow {
			return nil, fmt.Errorf("the entry %s is in a flow mapping", k.Value)
		}
		return nil, nil
	}}
}

// npMoveTo rewrites the card's entry: its own keys stay (match, set-name, optional, mtu), the port-only keys go
// (dhcp4: false and the like), the bridge's address settings are added.
func npMoveTo(card, bridge *npDef, moved []*yaml.Node) npRange {
	k := card.Key
	r := npRange{Start: k.Line - 1, End: -(k.Column)}
	r.build = func(lines []string) ([]string, error) {
		if card.Section.Style&yaml.FlowStyle != 0 || bridge.Val.Style&yaml.FlowStyle != 0 {
			return nil, fmt.Errorf("the entries of %s and %s must be in block style", card.Key.Value, bridge.Key.Value)
		}
		ind := k.Column - 1
		childInd := ind + 2
		var out []string
		var keep []*yaml.Node
		if card.Val.Style&yaml.FlowStyle == 0 && len(card.Val.Content) > 0 {
			childInd = card.Val.Content[0].Column - 1
			out = append(out, lines[k.Line-1])
		} else {
			out = append(out, strings.Repeat(" ", ind)+k.Value+":")
		}
		for i := 0; i+1 < len(card.Val.Content); i += 2 {
			if !npPortOnly[card.Val.Content[i].Value] {
				keep = append(keep, card.Val.Content[i], card.Val.Content[i+1])
			}
		}
		for i := 0; i+1 < len(keep); i += 2 {
			ls, err := npKeyText(lines, card.Val, keep[i], keep[i+1], childInd)
			if err != nil {
				return nil, err
			}
			out = append(out, ls...)
		}
		for _, mk := range moved {
			_, mv := mapGet(bridge.Val, mk.Value)
			ls, err := npKeyText(lines, bridge.Val, mk, mv, childInd)
			if err != nil {
				return nil, err
			}
			out = append(out, ls...)
		}
		return out, nil
	}
	return r
}

// npKeyText is the text of one key of a block mapping, moved to the indentation ind.
func npKeyText(lines []string, parent, k, v *yaml.Node, ind int) ([]string, error) {
	if parent.Style&yaml.FlowStyle != 0 {
		m := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{k, v}}
		b, err := yaml.Marshal(m)
		if err != nil {
			return nil, err
		}
		var out []string
		for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			out = append(out, strings.Repeat(" ", ind)+l)
		}
		return out, nil
	}
	from := k.Column - 1
	end := blockEnd(lines, k.Line-1, from)
	var out []string
	for _, l := range lines[k.Line-1 : end] {
		if strings.TrimSpace(l) == "" {
			out = append(out, "")
			continue
		}
		if indentOf(l) < from {
			return nil, fmt.Errorf("unexpected indentation at %q", l)
		}
		out = append(out, strings.Repeat(" ", ind)+l[from:])
	}
	return out, nil
}

// npApply applies the ranges to the text.
func npApply(text string, rs []npRange) (string, error) {
	lines := strings.Split(text, "\n")
	type done struct {
		start, end int
		lines      []string
	}
	var ds []done
	for _, r := range rs {
		start, end := r.Start, r.End
		if end < 0 {
			end = blockEnd(lines, start, -end-1)
			for r.comments && start > 0 && strings.HasPrefix(strings.TrimSpace(lines[start-1]), "#") && indentOf(lines[start-1]) == -r.End-1 {
				start--
			}
		}
		var ls []string
		if r.build != nil {
			var err error
			if ls, err = r.build(lines); err != nil {
				return "", err
			}
		}
		ds = append(ds, done{start, end, ls})
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i].start > ds[j].start })
	for i := 1; i < len(ds); i++ {
		if ds[i].end > ds[i-1].start {
			return "", fmt.Errorf("the entries overlap")
		}
	}
	for _, d := range ds {
		lines = append(lines[:d.start], append(append([]string(nil), d.lines...), lines[d.end:]...)...)
	}
	return strings.Join(lines, "\n"), nil
}

// npCheckEdits parses the edited files again: valid YAML without the bridge and the bond.
func npCheckEdits(edits []npEdit, br, bond string) *foreignDef {
	var files []*npFile
	for _, e := range edits {
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(e.Content), &doc); err != nil {
			return refuse("netplan file layout not supported", "Editing %s would not give valid YAML: %v.", e.Path, err)
		}
		f := &npFile{Path: e.Path, Doc: &doc}
		if len(doc.Content) > 0 {
			_, f.Net = mapGet(doc.Content[0], "network")
		}
		if f.Net == nil {
			return refuse("netplan file layout not supported", "Editing %s would leave no network: section.", e.Path)
		}
		files = append(files, f)
	}
	if len(npDefs(files, "bridges", br)) > 0 || bond != "" && len(npDefs(files, "bonds", bond)) > 0 {
		return refuse("netplan file layout not supported", "The entry of %s could not be removed cleanly.", br)
	}
	return nil
}

// --- the change that takes the definition over ---

func npWrites(fd *foreignDef) []netStep {
	var steps []netStep
	for _, e := range fd.npEdits {
		steps = append(steps, netStep{Kind: "write", Path: e.Path, Content: e.Content, Mode: e.Mode})
	}
	return steps
}

func npFileNames(fd *foreignDef) string {
	var r []string
	for _, e := range fd.npEdits {
		r = append(r, e.Path)
	}
	return strings.Join(r, ", ")
}

func npEditWords(fd *foreignDef, s *bridgeSpec, del bool) []string {
	w := []string{fmt.Sprintf("The netplan entry of %s is removed from %s; comments and the other entries stay.", s.Bridge, npFileNames(fd))}
	if s.Bond != "" {
		w[0] = fmt.Sprintf("The netplan entries of %s and the bond %s are removed from %s; comments and the other entries stay.", s.Bridge, s.Bond, npFileNames(fd))
	}
	switch {
	case s.MoveAddresses && del:
		w = append(w, fmt.Sprintf("The address settings of %s move to the entry of %s (%s).", s.Bridge, s.Uplink, strings.Join(s.UplinkExpect, ", ")))
	case s.MoveAddresses:
		w = append(w, fmt.Sprintf("The address settings of %s move to the entry of %s; it gets them back when the switch is deleted.", s.Bridge, s.Uplink))
	}
	return w
}

func npTakeover(c *Ctx, in *planInput) (*netPlanned, error) {
	fd, f, n := in.Foreign, in.Old, in.New
	pl := &netPlanned{Marker: "put", Spec: n}
	pl.Saves = []netSave{{"networkctl-list.txt", []string{"networkctl", "list", "--no-pager"}}}
	pl.Summary = append([]string{foreignWords(f.Bridge)}, npEditWords(fd, f, false)...)
	pl.Summary = append(pl.Summary, fmt.Sprintf("virticle's networkd files for %s take the place of the entry; netplan generate runs again.", n.Bridge))
	for _, x := range in.Added {
		if _, err := ndCheckOrder(c, n, x.Link, len(x.Link.globalAddrs()) > 0); err != nil {
			return nil, err
		}
	}
	d := diffCards(f, n)
	steps := npWrites(fd)
	steps = append(steps, ndWrites(n)...)
	steps = append(steps, stepRun("netplan", "generate"), stepRun("networkctl", "reload"),
		netStep{Kind: "wait-link", Path: n.Bridge, Secs: 20})
	if n.Bond != "" && n.Bond != f.Bond {
		steps = append(steps, netStep{Kind: "wait-link", Path: n.Bond, Secs: 20})
		for _, x := range f.cards() {
			steps = append(steps, stepRunIgnore("ip", "link", "set", x.Name, "nomaster"))
		}
	}
	steps = append(steps, vlanCommands(f, n)...)
	for _, x := range d.Removed {
		steps = append(steps, stepRunIgnore("ip", "link", "set", x.Name, "nomaster"),
			netStep{Kind: "run", Argv: []string{"networkctl", "reconfigure", x.Name}, Secs: 15, Ignore: true})
	}
	re := []string{"networkctl", "reconfigure", n.Bridge}
	if n.Bond != "" {
		re = append(re, n.Bond)
	}
	re = append(re, n.cardNames()...)
	steps = append(steps, netStep{Kind: "run", Argv: re, Secs: 15})
	for _, x := range d.Added {
		steps = append(steps, stepRunIgnore("ip", "addr", "flush", "dev", x.Name))
	}
	layers := mtuLayers(n)
	cur := mtuValues(in.Snap, layers)
	if n.MTUSet > 0 && n.MTUSet != f.MTUSet {
		steps = append(steps, mtuMoves(layers, cur, mtuTarget(layers, n.MTUSet))...)
	}
	for _, v := range n.VLANAddrs {
		steps = append(steps, ndVLANUp(n, v, false)...)
		if v.Method == nodeproto.AddrStatic {
			steps = append(steps, netStep{Kind: "wait-addr", Path: v.iface(n), Argv: []string{v.Address}, Secs: 20})
		}
	}
	exp := uplinkExpect(*in.Snap.link(f.Bridge))
	if len(exp) > 0 {
		steps = append(steps, netStep{Kind: "wait-addr", Path: n.Bridge, Argv: exp, Secs: 40})
	}
	foreignActionSummary(c, pl, in)
	pl.Steps = steps

	restore := []netStep{stepRunIgnore("netplan", "generate"), stepRunIgnore("networkctl", "reload")}
	for _, v := range n.VLANAddrs {
		restore = append(restore, ndVLANDown(n, v, true)...)
	}
	restore = append(restore, ignoreAll(vlanCommands(n, f))...)
	if n.Bond != "" && n.Bond != f.Bond {
		for _, x := range n.cards() {
			restore = append(restore, stepRunIgnore("ip", "link", "set", x.Name, "nomaster"))
		}
		restore = append(restore, stepRunIgnore("ip", "link", "del", n.Bond))
	}
	for _, x := range d.Added {
		restore = append(restore, stepRunIgnore("ip", "link", "set", x.Name, "nomaster"))
		if x.MTU > 0 && n.MTUSet > 0 {
			restore = append(restore, stepRunIgnore("ip", "link", "set", "dev", x.Name, "mtu", fmt.Sprint(x.MTU)))
		}
		restore = append(restore, netStep{Kind: "run", Argv: []string{"networkctl", "reconfigure", x.Name}, Secs: 15, Ignore: true})
	}
	if n.MTUSet > 0 && n.MTUSet != f.MTUSet {
		restore = append(restore, ignoreAll(mtuMoves(layers, mtuTarget(layers, n.MTUSet), cur))...)
	}
	re = []string{"networkctl", "reconfigure", f.Bridge}
	if f.Bond != "" {
		re = append(re, f.Bond)
	}
	re = append(re, f.cardNames()...)
	restore = append(restore, netStep{Kind: "run", Argv: re, Secs: 15, Ignore: true})
	if len(exp) > 0 {
		restore = append(restore, netStep{Kind: "wait-addr", Path: f.Bridge, Argv: exp, Secs: 40, Ignore: true})
	}
	pl.Restore = restore
	return pl, nil
}

func npForeignDeletePlan(in *planInput) *netPlanned {
	fd, f := in.Foreign, in.Old
	pl := &netPlanned{Marker: "remove", Spec: f}
	pl.Saves = []netSave{{"networkctl-list.txt", []string{"networkctl", "list", "--no-pager"}}}
	pl.Summary = append([]string{fmt.Sprintf("Delete the Linux bridge %s, which was set up outside virticle (%s). The files are saved first and come back with a rollback.", f.Bridge, strings.Join(fd.Where, "; "))}, npEditWords(fd, f, true)...)
	steps := npWrites(fd)
	steps = append(steps, stepRun("netplan", "generate"), stepRun("networkctl", "reload"))
	for _, x := range f.cards() {
		steps = append(steps, stepRunIgnore("ip", "link", "set", x.Name, "nomaster"))
	}
	if f.Bond != "" {
		steps = append(steps, stepRunIgnore("ip", "link", "del", f.Bond))
	}
	steps = append(steps, stepRunIgnore("ip", "link", "del", f.Bridge))
	for i, x := range f.cards() {
		steps = append(steps, netStep{Kind: "run", Argv: []string{"networkctl", "reconfigure", x.Name}, Secs: 15, Ignore: i > 0})
	}
	if f.MoveAddresses && len(f.UplinkExpect) > 0 {
		steps = append(steps, netStep{Kind: "wait-addr", Path: f.Uplink, Argv: f.UplinkExpect, Secs: 40})
	}
	pl.Steps = steps
	restore := []netStep{stepRunIgnore("netplan", "generate"), stepRunIgnore("networkctl", "reload"),
		{Kind: "wait-link", Path: f.Bridge, Secs: 20, Ignore: true}}
	re := []string{"networkctl", "reconfigure", f.Bridge}
	if f.Bond != "" {
		restore = append(restore, netStep{Kind: "wait-link", Path: f.Bond, Secs: 20, Ignore: true})
		re = append(re, f.Bond)
	}
	re = append(re, f.cardNames()...)
	restore = append(restore, netStep{Kind: "run", Argv: re, Secs: 15, Ignore: true})
	if len(f.UplinkExpect) > 0 {
		restore = append(restore, netStep{Kind: "wait-addr", Path: f.Bridge, Argv: f.UplinkExpect, Secs: 40, Ignore: true})
	}
	pl.Restore = restore
	return pl
}
