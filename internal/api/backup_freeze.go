package api

import (
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/cluster"
	"virticle/internal/hostconn"
)

// A live backup is the image of the moment libvirt's backup-begin runs. To make
// that image consistent for the guest (databases, journals), the guest's file
// systems are frozen through the QEMU guest agent for that one call and thawed
// right after it. libvirt's backup-begin has no flag for this.

const (
	freezeInfoWait = 5 // seconds to wait for the agent's guest-info: a hung agent costs this much and no more
	thawTries      = 4
)

// How long to wait for the freeze and for one thaw attempt. A Windows guest
// freezes through VSS: the agent first gathers the writers' metadata and
// prepares the backup, which have no limit of their own, and then waits up to
// 60 seconds for VSS to report the volumes frozen. On a busy host, or right
// after heavy disk activity, that took longer than 45 seconds. The guest's
// disks are not held while the freeze is being prepared, so a long wait costs
// the backup time and nothing else; the guest-info check has already shown
// that the agent answers.
var (
	freezeWait = 2 * time.Minute
	thawWait   = 40 * time.Second
)

var thawPause = time.Second

// Texts that the task and the manifest carry.
const (
	frozenText = "guest file systems frozen"
	frozenNo   = "not frozen: "
)

// guestFreezer is the guest agent, as far as the freeze needs it.
type guestFreezer interface {
	// Freeze freezes the guest's file systems; it fails when the guest cannot.
	Freeze() error
	// Thaw thaws them; thawing file systems that are not frozen is not an error.
	Thaw() error
}

// freezeOutcome says what happened to the guest during one backup-begin.
type freezeOutcome struct {
	Frozen  bool   // the file systems were frozen while backup-begin ran
	Text    string // the one clause the task shows
	ThawErr error  // set when the guest may still be frozen
}

// freezeLate is a freeze that did not finish in time. The call goes on in the
// background and the guest is thawed if it ever takes effect, so it needs no
// thaw now (a thaw would only wait behind it: libvirt runs one agent command
// per guest at a time).
type freezeLate struct{ err error }

func (e *freezeLate) Error() string { return e.err.Error() }
func (e *freezeLate) Unwrap() error { return e.err }

// freezeNotTried marks a Freeze error that came before anything was sent to the guest to freeze.
type freezeNotTried struct{ err error }

func (e *freezeNotTried) Error() string { return e.err.Error() }
func (e *freezeNotTried) Unwrap() error { return e.err }

// freezeFail is an explicit freeze request that could not be met; the backup is not started.
type freezeFail struct{ err error }

func (e *freezeFail) Error() string { return e.err.Error() }
func (e *freezeFail) Unwrap() error { return e.err }

// withFrozenGuest runs begin (the call that starts the backup job) with the
// guest's file systems frozen, as the request says:
//
//	quiesce nil   automatic: freeze when the agent is connected; if that fails, begin anyway
//	quiesce true  freeze or fail (a *freezeFail, begin is not called)
//	quiesce false never freeze
//
// Once a freeze was tried the guest is thawed whatever happens, with a few
// retries; ThawErr is set only when every try failed.
func withFrozenGuest(quiesce *bool, agent bool, g guestFreezer, begin func() error) (freezeOutcome, error) {
	if quiesce != nil && !*quiesce {
		return freezeOutcome{Text: frozenNo + "turned off for this backup"}, begin()
	}
	explicit := quiesce != nil
	if !agent {
		if explicit {
			return freezeOutcome{}, &freezeFail{errors.New("cannot freeze the guest file systems: the guest agent is not connected. Install and start qemu-guest-agent in the guest, or back up without freezing")}
		}
		return freezeOutcome{Text: frozenNo + "no guest agent"}, begin()
	}
	if ferr := g.Freeze(); ferr != nil {
		// A freeze that timed out may still have taken effect: thaw to be sure.
		out := freezeOutcome{Text: frozenNo + freezeReason(ferr)}
		var nt *freezeNotTried
		var late *freezeLate
		if !errors.As(ferr, &nt) && !errors.As(ferr, &late) {
			out.ThawErr = g.Thaw() // a failed freeze may have frozen some file systems
		}
		if explicit {
			return out, &freezeFail{fmt.Errorf("could not freeze the guest file systems: %v. Check that qemu-guest-agent runs in the guest, or back up without freezing", ferr)}
		}
		return out, begin()
	}
	err := begin()
	out := freezeOutcome{Frozen: true, Text: frozenText}
	if out.ThawErr = thawWithRetries(g); out.ThawErr != nil {
		log.Printf("backup: the guest file systems could not be thawed: %v", out.ThawErr)
	}
	return out, err
}

func thawWithRetries(g guestFreezer) (err error) {
	for i := 0; i < thawTries; i++ {
		if i > 0 {
			time.Sleep(thawPause)
		}
		if err = g.Thaw(); err == nil {
			return nil
		}
	}
	return err
}

// thawProblem is the sentence for a guest that may have been left frozen.
func thawProblem(vm string, err error) string {
	return fmt.Sprintf("the guest file systems of %s could not be thawed (%v); the guest may be frozen. Thaw it now with: virsh domfsthaw %s", vm, err, vm)
}

// freezeReason shortens an error to a clause for the task text.
func freezeReason(err error) string {
	s := err.Error()
	for i, c := range s {
		if c == '\n' {
			s = s[:i]
			break
		}
	}
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

// agentFreezer freezes through the guest agent of a running domain.
type agentFreezer struct {
	h   *hostconn.Host
	dom libvirt.Domain
}

func (a agentFreezer) Freeze() error {
	raw, err := agentCmdT(a.h, a.dom, "guest-info", nil, freezeInfoWait)
	if err != nil {
		return &freezeNotTried{fmt.Errorf("the guest agent does not answer (%v)", err)}
	}
	if _, can, perr := parseAgentInfo(raw); perr != nil || !can {
		return &freezeNotTried{errors.New("the guest agent cannot freeze file systems")}
	}
	// The freeze is libvirt's own (it sends guest-fsfreeze-freeze and waits for the agent as long as it takes).
	return freezeWithin(freezeWait, func() error {
		_, err := a.h.DomainFsfreeze(a.dom, nil, 0)
		return err
	}, func() { lateThaw(a, a.h.Name, cluster.FormatUUID(a.dom.UUID), a.dom.Name) })
}

// lateThaw thaws a guest whose freeze took effect after the backup stopped waiting for it. The task has ended by
// then, so a guest that stays frozen after every try becomes an alert (backup_frozen.go).
func lateThaw(g guestFreezer, host, uuid, name string) {
	log.Printf("backup: the guest file systems of %s froze after the wait had ended; thawing them", name)
	if err := thawWithRetries(g); err != nil {
		log.Printf("backup: %s", thawProblem(name, err))
		noteFrozen(host, uuid, name, err)
	}
}

// freezeWithin runs freeze and waits for it up to d. When the wait runs out the
// call goes on (libvirt cannot cancel it) and a *freezeLate is returned; if the
// call then succeeds, the guest is frozen with nobody to thaw it, so thaw runs.
func freezeWithin(d time.Duration, freeze func() error, thaw func()) error {
	done := make(chan error, 1)
	go func() { done <- freeze() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		go func() {
			if <-done == nil {
				thaw()
			}
		}()
		return &freezeLate{fmt.Errorf("the guest did not freeze within %s", waitText(d))}
	}
}

// waitText says a wait in words for the task note: "2 minutes", "45 seconds".
func waitText(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		if d == time.Minute {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", d/time.Minute)
	}
	return fmt.Sprintf("%d seconds", (d+time.Second/2)/time.Second)
}

func (a agentFreezer) Thaw() error {
	return within(thawWait, "the guest did not answer the thaw in time", func() error {
		_, err := a.h.DomainFsthaw(a.dom, nil, 0)
		return err
	})
}

// within runs fn and stops waiting after d (the call itself goes on in the background).
func within(d time.Duration, msg string, fn func() error) error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		return errors.New(msg)
	}
}
