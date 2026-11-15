package optrace

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	libvirt "github.com/digitalocean/go-libvirt"
)

// The libvirt calls Virticle makes that change something, and the virsh command that does the same.
//
// A call is recorded unless it is a read (its name says so, see isRead). For a call in the table below the
// trace shows the virsh command a person would type for the same effect, run as root on the node; a call that
// is not in the table, or whose entry has no virsh command, shows the libvirt function and its main arguments
// only. The table never guesses: an entry exists only where a virsh command does what the call does, with the
// same flags. Flags without a virsh option are listed beside the line.

// Describe tells what a libvirt call does. rec is false for a read, which is not recorded.
func Describe(program, proc uint32, payload []byte) (op Op, rec bool) {
	var name string
	switch program {
	case libvirt.ProgramRemote:
		name = remoteProcs[proc]
	case libvirt.ProgramQEMU:
		if n := qemuProcs[proc]; n != "" {
			name = "QEMU" + n
		}
	}
	if name == "" {
		return Op{API: fmt.Sprintf("libvirt procedure %d", proc)}, true
	}
	op.API = APIName(name)
	c, ok := calls[name]
	if !ok {
		return op, !isRead(name)
	}
	l := &line{}
	if !c.build(payload, l) {
		return op, false
	}
	if l.bad {
		op.Args = "arguments could not be read"
		return op, true
	}
	if len(l.words) > 0 {
		op.Cmd = strings.Join(l.words, " ")
	}
	op.Args = strings.Join(l.args, ", ")
	op.Files = l.files
	return op, true
}

// APIName is the libvirt function of a go-libvirt method name: DomainSnapshotCreateXML is
// virDomainSnapshotCreateXML.
func APIName(goName string) string {
	if n, ok := apiNames[goName]; ok {
		return n
	}
	return "vir" + goName
}

// apiNames are the libvirt functions whose name go-libvirt spells differently.
var apiNames = map[string]string{
	"DomainFsfreeze":                    "virDomainFSFreeze",
	"DomainFsthaw":                      "virDomainFSThaw",
	"DomainFstrim":                      "virDomainFSTrim",
	"DomainGetFsinfo":                   "virDomainGetFSInfo",
	"QEMUDomainMonitorCommand":          "virDomainQemuMonitorCommand",
	"QEMUDomainMonitorCommandWithFiles": "virDomainQemuMonitorCommandWithFiles",
	"QEMUDomainAgentCommand":            "virDomainQemuAgentCommand",
	"QEMUDomainAttach":                  "virDomainQemuAttach",
}

var readRE = regexp.MustCompile(`Get|List|Lookup|Num|Peek|Stats|Info|Is[A-Z]|Has[A-Z]|Current|Screenshot|Download|Open|Addresses|Event|Dumpxml|Compare|Baseline|Ping`)

// isRead is true for the calls that only read: lists, lookups, XML and state, statistics, and everything on
// the connection itself (opening it, capabilities, event registrations).
func isRead(name string) bool {
	return strings.HasPrefix(name, "Connect") || strings.HasPrefix(name, "Auth") || readRE.MatchString(name)
}

// call is one table entry: build decodes the arguments and writes the line; false means the call is a read
// after all (a monitor command that only queries).
type call struct {
	build func(payload []byte, l *line) bool
}

// entry makes a call whose arguments are decoded into a T.
func entry[T any](f func(a *T, l *line)) call {
	return call{build: func(p []byte, l *line) bool {
		a := new(T)
		if err := libvirt.DecodeArgs(p, a); err != nil {
			l.bad = true
			return true
		}
		f(a, l)
		return true
	}}
}

// entryRead is entry for a call that is sometimes a read: f returns false then.
func entryRead[T any](f func(a *T, l *line) bool) call {
	return call{build: func(p []byte, l *line) bool {
		a := new(T)
		if err := libvirt.DecodeArgs(p, a); err != nil {
			l.bad = true
			return true
		}
		return f(a, l)
	}}
}

// line builds a virsh command line.
type line struct {
	words []string
	args  []string
	files []File
	bad   bool
}

// virsh starts the line: virsh, the command and its quoted arguments.
func (l *line) virsh(cmd string, args ...string) *line {
	l.words = append(l.words, "virsh", cmd)
	return l.q(args...)
}

// q adds quoted words.
func (l *line) q(w ...string) *line {
	for _, s := range w {
		l.words = append(l.words, Quote(s))
	}
	return l
}

// opt adds an option and its quoted value.
func (l *line) opt(name, v string) *line {
	l.words = append(l.words, name, Quote(v))
	return l
}

// file keeps an XML document under name (its secrets are removed when the operation is stored) and returns
// the name for the line.
func (l *line) file(name, xml string) string {
	l.files = append(l.files, File{Name: name, Text: xml})
	return name
}

// arg adds a main argument for a call without a virsh command.
func (l *line) arg(format string, v ...any) *line {
	l.args = append(l.args, fmt.Sprintf(format, v...))
	return l
}

// flag is one flag bit and its virsh option.
type flag struct {
	bit uint32
	opt string
}

// flags adds the options of the bits set in f; bits without an option are listed beside the line.
func (l *line) flags(f uint32, tab ...flag) *line {
	for _, t := range tab {
		if f&t.bit != 0 {
			if t.opt != "" {
				l.words = append(l.words, t.opt)
			}
			f &^= t.bit
		}
	}
	if f != 0 {
		l.arg("flags without a virsh option: %#x", f)
	}
	return l
}

// The flags of most calls that change a defined VM: what they apply to.
var impact = []flag{{1, "--live"}, {2, "--config"}}

// Quote quotes a word for a POSIX shell when it needs it.
func Quote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_./:=@%+,-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func opt(s libvirt.OptString) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

// cpuList is a CPU bit map as virsh writes it: 0-3,6.
func cpuList(m []byte) string {
	var parts []string
	start := -1
	n := len(m) * 8
	for i := 0; i <= n; i++ {
		set := i < n && m[i/8]&(1<<(i%8)) != 0
		switch {
		case set && start < 0:
			start = i
		case !set && start >= 0:
			if i-1 == start {
				parts = append(parts, strconv.Itoa(start))
			} else {
				parts = append(parts, fmt.Sprintf("%d-%d", start, i-1))
			}
			start = -1
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ",")
}

// params lists typed parameters, keeping XML values as files.
func (l *line) params(ps []libvirt.TypedParam) {
	for _, p := range ps {
		v := p.Value.I
		switch x := v.(type) {
		case string:
			if strings.HasPrefix(strings.TrimSpace(x), "<") {
				l.arg("%s=%s", p.Field, l.file(strings.ReplaceAll(p.Field, "_", "-")+".xml", x))
				continue
			}
			l.arg("%s=%s", p.Field, Quote(x))
		default:
			l.arg("%s=%v", p.Field, x)
		}
	}
}

var shutdownModes = []flag{{1, "--mode acpi"}, {2, "--mode agent"}, {4, "--mode initctl"}, {8, "--mode signal"}, {16, "--mode paravirt"}}

var migrateFlags = []struct {
	bit  uint32
	name string
}{
	{1, "LIVE"}, {2, "PEER2PEER"}, {4, "TUNNELLED"}, {8, "PERSIST_DEST"}, {16, "UNDEFINE_SOURCE"}, {32, "PAUSED"},
	{64, "NON_SHARED_DISK"}, {128, "NON_SHARED_INC"}, {256, "CHANGE_PROTECTION"}, {512, "UNSAFE"}, {1024, "OFFLINE"},
	{2048, "COMPRESSED"}, {4096, "ABORT_ON_ERROR"}, {8192, "AUTO_CONVERGE"}, {16384, "RDMA_PIN_ALL"},
	{32768, "POSTCOPY"}, {65536, "TLS"}, {131072, "PARALLEL"}, {262144, "NON_SHARED_SYNCHRONOUS_WRITES"},
	{524288, "POSTCOPY_RESUME"}, {1048576, "ZEROCOPY"},
}

// migFlags names migration flags as libvirt does (VIR_MIGRATE_LIVE|VIR_MIGRATE_PERSIST_DEST).
func migFlags(f uint32) string {
	var n []string
	for _, m := range migrateFlags {
		if f&m.bit != 0 {
			n = append(n, "VIR_MIGRATE_"+m.name)
			f &^= m.bit
		}
	}
	if f != 0 {
		n = append(n, fmt.Sprintf("%#x", f))
	}
	if len(n) == 0 {
		return "0"
	}
	return strings.Join(n, "|")
}

// migration is a phase of a migration: there is no virsh command for one phase (virsh migrate runs them all).
func migration(dom string, flags uint32, ps []libvirt.TypedParam, l *line) {
	if dom != "" {
		l.arg("domain %s", Quote(dom))
	}
	l.arg("flags %s", migFlags(flags))
	l.params(ps)
}

var snapNameRE = regexp.MustCompile(`(?s)^\s*<domainsnapshot>\s*<name>([^<]+)</name>\s*(?:<description>([^<]*)</description>\s*)?</domainsnapshot>\s*$`)

// readOnlyAgent are guest agent commands that only read.
var readOnlyAgent = regexp.MustCompile(`^(guest-get-|guest-info$|guest-ping$|guest-sync|guest-network-get-|guest-fsfreeze-status$|query-)`)

// executeOf is the "execute" of a QMP or guest agent command.
func executeOf(cmd string) string {
	i := strings.Index(cmd, `"execute"`)
	if i < 0 {
		return ""
	}
	rest := strings.TrimLeft(cmd[i+len(`"execute"`):], " \t\n:")
	rest = strings.TrimPrefix(rest, `"`)
	if j := strings.IndexByte(rest, '"'); j >= 0 {
		return rest[:j]
	}
	return ""
}

var calls = map[string]call{
	// --- VMs: life cycle ---
	"DomainCreate": entry(func(a *libvirt.DomainCreateArgs, l *line) { l.virsh("start", a.Dom.Name) }),
	"DomainCreateWithFlags": entry(func(a *libvirt.DomainCreateWithFlagsArgs, l *line) {
		l.virsh("start", a.Dom.Name).flags(a.Flags, flag{1, "--paused"}, flag{2, "--autodestroy"}, flag{4, "--bypass-cache"}, flag{8, "--force-boot"}, flag{32, "--reset-nvram"})
	}),
	"DomainShutdown": entry(func(a *libvirt.DomainShutdownArgs, l *line) { l.virsh("shutdown", a.Dom.Name) }),
	"DomainShutdownFlags": entry(func(a *libvirt.DomainShutdownFlagsArgs, l *line) {
		l.virsh("shutdown", a.Dom.Name).flags(uint32(a.Flags), shutdownModes...)
	}),
	"DomainReboot": entry(func(a *libvirt.DomainRebootArgs, l *line) {
		l.virsh("reboot", a.Dom.Name).flags(uint32(a.Flags), shutdownModes...)
	}),
	"DomainReset":   entry(func(a *libvirt.DomainResetArgs, l *line) { l.virsh("reset", a.Dom.Name).flags(a.Flags) }),
	"DomainDestroy": entry(func(a *libvirt.DomainDestroyArgs, l *line) { l.virsh("destroy", a.Dom.Name) }),
	"DomainDestroyFlags": entry(func(a *libvirt.DomainDestroyFlagsArgs, l *line) {
		l.virsh("destroy", a.Dom.Name).flags(uint32(a.Flags), flag{1, "--graceful"}, flag{2, "--remove-logs"})
	}),
	"DomainSuspend": entry(func(a *libvirt.DomainSuspendArgs, l *line) { l.virsh("suspend", a.Dom.Name) }),
	"DomainResume":  entry(func(a *libvirt.DomainResumeArgs, l *line) { l.virsh("resume", a.Dom.Name) }),
	"DomainAbortJob": entry(func(a *libvirt.DomainAbortJobArgs, l *line) {
		l.virsh("domjobabort", a.Dom.Name)
	}),
	"DomainSetAutostart": entry(func(a *libvirt.DomainSetAutostartArgs, l *line) {
		l.virsh("autostart", a.Dom.Name)
		if a.Autostart == 0 {
			l.q("--disable")
		}
	}),

	// --- VMs: definition and devices ---
	"DomainDefineXMLFlags": entry(func(a *libvirt.DomainDefineXMLFlagsArgs, l *line) {
		l.virsh("define", l.file("domain.xml", a.XML)).flags(uint32(a.Flags), flag{1, "--validate"})
	}),
	"DomainUndefineFlags": entry(func(a *libvirt.DomainUndefineFlagsArgs, l *line) {
		l.virsh("undefine", a.Dom.Name).flags(uint32(a.Flags), flag{1, "--managed-save"}, flag{2, "--snapshots-metadata"},
			flag{4, "--nvram"}, flag{8, "--keep-nvram"}, flag{16, "--checkpoints-metadata"}, flag{32, "--tpm"}, flag{64, "--keep-tpm"})
	}),
	"DomainAttachDeviceFlags": entry(func(a *libvirt.DomainAttachDeviceFlagsArgs, l *line) {
		l.virsh("attach-device", a.Dom.Name, l.file("device.xml", a.XML)).flags(a.Flags, impact...)
	}),
	"DomainDetachDeviceFlags": entry(func(a *libvirt.DomainDetachDeviceFlagsArgs, l *line) {
		l.virsh("detach-device", a.Dom.Name, l.file("device.xml", a.XML)).flags(a.Flags, impact...)
	}),
	"DomainUpdateDeviceFlags": entry(func(a *libvirt.DomainUpdateDeviceFlagsArgs, l *line) {
		l.virsh("update-device", a.Dom.Name, l.file("device.xml", a.XML)).flags(uint32(a.Flags), append(impact, flag{4, "--force"})...)
	}),
	"DomainSetMetadata": entry(func(a *libvirt.DomainSetMetadataArgs, l *line) {
		md := RedactXML(opt(a.Metadata))
		switch a.Type {
		case 0, 1: // description, title
			l.virsh("desc", a.Dom.Name)
			if a.Type == 1 {
				l.q("--title")
			}
			if len(a.Metadata) == 0 {
				l.opt("--new-desc", "")
			} else {
				l.opt("--new-desc", md)
			}
		default:
			l.virsh("metadata", a.Dom.Name, opt(a.Uri))
			if len(a.Metadata) == 0 {
				l.q("--remove")
			} else {
				l.opt("--key", opt(a.Key)).opt("--set", md)
			}
		}
		l.flags(uint32(a.Flags), impact...)
	}),
	"DomainSetMemoryFlags": entry(func(a *libvirt.DomainSetMemoryFlagsArgs, l *line) {
		if a.Flags&4 != 0 { // VIR_DOMAIN_MEM_MAXIMUM
			l.virsh("setmaxmem", a.Dom.Name, strconv.FormatUint(a.Memory, 10)+"KiB").flags(a.Flags&^4, impact...)
			return
		}
		l.virsh("setmem", a.Dom.Name, strconv.FormatUint(a.Memory, 10)+"KiB").flags(a.Flags, impact...)
	}),
	"DomainSetMemoryStatsPeriod": entry(func(a *libvirt.DomainSetMemoryStatsPeriodArgs, l *line) {
		l.virsh("dommemstat", a.Dom.Name).opt("--period", strconv.Itoa(int(a.Period))).flags(uint32(a.Flags), impact...)
	}),
	"DomainSetVcpusFlags": entry(func(a *libvirt.DomainSetVcpusFlagsArgs, l *line) {
		l.virsh("setvcpus", a.Dom.Name, strconv.Itoa(int(a.Nvcpus))).flags(a.Flags, append(impact, flag{4, "--maximum"}, flag{8, "--guest"}, flag{16, "--hotpluggable"})...)
	}),
	"DomainPinVcpuFlags": entry(func(a *libvirt.DomainPinVcpuFlagsArgs, l *line) {
		l.virsh("vcpupin", a.Dom.Name, strconv.Itoa(int(a.Vcpu)), cpuList(a.Cpumap)).flags(a.Flags, impact...)
	}),
	"DomainPinEmulator": entry(func(a *libvirt.DomainPinEmulatorArgs, l *line) {
		l.virsh("emulatorpin", a.Dom.Name, cpuList(a.Cpumap)).flags(uint32(a.Flags), impact...)
	}),
	"DomainSetTime": entry(func(a *libvirt.DomainSetTimeArgs, l *line) {
		if a.Flags&1 != 0 { // VIR_DOMAIN_TIME_SYNC
			l.virsh("domtime", a.Dom.Name).q("--sync").flags(uint32(a.Flags) &^ 1)
			return
		}
		l.virsh("domtime", a.Dom.Name).opt("--time", strconv.FormatInt(a.Seconds, 10)).flags(uint32(a.Flags))
		if a.Nseconds != 0 {
			l.arg("nanoseconds %d (virsh sets whole seconds)", a.Nseconds)
		}
	}),
	"DomainSendKey": entry(func(a *libvirt.DomainSendKeyArgs, l *line) {
		sets := []string{"linux", "xt", "atset1", "atset2", "atset3", "os_x", "xt_kbd", "usb", "win32", "rfb", "qnum"}
		set := strconv.Itoa(int(a.Codeset))
		if int(a.Codeset) < len(sets) {
			set = sets[a.Codeset]
		}
		l.virsh("send-key", a.Dom.Name).opt("--codeset", set)
		if a.Holdtime != 0 {
			l.opt("--holdtime", strconv.Itoa(int(a.Holdtime)))
		}
		for _, k := range a.Keycodes {
			l.q(strconv.Itoa(int(k)))
		}
		l.flags(a.Flags)
	}),

	// --- snapshots, checkpoints, backups, block jobs ---
	"DomainSnapshotCreateXML": entry(func(a *libvirt.DomainSnapshotCreateXMLArgs, l *line) {
		fl := []flag{{1, "--redefine"}, {2, "--current"}, {4, "--no-metadata"}, {8, "--halt"}, {16, "--disk-only"},
			{32, "--reuse-external"}, {64, "--quiesce"}, {128, "--atomic"}, {256, "--live"}, {512, "--validate"}}
		// A document with only a name (and a description) is what snapshot-create-as writes itself.
		if m := snapNameRE.FindStringSubmatch(a.XMLDesc); m != nil && a.Flags&3 == 0 { // create-as has no --redefine, --current
			l.virsh("snapshot-create-as", a.Dom.Name, xmlText(m[1]))
			if m[2] != "" {
				l.opt("--description", xmlText(m[2]))
			}
			l.flags(a.Flags, fl...)
			return
		}
		l.virsh("snapshot-create", a.Dom.Name, l.file("snapshot.xml", a.XMLDesc)).flags(a.Flags, fl...)
	}),
	"DomainSnapshotDelete": entry(func(a *libvirt.DomainSnapshotDeleteArgs, l *line) {
		l.virsh("snapshot-delete", a.Snap.Dom.Name, a.Snap.Name).flags(uint32(a.Flags), flag{1, "--children"}, flag{2, "--metadata"}, flag{4, "--children-only"})
	}),
	"DomainRevertToSnapshot": entry(func(a *libvirt.DomainRevertToSnapshotArgs, l *line) {
		l.virsh("snapshot-revert", a.Snap.Dom.Name, a.Snap.Name).flags(a.Flags, flag{1, "--running"}, flag{2, "--paused"}, flag{4, "--force"}, flag{8, "--reset-nvram"})
	}),
	"DomainCheckpointDelete": entry(func(a *libvirt.DomainCheckpointDeleteArgs, l *line) {
		l.virsh("checkpoint-delete", a.Checkpoint.Dom.Name, a.Checkpoint.Name).flags(uint32(a.Flags), flag{1, "--children"}, flag{2, "--metadata"}, flag{4, "--children-only"})
	}),
	"DomainBackupBegin": entry(func(a *libvirt.DomainBackupBeginArgs, l *line) {
		l.virsh("backup-begin", a.Dom.Name).opt("--backupxml", l.file("backup.xml", a.BackupXML))
		if cp := opt(a.CheckpointXML); cp != "" {
			l.opt("--checkpointxml", l.file("checkpoint.xml", cp))
		}
		l.flags(uint32(a.Flags), flag{1, "--reuse-external"})
	}),
	"DomainBlockCopy": entry(func(a *libvirt.DomainBlockCopyArgs, l *line) {
		l.virsh("blockcopy", a.Dom.Name, a.Path).opt("--xml", l.file("destination.xml", a.Destxml))
		bytes := false
		for _, p := range a.Params {
			switch p.Field {
			case "bandwidth":
				l.opt("--bandwidth", fmt.Sprint(p.Value.I))
				bytes = true
			case "granularity":
				l.opt("--granularity", fmt.Sprint(p.Value.I))
			case "buf-size":
				l.opt("--buf-size", fmt.Sprint(p.Value.I))
			default:
				l.arg("%s=%v", p.Field, p.Value.I)
			}
		}
		if bytes {
			l.q("--bytes")
		}
		l.flags(uint32(a.Flags), flag{1, "--shallow"}, flag{2, "--reuse-external"}, flag{4, "--transient-job"}, flag{8, "--synchronous-writes"})
	}),
	"DomainBlockJobAbort": entry(func(a *libvirt.DomainBlockJobAbortArgs, l *line) {
		l.virsh("blockjob", a.Dom.Name, a.Path).q("--abort").flags(uint32(a.Flags), flag{1, "--async"}, flag{2, "--pivot"})
	}),
	"DomainBlockResize": entry(func(a *libvirt.DomainBlockResizeArgs, l *line) {
		size := strconv.FormatUint(a.Size, 10) + "KiB"
		if a.Flags&1 != 0 { // VIR_DOMAIN_BLOCK_RESIZE_BYTES
			size = strconv.FormatUint(a.Size, 10) + "B"
		}
		l.virsh("blockresize", a.Dom.Name, a.Disk, size).flags(uint32(a.Flags)&^1, flag{2, "--capacity"})
	}),
	"DomainFsfreeze": entry(func(a *libvirt.DomainFsfreezeArgs, l *line) {
		l.virsh("domfsfreeze", a.Dom.Name)
		for _, m := range a.Mountpoints {
			l.opt("--mountpoint", m)
		}
		l.flags(a.Flags)
	}),
	"DomainFsthaw": entry(func(a *libvirt.DomainFsthawArgs, l *line) {
		l.virsh("domfsthaw", a.Dom.Name)
		for _, m := range a.Mountpoints {
			l.opt("--mountpoint", m)
		}
		l.flags(a.Flags)
	}),

	// --- migration: libvirt's phases; virsh migrate runs them all, so none has a virsh command of its own ---
	"DomainMigrateBegin3Params": entry(func(a *libvirt.DomainMigrateBegin3ParamsArgs, l *line) {
		migration(a.Dom.Name, a.Flags, a.Params, l)
	}),
	"DomainMigratePrepare3Params": entry(func(a *libvirt.DomainMigratePrepare3ParamsArgs, l *line) {
		migration("", a.Flags, a.Params, l)
	}),
	"DomainMigratePerform3Params": entry(func(a *libvirt.DomainMigratePerform3ParamsArgs, l *line) {
		migration(a.Dom.Name, uint32(a.Flags), a.Params, l)
	}),
	"DomainMigrateFinish3Params": entry(func(a *libvirt.DomainMigrateFinish3ParamsArgs, l *line) {
		migration("", a.Flags, a.Params, l)
		if a.Cancelled != 0 {
			l.arg("cancelled")
		}
	}),
	"DomainMigrateConfirm3Params": entry(func(a *libvirt.DomainMigrateConfirm3ParamsArgs, l *line) {
		migration(a.Dom.Name, a.Flags, a.Params, l)
		if a.Cancelled != 0 {
			l.arg("cancelled")
		}
	}),
	"DomainMigrateSetMaxDowntime": entry(func(a *libvirt.DomainMigrateSetMaxDowntimeArgs, l *line) {
		l.virsh("migrate-setmaxdowntime", a.Dom.Name, strconv.FormatUint(a.Downtime, 10)).flags(a.Flags)
	}),

	// --- QEMU monitor and guest agent: recorded unless the command only reads ---
	"QEMUDomainMonitorCommand": entryRead(func(a *libvirt.QEMUDomainMonitorCommandArgs, l *line) bool {
		if readOnlyAgent.MatchString(executeOf(a.Cmd)) {
			return false
		}
		l.virsh("qemu-monitor-command", a.Dom.Name, RedactJSON(a.Cmd)).flags(a.Flags, flag{1, "--hmp"})
		return true
	}),
	"QEMUDomainAgentCommand": entryRead(func(a *libvirt.QEMUDomainAgentCommandArgs, l *line) bool {
		if readOnlyAgent.MatchString(executeOf(a.Cmd)) {
			return false
		}
		l.virsh("qemu-agent-command", a.Dom.Name)
		if a.Timeout > 0 {
			l.opt("--timeout", strconv.Itoa(int(a.Timeout)))
		}
		l.q(RedactJSON(a.Cmd)).flags(a.Flags)
		return true
	}),

	// --- networks ---
	"NetworkDefineXMLFlags": entry(func(a *libvirt.NetworkDefineXMLFlagsArgs, l *line) {
		l.virsh("net-define", l.file("network.xml", a.XML)).flags(a.Flags, flag{1, "--validate"})
	}),
	"NetworkCreate":   entry(func(a *libvirt.NetworkCreateArgs, l *line) { l.virsh("net-start", a.Net.Name) }),
	"NetworkDestroy":  entry(func(a *libvirt.NetworkDestroyArgs, l *line) { l.virsh("net-destroy", a.Net.Name) }),
	"NetworkUndefine": entry(func(a *libvirt.NetworkUndefineArgs, l *line) { l.virsh("net-undefine", a.Net.Name) }),
	"NetworkSetAutostart": entry(func(a *libvirt.NetworkSetAutostartArgs, l *line) {
		l.virsh("net-autostart", a.Net.Name)
		if a.Autostart == 0 {
			l.q("--disable")
		}
	}),
	"NetworkUpdate": entry(func(a *libvirt.NetworkUpdateArgs, l *line) {
		cmds := map[uint32]string{1: "modify", 2: "delete", 3: "add-last", 4: "add-first"}
		secs := map[uint32]string{1: "bridge", 2: "domain", 3: "ip", 4: "ip-dhcp-host", 5: "ip-dhcp-range", 6: "forward",
			7: "forward-interface", 8: "forward-pf", 9: "portgroup", 10: "dns-host", 11: "dns-txt", 12: "dns-srv"}
		c, s := cmds[a.Command], secs[a.Section]
		if c == "" || s == "" {
			l.arg("network %s, command %d, section %d", Quote(a.Net.Name), a.Command, a.Section)
			l.file("update.xml", a.XML)
			return
		}
		l.virsh("net-update", a.Net.Name, c, s, l.file("update.xml", a.XML))
		if a.ParentIndex >= 0 {
			l.opt("--parent-index", strconv.Itoa(int(a.ParentIndex)))
		}
		l.flags(uint32(a.Flags), impact...)
	}),

	// --- storage pools ---
	"StoragePoolDefineXML": entry(func(a *libvirt.StoragePoolDefineXMLArgs, l *line) {
		l.virsh("pool-define", l.file("pool.xml", a.XML)).flags(a.Flags, flag{1, "--validate"})
	}),
	"StoragePoolBuild": entry(func(a *libvirt.StoragePoolBuildArgs, l *line) {
		l.virsh("pool-build", a.Pool.Name).flags(uint32(a.Flags), flag{4, "--no-overwrite"}, flag{8, "--overwrite"})
	}),
	"StoragePoolCreate": entry(func(a *libvirt.StoragePoolCreateArgs, l *line) {
		l.virsh("pool-start", a.Pool.Name).flags(uint32(a.Flags), flag{1, "--build"}, flag{2, "--overwrite"}, flag{4, "--no-overwrite"})
	}),
	"StoragePoolDestroy":  entry(func(a *libvirt.StoragePoolDestroyArgs, l *line) { l.virsh("pool-destroy", a.Pool.Name) }),
	"StoragePoolUndefine": entry(func(a *libvirt.StoragePoolUndefineArgs, l *line) { l.virsh("pool-undefine", a.Pool.Name) }),
	"StoragePoolDelete": entry(func(a *libvirt.StoragePoolDeleteArgs, l *line) {
		l.virsh("pool-delete", a.Pool.Name).flags(uint32(a.Flags))
	}),
	"StoragePoolRefresh": entry(func(a *libvirt.StoragePoolRefreshArgs, l *line) {
		l.virsh("pool-refresh", a.Pool.Name).flags(a.Flags)
	}),
	"StoragePoolSetAutostart": entry(func(a *libvirt.StoragePoolSetAutostartArgs, l *line) {
		l.virsh("pool-autostart", a.Pool.Name)
		if a.Autostart == 0 {
			l.q("--disable")
		}
	}),

	// --- volumes ---
	"StorageVolCreateXML": entry(func(a *libvirt.StorageVolCreateXMLArgs, l *line) {
		l.virsh("vol-create", a.Pool.Name, l.file("volume.xml", a.XML)).flags(uint32(a.Flags), flag{1, "--prealloc-metadata"}, flag{4, "--validate"})
	}),
	"StorageVolCreateXMLFrom": entry(func(a *libvirt.StorageVolCreateXMLFromArgs, l *line) {
		l.virsh("vol-create-from", a.Pool.Name, l.file("volume.xml", a.XML)).opt("--inputpool", a.Clonevol.Pool).q(a.Clonevol.Name).
			flags(uint32(a.Flags), flag{1, "--prealloc-metadata"}, flag{2, "--reflink"}, flag{4, "--validate"})
	}),
	"StorageVolDelete": entry(func(a *libvirt.StorageVolDeleteArgs, l *line) {
		l.virsh("vol-delete", a.Vol.Name).opt("--pool", a.Vol.Pool).flags(uint32(a.Flags), flag{2, "--delete-snapshots"})
	}),
	"StorageVolResize": entry(func(a *libvirt.StorageVolResizeArgs, l *line) {
		l.virsh("vol-resize", a.Vol.Name, strconv.FormatUint(a.Capacity, 10)+"B").opt("--pool", a.Vol.Pool).
			flags(uint32(a.Flags), flag{1, "--allocate"}, flag{2, "--delta"}, flag{4, "--shrink"})
	}),
	"StorageVolUpload": entry(func(a *libvirt.StorageVolUploadArgs, l *line) {
		l.virsh("vol-upload", a.Vol.Name, "FILE").opt("--pool", a.Vol.Pool)
		if a.Offset != 0 {
			l.opt("--offset", strconv.FormatUint(a.Offset, 10))
		}
		if a.Length != 0 {
			l.opt("--length", strconv.FormatUint(a.Length, 10))
		}
		l.flags(uint32(a.Flags), flag{1, "--sparse"})
		l.arg("FILE: the data the controller sent")
	}),
}

// xmlText undoes the escapes of XML text.
func xmlText(s string) string {
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&quot;", `"`, "&apos;", "'", "&amp;", "&").Replace(s)
}
