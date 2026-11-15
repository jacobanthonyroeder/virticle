package api

import (
	"errors"
	"fmt"
	"strings"

	"virticle/internal/hostconn"
)

// A call that the storage gate refused or gave up on (hostconn.BusyError) says only that libvirt on the node did
// not answer in time. The gate cannot tell why. The controller knows two causes from elsewhere, without asking the
// node anything: an NFS share of the node that does not answer (the prober's last answer), and a node whose memory
// is nearly used up (the last statistics sample). busyFacts names those that apply; with neither, nothing is
// guessed.

// busyExplained marks an error that busyFacts already added to.
type busyExplained struct{ error }

func (e busyExplained) Unwrap() error { return e.error }

// busyFacts adds the known causes to an error of a node whose libvirt did not answer; other errors are returned
// unchanged. down is the node's NFS mounts that do not answer, memPct its memory use (0 when not known), memMax
// the share from which memory counts as the cause.
func busyFacts(err error, down []string, memPct, memMax float64) error {
	var be *hostconn.BusyError
	var done busyExplained
	if err == nil || !errors.As(err, &be) || errors.As(err, &done) {
		return err
	}
	var facts []string
	if len(down) > 0 {
		facts = append(facts, fmt.Sprintf("the NFS storage at %s does not answer, and libvirt calls that touch it wait for its server", strings.Join(down, ", ")))
	}
	if memPct >= memMax && memPct > 0 {
		facts = append(facts, fmt.Sprintf("the node's memory is %.0f%% used", memPct))
	}
	if len(facts) == 0 {
		return err
	}
	return busyExplained{fmt.Errorf("%w; %s", err, strings.Join(facts, "; "))}
}

// explainBusy is busyFacts with what this controller knows about the node of the error.
func (s *Server) explainBusy(err error) error {
	var be *hostconn.BusyError
	if err == nil || !errors.As(err, &be) || be.Host == nil {
		return err
	}
	down := nfsProber.downMounts(be.Host)
	var pct float64
	if s.Nodes != nil {
		if n, ok := s.Nodes.ConfigOfHost(be.Host); ok {
			if m, ok := latestHostMem(n.ID); ok && m.TotalKiB > 0 {
				pct = float64(m.UsedKiB) / float64(m.TotalKiB) * 100
			}
		}
	}
	return busyFacts(err, down, pct, alertConfig{Mem: s.Alerts.Mem}.withDefaults().Mem)
}
