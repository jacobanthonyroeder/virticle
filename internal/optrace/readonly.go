package optrace

import (
	"regexp"
	"strings"

	"virticle/internal/nodeproto"
)

// ReadOnlyCommand says whether a command line run on a node only reads: a verb of the node command that
// internal/nodeproto lists as read-only, or a plain shell command made of read-only programs (stat, test, cat,
// command -v and the like). Such a command is not recorded, as a read-only libvirt call is not. Anything this
// cannot tell for sure is recorded: a command that writes through a redirection, runs a program that changes
// things, or is not made of listed programs.
func ReadOnlyCommand(cmd string) bool {
	f := bare(strings.Fields(cmd))
	if len(f) == 0 {
		return false
	}
	if f[0] == NodeCmdPath {
		if len(f) < 2 {
			return false
		}
		args := make([]string, 0, len(f)-2)
		for _, a := range f[2:] {
			args = append(args, strings.Trim(a, "'"))
		}
		return nodeproto.ReadOnlyVerb(strings.Trim(f[1], "'"), args)
	}
	return readOnlyShell(cmd)
}

// readOnlyPrograms are programs that change nothing when run as the first word of a command. ip and systemctl
// are checked further (readOnlySub).
var readOnlyPrograms = map[string]bool{
	"stat": true, "test": true, "[": true, "cat": true, "ls": true, "command": true, "getent": true, "date": true,
	"cut": true, "head": true, "tail": true, "id": true, "uname": true, "readlink": true, "echo": true, "true": true,
	"false": true, "which": true, "findmnt": true, "df": true, "du": true, "ss": true, "awk": true, "grep": true,
	"wc": true, "sort": true, "nproc": true, "free": true, "lsblk": true, "uptime": true, "hostname": true,
	"sha256sum": true, "basename": true, "dirname": true, "printf": true, "ip": true, "systemctl": true,
}

// readOnlySub: for these programs the first argument decides.
var readOnlySub = map[string]map[string]bool{
	"ip":        {"route": true, "addr": true, "address": true, "link": true, "neigh": true, "-j": true, "-o": true, "-4": true, "-6": true, "-br": true},
	"systemctl": {"is-active": true, "is-enabled": true, "show": true, "status": true, "is-system-running": true, "list-units": true, "list-unit-files": true},
}

// changing finds programs that change things anywhere in a command line, also inside $( ) or a pipeline.
var changing = regexp.MustCompile(`(^|[^\w./-])(rm|mv|cp|mkdir|rmdir|tee|chmod|chown|ln|kill|pkill|touch|truncate|dd|mktemp|install|sed|virsh|qemu-img|nmcli|netplan|apt|apt-get|dnf|yum|curl|wget|useradd|usermod|userdel|sh|bash|eval|xargs|reboot|shutdown|tar|mount|umount|modprobe|sysctl|system)(\s|$|;|\()`)

// harmless redirections: to /dev/null, and between descriptors.
var harmlessRedirect = regexp.MustCompile(`\d*>&\d+|\d*>\s*/dev/null`)

var shellSplit = regexp.MustCompile(`&&|\|\||;|\|`)

func readOnlyShell(cmd string) bool {
	if strings.TrimSpace(cmd) == "" {
		return false
	}
	if changing.MatchString(cmd) {
		return false
	}
	// "sudo -n true" and "timeout 5 ls": drop the wrapping of the first segment only; later segments are
	// plain commands.
	rest := harmlessRedirect.ReplaceAllString(cmd, "")
	if strings.ContainsAny(rest, "<>`\n") {
		return false
	}
	for i, seg := range shellSplit.Split(rest, -1) {
		f := strings.Fields(seg)
		f = bare(f)
		if len(f) == 0 {
			if i == 0 {
				return false
			}
			continue // "ls; true;" leaves an empty end
		}
		p := f[0]
		if !readOnlyPrograms[p] || writes(p, f[1:]) {
			return false
		}
		if sub, ok := readOnlySub[p]; ok {
			if len(f) < 2 || !sub[f[1]] {
				return false
			}
			// ip -j route get: the first word that is not an option decides
			if p == "ip" {
				w := ""
				for _, a := range f[1:] {
					if !strings.HasPrefix(a, "-") {
						w = a
						break
					}
				}
				if !sub[w] {
					return false
				}
				for _, a := range f[1:] {
					switch a {
					case "add", "del", "delete", "set", "flush", "replace", "change":
						return false
					}
				}
			}
		}
	}
	return true
}

// writes finds the options with which an otherwise reading program changes something.
func writes(p string, args []string) bool {
	switch p {
	case "sort":
		for _, a := range args {
			if strings.HasPrefix(a, "-o") || strings.HasPrefix(a, "--output") {
				return true
			}
		}
	case "date":
		for _, a := range args {
			if strings.HasPrefix(a, "-s") || strings.HasPrefix(a, "--set") {
				return true
			}
		}
	case "hostname":
		return len(args) > 0
	}
	return false
}
