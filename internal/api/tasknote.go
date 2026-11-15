package api

import (
	"strings"

	"virticle/internal/hostconn"
)

// noted wraps the function of a task: when fn ends well, the task ends with the note (see taskDone). The note
// is built after fn has run, so it can use what fn found out. An empty note ends the task with no Result.
func noted(fn func(progress func(float64)) error, note func() string) func(progress func(float64)) error {
	return func(progress func(float64)) error {
		if err := fn(progress); err != nil {
			return err
		}
		if n := note(); n != "" {
			return taskDone("%s", n)
		}
		return nil
	}
}

// doneOr ends a task with a note when err is nil, so "return h.Call()" becomes
// "return doneOr(h.Call(), "name")". A failure passes through unchanged.
func doneOr(err error, format string, a ...any) error {
	if err != nil {
		return err
	}
	return taskDone(format, a...)
}

// fileNote is the Result of a task that wrote a file on a node: its name and its size when the node tells it.
func fileNote(h *hostconn.Host, name, path string) string {
	if st, err := h.Stat(path); err == nil && len(st) == 1 && st[0].Exists {
		return fileFact(name, st[0].Size)
	}
	return name
}

// hwNote is the Result of a hardware change: the fact, and "after restart" when the running VM does not have it
// yet.
func hwNote(fact string, res hwResult) string {
	if res.NeedsRestart {
		return fact + ", after restart"
	}
	return fact
}

// joinFacts joins the non-empty facts of a Result with commas.
func joinFacts(facts ...string) string {
	var out []string
	for _, f := range facts {
		if f != "" {
			out = append(out, f)
		}
	}
	return strings.Join(out, ", ")
}

// mibText writes a size given in MiB ("2 GiB", "1536 MiB" is written as "1.5 GiB").
func mibText(mib uint64) string { return fmtBytes(mib << 20) }

// gibText writes a size given in GiB ("20 GiB").
func gibText(gib int) string { return fmtBytes(uint64(gib) << 30) }
