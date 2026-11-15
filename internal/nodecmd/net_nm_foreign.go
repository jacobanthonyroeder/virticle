package nodecmd

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"virticle/internal/nodeproto"
)

// NetworkManager: a bridge whose profiles somebody else made.
//
// Recognition reads the keyfiles of every profile (NetworkManager writes only the settings that differ from
// its defaults) and accepts a definition only when each section and key is in the lists below and has a
// value the model can express. The bridge, its port (a card or a bond) and the bond's members must each have
// exactly one profile, saved in /etc/NetworkManager/system-connections and active now.
//
// Taking it over replaces the profiles: virticle's profiles (virticle-BRIDGE, virticle-port-CARD,
// virticle-bond-BOND) are added with the same settings and activated on the existing devices, which
// NetworkManager does without removing the bridge, and then the original profiles are deleted. Their keyfiles
// are saved with the change; a rollback puts them back, reloads, activates them and deletes virticle's.
// When the bridge has addresses, the first card also gets a profile of its own with the bridge's address
// settings, not started by itself: it is what the card gets back when the switch is deleted, like the card's
// own profile of a switch that virticle created.

const nmKeyfileDir = "/etc/NetworkManager/system-connections"

type nmProfile struct {
	UUID, Type, Device, File, ID string
	Active                       bool
	secs                         []iniSec
}

func (p *nmProfile) get(sec, key string) (string, bool) {
	s := secByName(p.secs, sec)
	if s == nil {
		return "", false
	}
	v := s.values(key)
	if len(v) == 0 {
		return "", false
	}
	return v[len(v)-1], true
}

// controller is the profile's controller (master) and the port type.
func (p *nmProfile) controller() (string, string) {
	c, ok := p.get("connection", "controller")
	if !ok {
		c, _ = p.get("connection", "master")
	}
	t, ok := p.get("connection", "port-type")
	if !ok {
		t, _ = p.get("connection", "slave-type")
	}
	return c, t
}

// splitTerse splits one line of `nmcli -t` output at the colons that are not escaped.
func splitTerse(l string) []string {
	var out []string
	var b strings.Builder
	for i := 0; i < len(l); i++ {
		switch {
		case l[i] == '\\' && i+1 < len(l):
			i++
			b.WriteByte(l[i])
		case l[i] == ':':
			out = append(out, b.String())
			b.Reset()
		default:
			b.WriteByte(l[i])
		}
	}
	return append(out, b.String())
}

func kfBool(v string) (bool, bool) {
	switch strings.ToLower(v) {
	case "true", "yes", "1", "on":
		return true, true
	case "false", "no", "0", "off":
		return false, true
	}
	return false, false
}

// nmProfiles lists every profile with its keyfile.
func (c *Ctx) nmProfiles() ([]nmProfile, error) {
	out, err := c.netRun("nmcli", "-t", "-f", "UUID,TYPE,ACTIVE,DEVICE,FILENAME,NAME", "connection", "show")
	if err != nil {
		return nil, err
	}
	var ps []nmProfile
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		f := splitTerse(l)
		if len(f) != 6 {
			continue
		}
		p := nmProfile{UUID: f[0], Type: f[1], Active: f[2] == "yes", Device: f[3], File: f[4], ID: f[5]}
		if strings.HasSuffix(p.File, ".nmconnection") {
			if b, err := readHostFile(c, p.File); err == nil {
				p.secs = parseINI(b)
			}
		}
		ps = append(ps, p)
	}
	return ps, nil
}

// nmKeyRE turns the numbered keys of the ipv4 and ipv6 sections (address1, route2_options) into their name.
var nmKeyRE = regexp.MustCompile(`^(address|route|routing-rule)[0-9]+(_options)?$`)

// The keys of each section that a profile of the given role may have. A key not listed refuses the bridge.
var (
	nmIPKeys = map[string]bool{"method": true, "address": true, "gateway": true, "dns": true, "dns-search": true, "dns-priority": true,
		"route": true, "route_options": true, "route-metric": true, "route-table": true, "ignore-auto-dns": true, "ignore-auto-routes": true,
		"never-default": true, "may-fail": true, "dhcp-client-id": true, "dhcp-hostname": true, "dhcp-send-hostname": true,
		"dhcp-timeout": true, "dhcp-iaid": true, "addr-gen-mode": true, "ip6-privacy": true, "dhcp-duid": true, "ra-timeout": true}
	nmConnKeys   = map[string]bool{"id": true, "uuid": true, "type": true, "interface-name": true, "timestamp": true, "autoconnect": true, "permissions": true}
	nmPortConn   = map[string]bool{"controller": true, "master": true, "port-type": true, "slave-type": true}
	nmBridgeKeys = map[string]map[string]bool{
		"connection": nmConnKeys,
		"ethernet":   {"mtu": true},
		"bridge":     {"stp": true, "mac-address": true, "vlan-filtering": true, "vlan-default-pvid": true},
		"ipv4":       nmIPKeys,
		"ipv6":       nmIPKeys,
		"proxy":      {},
	}
	nmPortKeys = map[string]map[string]bool{
		"connection":  merge(nmConnKeys, nmPortConn),
		"ethernet":    {"mtu": true, "mac-address": true},
		"bridge-port": {"vlans": true},
		"proxy":       {},
	}
	nmBondKeys = map[string]map[string]bool{
		"connection":  merge(nmConnKeys, nmPortConn),
		"ethernet":    {"mtu": true, "cloned-mac-address": true},
		"bond":        {"mode": true, "miimon": true, "lacp_rate": true, "xmit_hash_policy": true},
		"bridge-port": {"vlans": true},
		"proxy":       {},
	}
	nmMemberKeys = map[string]map[string]bool{
		"connection": merge(nmConnKeys, nmPortConn),
		"ethernet":   {"mtu": true, "mac-address": true},
		"bond-port":  {}, // written empty; a queue id or priority refuses
		"proxy":      {},
	}
)

func merge(a, b map[string]bool) map[string]bool {
	m := map[string]bool{}
	for k := range a {
		m[k] = true
	}
	for k := range b {
		m[k] = true
	}
	return m
}

// nmAllowed finds the first section or key of p that is not in the list ("" when there is none).
func nmAllowed(p *nmProfile, allow map[string]map[string]bool) string {
	for _, s := range p.secs {
		keys, ok := allow[s.Name]
		if !ok {
			return s.Name
		}
		for _, l := range s.Lines {
			k, _, _ := strings.Cut(l, "=")
			k = strings.TrimSpace(k)
			if m := nmKeyRE.FindStringSubmatch(k); m != nil {
				k = m[1] + m[2]
			}
			if !keys[k] {
				return s.Name + "." + k
			}
		}
	}
	return ""
}

// nmCommon checks what every profile of a definition needs: a keyfile in /etc, active on its device,
// started by itself, the device's name as interface-name.
func nmCommon(p *nmProfile, ifname, br string) *foreignDef {
	switch {
	case filepath.Dir(p.File) != nmKeyfileDir || !strings.HasSuffix(p.File, ".nmconnection") || p.secs == nil:
		if strings.HasPrefix(p.File, "/run/") || p.File == "" {
			return refuse("No saved definition", "The NetworkManager profile %q of %s is not saved (%s).", p.ID, br, orNone(p.File))
		}
		return refuse("Defined in "+p.File, "The NetworkManager profile %q of %s is stored in %s, which virticle does not edit; it edits keyfiles in %s.", p.ID, br, p.File, nmKeyfileDir)
	case !p.Active || p.Device != ifname:
		return refuse("Profile "+p.ID+" is not active", "The NetworkManager profile %q (%s) is not the active one on %s.", p.ID, p.File, ifname)
	}
	if v, ok := p.get("connection", "interface-name"); !ok || v != ifname {
		return refuse("Profile "+p.ID+" not bound by name", "The NetworkManager profile %q is not bound to the interface name %s.", p.ID, ifname)
	}
	if v, ok := p.get("connection", "autoconnect"); ok {
		if b, _ := kfBool(v); !b {
			return refuse("Profile "+p.ID+" does not start by itself", "The NetworkManager profile %q has autoconnect off, so %s does not come up at boot.", p.ID, ifname)
		}
	}
	if v, ok := p.get("connection", "permissions"); ok && v != "" {
		return refuse("Profile "+p.ID+" has user permissions", "The NetworkManager profile %q is limited to some users.", p.ID)
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "in memory only"
	}
	return s
}

func keyRefusal(p *nmProfile, key string) *foreignDef {
	return refuse("Setting "+key+" not supported", "The NetworkManager profile %q (%s) has the setting %s, which virticle does not have.", p.ID, p.File, key)
}

// nmMTU reads the MTU a profile sets (0: none).
func nmMTU(p *nmProfile) (int, bool) {
	v, ok := p.get("ethernet", "mtu")
	if !ok || v == "0" || v == "auto" {
		return 0, true
	}
	n, err := strconv.Atoi(v)
	return n, err == nil && n >= 1280 && n <= 9000
}

// nmPortVLANList reads bridge-port.vlans in the form virticle writes: "1 pvid untagged" and plain ids.
func nmPortVLANList(v string) (string, bool) {
	var ids []string
	pvid := false
	for _, it := range strings.Split(v, ",") {
		f := strings.Fields(it)
		if len(f) == 0 {
			continue
		}
		if len(f) == 3 && f[0] == "1" && ((f[1] == "pvid" && f[2] == "untagged") || (f[1] == "untagged" && f[2] == "pvid")) {
			pvid = true
			continue
		}
		if len(f) != 1 {
			return "", false
		}
		ids = append(ids, f[0])
	}
	if !pvid {
		return "", false
	}
	if len(ids) == 0 {
		return "1", true
	}
	canon, err := nodeproto.ParseVLANs(strings.Join(ids, ","))
	if err != nil || strings.HasPrefix(canon, "1,") || canon == "1" || strings.HasPrefix(canon, "1-") {
		return "", false
	}
	return canon, true
}

func (c *Ctx) nmRecognise(snap *netSnap, l *netLink) *foreignDef {
	br := l.Name
	port, fd := c.runtimePort(snap, l)
	if fd != nil {
		return fd
	}
	all, err := c.nmProfiles()
	if err != nil {
		return refuse("Profiles not readable", "The NetworkManager profiles cannot be listed: %v.", err)
	}
	var bridge *nmProfile
	for i := range all {
		p := &all[i]
		if p.Type == "bridge" && p.Device == br {
			bridge = p
		}
	}
	if bridge == nil {
		return refuse("No saved definition (made with ip link)", "NetworkManager has no active profile for %s.", br)
	}
	if fd := nmCommon(bridge, br, br); fd != nil {
		return fd
	}
	// Every profile that names the bridge, its profile or the bond.
	ref := map[string]bool{br: true, bridge.UUID: true}
	var portP, bondP *nmProfile
	members := map[string]*nmProfile{}
	for i := range all {
		p := &all[i]
		if p == bridge {
			continue
		}
		if p.secs == nil {
			if strings.HasSuffix(p.File, ".nmconnection") || p.Device == br || p.Type == "loopback" || p.File == "" {
				continue
			}
			if p.Type == "bridge" || p.Type == "bond" || p.Type == "802-3-ethernet" || p.Type == "vlan" {
				return refuse("Defined in "+p.File, "The NetworkManager profile %q is stored in %s, which virticle cannot read; it may belong to %s.", p.ID, p.File, br)
			}
			continue
		}
		if v, _ := p.get("connection", "interface-name"); p.Type == "bridge" && v == br {
			return refuse("Two profiles for "+br, "NetworkManager has two profiles for %s: %q and %q.", br, bridge.ID, p.ID)
		}
		if par, _ := p.get("vlan", "parent"); p.Type == "vlan" && ref[par] {
			return refuse("VLAN interface on "+br, "The NetworkManager profile %q is a VLAN interface on %s; virticle does not take those over.", p.ID, br)
		}
		ctl, ptype := p.controller()
		if !ref[ctl] {
			continue
		}
		if ptype != "bridge" {
			return refuse("Profile "+p.ID+" not supported", "The NetworkManager profile %q has %s as its controller with the port type %q.", p.ID, br, ptype)
		}
		switch {
		case p.Type == "bond" && port.Bond != "" && p.Device == port.Bond && bondP == nil:
			bondP = p
		case p.Type == "802-3-ethernet" && port.Bond == "" && len(port.Cards) == 1 && p.Device == port.Cards[0] && portP == nil:
			portP = p
		default:
			return refuse("Port profile "+p.ID+" not supported", "The NetworkManager profile %q is a port of %s, but it is not the active profile of its single card or bond.", p.ID, br)
		}
	}
	if port.Bond != "" && bondP == nil || port.Bond == "" && len(port.Cards) == 1 && portP == nil {
		return refuse("Port has no saved profile", "The port of %s has no NetworkManager profile that makes it a port.", br)
	}
	if bondP != nil {
		ref := map[string]bool{port.Bond: true, bondP.UUID: true}
		for i := range all {
			p := &all[i]
			ctl, ptype := p.controller()
			if p.secs == nil || !ref[ctl] {
				continue
			}
			if p.Type != "802-3-ethernet" || ptype != "bond" || !containsStr(port.Cards, p.Device) || members[p.Device] != nil {
				return refuse("Bond profile "+p.ID+" not supported", "The NetworkManager profile %q belongs to the bond %s, but it is not the active profile of one of its cards.", p.ID, port.Bond)
			}
			members[p.Device] = p
		}
		if len(members) != len(port.Cards) {
			return refuse("Bond member without profile", "A card of the bond %s has no NetworkManager profile that makes it a member.", port.Bond)
		}
	}

	s := foreignSpec(snap, l, port, "networkmanager")
	fd = &foreignDef{Spec: s}
	fd.nmProfiles = append(fd.nmProfiles, *bridge)

	// The bridge.
	if k := nmAllowed(bridge, nmBridgeKeys); k != "" {
		return keyRefusal(bridge, k)
	}
	stp := true // NetworkManager's default
	if v, ok := bridge.get("bridge", "stp"); ok {
		stp, _ = kfBool(v)
	}
	if stp {
		return refuse("STP is on", "The NetworkManager profile %q has STP on (the default when bridge.stp is not set); virticle's switches have it off.", bridge.ID)
	}
	aware := false
	if v, ok := bridge.get("bridge", "vlan-filtering"); ok {
		aware, _ = kfBool(v)
	}
	if v, ok := bridge.get("bridge", "vlan-default-pvid"); ok && v != "1" {
		return refuse("Default VLAN "+v, "The NetworkManager profile %q sets the default VLAN id to %s; virticle's switches use 1.", bridge.ID, v)
	}
	if aware != l.VLANFiltering {
		return refuse("VLAN filtering differs from the profile", "VLAN filtering of %s is not what its profile %q says.", br, bridge.ID)
	}
	s.VLANAware = aware
	mtus := map[string]int{}
	ok := true
	if mtus[br], ok = nmMTU(bridge); !ok {
		return keyRefusal(bridge, "ethernet.mtu")
	}
	// The address settings: what nmcli shows, the same keys virticle copies from a card.
	out, err := c.netRun("nmcli", "-t", "-f", "ipv4,ipv6", "connection", "show", "uuid", bridge.UUID)
	if err != nil {
		return refuse("Profile not readable", "The NetworkManager profile %q cannot be read: %v.", bridge.ID, err)
	}
	prof := nmTerse(out)
	var kv []string
	for k, v := range prof {
		if nmCopyKeys[k] && v != "" && v != "--" {
			kv = append(kv, k, v)
		}
	}
	sortPairs(kv)
	b, _ := json.Marshal(kv)
	s.Moved = string(b)
	has := prof["ipv4.method"] != "disabled" || (prof["ipv6.method"] != "disabled" && prof["ipv6.method"] != "ignore")
	setMoved(s, l, has)
	if v, ok := bridge.get("bridge", "mac-address"); ok && (!s.MoveAddresses || !strings.EqualFold(v, l.MAC)) {
		return refuse("MAC address set", "The NetworkManager profile %q sets the MAC address %s, which virticle only sets on a bridge with addresses (to the address it has now).", bridge.ID, v)
	}

	// The port, the bond and its members.
	vlansOf := func(p *nmProfile) (string, *foreignDef) {
		v, ok := p.get("bridge-port", "vlans")
		switch {
		case !aware && ok && v != "":
			return "", refuse("VLANs set without VLAN filtering", "The NetworkManager profile %q lists VLANs, but %s does not filter VLANs.", p.ID, br)
		case !aware:
			return "", nil
		case !ok:
			return "1", nil
		}
		canon, good := nmPortVLANList(v)
		if !good {
			return "", refuse("VLAN list not supported", "The NetworkManager profile %q allows the VLANs %q; virticle's switches use VLAN 1 untagged as the port's default and plain tagged ids.", p.ID, v)
		}
		return canon, nil
	}
	checkCard := func(p *nmProfile, allow map[string]map[string]bool) *foreignDef {
		if fd := nmCommon(p, p.Device, br); fd != nil {
			return fd
		}
		if k := nmAllowed(p, allow); k != "" {
			return keyRefusal(p, k)
		}
		if v, ok := p.get("ethernet", "mac-address"); ok && !strings.EqualFold(v, snap.link(p.Device).MAC) {
			return refuse("Profile "+p.ID+" bound to another MAC", "The NetworkManager profile %q is bound to the MAC address %s, which %s does not have.", p.ID, v, p.Device)
		}
		var good bool
		if mtus[p.Device], good = nmMTU(p); !good {
			return keyRefusal(p, "ethernet.mtu")
		}
		return nil
	}
	switch {
	case portP != nil:
		if fd := checkCard(portP, nmPortKeys); fd != nil {
			return fd
		}
		vl, bad := vlansOf(portP)
		if bad != nil {
			return bad
		}
		s.VLANs = vl
		fd.nmProfiles = append(fd.nmProfiles, *portP)
	case bondP != nil:
		if bad := nmCommon(bondP, port.Bond, br); bad != nil {
			return bad
		}
		if k := nmAllowed(bondP, nmBondKeys); k != "" {
			return keyRefusal(bondP, k)
		}
		mode, bad := nmBondMode(bondP)
		if bad != nil {
			return bad
		}
		s.BondMode = mode
		if v, ok := bondP.get("ethernet", "cloned-mac-address"); ok && (!s.MoveAddresses || !strings.EqualFold(v, l.MAC)) {
			return refuse("Bond MAC address set", "The NetworkManager profile %q gives the bond the MAC address %s; virticle only sets the bridge's own address there.", bondP.ID, v)
		}
		var good bool
		if mtus[port.Bond], good = nmMTU(bondP); !good {
			return keyRefusal(bondP, "ethernet.mtu")
		}
		vl, bad := vlansOf(bondP)
		if bad != nil {
			return bad
		}
		s.VLANs = vl
		fd.nmProfiles = append(fd.nmProfiles, *bondP)
		for _, card := range port.Cards {
			m := members[card]
			if bad := checkCard(m, nmMemberKeys); bad != nil {
				return bad
			}
			fd.nmProfiles = append(fd.nmProfiles, *m)
		}
	}
	if s.VLANAware && s.Uplink == "" {
		s.VLANs = "2-4094"
	}
	// The MTU: none set, or the same value on every layer (what virticle's MTU setting writes).
	mtu, bad := sameMTU(mtus)
	if bad != nil {
		return bad
	}
	s.MTUSet = mtu
	if out, err := c.netRun("nmcli", "--version"); err == nil && nmVersionAtLeast(out, 1, 46) {
		s.Data["portKeys"] = "new"
	}
	for _, p := range fd.nmProfiles {
		fd.Where = append(fd.Where, fmt.Sprintf("NetworkManager profile %q, %s", p.ID, p.File))
	}
	return fd
}

func containsStr(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// sameMTU: no layer sets an MTU, or every one sets the same.
func sameMTU(m map[string]int) (int, *foreignDef) {
	v := 0
	var set, unset []string
	for k, x := range m {
		if x == 0 {
			unset = append(unset, k)
			continue
		}
		if v != 0 && x != v {
			return 0, refuse("MTU differs between layers", "The MTU is not the same on every device of the switch (%s: %d, another: %d); virticle sets one MTU for all of them.", k, x, v)
		}
		v = x
		set = append(set, k)
	}
	if v != 0 && len(unset) > 0 {
		sort.Strings(set)
		sort.Strings(unset)
		return 0, refuse("MTU set on some layers only", "The MTU %d is set on %s but not on %s; virticle sets one MTU for every device of the switch.", v, strings.Join(set, ", "), strings.Join(unset, ", "))
	}
	return v, nil
}

// nmBondMode maps the bond's options to the two modes of the model, with exactly the options virticle writes.
func nmBondMode(p *nmProfile) (string, *foreignDef) {
	opts := map[string]string{}
	if sec := secByName(p.secs, "bond"); sec != nil {
		for _, l := range sec.Lines {
			k, v, _ := strings.Cut(l, "=")
			opts[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	var words []string
	for k, v := range opts {
		words = append(words, k+"="+v)
	}
	sort.Strings(words)
	mode := opts["mode"]
	switch {
	case (mode == "active-backup" || mode == "1") && opts["miimon"] == "100" && len(opts) == 2:
		return nodeproto.BondFailover, nil
	case (mode == "802.3ad" || mode == "4") && opts["miimon"] == "100" && (opts["lacp_rate"] == "fast" || opts["lacp_rate"] == "1") &&
		opts["xmit_hash_policy"] == "layer2+3" && len(opts) == 4:
		return nodeproto.BondLACP, nil
	}
	return "", refuse("Bond options not supported", "The bond options %q of the profile %q are not one of virticle's two modes (mode=active-backup,miimon=100 or mode=802.3ad,miimon=100,lacp_rate=fast,xmit_hash_policy=layer2+3).", strings.Join(words, ","), p.ID)
}

// --- the change that takes the profiles over ---

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// nmOwnID is the name of the profile a card gets for when the switch is deleted.
func nmOwnID(card string, taken map[string]bool) string {
	if !taken[card] {
		return card
	}
	return "virticle-card-" + card
}

func nmOwnAdd(c cardSpec, moved string, autoconnect bool) netStep {
	ac := "no"
	if autoconnect {
		ac = "yes"
	}
	a := []string{"nmcli", "connection", "add", "type", "ethernet", "ifname", c.Name, "con-name", c.ID, "connection.uuid", c.UUID, "connection.autoconnect", ac}
	var kv []string
	_ = json.Unmarshal([]byte(moved), &kv)
	return netStep{Kind: "run", Argv: append(a, kv...)}
}

func nmUpUUID(uuid string, ignore bool) netStep {
	return netStep{Kind: "run", Argv: []string{"nmcli", "--wait", "0", "connection", "up", "uuid", uuid}, Ignore: ignore}
}

// nmForeignRestore brings the original profiles back; the engine has already put their keyfiles back.
func nmForeignRestore(fd *foreignDef) []netStep {
	steps := []netStep{stepRunIgnore("nmcli", "connection", "reload")}
	for _, p := range fd.nmProfiles {
		steps = append(steps, nmUpUUID(p.UUID, true))
	}
	return steps
}

func (c *Ctx) nmTaken() map[string]bool {
	ids, _ := c.netRun("nmcli", "-t", "-f", "NAME", "connection", "show")
	taken := map[string]bool{}
	for _, l := range strings.Split(ids, "\n") {
		taken[l] = true
	}
	return taken
}

func nmForeignDelete(fd *foreignDef) netStep {
	a := []string{"nmcli", "connection", "delete"}
	for _, p := range fd.nmProfiles {
		a = append(a, "uuid", p.UUID)
	}
	return stepRun(a...)
}

func nmProfileNames(fd *foreignDef) string {
	var names []string
	for _, p := range fd.nmProfiles {
		names = append(names, fmt.Sprintf("%q (%s)", p.ID, p.File))
	}
	return strings.Join(names, ", ")
}

// nmTakeover plans the first change of a recognised bridge: in.Old is the bridge as it is, in.New as it is
// to be. virticle's profiles of New replace the original ones.
func nmTakeover(c *Ctx, in *planInput) (*netPlanned, error) {
	fd, f, n := in.Foreign, in.Old, in.New
	pl := &netPlanned{Marker: "put", Spec: n}
	for _, p := range fd.nmProfiles {
		pl.Keep = append(pl.Keep, p.File)
		pl.Saves = append(pl.Saves, netSave{"nm-original-profile-" + p.UUID + ".txt", []string{"nmcli", "-t", "-s", "connection", "show", "uuid", p.UUID}})
	}
	taken := c.nmTaken()
	ours := []string{nmBridgeID(n)}
	if n.Bond != "" {
		ours = append(ours, nmBondID(n))
	}
	for _, x := range n.cardNames() {
		ours = append(ours, nmPortID(x))
	}
	for _, id := range ours {
		if taken[id] {
			return nil, Fail("NetworkManager already has a connection named %s", id)
		}
	}
	pl.Summary = append(pl.Summary, foreignWords(f.Bridge),
		fmt.Sprintf("The NetworkManager profiles %s are replaced by virticle's profiles %s and deleted.", nmProfileNames(fd), strings.Join(ours, ", ")))
	// The card's own profile, for when the switch is deleted.
	var own *cardSpec
	if n.MoveAddresses && n.Data["origUUID"] == "" {
		cs := n.cards()
		cs[0].UUID, cs[0].ID, cs[0].Autoconnect = newUUID(), nmOwnID(cs[0].Name, taken), "yes"
		n.setCards(cs)
		own = &cs[0]
		pl.Summary = append(pl.Summary, fmt.Sprintf("%s gets a profile of its own, %q, with the address settings of %s. It does not start while the card belongs to the switch; the card gets it back when the switch is deleted.", own.Name, own.ID, f.Bridge))
	}
	// Cards that join the switch in this change keep their own profiles, switched off (set-uplinks).
	for i := range in.Added {
		x := in.Added[i]
		xc, err := nmCaptureCard(c, x.Link, len(x.Link.globalAddrs()) > 0, pl)
		if err != nil {
			return nil, err
		}
		xc.card.Expect = x.Expect
		cs := n.cards()
		for j := range cs {
			if cs[j].Name == x.Link.Name {
				cs[j] = xc.card
			}
		}
		n.setCards(cs)
	}
	d := diffCards(f, n)
	steps := []netStep{nmBridgeAdd(n)}
	if n.Bond != "" {
		steps = append(steps, nmBondAdd(n))
	}
	for _, x := range n.cardNames() {
		steps = append(steps, nmPortAdd(n, x))
	}
	if own != nil {
		steps = append(steps, nmOwnAdd(*own, n.Moved, false))
	}
	for _, x := range d.Added {
		if cs := n.card(x.Name); cs != nil {
			steps = append(steps, nmCardOff(*cs)...)
		}
	}
	for _, v := range n.VLANAddrs {
		steps = append(steps, nmVLANAdd(n, v))
	}
	// Activating virticle's profiles on the existing devices replaces the original ones without removing the
	// bridge; the originals are deleted once they are no longer active.
	steps = append(steps, nmUp(nmBridgeID(n)))
	if n.Bond != "" {
		steps = append(steps, nmUp(nmBondID(n)))
	}
	for _, x := range n.cardNames() {
		steps = append(steps, nmUp(nmPortID(x)))
	}
	for _, v := range n.VLANAddrs {
		steps = append(steps, nmUp(nmVLANID(n, v)))
	}
	steps = append(steps, nmForeignDelete(fd))
	for _, x := range d.Added {
		steps = append(steps, stepRunIgnore("ip", "addr", "flush", "dev", x.Name))
	}
	layers := mtuLayers(n)
	cur := mtuValues(in.Snap, layers)
	if n.MTUSet > 0 && n.MTUSet != f.MTUSet {
		steps = append(steps, mtuMoves(layers, cur, mtuTarget(layers, n.MTUSet))...)
	}
	exp := uplinkExpect(*in.Snap.link(f.Bridge))
	if len(exp) > 0 {
		steps = append(steps, netStep{Kind: "wait-addr", Path: n.Bridge, Argv: exp, Secs: 40})
	}
	for _, v := range n.VLANAddrs {
		if v.Method == nodeproto.AddrStatic {
			steps = append(steps, netStep{Kind: "wait-addr", Path: v.iface(n), Argv: []string{v.Address}, Secs: 20})
		}
	}
	foreignActionSummary(c, pl, in)
	pl.Steps = steps

	restore := nmForeignRestore(fd)
	for _, v := range n.VLANAddrs {
		restore = append(restore, nmDel(nmVLANID(n, v)), stepRunIgnore("ip", "link", "del", v.iface(n)))
	}
	for _, x := range n.cardNames() {
		restore = append(restore, nmDel(nmPortID(x)))
	}
	if n.Bond != "" {
		restore = append(restore, nmDel(nmBondID(n)))
		if n.Bond != f.Bond {
			restore = append(restore, stepRunIgnore("ip", "link", "del", n.Bond))
		}
	}
	restore = append(restore, nmDel(nmBridgeID(n)))
	if own != nil {
		restore = append(restore, stepRunIgnore("nmcli", "connection", "delete", "uuid", own.UUID))
	}
	for _, x := range d.Added {
		if cs := n.card(x.Name); cs != nil {
			restore = append(restore, nmGiveBack(n, *cs, true, false)...)
		}
	}
	if n.MTUSet > 0 && n.MTUSet != f.MTUSet {
		restore = append(restore, ignoreAll(mtuMoves(layers, mtuTarget(layers, n.MTUSet), cur))...)
	}
	if len(exp) > 0 {
		restore = append(restore, netStep{Kind: "wait-addr", Path: f.Bridge, Argv: exp, Secs: 40, Ignore: true})
	}
	pl.Restore = restore
	return pl, nil
}

// nmForeignDeletePlan deletes a recognised bridge: its profiles go, and the first card gets the bridge's
// address settings in a profile of its own.
func nmForeignDeletePlan(c *Ctx, in *planInput) *netPlanned {
	fd, f := in.Foreign, in.Old
	pl := &netPlanned{Marker: "remove", Spec: f}
	for _, p := range fd.nmProfiles {
		pl.Keep = append(pl.Keep, p.File)
	}
	pl.Summary = append(pl.Summary, fmt.Sprintf("Delete the Linux bridge %s, which was set up outside virticle: the NetworkManager profiles %s are deleted. They are saved first and come back with a rollback.", f.Bridge, nmProfileNames(fd)))
	var own *cardSpec
	if f.MoveAddresses {
		cs := f.cards()[0]
		cs.UUID, cs.ID = newUUID(), nmOwnID(cs.Name, c.nmTaken())
		own = &cs
		pl.Summary = append(pl.Summary, fmt.Sprintf("%s gets the address settings of %s in a new profile %q (%s).", cs.Name, f.Bridge, cs.ID, strings.Join(f.UplinkExpect, ", ")))
	}
	var steps []netStep
	if own != nil {
		steps = append(steps, nmOwnAdd(*own, f.Moved, true))
	}
	steps = append(steps, nmForeignDelete(fd), stepRunIgnore("ip", "link", "del", f.Bridge))
	if f.Bond != "" {
		steps = append(steps, stepRunIgnore("ip", "link", "del", f.Bond))
	}
	if own != nil {
		steps = append(steps, nmUpUUID(own.UUID, false))
		if len(f.UplinkExpect) > 0 {
			steps = append(steps, netStep{Kind: "wait-addr", Path: own.Name, Argv: f.UplinkExpect, Secs: 40})
		}
	}
	pl.Steps = steps
	pl.Restore = nmForeignRestore(fd)
	if own != nil {
		pl.Restore = append(pl.Restore, stepRunIgnore("nmcli", "connection", "delete", "uuid", own.UUID))
	}
	if len(f.UplinkExpect) > 0 {
		pl.Restore = append(pl.Restore, netStep{Kind: "wait-addr", Path: f.Bridge, Argv: f.UplinkExpect, Secs: 40, Ignore: true})
	}
	return pl
}

func readHostFile(c *Ctx, p string) (string, error) {
	b, err := os.ReadFile(c.hostPath(p))
	return string(b), err
}
