package hostconn

import (
	"strings"
)

// RunInput is Run with stdin fed to the command. Use it to hand secrets (key
// material) to a command: they never appear on a command line, in the process
// list or in logs.
func (h *Host) RunInput(cmd, stdin string) (string, error) {
	s, err := h.ssh.NewSession()
	if err != nil {
		return "", err
	}
	defer s.Close()
	s.Stdin = strings.NewReader(stdin)
	done := h.exec(cmd) // the command line only: what goes to stdin is never recorded
	out, err := s.CombinedOutput(cmd)
	done(err)
	return strings.TrimSpace(string(out)), err
}
