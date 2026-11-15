package optrace

import "testing"

func TestReadOnlyCommand(t *testing.T) {
	const node = "sudo -n /usr/local/libexec/virticle-node "
	for cmd, want := range map[string]bool{
		// the node command: by verb
		node + "stat '/var/lib/libvirt/images/a.qcow2'": true,
		node + "qemu-img-info '/x'":                     true,
		node + "read '/x'":                              true,
		node + "files-size '/d' 'a' 'b'":                true,
		node + "tools-missing":                          true,
		node + "backup-scan '/d' 'M'":                   true,
		node + "selfcheck":                              true,
		node + "selfcheck repair":                       false,
		node + "copy '/a' '/b'":                         false,
		node + "write '/x'":                             false,
		node + "net-apply":                              false,
		node + "no-such-verb":                           false, // an unknown verb is recorded
		node:                                            false,
		"/usr/local/libexec/virticle-node df '/x'":      true,
		"timeout 480 " + node + "sha256 '/x' progress":  true,
		"timeout 480 " + node + "tar-unpack '/d'":       false,
		// plain commands that read
		"command -v 'qemu-img'":                                      true,
		"test -d /sys/class/net/br0/bridge":                          true,
		"test -e /sys/class/net/'eth0'":                              true,
		"stat -c '%u %a' '/mnt/x'":                                   true,
		"cat /proc/sys/kernel/random/boot_id":                        true,
		"cut -d. -f1 /proc/uptime":                                   true,
		"date +%s%N":                                                 true,
		"echo uid=$(id -u); echo user=$(id -un)":                     true,
		"sudo -n true":                                               true,
		"timeout 5 ls -f '/d' >/dev/null 2>&1; true":                 true,
		"timeout 5 getent ahostsv4 'h' | awk '{print $1}' | sort -u": true,
		"ip -j route get 192.0.2.1":                                  true,
		"systemctl is-active libvirtd":                               true,
		"awk -v m='/mnt' '$2==m{print $4}' /proc/mounts | tail -n 1": true,
		// plain commands that change something, or that this cannot tell
		"rm -f /tmp/x":                    false,
		"test -d /x && rm -rf /x":         false,
		"cat /a > /b":                     false,
		"cat /a >> /b":                    false,
		"echo x | tee /b":                 false,
		"echo $(rm /x)":                   false,
		"ip link set eth0 down":           false,
		"ip addr add 1.2.3.4/24 dev eth0": false,
		"systemctl restart libvirtd":      false,
		"sort -o /x /y":                   false,
		"date -s 12:00":                   false,
		"hostname newname":                false,
		"awk 'BEGIN{system(\"rm x\")}'":   false,
		"t=$(mktemp) || exit 1; cat /x":   false,
		"virsh list":                      false,
		"sh -c 'ls'":                      false,
		"curl -q http://x":                false,
		"":                                false,
		"   ":                             false,
	} {
		if got := ReadOnlyCommand(cmd); got != want {
			t.Errorf("ReadOnlyCommand(%q) = %v, want %v", cmd, got, want)
		}
	}
}

// A read-only command is not recorded; a changing one is.
func TestExecSkipsReads(t *testing.T) {
	var s sinkRec
	tr := New(s.add)
	if done := tr.Exec("n", "sudo -n /usr/local/libexec/virticle-node stat '/x'"); done != nil {
		t.Error("a read was recorded")
	}
	if done := tr.Exec("n", "command -v 'qemu-img'"); done != nil {
		t.Error("a read was recorded")
	}
	done := tr.Exec("n", "sudo -n /usr/local/libexec/virticle-node copy '/a' '/b'")
	if done == nil {
		t.Fatal("a change was not recorded")
	}
	done(0, nil)
	if len(s.ops) != 1 || s.ops[0].Seq != 1 {
		t.Errorf("%+v", s.ops)
	}
}
