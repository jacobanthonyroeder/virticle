package api

// How a clone gets the user, password, SSH keys and packages of its Guest
// setup section. A guest with cloud-init reads them from a seed disc at its
// first start; a Linux guest without cloud-init gets them written into the
// clone's disks while it is shut off (node command verb guest-customize); in
// every other case the request is refused with the reason. What the guest has
// is found out by looking into the source's disks (verb guest-inspect), and
// remembered while the disks do not change.

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cloudinit"
	"virticle/internal/cluster"
	"virticle/internal/hostconn"
)

// guestFacts is the JSON of the node command's guest-inspect (nodecmd.GuestFacts).
type guestFacts struct {
	Type              string `json:"type"`
	Distro            string `json:"distro"`
	Product           string `json:"product"`
	Major             string `json:"major"`
	PackageFormat     string `json:"packageFormat"`
	PackageManagement string `json:"packageManagement"`
	Encrypted         bool   `json:"encrypted"`
	Systems           int    `json:"systems"`
	CloudInit         bool   `json:"cloudInit"`
	CloudInitDisabled bool   `json:"cloudInitDisabled"`
	Useradd           bool   `json:"useradd"`
	Sudo              bool   `json:"sudo"`
	GuestAgent        bool   `json:"guestAgent"`
	SELinux           bool   `json:"selinux"`
}

const (
	setupSeed     = "cloud-init" // NoCloud seed disc, read by cloud-init at first start
	setupUnattend = "unattend"   // Windows answer file on a disc, read by Windows setup
	setupDisk     = "disk"       // written into the clone's disks before its first start
)

// guestSetup is the answer of GET .../guest-setup and the decision vmClone acts on.
type guestSetup struct {
	// Setup is how the settings reach the guest; "" when they cannot (see Problem).
	Setup   string `json:"setup,omitempty"`
	Problem string `json:"problem,omitempty"`
	// Unchecked says why the disks were not looked into (Setup is then the seed disc, as before).
	Unchecked string `json:"unchecked,omitempty"`
	// System names what was found, e.g. "Debian 13".
	System string      `json:"system,omitempty"`
	facts  *guestFacts // nil when not looked into
}

// distroFamily groups the distributions the offline way supports; "" for any other.
func distroFamily(distro string) string {
	switch distro {
	case "debian", "ubuntu", "linuxmint", "kalilinux":
		return "debian"
	case "rhel", "redhat-based", "centos", "fedora", "rocky", "almalinux", "oraclelinux", "scientificlinux", "circle":
		return "rhel"
	case "opensuse", "sles", "suse-based":
		return "suse"
	}
	return ""
}

var distroNames = map[string]string{"debian": "Debian", "ubuntu": "Ubuntu", "rhel": "RHEL", "centos": "CentOS", "fedora": "Fedora",
	"rocky": "Rocky Linux", "almalinux": "AlmaLinux", "oraclelinux": "Oracle Linux", "opensuse": "openSUSE", "sles": "SLES",
	"alpinelinux": "Alpine Linux", "archlinux": "Arch Linux", "cirros": "CirrOS", "nixos": "NixOS", "linuxmint": "Linux Mint"}

// systemName is a short name for what inspection found.
func (f guestFacts) systemName() string {
	if strings.IndexFunc(f.Product, unicode.IsLetter) >= 0 {
		return f.Product
	}
	n := distroNames[f.Distro]
	if n == "" {
		n = f.Distro
	}
	if n == "" {
		switch f.Type {
		case "linux":
			n = "Linux"
		case "windows":
			n = "Windows"
		default:
			n = f.Type
		}
	}
	if f.Major != "" && f.Major != "0" {
		n += " " + f.Major
	}
	return n
}

// guestTools says which guest disk tools the node has.
type guestTools struct{ guestfish, virtCustomize bool }

// chooseSetup decides how the Guest setup settings reach a clone. guest is the
// family the clone is handled as (guestFamily); facts and inspectErr are the
// result of looking into the source's disks (facts nil: not looked into).
func chooseSetup(guest string, tools guestTools, facts *guestFacts, inspectErr error) guestSetup {
	if guest == "windows" {
		return guestSetup{Setup: setupUnattend}
	}
	if !tools.guestfish {
		return guestSetup{Setup: setupSeed, Unchecked: "no guest disk tools on the node"}
	}
	if inspectErr != nil {
		return guestSetup{Problem: "the guest disk tools could not look into the disk: " + inspectErr.Error()}
	}
	if facts == nil {
		return guestSetup{Setup: setupSeed, Unchecked: "not looked into"}
	}
	g := guestSetup{facts: facts}
	if facts.Type != "" {
		g.System = facts.systemName()
	}
	switch {
	case facts.Type == "" && facts.Encrypted:
		g.Problem = "the disk is encrypted"
	case facts.Type == "":
		g.Problem = "no system the guest disk tools recognise on the disk"
	case facts.Type == "windows":
		g.Problem = "Windows on the disk, but the VM is not defined as a Windows guest"
	case facts.Type != "linux":
		g.Problem = g.System + " is not supported"
	case facts.CloudInit && !facts.CloudInitDisabled:
		g.Setup = setupSeed
	case distroFamily(facts.Distro) == "":
		g.Problem = g.System + " without cloud-init: the guest disk tools cannot set it up"
	case !facts.Useradd:
		g.Problem = "no useradd in " + g.System
	case !tools.virtCustomize:
		g.Problem = "virt-customize is not installed on the node"
	default:
		g.Setup = setupDisk
	}
	return g
}

// diskRequest checks a clone's settings against the decision and, for the
// offline way, builds the JSON request of guest-customize (password hashed
// here; the plain text never leaves the controller).
func (g guestSetup) diskRequest(cloneName string, c cloneCustomize) (string, error) {
	if g.Setup == "" {
		return "", badRequest("user, password, SSH keys and packages are not possible for this guest: " + g.Problem)
	}
	if g.Setup != setupDisk {
		return "", nil
	}
	if c.Profile != 0 {
		return "", badRequest("cloud-init profiles need cloud-init in the guest, and " + g.System + " has none")
	}
	if strings.TrimSpace(c.RawUserData) != "" {
		return "", badRequest("raw user-data needs cloud-init in the guest, and " + g.System + " has none")
	}
	host := c.Hostname
	if host == "" {
		host = cloneName
	}
	var keys []string
	for _, k := range c.SSHKeys {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	cfg := cloudinit.Config{Hostname: host, User: c.User, Password: c.Password, SSHKeys: keys, Packages: c.Packages}
	if err := cfg.Validate(); err != nil {
		return "", badRequest(err.Error())
	}
	f := g.facts
	pk := append([]string{}, c.Packages...)
	knownPM := map[string]bool{"apt": true, "dnf": true, "yum": true, "zypper": true}[f.PackageManagement]
	if len(pk) > 0 && !knownPM {
		return "", badRequest("packages cannot be installed: the package manager of " + g.System + " is not known")
	}
	if knownPM {
		// What the seed disc would bring: the guest agent; and sudo for the new user's sudo group.
		has := map[string]bool{}
		for _, p := range pk {
			has[p] = true
		}
		if !f.GuestAgent && !has[cloudinit.GuestAgentPackage] {
			pk = append(pk, cloudinit.GuestAgentPackage)
		}
		if c.User != "" && !f.Sudo && !has["sudo"] {
			pk = append(pk, "sudo")
		}
	}
	req := struct {
		Hostname     string   `json:"hostname"`
		User         string   `json:"user,omitempty"`
		PasswordHash string   `json:"passwordHash,omitempty"`
		SSHKeys      []string `json:"sshKeys,omitempty"`
		Packages     []string `json:"packages,omitempty"`
		Manager      string   `json:"packageManager,omitempty"`
		SELinux      bool     `json:"selinux,omitempty"`
	}{Hostname: host, User: c.User, SSHKeys: keys, Packages: pk, SELinux: f.SELinux}
	if len(pk) > 0 {
		req.Manager = f.PackageManagement
	}
	if c.Password != "" {
		h, err := cloudinit.HashPassword(c.Password)
		if err != nil {
			return "", err
		}
		req.PasswordHash = h
	}
	b, err := json.Marshal(req)
	return string(b), err
}

// --- looking into the disks, remembered ---

type guestCacheEntry struct {
	stamp string
	at    time.Time
	facts *guestFacts
}

type guestCache struct {
	mu      sync.Mutex
	entries map[string]guestCacheEntry
	running map[string]chan struct{}
}

// guestCacheAge is how long the result for a VM that is not a template is
// believed; a template's disks do not change while it is one.
const guestCacheAge = 10 * time.Minute

// inspectGuest returns what is on the disks, looking only when they changed
// (size or allocation) since the last look. Concurrent calls for the same VM
// share one look.
func (s *Server) inspectGuest(h *hostconn.Host, uuid string, template bool, disks []string) (*guestFacts, error) {
	st, err := h.Stat(disks...)
	if err != nil {
		return nil, err
	}
	stamp := fmt.Sprint(template)
	for _, f := range st {
		stamp += fmt.Sprintf("|%s:%d:%d", f.Path, f.Size, f.Allocated())
	}
	key := h.Name + "/" + uuid
	c := &s.guestInfo
	for {
		c.mu.Lock()
		if c.entries == nil {
			c.entries, c.running = map[string]guestCacheEntry{}, map[string]chan struct{}{}
		}
		if e, ok := c.entries[key]; ok && e.stamp == stamp && (template || time.Since(e.at) < guestCacheAge) {
			c.mu.Unlock()
			return e.facts, nil
		}
		if ch, ok := c.running[key]; ok {
			c.mu.Unlock()
			<-ch
			continue
		}
		ch := make(chan struct{})
		c.running[key] = ch
		c.mu.Unlock()
		facts, err := lookIntoDisks(h, disks)
		c.mu.Lock()
		delete(c.running, key)
		if err == nil {
			c.entries[key] = guestCacheEntry{stamp, time.Now(), facts}
		}
		c.mu.Unlock()
		close(ch)
		return facts, err
	}
}

func lookIntoDisks(h *hostconn.Host, disks []string) (*guestFacts, error) {
	out, err := h.GuestInspect(disks...)
	if err != nil {
		return nil, err
	}
	var f guestFacts
	if err := json.Unmarshal([]byte(out), &f); err != nil {
		return nil, fmt.Errorf("unexpected answer from the node: %.200s", out)
	}
	return &f, nil
}

// cloneSourceDisks are the writable disks of a domain that a clone copies.
func cloneSourceDisks(x domainXML) []string {
	var out []string
	for _, k := range x.Devices.Disks {
		if k.Device == "disk" && k.ReadOnly == nil && k.Source.File != "" {
			out = append(out, k.Source.File)
		}
	}
	return out
}

func nodeGuestTools(h *hostconn.Host) guestTools {
	return guestTools{guestfish: h.HasTool("guestfish"), virtCustomize: h.HasTool("virt-customize")}
}

// decideSetup looks into the source's disks when that is needed and possible
// and chooses the way (chooseSetup).
func (s *Server) decideSetup(h *hostconn.Host, dom libvirt.Domain, guest string, template bool, x domainXML) guestSetup {
	if guest == "windows" {
		return chooseSetup(guest, guestTools{}, nil, nil)
	}
	tools := nodeGuestTools(h)
	if !tools.guestfish {
		return chooseSetup(guest, tools, nil, nil)
	}
	disks := cloneSourceDisks(x)
	if len(disks) == 0 {
		return guestSetup{Problem: "the VM has no disk"}
	}
	facts, err := s.inspectGuest(h, cluster.FormatUUID(dom.UUID), template, disks)
	return chooseSetup(guest, tools, facts, err)
}

// guestSetupInfo tells the clone dialog how its Guest setup section would be applied.
func (s *Server) guestSetupInfo(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	mark, err := getMark(h, dom)
	if err != nil {
		fail(w, err)
		return
	}
	raw, err := h.DomainGetXMLDesc(dom, libvirt.DomainXMLInactive)
	if err != nil {
		fail(w, err)
		return
	}
	var x domainXML
	if err := xml.Unmarshal([]byte(raw), &x); err != nil {
		fail(w, err)
		return
	}
	guest := guestFamily("", mark, raw)
	if state, _, _, _, _, err := h.DomainGetInfo(dom); err == nil && stateName(state) != "shutoff" && guest != "windows" {
		writeJSON(w, guestSetup{Setup: setupSeed, Unchecked: "the VM is " + stateName(state)})
		return
	}
	writeJSON(w, s.decideSetup(h, dom, guest, mark.Template, x))
}
