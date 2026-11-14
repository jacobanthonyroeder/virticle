package minver

import (
	"strings"
	"testing"
)

func TestEncodeAndFormat(t *testing.T) {
	if got := Encode(11, 3, 0); got != 11003000 {
		t.Fatalf("Encode(11,3,0) = %d", got)
	}
	if got := Encode(10, 0, 2); got != 10000002 {
		t.Fatalf("Encode(10,0,2) = %d", got)
	}
	for v, want := range map[uint64]string{11003000: "11.3.0", 10000002: "10.0.2", 8002002: "8.2.2", 11010005: "11.10.5"} {
		if got := Format(v); got != want {
			t.Errorf("Format(%d) = %q, want %q", v, got, want)
		}
	}
	if Libvirt != 11000000 || QEMU != 10000000 {
		t.Fatal("the minimum is libvirt 11.0.0 and QEMU 10.0.0")
	}
}

func TestCheck(t *testing.T) {
	cases := []struct {
		name         string
		libvirt, qem uint64
		ok           bool
	}{
		{"Debian 13", 11003000, 10000002, true},
		{"Rocky 10", 11010000, 10001000, true},
		{"exactly the minimum", 11000000, 10000000, true},
		{"newer", 12001000, 11000000, true},
		{"Ubuntu 24.04", 10000000, 8002002, false},
		{"Debian 12", 9003000, 7002015, false},
		{"libvirt just below", 10009009, 10001000, false},
		{"QEMU just below", 11003000, 9002009, false},
		{"nothing known", 0, 0, false},
	}
	for _, c := range cases {
		err := Check(c.libvirt, c.qem)
		if (err == nil) != c.ok {
			t.Errorf("%s: Check(%d, %d) = %v, want ok=%v", c.name, c.libvirt, c.qem, err, c.ok)
		}
	}
}

func TestCheckSentence(t *testing.T) {
	err := Check(10000000, 8002002)
	want := "This node has libvirt 10.0.0 and QEMU 8.2.2. Virticle needs libvirt 11.0 and QEMU 10.0 or newer."
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
	if strings.Contains(err.Error(), "...") {
		t.Fatal("no ellipsis in a message")
	}
}

func TestParse(t *testing.T) {
	cases := map[string]uint64{
		"11.3.0":        11003000,
		"11.10.0":       11010000,
		"1:11.3.0-3":    11003000,
		"1:10.0.2+ds-2": 10000002,
		"10.1.0":        10001000,
		"11.0":          11000000,
		"QEMU emulator version 10.0.2 (Debian 1:10.0.2+ds-2)":   10000002,
		"QEMU emulator version 10.1.0 (qemu-kvm-10.1.0-3.el10)": 10001000,
		"9.0.0-4ubuntu1": 9000000,
	}
	for in, want := range cases {
		got, ok := Parse(in)
		if !ok || got != want {
			t.Errorf("Parse(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "(none)", "libvirt", "7"} {
		if _, ok := Parse(in); ok {
			t.Errorf("Parse(%q) should fail", in)
		}
	}
}
