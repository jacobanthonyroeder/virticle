package optrace

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	libvirt "github.com/digitalocean/go-libvirt"
)

// procOf finds the procedure number of a go-libvirt method name.
func procOf(t *testing.T, name string) (program, proc uint32) {
	t.Helper()
	if strings.HasPrefix(name, "QEMU") {
		for n, s := range qemuProcs {
			if "QEMU"+s == name {
				return libvirt.ProgramQEMU, n
			}
		}
	}
	for n, s := range remoteProcs {
		if s == name {
			return libvirt.ProgramRemote, n
		}
	}
	t.Fatalf("no procedure %s", name)
	return 0, 0
}

func describe(t *testing.T, name string, args any) (Op, bool) {
	t.Helper()
	p, err := libvirt.EncodeArgs(args)
	if err != nil {
		t.Fatal(err)
	}
	prog, proc := procOf(t, name)
	return Describe(prog, proc, p)
}

var web1 = libvirt.Domain{Name: "web-1", ID: 3}

func TestVirshEquivalents(t *testing.T) {
	snapXML := "<domainsnapshot><name>before-upgrade</name><description>apt upgrade</description></domainsnapshot>"
	diskXML := "<disk type='file' device='disk'><driver name='qemu' type='qcow2'/><source file='/var/lib/libvirt/images/web-1-data.qcow2'/><target dev='vdb' bus='virtio'/></disk>"
	cases := []struct {
		name string
		args any
		api  string
		cmd  string
		file string
	}{
		{"DomainCreate", &libvirt.DomainCreateArgs{Dom: web1}, "virDomainCreate", "virsh start web-1", ""},
		{"DomainCreateWithFlags", &libvirt.DomainCreateWithFlagsArgs{Dom: web1, Flags: 1}, "virDomainCreateWithFlags", "virsh start web-1 --paused", ""},
		{"DomainShutdownFlags", &libvirt.DomainShutdownFlagsArgs{Dom: web1, Flags: 2}, "virDomainShutdownFlags", "virsh shutdown web-1 --mode agent", ""},
		{"DomainDestroyFlags", &libvirt.DomainDestroyFlagsArgs{Dom: web1, Flags: 1}, "virDomainDestroyFlags", "virsh destroy web-1 --graceful", ""},
		{"DomainUndefineFlags", &libvirt.DomainUndefineFlagsArgs{Dom: web1, Flags: 2 | 4 | 16}, "virDomainUndefineFlags", "virsh undefine web-1 --snapshots-metadata --nvram --checkpoints-metadata", ""},
		{"DomainDefineXMLFlags", &libvirt.DomainDefineXMLFlagsArgs{XML: "<domain type='kvm'><name>web-1</name></domain>", Flags: 1}, "virDomainDefineXMLFlags", "virsh define domain.xml --validate", "domain.xml"},
		{"DomainAttachDeviceFlags", &libvirt.DomainAttachDeviceFlagsArgs{Dom: web1, XML: diskXML, Flags: 3}, "virDomainAttachDeviceFlags", "virsh attach-device web-1 device.xml --live --config", "device.xml"},
		{"DomainSnapshotCreateXML", &libvirt.DomainSnapshotCreateXMLArgs{Dom: web1, XMLDesc: snapXML, Flags: 16 | 64}, "virDomainSnapshotCreateXML", "virsh snapshot-create-as web-1 before-upgrade --description 'apt upgrade' --disk-only --quiesce", ""},
		{"DomainSnapshotCreateXML", &libvirt.DomainSnapshotCreateXMLArgs{Dom: web1, XMLDesc: "<domainsnapshot><name>s1</name><memory snapshot='internal'/></domainsnapshot>", Flags: 0}, "virDomainSnapshotCreateXML", "virsh snapshot-create web-1 snapshot.xml", "snapshot.xml"},
		{"DomainSnapshotCreateXML", &libvirt.DomainSnapshotCreateXMLArgs{Dom: web1, XMLDesc: "<domainsnapshot><name>s1</name></domainsnapshot>", Flags: 1 | 2}, "virDomainSnapshotCreateXML", "virsh snapshot-create web-1 snapshot.xml --redefine --current", "snapshot.xml"},
		{"DomainSnapshotDelete", &libvirt.DomainSnapshotDeleteArgs{Snap: libvirt.DomainSnapshot{Name: "before-upgrade", Dom: web1}, Flags: 2}, "virDomainSnapshotDelete", "virsh snapshot-delete web-1 before-upgrade --metadata", ""},
		{"DomainRevertToSnapshot", &libvirt.DomainRevertToSnapshotArgs{Snap: libvirt.DomainSnapshot{Name: "s 1", Dom: web1}, Flags: 1}, "virDomainRevertToSnapshot", "virsh snapshot-revert web-1 's 1' --running", ""},
		{"DomainBackupBegin", &libvirt.DomainBackupBeginArgs{Dom: web1, BackupXML: "<domainbackup/>", CheckpointXML: libvirt.OptString{"<domaincheckpoint/>"}}, "virDomainBackupBegin", "virsh backup-begin web-1 --backupxml backup.xml --checkpointxml checkpoint.xml", "backup.xml"},
		{"DomainBlockJobAbort", &libvirt.DomainBlockJobAbortArgs{Dom: web1, Path: "vda", Flags: 2}, "virDomainBlockJobAbort", "virsh blockjob web-1 vda --abort --pivot", ""},
		{"DomainBlockResize", &libvirt.DomainBlockResizeArgs{Dom: web1, Disk: "vdb", Size: 21474836480, Flags: 1}, "virDomainBlockResize", "virsh blockresize web-1 vdb 21474836480B", ""},
		{"DomainBlockCopy", &libvirt.DomainBlockCopyArgs{Dom: web1, Path: "vda", Destxml: "<disk type='file'/>", Params: []libvirt.TypedParam{{Field: "bandwidth", Value: *libvirt.NewTypedParamValueUllong(1048576)}}, Flags: 2 | 4}, "virDomainBlockCopy", "virsh blockcopy web-1 vda --xml destination.xml --bandwidth 1048576 --bytes --reuse-external --transient-job", "destination.xml"},
		{"DomainFsfreeze", &libvirt.DomainFsfreezeArgs{Dom: web1}, "virDomainFSFreeze", "virsh domfsfreeze web-1", ""},
		{"DomainFsthaw", &libvirt.DomainFsthawArgs{Dom: web1}, "virDomainFSThaw", "virsh domfsthaw web-1", ""},
		{"DomainSetVcpusFlags", &libvirt.DomainSetVcpusFlagsArgs{Dom: web1, Nvcpus: 4, Flags: 2 | 4}, "virDomainSetVcpusFlags", "virsh setvcpus web-1 4 --config --maximum", ""},
		{"DomainSetMemoryFlags", &libvirt.DomainSetMemoryFlagsArgs{Dom: web1, Memory: 2097152, Flags: 2 | 4}, "virDomainSetMemoryFlags", "virsh setmaxmem web-1 2097152KiB --config", ""},
		{"DomainSetAutostart", &libvirt.DomainSetAutostartArgs{Dom: web1, Autostart: 0}, "virDomainSetAutostart", "virsh autostart web-1 --disable", ""},
		{"DomainPinVcpuFlags", &libvirt.DomainPinVcpuFlagsArgs{Dom: web1, Vcpu: 1, Cpumap: []byte{0x0f, 0x01}, Flags: 3}, "virDomainPinVcpuFlags", "virsh vcpupin web-1 1 0-3,8 --live --config", ""},
		{"DomainSetTime", &libvirt.DomainSetTimeArgs{Dom: web1, Flags: 1}, "virDomainSetTime", "virsh domtime web-1 --sync", ""},
		{"DomainSendKey", &libvirt.DomainSendKeyArgs{Dom: web1, Codeset: 0, Holdtime: 100, Keycodes: []uint32{29, 56, 111}}, "virDomainSendKey", "virsh send-key web-1 --codeset linux --holdtime 100 29 56 111", ""},
		{"DomainSetMetadata", &libvirt.DomainSetMetadataArgs{Dom: web1, Type: 2, Metadata: libvirt.OptString{"<v:tags>a</v:tags>"}, Key: libvirt.OptString{"v"}, Uri: libvirt.OptString{"https://virticle.dev/xml"}, Flags: 2}, "virDomainSetMetadata", "virsh metadata web-1 https://virticle.dev/xml --key v --set '<v:tags>a</v:tags>' --config", ""},
		{"DomainSetMetadata", &libvirt.DomainSetMetadataArgs{Dom: web1, Type: 0, Metadata: libvirt.OptString{"Web server"}, Flags: 3}, "virDomainSetMetadata", "virsh desc web-1 --new-desc 'Web server' --live --config", ""},
		{"NetworkDefineXMLFlags", &libvirt.NetworkDefineXMLFlagsArgs{XML: "<network><name>lan</name></network>"}, "virNetworkDefineXMLFlags", "virsh net-define network.xml", "network.xml"},
		{"NetworkCreate", &libvirt.NetworkCreateArgs{Net: libvirt.Network{Name: "lan"}}, "virNetworkCreate", "virsh net-start lan", ""},
		{"NetworkUpdate", &libvirt.NetworkUpdateArgs{Net: libvirt.Network{Name: "lan"}, Command: 3, Section: 4, ParentIndex: -1, XML: "<host mac='52:54:00:00:00:01' ip='10.0.0.5'/>", Flags: 3}, "virNetworkUpdate", "virsh net-update lan add-last ip-dhcp-host update.xml --live --config", "update.xml"},
		{"StoragePoolDefineXML", &libvirt.StoragePoolDefineXMLArgs{XML: "<pool type='dir'/>"}, "virStoragePoolDefineXML", "virsh pool-define pool.xml", "pool.xml"},
		{"StoragePoolBuild", &libvirt.StoragePoolBuildArgs{Pool: libvirt.StoragePool{Name: "images"}, Flags: 4}, "virStoragePoolBuild", "virsh pool-build images --no-overwrite", ""},
		{"StoragePoolCreate", &libvirt.StoragePoolCreateArgs{Pool: libvirt.StoragePool{Name: "images"}}, "virStoragePoolCreate", "virsh pool-start images", ""},
		{"StoragePoolRefresh", &libvirt.StoragePoolRefreshArgs{Pool: libvirt.StoragePool{Name: "images"}}, "virStoragePoolRefresh", "virsh pool-refresh images", ""},
		{"StorageVolCreateXML", &libvirt.StorageVolCreateXMLArgs{Pool: libvirt.StoragePool{Name: "images"}, XML: "<volume><name>a.qcow2</name></volume>"}, "virStorageVolCreateXML", "virsh vol-create images volume.xml", "volume.xml"},
		{"StorageVolCreateXMLFrom", &libvirt.StorageVolCreateXMLFromArgs{Pool: libvirt.StoragePool{Name: "images"}, XML: "<volume/>", Clonevol: libvirt.StorageVol{Pool: "base", Name: "debian.qcow2"}}, "virStorageVolCreateXMLFrom", "virsh vol-create-from images volume.xml --inputpool base debian.qcow2", "volume.xml"},
		{"StorageVolDelete", &libvirt.StorageVolDeleteArgs{Vol: libvirt.StorageVol{Pool: "images", Name: "web-1.qcow2"}}, "virStorageVolDelete", "virsh vol-delete web-1.qcow2 --pool images", ""},
		{"StorageVolResize", &libvirt.StorageVolResizeArgs{Vol: libvirt.StorageVol{Pool: "images", Name: "web-1.qcow2"}, Capacity: 10737418240}, "virStorageVolResize", "virsh vol-resize web-1.qcow2 10737418240B --pool images", ""},
		{"QEMUDomainAgentCommand", &libvirt.QEMUDomainAgentCommandArgs{Dom: web1, Cmd: `{"execute":"guest-fstrim"}`, Timeout: 15}, "virDomainQemuAgentCommand", `virsh qemu-agent-command web-1 --timeout 15 '{"execute":"guest-fstrim"}'`, ""},
	}
	for _, c := range cases {
		op, rec := describe(t, c.name, c.args)
		if !rec {
			t.Errorf("%s: not recorded", c.name)
			continue
		}
		if op.API != c.api || op.Cmd != c.cmd {
			t.Errorf("%s:\n got %s | %s\nwant %s | %s", c.name, op.API, op.Cmd, c.api, c.cmd)
		}
		if c.file != "" && (len(op.Files) == 0 || op.Files[0].Name != c.file) {
			t.Errorf("%s: files %+v, want %s", c.name, op.Files, c.file)
		}
		if op.Args != "" {
			t.Errorf("%s: args %q", c.name, op.Args)
		}
	}
}

func TestNoInventedCommand(t *testing.T) {
	// A migration phase has no virsh command of its own: the API name and its arguments only.
	op, rec := describe(t, "DomainMigratePerform3Params", &libvirt.DomainMigratePerform3ParamsArgs{
		Dom: web1, Params: []libvirt.TypedParam{
			{Field: "migrate_uri", Value: *libvirt.NewTypedParamValueString("tcp://10.0.0.2:49152")},
			{Field: "destination_xml", Value: *libvirt.NewTypedParamValueString("<domain><name>web-1</name></domain>")},
		}, Flags: 1 | 8 | 16 | 65536,
	})
	if !rec || op.Cmd != "" || op.API != "virDomainMigratePerform3Params" {
		t.Fatalf("got %+v", op)
	}
	want := "domain web-1, flags VIR_MIGRATE_LIVE|VIR_MIGRATE_PERSIST_DEST|VIR_MIGRATE_UNDEFINE_SOURCE|VIR_MIGRATE_TLS, migrate_uri=tcp://10.0.0.2:49152, destination_xml=destination-xml.xml"
	if op.Args != want {
		t.Errorf("args\n got %s\nwant %s", op.Args, want)
	}
	if len(op.Files) != 1 || op.Files[0].Name != "destination-xml.xml" {
		t.Errorf("files %+v", op.Files)
	}
	// A changing call that is not in the table: its name, nothing else.
	op, rec = describe(t, "DomainBlockPull", &libvirt.DomainBlockPullArgs{Dom: web1, Path: "vda"})
	if !rec || op.API != "virDomainBlockPull" || op.Cmd != "" {
		t.Errorf("unlisted call: %+v %v", op, rec)
	}
	// A flag without a virsh option is listed, not dropped.
	op, _ = describe(t, "DomainCreateWithFlags", &libvirt.DomainCreateWithFlagsArgs{Dom: web1, Flags: 16})
	if op.Cmd != "virsh start web-1" || !strings.Contains(op.Args, "0x10") {
		t.Errorf("unknown flag: %+v", op)
	}
}

func TestReadsAreNotRecorded(t *testing.T) {
	for _, n := range []string{"DomainGetXMLDesc", "ConnectListAllDomains", "DomainLookupByUUID", "DomainGetJobStats", "StoragePoolGetInfo",
		"DomainIsActive", "DomainSnapshotListNames", "ConnectGetAllDomainStats", "DomainOpenConsole", "DomainSnapshotCurrent", "DomainInterfaceAddresses",
		"NodeGetFreeMemory", "ConnectBaselineHypervisorCPU", "DomainMemoryStats"} {
		_, proc := procOf(t, n)
		if _, rec := Describe(libvirt.ProgramRemote, proc, nil); rec {
			t.Errorf("%s is recorded", n)
		}
	}
	// Agent and monitor commands that only query.
	for _, c := range []string{`{"execute":"guest-get-osinfo"}`, `{"execute":"guest-ping"}`, `{"execute": "guest-network-get-interfaces"}`} {
		if _, rec := describe(t, "QEMUDomainAgentCommand", &libvirt.QEMUDomainAgentCommandArgs{Dom: web1, Cmd: c}); rec {
			t.Errorf("%s is recorded", c)
		}
	}
	if _, rec := describe(t, "QEMUDomainMonitorCommand", &libvirt.QEMUDomainMonitorCommandArgs{Dom: web1, Cmd: `{"execute":"query-named-block-nodes"}`}); rec {
		t.Error("query-named-block-nodes is recorded")
	}
	if op, rec := describe(t, "QEMUDomainMonitorCommand", &libvirt.QEMUDomainMonitorCommandArgs{Dom: web1, Cmd: `{"execute":"block-dirty-bitmap-remove","arguments":{"node":"libvirt-1-format","name":"b1"}}`}); !rec || !strings.HasPrefix(op.Cmd, "virsh qemu-monitor-command web-1 ") {
		t.Errorf("bitmap removal: %+v %v", op, rec)
	}
}

// Every call the controller makes that changes something has a table entry; the list is what
// grep finds in internal/ (see the report of the change that added this package).
func TestControllerCallsAreCovered(t *testing.T) {
	changing := []string{"DomainAbortJob", "DomainAttachDeviceFlags", "DomainBackupBegin", "DomainBlockCopy", "DomainBlockJobAbort",
		"DomainBlockResize", "DomainCheckpointDelete", "DomainCreate", "DomainCreateWithFlags", "DomainDefineXMLFlags", "DomainDestroy",
		"DomainDestroyFlags", "DomainDetachDeviceFlags", "DomainFsfreeze", "DomainFsthaw", "DomainMigrateSetMaxDowntime", "DomainPinEmulator",
		"DomainPinVcpuFlags", "DomainReboot", "DomainReset", "DomainResume", "DomainRevertToSnapshot", "DomainSendKey", "DomainSetAutostart",
		"DomainSetMemoryFlags", "DomainSetMemoryStatsPeriod", "DomainSetMetadata", "DomainSetTime", "DomainSetVcpusFlags", "DomainShutdown",
		"DomainShutdownFlags", "DomainSnapshotCreateXML", "DomainSnapshotDelete", "DomainSuspend", "DomainUndefineFlags", "DomainUpdateDeviceFlags",
		"DomainMigrateBegin3Params", "DomainMigratePrepare3Params", "DomainMigratePerform3Params", "DomainMigrateFinish3Params",
		"DomainMigrateConfirm3Params", "QEMUDomainAgentCommand", "QEMUDomainMonitorCommand", "NetworkCreate", "NetworkDefineXMLFlags",
		"NetworkDestroy", "NetworkSetAutostart", "NetworkUndefine", "NetworkUpdate", "StoragePoolBuild", "StoragePoolCreate",
		"StoragePoolDefineXML", "StoragePoolDelete", "StoragePoolDestroy", "StoragePoolRefresh", "StoragePoolSetAutostart",
		"StoragePoolUndefine", "StorageVolCreateXML", "StorageVolCreateXMLFrom", "StorageVolDelete", "StorageVolResize", "StorageVolUpload"}
	for _, n := range changing {
		if _, ok := calls[n]; !ok {
			t.Errorf("%s has no entry", n)
		}
		if isRead(n) && n != "DomainSetMemoryStatsPeriod" {
			t.Errorf("%s counts as a read", n)
		}
	}
}

// procs.gen.go matches the vendored go-libvirt (run go generate ./internal/optrace after updating it).
func TestProcNamesMatchGoLibvirt(t *testing.T) {
	re := regexp.MustCompile(`(?m)^\t(QEMU)?Proc([A-Za-z0-9]+) = ([0-9]+)$`)
	for _, f := range []string{"remote_protocol.gen.go", "qemu_protocol.gen.go"} {
		b, err := os.ReadFile("../../third_party/go-libvirt/internal/constants/" + f)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			id, _ := strconv.Atoi(m[3])
			tab := remoteProcs
			if m[1] != "" {
				tab = qemuProcs
			}
			if tab[uint32(id)] != m[2] {
				t.Errorf("%s: procedure %d is %s, table has %q", f, id, m[2], tab[uint32(id)])
			}
			n++
		}
		if n < 7 {
			t.Errorf("%s: only %d procedures found", f, n)
		}
	}
}

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{"web-1": "web-1", "a b": "'a b'", "it's": `'it'\''s'`, "": "''", "/var/lib/x.qcow2": "/var/lib/x.qcow2", "$(id)": "'$(id)'"} {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
}
