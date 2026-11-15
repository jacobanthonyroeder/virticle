package optrace

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	libvirt "github.com/digitalocean/go-libvirt"
)

type sinkRec struct {
	mu  sync.Mutex
	ids map[int64]bool
	ops []Op
}

func (s *sinkRec) add(id int64, op Op) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ids == nil {
		s.ids = map[int64]bool{}
	}
	s.ids[id] = true
	s.ops = append(s.ops, op)
}

func TestTraceRecordsLibvirtCalls(t *testing.T) {
	var s sinkRec
	tr := New(s.add)
	if tr.Assigned() != 0 {
		t.Fatal("a new trace has an id")
	}
	p, _ := libvirt.EncodeArgs(&libvirt.DomainSnapshotCreateXMLArgs{Dom: web1, XMLDesc: "<domainsnapshot><name>s1</name></domainsnapshot>", Flags: 16})
	_, proc := procOf(t, "DomainSnapshotCreateXML")
	done := tr.Libvirt("node-a", libvirt.ProgramRemote, proc, p)
	if done == nil {
		t.Fatal("not recorded")
	}
	done(libvirt.Error{Code: 55, Message: "Requested operation is not valid: domain is not running"})
	// A read costs nothing: no function, no operation.
	_, read := procOf(t, "DomainGetXMLDesc")
	if tr.Libvirt("node-a", libvirt.ProgramRemote, read, nil) != nil {
		t.Error("a read is recorded")
	}
	if len(s.ops) != 1 {
		t.Fatalf("ops %+v", s.ops)
	}
	op := s.ops[0]
	if op.Node != "node-a" || op.Kind != KindLibvirt || op.OK || op.Seq != 1 || op.Cmd != "virsh snapshot-create-as web-1 s1 --disk-only" ||
		!strings.Contains(op.Error, "not running") || op.Time == 0 {
		t.Errorf("op %+v", op)
	}
	if !s.ids[tr.Assigned()] || tr.Assigned() == 0 || tr.ID() != tr.Assigned() {
		t.Errorf("ids %v, trace %d", s.ids, tr.Assigned())
	}
}

func TestTraceRecordsExec(t *testing.T) {
	var s sinkRec
	tr := New(s.add)
	tr.Exec("n", "sudo -n /usr/local/libexec/virticle-node clone-disk 'a' 'b'")(0, nil)
	tr.Exec("n", "timeout 5 ssh-keyscan 'nfs.lan'")(2, errors.New("Process exited with status 2"))
	tr.Exec("n", "virt-customize --root-password password:hunter2")(-1, context.Canceled)
	if len(s.ops) != 3 {
		t.Fatalf("ops %+v", s.ops)
	}
	if o := s.ops[0]; o.API != "virticle-node clone-disk" || !o.OK || o.Kind != KindExec {
		t.Errorf("op 1 %+v", o)
	}
	if o := s.ops[1]; o.API != "ssh-keyscan" || o.OK || o.Error != "exit status 2" {
		t.Errorf("op 2 %+v", o)
	}
	if o := s.ops[2]; strings.Contains(o.Cmd, "hunter2") || o.Error != "stopped" || o.OK {
		t.Errorf("op 3 %+v", o)
	}
}

func TestTraceBounds(t *testing.T) {
	var s sinkRec
	tr := New(s.add)
	var wg sync.WaitGroup
	for i := 0; i < MaxOps+50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if done := tr.Exec("n", "touch /x"); done != nil {
				done(0, nil)
			}
		}()
	}
	wg.Wait()
	limits := 0
	for _, o := range s.ops {
		if o.Kind == KindLimit {
			limits++
		}
	}
	if len(s.ops) != MaxOps+1 || limits != 1 {
		t.Errorf("%d ops, %d limit entries", len(s.ops), limits)
	}

	// Long documents and commands are cut, and say so.
	s = sinkRec{}
	tr = New(s.add)
	big := "<domain>" + strings.Repeat("<x/>", MaxFile) + "</domain>"
	p, _ := libvirt.EncodeArgs(&libvirt.DomainDefineXMLFlagsArgs{XML: big})
	_, proc := procOf(t, "DomainDefineXMLFlags")
	tr.Libvirt("n", libvirt.ProgramRemote, proc, p)(nil)
	tr.Exec("n", "sh -c "+strings.Repeat("a", 2*MaxCmd))(0, nil)
	if f := s.ops[0].Files[0].Text; len(f) > MaxFile+100 || !strings.Contains(f, "<!-- cut: ") {
		t.Errorf("file not cut: %d bytes", len(f))
	}
	if c := s.ops[1].Cmd; len(c) > MaxCmd+100 || !strings.Contains(c, "[cut: ") {
		t.Errorf("command not cut: %d bytes", len(c))
	}
}

func TestNilTrace(t *testing.T) {
	var tr *Trace
	if tr.Libvirt("n", 0, 0, nil) != nil || tr.Exec("n", "true") != nil {
		t.Error("a nil trace records")
	}
	if From(context.Background()) != nil {
		t.Error("empty context has a trace")
	}
	x := New(nil)
	if From(With(context.Background(), x)) != x {
		t.Error("trace lost in context")
	}
}

func TestIDsGrow(t *testing.T) {
	a, b := New(nil).ID(), New(nil).ID()
	if b <= a {
		t.Errorf("ids %d, %d", a, b)
	}
}

func TestProgram(t *testing.T) {
	for in, want := range map[string]string{
		"sudo -n /usr/local/libexec/virticle-node net-confirm 'x'": "virticle-node net-confirm",
		"/usr/local/libexec/virticle-node selfcheck":               "virticle-node selfcheck",
		"timeout 5 getent ahostsv4 'a'":                            "getent",
		"exec virsh -c qemu:///system event --loop":                "virsh",
		"f=$(mktemp) && timeout 20 virsh screenshot":               "sh",
		"/usr/bin/curl -q x":                                       "curl",
		"":                                                         "sh",
	} {
		if got := program(in); got != want {
			t.Errorf("program(%q) = %q, want %q", in, got, want)
		}
	}
}
