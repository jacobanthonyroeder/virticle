package prepare

import (
	"strings"
	"testing"
)

// noKVM is Debian 13 on an Intel CPU whose KVM module is not loaded.
func noKVM(extra string) string {
	f := strings.Replace(debian13Facts, "kvm_dev=660 root:kvm", "kvm_dev=", 1)
	return f + "cpu_vendor=GenuineIntel\nbootloader=grub\nkmod_kvm_intel=absent\nkmod_kvm_amd=absent\n" + extra
}

func TestKVMModuleMissingIsLoadedByPrepare(t *testing.T) {
	rows := Evaluate(plan(noKVM("")), Options{ControllerKey: testKey})
	r := find(rows, "kvm")
	if r.Status != Info || !strings.Contains(r.Detail, "prepare loads kvm_intel") || !strings.Contains(r.Detail, "/etc/modules-load.d/virticle.conf") {
		t.Errorf("kvm: %+v", r)
	}
	if HasError(rows) {
		t.Errorf("a missing module is not an error: %+v", rows)
	}
	amd := strings.Replace(noKVM(""), "cpu_virt=vmx", "cpu_virt=svm", 1)
	if r := find(Evaluate(plan(amd), Options{ControllerKey: testKey}), "kvm"); !strings.Contains(r.Detail, "kvm_amd") {
		t.Errorf("kvm on AMD: %+v", r)
	}
}

func TestKVMLoadedButNoDevice(t *testing.T) {
	f := strings.Replace(noKVM(""), "kmod_kvm_intel=absent", "kmod_kvm_intel=loaded", 1)
	r := find(Evaluate(plan(f), Options{ControllerKey: testKey}), "kvm")
	if r.Status != Error || !strings.Contains(r.Detail, "although kvm_intel is loaded") {
		t.Errorf("kvm: %+v", r)
	}
}

func TestKernelArgumentBlocksKVM(t *testing.T) {
	f := noKVM("cmdline=BOOT_IMAGE=/vmlinuz ro quiet module_blacklist=nouveau,kvm_intel\n")
	r := find(Evaluate(plan(f), Options{ControllerKey: testKey}), "kvm")
	if r.Status != Error || !strings.Contains(r.Detail, "remove kvm_intel from module_blacklist=nouveau,kvm_intel") ||
		!strings.Contains(r.Help, "update-grub") {
		t.Errorf("kvm: %+v", r)
	}
	f = noKVM("cmdline=ro initcall_blacklist=vmx_init\n")
	if r := find(Evaluate(plan(f), Options{ControllerKey: testKey}), "kvm"); !strings.Contains(r.Detail, "remove initcall_blacklist=vmx_init") {
		t.Errorf("kvm: %+v", r)
	}
	// modprobe.blacklist only stops loading by alias: prepare loads by name.
	f = noKVM("cmdline=ro modprobe.blacklist=kvm_intel\n")
	if r := find(Evaluate(plan(f), Options{ControllerKey: testKey}), "kvm"); r.Status != Info {
		t.Errorf("kvm: %+v", r)
	}
}

func TestBlacklistedModuleIsAnErrorInThePlan(t *testing.T) {
	rows := Evaluate(plan(noKVM("modblock_kvm_intel=/etc/modprobe.d/x.conf blacklist\nmodblock_kvm_amd= \n")), Options{ControllerKey: testKey})
	r := find(rows, "kvm")
	if r.Status != Error || !strings.Contains(r.Detail, "kvm_intel is blacklisted in /etc/modprobe.d/x.conf") {
		t.Errorf("kvm: %+v", r)
	}
	// The blacklist of the module that is not wanted does not matter.
	rows = Evaluate(plan(noKVM("modblock_kvm_amd=/etc/modprobe.d/y.conf blacklist\n")), Options{ControllerKey: testKey})
	if r := find(rows, "kvm"); r.Status != Info {
		t.Errorf("kvm: %+v", r)
	}
}

func TestModulesWanted(t *testing.T) {
	file := "modules_file=# Written by Virticle (virticle node prepare)|# x|kvm_intel|\n"
	cases := []struct {
		name, facts string
		want        string
	}{
		{"absent", noKVM(""), "kvm_intel"},
		{"loaded by the system", strings.Replace(noKVM(""), "kmod_kvm_intel=absent", "kmod_kvm_intel=loaded", 1), ""},
		{"built in", strings.Replace(noKVM(""), "kmod_kvm_intel=absent", "kmod_kvm_intel=builtin", 1), ""},
		{"loaded because of our file", strings.Replace(noKVM(file), "kmod_kvm_intel=absent", "kmod_kvm_intel=loaded", 1), "kvm_intel"},
		{"listed in a file that is not ours", strings.Replace(noKVM("modules_file=kvm_intel|\n"), "kmod_kvm_intel=absent", "kmod_kvm_intel=loaded", 1), ""},
		{"no virtualisation", strings.Replace(noKVM(""), "cpu_virt=vmx", "cpu_virt=", 1), ""},
	}
	for _, c := range cases {
		got := strings.Join(ModulesWanted(ParseFacts(c.facts)), " ")
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func iommuRow(vendor, cmd, boot, osid, like string) Row {
	f := strings.Replace(debian13Facts, "os_id=debian", "os_id="+osid, 1)
	f = strings.Replace(f, "os_like=", "os_like="+like, 1)
	f += "cpu_vendor=" + vendor + "\ncmdline=" + cmd + "\nbootloader=" + boot + "\n"
	return find(Evaluate(plan(f), Options{ControllerKey: testKey}), "iommu")
}

func TestIOMMURowPerSystem(t *testing.T) {
	r := iommuRow("GenuineIntel", "ro quiet", "grub", "debian", "")
	if r.Status != Info || r.Detail != "Off. For device passthrough: add intel_iommu=on iommu=pt to the kernel command line" ||
		!strings.Contains(r.Help, "GRUB_CMDLINE_LINUX_DEFAULT in /etc/default/grub, run update-grub") {
		t.Errorf("intel debian: %+v", r)
	}
	r = iommuRow("AuthenticAMD", "ro quiet", "grub", "rocky", "rhel centos fedora")
	if !strings.Contains(r.Detail, "add amd_iommu=on iommu=pt") || strings.Contains(r.Detail, "intel") ||
		!strings.Contains(r.Help, `grubby --update-kernel=ALL --args="amd_iommu=on iommu=pt"`) {
		t.Errorf("amd rocky: %+v", r)
	}
	if r = iommuRow("GenuineIntel", "ro", "systemd-boot", "debian", ""); !strings.Contains(r.Help, "/etc/kernel/cmdline") {
		t.Errorf("systemd-boot: %+v", r)
	}
	r = iommuRow("GenuineIntel", "ro", "", "arch", "")
	if r.Help != "Change the kernel command line and reboot. Virticle does not change the boot loader or reboot." {
		t.Errorf("unknown: %+v", r)
	}
}

func TestIOMMURowArgumentsAlreadyThere(t *testing.T) {
	r := iommuRow("GenuineIntel", "ro iommu=pt", "grub", "debian", "")
	if !strings.Contains(r.Detail, "add intel_iommu=on to the") || strings.Contains(r.Detail, "iommu=pt") {
		t.Errorf("partly there: %+v", r)
	}
	// An argument that turns it off is to be removed.
	r = iommuRow("GenuineIntel", "ro intel_iommu=off", "grub", "rocky", "rhel fedora")
	if !strings.Contains(r.Detail, "remove intel_iommu=off from the kernel command line and add intel_iommu=on iommu=pt") ||
		!strings.Contains(r.Help, `--remove-args="intel_iommu=off"`) {
		t.Errorf("off: %+v", r)
	}
	// Command line right, still no groups: the firmware.
	r = iommuRow("GenuineIntel", "ro intel_iommu=on iommu=pt", "grub", "debian", "")
	if !strings.Contains(r.Detail, "turn on VT-d in the firmware") || r.Help != "" {
		t.Errorf("firmware: %+v", r)
	}
	// On: nothing to say.
	f := strings.Replace(debian13Facts, "iommu_groups=0", "iommu_groups=12", 1)
	if r := find(Evaluate(plan(f), Options{ControllerKey: testKey}), "iommu"); !strings.Contains(r.Detail, "On (12 groups)") || r.Help != "" {
		t.Errorf("on: %+v", r)
	}
}

func TestUnknownCPUVendorNamesBoth(t *testing.T) {
	k := KernelFacts{OSID: "debian", Bootloader: "grub", Cmdline: "ro"}
	what, _, ok := k.IOMMUAdvice()
	if !ok || !strings.Contains(what, "intel_iommu=on (Intel CPU) or amd_iommu=on (AMD CPU)") || !strings.Contains(what, "iommu=pt") {
		t.Errorf("%q", what)
	}
}

func TestParseKernelFacts(t *testing.T) {
	k := ParseKernelFacts("os_id=Rocky\nos_like=rhel fedora\ncpu_vendor=AuthenticAMD\ncpu_virt=svm\ncmdline=BOOT_IMAGE=(hd0,gpt2)/vmlinuz root=/dev/mapper/r-root ro\nbootloader=grub\nok\n")
	if k.OSID != "rocky" || k.CPUVendor != "AuthenticAMD" || k.Bootloader != "grub" || !strings.HasSuffix(k.Cmdline, "root=/dev/mapper/r-root ro") {
		t.Errorf("%+v", k)
	}
}

func moduleHost(facts, load string) (*fakeNode, []Row) {
	n := hostOnDebian(true, func(cmd, stdin string) (string, bool, error) {
		switch {
		case strings.Contains(cmd, "os_id="):
			return facts, true, nil
		case strings.Contains(cmd, `modprobe "$m"`):
			return load, true, nil
		}
		return "", false, nil
	})
	rows := (&Runner{N: n}).Prepare(Options{ControllerKey: testKey}, nil, nil)
	return n, rows
}

func logHas(n *fakeNode, parts ...string) bool {
	for _, l := range n.log {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(l, p)
		}
		if ok {
			return true
		}
	}
	return false
}

func TestStepKernelLoadsAndWritesDropIn(t *testing.T) {
	n, rows := moduleHost(noKVM(""), "result=ok\n")
	r := find(rows, "do-kernel")
	if r.Status != OK || !strings.Contains(r.Detail, "loaded kvm_intel") {
		t.Errorf("do-kernel: %+v", r)
	}
	if !logHas(n, "kvm_intel", `modprobe "$m"`) {
		t.Error("the module was not loaded by name")
	}
	if !logHas(n, ModulesFile, "--stdin--\n# Written by Virticle", "\nkvm_intel\n") {
		t.Error("the drop-in was not written whole with its header")
	}
	if logHas(n, "modprobe.d", "rm ") || logHas(n, "options kvm") {
		t.Error("nothing else is touched")
	}
}

func TestStepKernelFailureNamesTheKernelsReason(t *testing.T) {
	_, rows := moduleHost(noKVM(""), "result=fail\nerr=modprobe: ERROR: could not insert 'kvm_intel': Operation not supported\nklog=kvm: disabled by bios\n")
	r := find(rows, "do-kernel")
	if r.Status != Error || !strings.Contains(r.Detail, "kvm: disabled by bios") || !strings.Contains(r.Detail, "Turn on hardware virtualisation") {
		t.Errorf("do-kernel: %+v", r)
	}
	if find(rows, "do-key").Status == OK {
		t.Error("the run stops at the failed step")
	}
	_, rows = moduleHost(noKVM(""), "result=fail\nblocker=/etc/modprobe.d/x.conf\nblockkind=blacklist\n")
	if r := find(rows, "do-kernel"); r.Status != Error || r.Detail != "kvm_intel is blacklisted in /etc/modprobe.d/x.conf, so the kernel would not load it at boot. Remove that line, then run prepare again" {
		t.Errorf("blacklist: %+v", r)
	}
	_, rows = moduleHost(noKVM(""), "result=fail\nblocker=/etc/modprobe.d/x.conf\nblockkind=install\n")
	if r := find(rows, "do-kernel"); !strings.Contains(r.Detail, "replaces the loading of kvm_intel by a command") {
		t.Errorf("install: %+v", r)
	}
}

func TestStepKernelLeavesAForeignFileAlone(t *testing.T) {
	n, rows := moduleHost(noKVM("modules_file=kvm_intel|\n"), "result=ok\n")
	r := find(rows, "do-kernel")
	if r.Status != Warning || !strings.Contains(r.Detail, "was not written by Virticle") {
		t.Errorf("do-kernel: %+v", r)
	}
	if logHas(n, ModulesFile, "--stdin--\n# Written") {
		t.Error("a file that is not ours was written")
	}
}

func TestStepKernelNothingToDo(t *testing.T) {
	n, rows := moduleHost(debian13Facts+"cpu_vendor=GenuineIntel\nkmod_kvm_intel=loaded\n", "")
	r := find(rows, "do-kernel")
	if r.Status != OK || !strings.Contains(r.Detail, "kvm_intel is loaded by the system") {
		t.Errorf("do-kernel: %+v", r)
	}
	if logHas(n, `modprobe "$m"`) || logHas(n, ModulesFile, "--stdin--\n# Written") {
		t.Error("nothing should be loaded or written")
	}
}

func TestUnprepareRemovesOnlyOurModuleFile(t *testing.T) {
	if !strings.Contains(unprepareScript, "head -c 400 "+ModulesFile+" | grep -q 'Written by Virticle'") {
		t.Error("unprepare removes the drop-in only when it is ours")
	}
	if titleOf("kernel module list removed") != "Kernel modules" {
		t.Error("title")
	}
}
