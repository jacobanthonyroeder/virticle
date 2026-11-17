package prepare

import (
	"fmt"
	"strings"
)

// Kernel modules and kernel command line.
//
// The rule (the owner's): Virticle never edits the boot loader and never reboots. A kernel module it needs is loaded
// and listed in a file it owns in /etc/modules-load.d. A kernel command line argument it needs is named exactly, for
// this node, and the administrator adds it.

// ModulesFile is the drop-in Virticle owns: the modules to load at boot. It is written whole and never edited in
// place. Other files in /etc/modules-load.d and /etc/modprobe.d are never touched.
const ModulesFile = "/etc/modules-load.d/virticle.conf"

// modulesMark is in the header of the drop-in; a file without it is not ours.
const modulesMark = "Written by Virticle"

// KernelProbe prints what the kernel command line advice needs: the CPU vendor, the kernel command line and the
// boot loader. It only reads. The operating system's id and family come from os_id and os_like (FactsScript prints
// them; the passthrough check adds them).
const KernelProbe = `echo "cpu_vendor=$(grep -m1 -w vendor_id /proc/cpuinfo 2>/dev/null | cut -d: -f2 | tr -d ' ')"
echo "cmdline=$(cat /proc/cmdline 2>/dev/null)"
if ls /sys/firmware/efi/efivars/LoaderInfo-* >/dev/null 2>&1; then echo "bootloader=systemd-boot"
elif [ -f /etc/default/grub ]; then echo "bootloader=grub"
elif [ -f /etc/kernel/cmdline ]; then echo "bootloader=systemd-boot"
else echo "bootloader="; fi`

// OSProbe prints the operating system's id and family.
const OSProbe = `echo "os_id=$( . /etc/os-release 2>/dev/null; echo "$ID")"
echo "os_like=$( . /etc/os-release 2>/dev/null; echo "$ID_LIKE")"
echo "cpu_virt=$(grep -m1 -o -w -E 'vmx|svm' /proc/cpuinfo 2>/dev/null | head -n 1)"`

// modprobeDirs are the directories modprobe reads its configuration from.
const modprobeDirs = "/etc/modprobe.d /usr/lib/modprobe.d /lib/modprobe.d /run/modprobe.d"

// moduleProbe prints the state of the KVM modules and of the drop-in.
const moduleProbe = `for m in kvm_intel kvm_amd; do
  if grep -q "^$m " /proc/modules 2>/dev/null; then s=loaded; elif [ -d /sys/module/$m ]; then s=builtin; else s=absent; fi
  echo "kmod_$m=$s"
done
for m in kvm_intel kvm_amd; do
  b=$(grep -rE "^[[:space:]]*(blacklist|install)[[:space:]]+($m|$(echo $m | tr _ -))([[:space:]]|\$)" ` + modprobeDirs + ` 2>/dev/null | grep -v -e '--ignore-install' | head -n 1)
  echo "modblock_$m=$(echo "$b" | cut -d: -f1) $(echo "$b" | cut -d: -f2- | awk '{print $1}')"
done
echo "modules_file=$([ -f ` + ModulesFile + ` ] && head -c 2048 ` + ModulesFile + ` | tr '\n' '|')"`

// KernelFacts are what the kernel command line advice needs.
type KernelFacts struct {
	OSID, OSLike string
	CPUVendor    string // vendor_id of the first CPU ("GenuineIntel")
	CPUVirt      string // "vmx", "svm" or ""
	Cmdline      string
	Bootloader   string // "grub", "systemd-boot" or "" when unsure
}

// ParseKernelFacts reads the lines of KernelProbe and OSProbe.
func ParseKernelFacts(out string) KernelFacts {
	var k KernelFacts
	for _, l := range strings.Split(out, "\n") {
		key, v, ok := strings.Cut(strings.TrimRight(l, "\r"), "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch key {
		case "os_id":
			k.OSID = strings.ToLower(v)
		case "os_like":
			k.OSLike = strings.ToLower(v)
		case "cpu_vendor":
			k.CPUVendor = v
		case "cpu_virt":
			k.CPUVirt = v
		case "cmdline":
			k.Cmdline = v
		case "bootloader":
			k.Bootloader = v
		}
	}
	return k
}

// KernelInfo is the node's kernel facts, from what prepare read.
func (f Facts) KernelInfo() KernelFacts {
	return KernelFacts{OSID: f.OSID, OSLike: f.OSLike, CPUVendor: f.CPUVendor, CPUVirt: f.CPUVirt, Cmdline: f.Cmdline, Bootloader: f.Bootloader}
}

// vendor is "intel", "amd" or "" (not known).
func (k KernelFacts) vendor() string {
	switch k.CPUVendor {
	case "GenuineIntel":
		return "intel"
	case "AuthenticAMD", "HygonGenuine":
		return "amd"
	}
	switch k.CPUVirt {
	case "vmx":
		return "intel"
	case "svm":
		return "amd"
	}
	return ""
}

func (k KernelFacts) family() Family {
	return Classify(k.OSID, "", k.OSLike, "").Family
}

func hasArg(cmdline, arg string) bool {
	for _, a := range strings.Fields(cmdline) {
		if a == arg {
			return true
		}
	}
	return false
}

// IOMMUChange is what the kernel command line needs for device passthrough: arguments to add and arguments to
// remove. Both are empty when the command line is right.
type IOMMUChange struct {
	Add, Remove []string
}

// Empty reports whether nothing has to change.
func (c IOMMUChange) Empty() bool { return len(c.Add) == 0 && len(c.Remove) == 0 }

// IOMMUChange works out what this node's command line lacks: intel_iommu=on or amd_iommu=on by CPU vendor, and
// iommu=pt, minus what is there already. An argument that turns the IOMMU off is to be removed. For a CPU of unknown
// vendor both vendor arguments are named.
func (k KernelFacts) IOMMUChange() IOMMUChange {
	var c IOMMUChange
	for _, off := range []string{"intel_iommu=off", "amd_iommu=off", "iommu=off"} {
		if hasArg(k.Cmdline, off) {
			c.Remove = append(c.Remove, off)
		}
	}
	switch k.vendor() {
	case "intel":
		if !hasArg(k.Cmdline, "intel_iommu=on") {
			c.Add = append(c.Add, "intel_iommu=on")
		}
	case "amd":
		if !hasArg(k.Cmdline, "amd_iommu=on") {
			c.Add = append(c.Add, "amd_iommu=on")
		}
	default:
		if !hasArg(k.Cmdline, "intel_iommu=on") && !hasArg(k.Cmdline, "amd_iommu=on") {
			c.Add = append(c.Add, "intel_iommu=on (Intel CPU) or amd_iommu=on (AMD CPU)")
		}
	}
	if !hasArg(k.Cmdline, "iommu=pt") {
		c.Add = append(c.Add, "iommu=pt")
	}
	return c
}

// Words says the change in a sentence without a final stop: "add intel_iommu=on iommu=pt to the kernel command
// line", "remove intel_iommu=off from the kernel command line and add intel_iommu=on".
func (c IOMMUChange) Words() string {
	var parts []string
	if len(c.Remove) > 0 {
		parts = append(parts, "remove "+strings.Join(c.Remove, " ")+" from the kernel command line")
	}
	if len(c.Add) > 0 {
		s := "add " + strings.Join(c.Add, " ")
		if len(c.Remove) == 0 {
			s += " to the kernel command line"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " and ")
}

const neverBoot = " Virticle does not change the boot loader or reboot."

// Where says in one sentence where the change goes on this node's system. The wording is for what is known: GRUB
// on Debian and Ubuntu, GRUB on the RHEL family, systemd-boot; for anything else it says only "kernel command
// line". Virticle does not make the change.
func (k KernelFacts) Where(c IOMMUChange) string {
	switch {
	case k.Bootloader == "grub" && k.family() == FamilyDebian:
		return "Change GRUB_CMDLINE_LINUX_DEFAULT in /etc/default/grub, run update-grub and reboot." + neverBoot
	case k.Bootloader == "grub" && k.family() == FamilyRHEL && k.vendor() != "":
		args := ""
		if len(c.Add) > 0 {
			args += ` --args="` + strings.Join(c.Add, " ") + `"`
		}
		if len(c.Remove) > 0 {
			args += ` --remove-args="` + strings.Join(c.Remove, " ") + `"`
		}
		return "Run grubby --update-kernel=ALL" + args + " and reboot." + neverBoot
	case k.Bootloader == "systemd-boot":
		return "Change /etc/kernel/cmdline, refresh the boot entries and reboot." + neverBoot
	}
	return "Change the kernel command line and reboot." + neverBoot
}

// IOMMUAdvice is the text for a node without an active IOMMU: what to change, and where. ok is false when the
// command line already has everything (then the firmware is what keeps it off).
func (k KernelFacts) IOMMUAdvice() (what, where string, ok bool) {
	c := k.IOMMUChange()
	if c.Empty() {
		return "", "", false
	}
	return c.Words(), k.Where(c), true
}

// IOMMUFirmware says what to do in the firmware when the command line is right and there is still no IOMMU.
func (k KernelFacts) IOMMUFirmware() string {
	switch k.vendor() {
	case "intel":
		return "turn on VT-d in the firmware"
	case "amd":
		return "turn on the IOMMU (AMD-Vi) in the firmware"
	}
	return "turn on VT-d or the IOMMU (AMD-Vi) in the firmware"
}

// KVMBlock is an argument on the kernel command line that keeps the KVM module from loading.
type KVMBlock struct {
	Arg string // as it is on the command line
	Fix string // what to do: "remove module_blacklist=kvm_intel"
}

var kvmNames = map[string]bool{"kvm": true, "kvm_intel": true, "kvm_amd": true, "kvm-intel": true, "kvm-amd": true, "vmx_init": true, "svm_init": true}

// KVMBlocks finds arguments that make loading the KVM modules impossible: module_blacklist= (it refuses even a load
// by name) and initcall_blacklist= with the KVM init functions. modprobe.blacklist= only stops loading by alias, so
// it is not one.
func (k KernelFacts) KVMBlocks() []KVMBlock {
	var out []KVMBlock
	for _, a := range strings.Fields(k.Cmdline) {
		key, val, ok := strings.Cut(a, "=")
		if !ok || (key != "module_blacklist" && key != "initcall_blacklist") {
			continue
		}
		var bad, rest []string
		for _, n := range strings.Split(val, ",") {
			if kvmNames[n] {
				bad = append(bad, n)
			} else {
				rest = append(rest, n)
			}
		}
		switch {
		case len(bad) == 0:
		case len(rest) == 0:
			out = append(out, KVMBlock{a, "remove " + a})
		default:
			out = append(out, KVMBlock{a, "remove " + strings.Join(bad, ",") + " from " + a})
		}
	}
	return out
}

// KVMBlockFix is the text for a node that cannot load KVM because of its kernel command line, with where to
// change it; ok is false when nothing there blocks it.
func (k KernelFacts) KVMBlockFix() (what, where string, ok bool) {
	b := k.KVMBlocks()
	if len(b) == 0 {
		return "", "", false
	}
	var fixes, args []string
	for _, x := range b {
		fixes = append(fixes, x.Fix)
		args = append(args, x.Arg)
	}
	return strings.Join(fixes, " and ") + " on the kernel command line", k.Where(IOMMUChange{Remove: args}), true
}

// --- kernel modules ---

// kvmModule is the KVM module for a CPU: kvm_intel for vmx, kvm_amd for svm.
func kvmModule(virt string) string {
	switch virt {
	case "vmx":
		return "kvm_intel"
	case "svm":
		return "kvm_amd"
	}
	return ""
}

// ModuleFile is the drop-in's content for these modules.
func ModuleFile(mods []string) string {
	return "# " + modulesMark + " (virticle node prepare): the kernel modules this node needs at boot.\n" +
		"# Virticle owns this file and writes it whole. Put your own modules in another file.\n" +
		strings.Join(mods, "\n") + "\n"
}

// fileMods lists the modules in a drop-in given as its lines joined by "|", and whether it is ours.
func fileMods(raw string) (mods []string, ours bool) {
	for _, l := range strings.Split(raw, "|") {
		l = strings.TrimSpace(l)
		switch {
		case l == "":
		case strings.HasPrefix(l, "#"):
			if strings.Contains(l, modulesMark) {
				ours = true
			}
		case strings.HasPrefix(l, ";"):
		default:
			mods = append(mods, l)
		}
	}
	return mods, ours
}

// ModulesWanted lists the modules prepare loads and keeps loaded. Today that is the KVM module of the CPU, when
// the kernel has neither loaded it nor built it in. One that Virticle's own drop-in already lists stays listed (it is
// loaded because of the file). The other modules Virticle's features use (tun, vhost_net, vfio-pci, bridge, nbd,
// openvswitch) are loaded on demand by the kernel and libvirt, so they are not listed.
func ModulesWanted(f Facts) []string {
	m := kvmModule(f.CPUVirt)
	if m == "" {
		return nil
	}
	listed, ours := fileMods(f.ModulesFile)
	for _, l := range listed {
		if ours && l == m {
			return []string{m}
		}
	}
	if f.KMod[m] == "absent" {
		return []string{m}
	}
	return nil
}

// ModuleBlock is the modprobe.d file that blacklists the module or replaces its load, and which of the two it does.
func (f Facts) ModuleBlock(mod string) (file, kind string) {
	file, kind, _ = strings.Cut(f.ModBlock[mod], " ")
	return file, kind
}

// ModulesFileForeign reports whether the drop-in exists and is not ours.
func ModulesFileForeign(f Facts) bool {
	if f.ModulesFile == "" {
		return false
	}
	_, ours := fileMods(f.ModulesFile)
	return !ours
}

// ModuleFailure explains a failed module load. loadErr is modprobe's own message, klog the kernel log's lines about
// it, blocker the modprobe.d file that blacklists the module or replaces its load ("" when none), blockKind
// "blacklist" or "install", and k the node's kernel facts.
func ModuleFailure(mod, loadErr, klog, blocker, blockKind string, k KernelFacts) string {
	reason := strings.TrimSpace(loadErr)
	if kl := strings.TrimSpace(klog); kl != "" {
		reason = strings.TrimSpace(reason + " (kernel log: " + strings.ReplaceAll(kl, "\n", "; ") + ")")
	}
	low := strings.ToLower(reason)
	var fix string
	switch {
	case blocker != "" && blockKind == "blacklist":
		return mod + " is blacklisted in " + blocker + ", so the kernel would not load it at boot. Remove that line, then run prepare again"
	case blocker != "":
		return "the file " + blocker + " replaces the loading of " + mod + " by a command (install " + mod + "). Remove that line, then run prepare again"
	case strings.Contains(low, "disabled by bios"), strings.Contains(low, "disabled in bios"), strings.Contains(low, "disabled by firmware"):
		fix = "Turn on hardware virtualisation (Intel VT-x or AMD SVM) in the firmware, then run prepare again"
	case strings.Contains(low, "not found"):
		fix = "The running kernel has no " + mod + " module. Install the module package of the running kernel (or boot a kernel that has it), then run prepare again"
	default:
		fix = "Fix the cause shown, then run prepare again"
	}
	if what, where, ok := k.KVMBlockFix(); ok {
		fix = "The kernel command line blocks it: " + what + ". " + where
	}
	return fmt.Sprintf("could not load %s: %s. %s", mod, reason, fix)
}
