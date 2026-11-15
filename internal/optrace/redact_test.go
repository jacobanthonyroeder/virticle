package optrace

import (
	"strings"
	"testing"
)

func TestRedactXML(t *testing.T) {
	domain := `<domain type='kvm'>
  <name>web-1</name>
  <sysinfo type='smbios'>
    <oemStrings>
      <entry>io.systemd.credential:passwd.plaintext-password.root=hunter2</entry>
    </oemStrings>
  </sysinfo>
  <sysinfo type='fwcfg'>
    <entry name='opt/com.coreos/config'>{"passwd":{"users":[{"name":"core","passwordHash":"$6$abc"}]}}</entry>
  </sysinfo>
  <devices>
    <graphics type='vnc' port='-1' autoport='yes' listen='127.0.0.1' passwd='vncS3cret' keymap='de'/>
    <graphics type="spice" autoport="yes" passwd="spiceS3cret" passwdValidTo="2026-11-16T00:00:00"/>
    <tpm model='tpm-crb'><backend type='emulator' version='2.0'><encryption secret='6dd3e4a5-1d76-44ce-961f-f119f5aad935'/></backend></tpm>
    <disk type='network' device='disk'>
      <source protocol='rbd' name='pool/img'><auth username='libvirt'><secret type='ceph' uuid='2a5b08e2-b6ee-4fa5-a5e6-a3b4a7f2b0d1'/></auth></source>
      <target dev='vda' bus='virtio'/>
    </disk>
  </devices>
  <launchSecurity type='sev'><dhCert>AAAAdhcertBase64</dhCert><session>AAAAsessionBase64</session><policy>0x0033</policy></launchSecurity>
  <qemu:commandline xmlns:qemu='http://libvirt.org/schemas/domain/qemu/1.0'>
    <qemu:arg value='-object'/>
    <qemu:arg value='secret,id=sec0,data=cGFzc3dvcmQ='/>
    <qemu:env name='HTTP_PROXY_PASSWORD' value='proxyS3cret'/>
  </qemu:commandline>
</domain>`
	got := RedactXML(domain)
	for _, secret := range []string{"hunter2", "passwordHash", "vncS3cret", "spiceS3cret", "6dd3e4a5", "AAAAdhcert", "AAAAsession", "cGFzc3dvcmQ", "proxyS3cret"} {
		if strings.Contains(got, secret) {
			t.Errorf("%s left in:\n%s", secret, got)
		}
	}
	for _, keep := range []string{"<name>web-1</name>", "keymap='de'", "passwd='" + Redacted + "'", `passwd="` + Redacted + `"`, "<policy>0x0033</policy>",
		"<qemu:arg value='-object'/>", "uuid='2a5b08e2-b6ee-4fa5-a5e6-a3b4a7f2b0d1'", "username='libvirt'", "<target dev='vda' bus='virtio'/>"} {
		if !strings.Contains(got, keep) {
			t.Errorf("%s missing:\n%s", keep, got)
		}
	}
}

func TestRedactXMLLeavesPlainDocuments(t *testing.T) {
	doc := `<volume type='file'><name>web-1.qcow2</name><key>/var/lib/libvirt/images/web-1.qcow2</key><capacity unit='bytes'>10737418240</capacity></volume>`
	if got := RedactXML(doc); got != doc {
		t.Errorf("changed:\n%s", got)
	}
}

func TestRedactCommand(t *testing.T) {
	cases := []struct{ in, want string }{
		{"virt-customize -a /var/lib/libvirt/images/web-1.qcow2 --root-password password:hunter2 --hostname web-1",
			"virt-customize -a /var/lib/libvirt/images/web-1.qcow2 --root-password " + Redacted + " --hostname web-1"},
		{"mysqladmin --password='s3 cret' status", "mysqladmin --password='" + Redacted + "' status"},
		{"tool --token=abc123 --name x", "tool --token=" + Redacted + " --name x"},
		{"sshpass -p hunter2 ssh root@node", "sshpass -p " + Redacted + " ssh root@node"},
		{"DB_PASSWORD=hunter2 AWS_SECRET_ACCESS_KEY='abc/def' ./run", "DB_PASSWORD=" + Redacted + " AWS_SECRET_ACCESS_KEY='" + Redacted + "' ./run"},
		{"curl -q -sIL --max-time 20 -- 'https://user:hunter2@example.com/disk.qcow2?X-Amz-Signature=abcdef&x=1'",
			"curl -q -sIL --max-time 20 -- 'https://user:" + Redacted + "@example.com/disk.qcow2?X-Amz-Signature=" + Redacted + "&x=1'"},
		{"sudo -n /usr/local/libexec/virticle-node clone-disk '/var/lib/libvirt/images/a.qcow2' 'b.qcow2'",
			"sudo -n /usr/local/libexec/virticle-node clone-disk '/var/lib/libvirt/images/a.qcow2' 'b.qcow2'"},
		{"test -d /sys/class/net/br0/bridge", "test -d /sys/class/net/br0/bridge"},
	}
	for _, c := range cases {
		if got := RedactCommand(c.in); got != c.want {
			t.Errorf("RedactCommand(%q)\n got %s\nwant %s", c.in, got, c.want)
		}
	}
}

func TestRedactPrivateKey(t *testing.T) {
	in := "printf '%s' '-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmU=\n-----END OPENSSH PRIVATE KEY-----' > /root/.ssh/id"
	got := RedactCommand(in)
	if strings.Contains(got, "b3BlbnNzaC") || strings.Contains(got, "PRIVATE KEY") {
		t.Errorf("key left in: %s", got)
	}
	// A key cut off before its end is removed to the end.
	if got := RedactText("x -----BEGIN RSA PRIVATE KEY-----\nMIIEow"); strings.Contains(got, "MIIEow") {
		t.Errorf("cut key left in: %s", got)
	}
}

func TestRedactJSON(t *testing.T) {
	in := `{"execute":"guest-set-user-password","arguments":{"username":"root","password":"aHVudGVyMg==","crypted":false}}`
	got := RedactJSON(in)
	if strings.Contains(got, "aHVudGVyMg") || !strings.Contains(got, `"crypted":false`) || !strings.Contains(got, `"username":"root"`) {
		t.Errorf("got %s", got)
	}
	in = `{"execute":"guest-exec","arguments":{"path":"/bin/sh","input-data":"ZWNobyBzM2NyZXQ=","capture-output":true}}`
	if got := RedactJSON(in); strings.Contains(got, "ZWNobyBzM2NyZXQ") || !strings.Contains(got, `"path":"/bin/sh"`) {
		t.Errorf("got %s", got)
	}
	if got := RedactJSON("not json password=x"); strings.Contains(got, "=x") {
		t.Errorf("got %s", got)
	}
}

func TestRedactText(t *testing.T) {
	for _, in := range []string{
		"internal error: unable to set password: hunter2",
		"auth failed for token=abc123",
		"cannot open https://u:hunter2@host/x",
	} {
		got := RedactText(in)
		if strings.Contains(got, "hunter2") || strings.Contains(got, "abc123") {
			t.Errorf("RedactText(%q) = %s", in, got)
		}
	}
	if got := RedactText("Requested operation is not valid: domain is not running"); got != "Requested operation is not valid: domain is not running" {
		t.Errorf("plain message changed: %s", got)
	}
}
