package hostconn

import (
	"errors"
	"strings"
	"testing"
)

func TestAgeLines(t *testing.T) {
	h := &Host{Name: "n1"}
	var got [][2]uint64
	a := &ageLines{progress: func(d, tot uint64) { got = append(got, [2]uint64{d, tot}) }}
	sum := strings.Repeat("a", 64)
	enc := strings.Repeat("b", 64)
	a.Write([]byte("@@progress 10 100\r\n@@prog"))
	a.Write([]byte("ress 100 100\nAGE-SECRET-KEY-1QQQ\n" + sum + " 100 " + enc + " 300\n"))
	if len(got) != 2 || got[1] != [2]uint64{100, 100} {
		t.Fatalf("progress %v", got)
	}
	s, err := a.result(h, "encrypt", "/p/x", nil, true)
	if err != nil || s.PlainSHA != sum || s.PlainBytes != 100 || s.StoredSHA != enc || s.StoredBytes != 300 {
		t.Fatalf("%+v %v", s, err)
	}
	d := &ageLines{}
	d.line(enc + " 300 " + sum + " 100")
	if s, _ := d.result(h, "decrypt", "/p/x", nil, false); s.StoredSHA != enc || s.PlainBytes != 100 {
		t.Fatalf("decrypt order %+v", s)
	}
	old := &ageLines{}
	old.line("virticle-node: unknown subcommand")
	if _, err := old.result(h, "encrypt", "/p/x", errors.New("exit 2"), true); !errors.Is(err, ErrNoAge) || !strings.Contains(err.Error(), "prepare the node again") {
		t.Fatalf("old node command: %v", err)
	}
	bad := &ageLines{}
	bad.line("AGE-SECRET-KEY-1ABC")
	bad.line("virticle-node: /p/x is not encrypted to this backup key")
	_, err = bad.result(h, "decrypt", "/p/x", errors.New("failed"), false)
	if err == nil || strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), "not encrypted to this backup key") || strings.Contains(err.Error(), "virticle-node:") {
		t.Fatalf("failure message %v", err)
	}
	for _, l := range []string{"", "x 1 y 2", sum + " -1 " + enc + " 2", sum + " 1 " + enc} {
		if _, ok := parseAgeSums(l, true); ok {
			t.Errorf("%q parsed", l)
		}
	}
}
