package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type fakeFreezer struct {
	freezeErr error
	thawErrs  []error // one per Thaw call; the last repeats
	events    []string
}

func (f *fakeFreezer) Freeze() error {
	f.events = append(f.events, "freeze")
	return f.freezeErr
}

func (f *fakeFreezer) Thaw() error {
	n := 0
	for _, e := range f.events {
		if e == "thaw" {
			n++
		}
	}
	f.events = append(f.events, "thaw")
	if len(f.thawErrs) == 0 {
		return nil
	}
	if n >= len(f.thawErrs) {
		n = len(f.thawErrs) - 1
	}
	return f.thawErrs[n]
}

func boolp(b bool) *bool { return &b }

func TestWithFrozenGuest(t *testing.T) {
	thawPause = time.Millisecond
	boom := errors.New("boom")
	cases := []struct {
		name       string
		quiesce    *bool
		agent      bool
		freezeErr  error
		beginErr   error
		wantEvents string // freeze/begin/thaw order
		wantFrozen bool
		wantText   string
		wantErr    string // "" none, "freeze" a *freezeFail, "begin" the begin error
	}{
		{"auto, agent, freeze ok", nil, true, nil, nil, "freeze,begin,thaw", true, "guest file systems frozen", ""},
		{"auto, agent, freeze fails", nil, true, boom, nil, "freeze,thaw,begin", false, "not frozen: boom", ""},
		{"auto, agent, freeze never sent", nil, true, &freezeNotTried{boom}, nil, "freeze,begin", false, "not frozen: boom", ""},
		{"auto, agent, freeze late", nil, true, &freezeLate{boom}, nil, "freeze,begin", false, "not frozen: boom", ""},
		{"true, agent, freeze late", boolp(true), true, &freezeLate{boom}, nil, "freeze", false, "not frozen: boom", "freeze"},
		{"auto, no agent", nil, false, nil, nil, "begin", false, "not frozen: no guest agent", ""},
		{"true, agent, freeze ok", boolp(true), true, nil, nil, "freeze,begin,thaw", true, "guest file systems frozen", ""},
		{"true, agent, freeze fails", boolp(true), true, boom, nil, "freeze,thaw", false, "not frozen: boom", "freeze"},
		{"true, agent, freeze never sent", boolp(true), true, &freezeNotTried{boom}, nil, "freeze", false, "not frozen: boom", "freeze"},
		{"true, no agent", boolp(true), false, nil, nil, "", false, "", "freeze"},
		{"false, agent", boolp(false), true, nil, nil, "begin", false, "not frozen: turned off for this backup", ""},
		{"false, no agent", boolp(false), false, nil, nil, "begin", false, "not frozen: turned off for this backup", ""},
		{"auto, freeze ok, begin fails", nil, true, nil, boom, "freeze,begin,thaw", true, "guest file systems frozen", "begin"},
		{"true, freeze ok, begin fails", boolp(true), true, nil, boom, "freeze,begin,thaw", true, "guest file systems frozen", "begin"},
		{"auto, freeze fails, begin fails", nil, true, boom, boom, "freeze,thaw,begin", false, "not frozen: boom", "begin"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeFreezer{freezeErr: c.freezeErr}
			begin := func() error {
				f.events = append(f.events, "begin")
				return c.beginErr
			}
			out, err := withFrozenGuest(c.quiesce, c.agent, f, begin)
			if got := strings.Join(f.events, ","); got != c.wantEvents {
				t.Errorf("events = %q, want %q", got, c.wantEvents)
			}
			if out.Frozen != c.wantFrozen || out.Text != c.wantText {
				t.Errorf("outcome = %+v, want frozen=%v text=%q", out, c.wantFrozen, c.wantText)
			}
			if out.ThawErr != nil {
				t.Errorf("unexpected ThawErr %v", out.ThawErr)
			}
			var ff *freezeFail
			switch c.wantErr {
			case "":
				if err != nil {
					t.Errorf("err = %v", err)
				}
			case "freeze":
				if !errors.As(err, &ff) {
					t.Errorf("err = %v, want a freeze failure", err)
				} else if strings.Contains(err.Error(), "\n") || err.Error() == "" {
					t.Errorf("unclear message %q", err)
				}
			case "begin":
				if !errors.Is(err, c.beginErr) || errors.As(err, &ff) {
					t.Errorf("err = %v, want the begin error", err)
				}
			}
			// A freeze that went through is always followed by a thaw, and the thaw comes after begin.
			froze := false
			for i, e := range f.events {
				if e == "freeze" && c.freezeErr == nil {
					froze = true
				}
				if e == "begin" && froze {
					rest := strings.Join(f.events[i:], ",")
					if !strings.HasSuffix(rest, "thaw") {
						t.Errorf("no thaw after begin: %q", rest)
					}
				}
			}
		})
	}
}

func TestThawRetriesAndReportsFailure(t *testing.T) {
	thawPause = time.Millisecond
	bad := errors.New("agent busy")
	// Two failures, then success: the guest ends thawed, no problem reported.
	f := &fakeFreezer{thawErrs: []error{bad, bad, nil}}
	out, err := withFrozenGuest(nil, true, f, func() error { f.events = append(f.events, "begin"); return nil })
	if err != nil || out.ThawErr != nil {
		t.Fatalf("err=%v thawErr=%v", err, out.ThawErr)
	}
	if got := strings.Join(f.events, ","); got != "freeze,begin,thaw,thaw,thaw" {
		t.Errorf("events = %q", got)
	}
	// Every try fails: the backup result stands, the outcome says the guest may be frozen.
	f = &fakeFreezer{thawErrs: []error{bad}}
	out, err = withFrozenGuest(nil, true, f, func() error { return nil })
	if err != nil || out.ThawErr == nil {
		t.Fatalf("err=%v thawErr=%v", err, out.ThawErr)
	}
	if n := strings.Count(strings.Join(f.events, ","), "thaw"); n != thawTries {
		t.Errorf("%d thaw tries, want %d", n, thawTries)
	}
}

func TestFinishFreeze(t *testing.T) {
	boom := errors.New("boom")
	var nilRun *bkRun
	if err := nilRun.finishFreeze(boom); err != boom {
		t.Errorf("nil run changed the error: %v", err)
	}
	m := &bkRun{name: "vm1", fz: freezeOutcome{Frozen: true, Text: frozenText}}
	if err := m.finishFreeze(nil); err != doneNote(frozenText) {
		t.Errorf("good run: %v", err)
	}
	if err := m.finishFreeze(boom); err != boom {
		t.Errorf("failed run without thaw problem: %v", err)
	}
	if err := (&bkRun{}).finishFreeze(nil); err != nil {
		t.Errorf("offline run: %v", err)
	}
	m.thawErr = errors.New("no answer")
	if err := m.finishFreeze(nil); err == nil || !strings.Contains(err.Error(), "virsh domfsthaw vm1") {
		t.Errorf("a guest left frozen must be an error that says what to do: %v", err)
	}
	if err := m.finishFreeze(boom); err == nil || !errors.Is(err, boom) || !strings.Contains(err.Error(), "may be frozen") {
		t.Errorf("failed run with thaw problem: %v", err)
	}
}

func TestWithin(t *testing.T) {
	if err := within(time.Second, "late", func() error { return nil }); err != nil {
		t.Errorf("fast call: %v", err)
	}
	boom := errors.New("boom")
	if err := within(time.Second, "late", func() error { return boom }); err != boom {
		t.Errorf("failing call: %v", err)
	}
	release := make(chan struct{})
	defer close(release)
	start := time.Now()
	err := within(20*time.Millisecond, "late", func() error { <-release; return nil })
	if err == nil || err.Error() != "late" || time.Since(start) > time.Second {
		t.Errorf("hung call: %v after %v", err, time.Since(start))
	}
}

func TestFreezeWithin(t *testing.T) {
	boom := errors.New("boom")
	thawed := make(chan struct{}, 1)
	thaw := func() { thawed <- struct{}{} }
	if err := freezeWithin(time.Second, func() error { return nil }, thaw); err != nil {
		t.Errorf("fast freeze: %v", err)
	}
	if err := freezeWithin(time.Second, func() error { return boom }, thaw); err != boom {
		t.Errorf("failing freeze: %v", err)
	}
	// A freeze that lands after the wait is thawed; the caller is not kept waiting.
	release := make(chan error)
	start := time.Now()
	err := freezeWithin(20*time.Millisecond, func() error { return <-release }, thaw)
	var late *freezeLate
	if !errors.As(err, &late) || time.Since(start) > time.Second {
		t.Fatalf("late freeze: %v after %v", err, time.Since(start))
	}
	if !strings.Contains(err.Error(), "did not freeze within") {
		t.Errorf("message %q", err)
	}
	select {
	case <-thawed:
		t.Fatal("thawed before the freeze landed")
	default:
	}
	release <- nil
	select {
	case <-thawed:
	case <-time.After(5 * time.Second):
		t.Fatal("a freeze that landed late was not thawed")
	}
	// A late freeze that fails froze nothing: no thaw.
	release2 := make(chan error)
	_ = freezeWithin(20*time.Millisecond, func() error { return <-release2 }, thaw)
	release2 <- boom
	select {
	case <-thawed:
		t.Error("thawed after a failed freeze")
	case <-time.After(100 * time.Millisecond):
	}
	// Nothing was thawed from earlier calls either.
	if len(thawed) != 0 {
		t.Error("unexpected thaw")
	}
}

// The Windows agent's VSS errors (qga/vss-win32/requester.cpp, as libvirt passes them on) get a clause that says
// what to look at in the guest; Linux errors and unknown ones stay as they are.
func TestFreezeHint(t *testing.T) {
	agentErr := func(s string) error {
		return errors.New("internal error: unable to execute QEMU agent command 'guest-fsfreeze-freeze': " + s)
	}
	cases := []struct {
		err     error
		windows bool
		want    string
	}{
		{agentErr(`failed to add C:\ to snapshot set: Windows error 0x80042304`), true, hintVSSProvider},
		{agentErr("unexpected error in VSS provider: Windows error 0x8004230f"), true, hintVSSProvider},
		{agentErr("failed to do snapshot set: Windows error 0x80042316"), true, hintVSSBusy},
		{agentErr("failed to gather writer metadata: Windows error 0x800423f2"), true, hintVSSWriter},
		{agentErr("timeout when try to receive Frozen event from VSS provider: Windows error 0x80004005"), true, hintVSSSlow},
		{agentErr("couldn't receive Frozen event from VSS provider: Windows error 0x6"), false, hintVSSSlow},
		{agentErr("failed to load requester_freeze from qga-vss.dll"), true, hintVSSMissing},
		{&freezeLate{errors.New("the guest did not freeze within 2 minutes")}, true, hintVSSSlow},
		{&freezeLate{errors.New("the guest did not freeze within 2 minutes")}, false, ""},
		{&freezeNotTried{errors.New("the guest agent cannot freeze file systems")}, true, hintVSSMissing},
		{&freezeNotTried{errors.New("the guest agent cannot freeze file systems")}, false, ""},
		{&freezeNotTried{errors.New("the guest agent does not answer (timeout)")}, true, ""},
		{agentErr("failed to freeze /: Device or resource busy"), false, ""},
	}
	for _, c := range cases {
		if got := freezeHint(c.err, c.windows); got != c.want {
			t.Errorf("freezeHint(%q, %v) = %q, want %q", c.err, c.windows, got, c.want)
		}
	}
	// The hint keeps the kind of the error and follows the shortened message in the task text.
	late := withGuestHint(&freezeLate{errors.New("the guest did not freeze within 2 minutes")}, true)
	var fl *freezeLate
	if !errors.As(late, &fl) {
		t.Error("a hinted late freeze is no longer a late freeze")
	}
	if got := freezeReason(late); got != "the guest did not freeze within 2 minutes; "+hintVSSSlow {
		t.Errorf("reason %q", got)
	}
	if withGuestHint(nil, true) != nil {
		t.Error("no error, no hint")
	}
}

// When the Windows agent's thaw says VSS let writes go on after 10 seconds, the backup is not called frozen.
func TestFreezeLapsedOnWindows(t *testing.T) {
	thawPause = time.Millisecond
	lapse := errors.New("internal error: unable to execute QEMU agent command 'guest-fsfreeze-thaw': couldn't hold writes: fsfreeze is limited up to 10 seconds: Windows error 0x8004231f")
	f := &fakeFreezer{thawErrs: []error{lapse, nil}}
	out, err := withFrozenGuest(nil, true, f, func() error { return nil })
	if err != nil || out.ThawErr != nil {
		t.Fatalf("err=%v thawErr=%v", err, out.ThawErr)
	}
	if out.Frozen || !strings.HasPrefix(out.Text, frozenNo+"VSS in the guest ended the freeze after 10 seconds") {
		t.Errorf("outcome %+v", out)
	}
	// Any other thaw error that a retry clears leaves the backup frozen.
	f = &fakeFreezer{thawErrs: []error{errors.New("agent busy"), nil}}
	if out, _ = withFrozenGuest(nil, true, f, func() error { return nil }); !out.Frozen || out.Text != frozenText {
		t.Errorf("outcome %+v", out)
	}
}

// A guest left frozen by an earlier freeze: its agent lists the freeze as disabled. It is thawed and frozen anew;
// an agent that cannot freeze for another reason is not touched.
func TestFreezeReady(t *testing.T) {
	info := func(enabled bool) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"version":"9.2","supported_commands":[{"name":"guest-fsfreeze-freeze","enabled":%v}]}`, enabled))
	}
	var nt *freezeNotTried
	run := func(infos []bool, status string, thawErr error) (error, []string) {
		var ev []string
		i := 0
		err := freezeReady(
			func() (json.RawMessage, error) { ev = append(ev, "info"); r := info(infos[i]); i++; return r, nil },
			func() (string, error) { ev = append(ev, "status"); return status, nil },
			func() error { ev = append(ev, "thaw"); return thawErr })
		return err, ev
	}
	if err, ev := run([]bool{true}, "thawed", nil); err != nil || strings.Join(ev, ",") != "info" {
		t.Errorf("can freeze: %v %v", err, ev)
	}
	if err, ev := run([]bool{false, true}, "frozen", nil); err != nil || strings.Join(ev, ",") != "info,status,thaw,info" {
		t.Errorf("left frozen: %v %v", err, ev)
	}
	if err, ev := run([]bool{false}, "thawed", nil); !errors.As(err, &nt) || strings.Join(ev, ",") != "info,status" {
		t.Errorf("cannot freeze: %v %v", err, ev)
	}
	if err, ev := run([]bool{false, false}, "frozen", nil); !errors.As(err, &nt) || strings.Join(ev, ",") != "info,status,thaw,info" {
		t.Errorf("still disabled after the thaw: %v %v", err, ev)
	}
	err, ev := run([]bool{false}, "frozen", errors.New("agent busy"))
	if err == nil || errors.As(err, &nt) || !strings.Contains(err.Error(), "frozen already") || strings.Join(ev, ",") != "info,status,thaw" {
		t.Errorf("thaw fails: %v %v", err, ev)
	}
	if err := freezeReady(func() (json.RawMessage, error) { return nil, errors.New("timeout") }, nil, nil); !errors.As(err, &nt) {
		t.Errorf("no answer: %v", err)
	}
}

func TestWaitText(t *testing.T) {
	for d, want := range map[time.Duration]string{2 * time.Minute: "2 minutes", time.Minute: "1 minute", 45 * time.Second: "45 seconds", 90 * time.Second: "90 seconds", time.Second: "1 second", 100 * time.Millisecond: "under 1 second", 12300 * time.Millisecond: "12 seconds"} {
		if got := waitText(d); got != want {
			t.Errorf("waitText(%v) = %q, want %q", d, got, want)
		}
	}
}
