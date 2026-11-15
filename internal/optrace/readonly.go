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

// harmlessRedirect: a redirection to /dev/null or between descriptors, at the start of the text it is matched on.
var harmlessRedirect = regexp.MustCompile(`^>&\d|^>\s*/dev/null|^>>\s*/dev/null`)

// probeRE is the reachability probe of the migration checks: it opens a TCP connection and closes it.
var probeRE = regexp.MustCompile(`^timeout [0-9]+ bash -c 'exec 3<>/dev/tcp/[A-Za-z0-9.:_\[\]-]+/[0-9]+'$`)

// readOnlyShell: every simple command of the script (split at ; && || | and new lines, outside quotes) is a
// listed reading program, nothing is redirected to a file, and nothing in the text is a program that changes
// things. for and if are understood; the commands inside $( ) are checked the same way.
func readOnlyShell(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return false
	}
	if probeRE.MatchString(cmd) {
		return true
	}
	if changing.MatchString(cmd) {
		return false
	}
	segs, subs, ok := splitShell(cmd)
	if !ok {
		return false
	}
	for _, sub := range subs {
		if !readOnlyShell(sub) {
			return false
		}
	}
	commands := 0
	for _, seg := range segs {
		f := strings.Fields(seg)
		for len(f) > 0 { // leading keywords of if and for
			switch f[0] {
			case "do", "then", "else", "elif", "if", "!":
				f = f[1:]
				continue
			}
			break
		}
		if len(f) == 0 || f[0] == "done" || f[0] == "fi" {
			continue
		}
		if f[0] == "for" { // for NAME in WORDS: the words were checked as $( ) parts
			commands++
			continue
		}
		f = bare(f)
		if len(f) == 0 {
			return false
		}
		commands++
		p := f[0]
		if !readOnlyPrograms[p] || writes(p, f[1:]) {
			return false
		}
		if sub, ok := readOnlySub[p]; ok && !subReads(p, sub, f[1:]) {
			return false
		}
	}
	return commands > 0
}

// subReads checks the arguments of ip and systemctl.
func subReads(p string, sub map[string]bool, args []string) bool {
	if len(args) == 0 {
		return false
	}
	if p == "ip" {
		w := ""
		for _, a := range args {
			if !strings.HasPrefix(a, "-") {
				w = a
				break
			}
		}
		if !sub[w] {
			return false
		}
		for _, a := range args {
			switch a {
			case "add", "del", "delete", "set", "flush", "replace", "change":
				return false
			}
		}
		return true
	}
	return sub[args[0]]
}

// splitShell cuts a script into its simple commands. Quotes keep their text together; a '>' or '<' outside single
// quotes that is not a harmless redirection, or a back tick, makes it not ok. subs are the texts of $( ) parts.
func splitShell(s string) (segs, subs []string, ok bool) {
	var cur strings.Builder
	var quote byte
	depth := 0
	var subStart []int
	flush := func() {
		segs = append(segs, cur.String())
		cur.Reset()
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote == '\'' {
			cur.WriteByte(c)
			if c == '\'' {
				quote = 0
			}
			continue
		}
		switch {
		case c == '\\' && i+1 < len(s):
			cur.WriteByte(c)
			cur.WriteByte(s[i+1])
			i++
			continue
		case c == '`':
			return nil, nil, false
		case c == '\'':
			if quote == 0 {
				quote = '\''
			}
		case c == '"':
			if quote == '"' {
				quote = 0
			} else {
				quote = '"'
			}
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			depth++
			subStart = append(subStart, i+2)
		case c == ')' && depth > 0:
			depth--
			from := subStart[len(subStart)-1]
			subStart = subStart[:len(subStart)-1]
			subs = append(subs, s[from:i])
		case c == '>' || c == '<':
			m := ""
			if c == '>' {
				m = harmlessRedirect.FindString(s[i:])
			}
			if m == "" {
				return nil, nil, false
			}
			// the redirection is dropped, with its descriptor number ("2>/dev/null")
			t := strings.TrimRight(cur.String(), "0123456789")
			if t == "" || t[len(t)-1] == ' ' || t[len(t)-1] == '\t' {
				cur.Reset()
				cur.WriteString(t)
			}
			i += len(m) - 1
			continue
		}
		if quote == 0 && depth == 0 {
			switch {
			case c == ';' || c == '\n':
				flush()
				continue
			case c == '&' && i+1 < len(s) && s[i+1] == '&', c == '|' && i+1 < len(s) && s[i+1] == '|':
				flush()
				i++
				continue
			case c == '|':
				flush()
				continue
			}
		}
		cur.WriteByte(c)
	}
	if quote != 0 || depth != 0 {
		return nil, nil, false
	}
	flush()
	return segs, subs, true
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
		for _, a := range args {
			if !strings.HasPrefix(a, "-") {
				return true
			}
		}
	case "awk":
		for _, a := range args {
			if strings.ContainsAny(a, ">|") {
				return true
			}
		}
	}
	return false
}
