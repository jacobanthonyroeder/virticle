package libvirt

import (
	"sync"
	"testing"

	"github.com/digitalocean/go-libvirt/internal/constants"
	"github.com/digitalocean/go-libvirt/libvirttest"
)

// A view from WithCallHook sees its own calls, with their arguments and outcome; the connection it shares
// does not.
func TestCallHookSeesOnlyItsView(t *testing.T) {
	l := NewWithDialer(libvirttest.New())
	if err := l.Connect(); err != nil {
		t.Fatal(err)
	}
	defer l.Disconnect()

	type seen struct {
		proc  uint32
		args  []byte
		ended bool
		err   error
	}
	var mu sync.Mutex
	var calls []*seen
	v := l.WithCallHook(func(program, proc uint32, payload []byte) func(error) {
		if program != ProgramRemote {
			t.Errorf("program %#x", program)
		}
		s := &seen{proc: proc, args: append([]byte(nil), payload...)}
		mu.Lock()
		calls = append(calls, s)
		mu.Unlock()
		return func(err error) { s.ended, s.err = true, err }
	})

	dom := Domain{Name: "test"}
	if _, err := l.DomainCreateWithFlags(dom, 0); err != nil { // not through the view
		t.Fatal(err)
	}
	if _, err := v.DomainCreateWithFlags(dom, 1); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].proc != constants.ProcDomainCreateWithFlags || !calls[0].ended || calls[0].err != nil {
		t.Fatalf("calls %+v", calls)
	}
	var a DomainCreateWithFlagsArgs
	if err := DecodeArgs(calls[0].args, &a); err != nil || a.Dom.Name != "test" || a.Flags != 1 {
		t.Errorf("args %+v, %v", a, err)
	}
	if !v.IsConnected() {
		t.Error("the view does not share the connection")
	}
}

func TestDecodeTypedParams(t *testing.T) {
	in := DomainMigratePerform3ParamsArgs{Dom: Domain{Name: "web-1"}, Params: []TypedParam{
		{Field: "migrate_uri", Value: *NewTypedParamValueString("tcp://10.0.0.2")},
		{Field: "bandwidth", Value: *NewTypedParamValueUllong(100)},
	}, Flags: 1}
	p, err := EncodeArgs(&in)
	if err != nil {
		t.Fatal(err)
	}
	var out DomainMigratePerform3ParamsArgs
	if err := DecodeArgs(p, &out); err != nil {
		t.Fatal(err)
	}
	if out.Dom.Name != "web-1" || len(out.Params) != 2 || out.Params[0].Value.I != "tcp://10.0.0.2" || out.Params[1].Value.I != uint64(100) {
		t.Errorf("got %+v", out)
	}
}
