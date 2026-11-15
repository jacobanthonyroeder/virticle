package api

import (
	"errors"
	"testing"

	"virticle/internal/hostconn"
)

func TestBusyFacts(t *testing.T) {
	be := &hostconn.BusyError{Host: &hostconn.Host{Name: "kvm3"}, Slow: true}
	base := "libvirt on kvm3 did not answer within 6 seconds"
	if be.Error() != base {
		t.Fatalf("base text %q", be.Error())
	}
	cases := []struct {
		name   string
		err    error
		down   []string
		mem    float64
		want   string
		isBusy bool
	}{
		{"nothing known: no guess", be, nil, 40, base, true},
		{"memory nearly full", be, nil, 97.4, base + "; the node's memory is 97% used", true},
		{"NFS share down", be, []string{"/mnt/nas"}, 0, base + "; the NFS storage at /mnt/nas does not answer, and libvirt calls that touch it wait for its server", true},
		{"both", be, []string{"/mnt/a", "/mnt/b"}, 95, base + "; the NFS storage at /mnt/a, /mnt/b does not answer, and libvirt calls that touch it wait for its server; the node's memory is 95% used", true},
		{"other errors unchanged", errors.New("boom"), []string{"/mnt/nas"}, 99, "boom", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := busyFacts(c.err, c.down, c.mem, 90)
			if got.Error() != c.want {
				t.Fatalf("got %q\nwant %q", got.Error(), c.want)
			}
			if errors.Is(got, hostconn.ErrStorageBusy) != c.isBusy {
				t.Fatal("the result must stay ErrStorageBusy exactly when the input was")
			}
			// Twice: the facts are added once.
			if again := busyFacts(got, c.down, c.mem, 90); again.Error() != c.want {
				t.Fatalf("again: %q", again.Error())
			}
		})
	}
	if busyFacts(nil, []string{"/x"}, 99, 90) != nil {
		t.Error("nil stays nil")
	}
}
