package nodeproto

import "sort"

// Every verb of the node command is classified here, once: ReadOnly changes nothing on the node (it may read
// files, ask a peer, or hold a temporary file of its own), Changing does anything else. The operation trace
// (internal/optrace) leaves out the read-only verbs, as it leaves out read-only libvirt calls.
//
// A new verb must be added to one of the two lists: TestVerbsClassified in internal/nodecmd fails otherwise.
// When unsure, list it as changing: a verb in neither list is recorded, which is the safe side.

// ReadOnlyVerbs are the verbs that change nothing on the node.
var ReadOnlyVerbs = []string{
	"backup-scan", "convert-poll", "df", "disk-boot", "files-size", "free-space", "gpu-users", "guest-inspect",
	"hook-qemu", "image-info", "import-convert-status", "migtls-state", "mtime", "net-info", "net-plan", "net-status",
	"peer-ask", "peer-key", "peer-list", "peer-ping", "qemu-img-info", "read", "realpath", "repl-ls", "roots",
	"selfcheck", "sha256", "stat", "svc-hosts", "svc-list", "svc-peerkey", "tar-list", "tar-pack", "tools-missing",
	"update-status", "version", "vm-probe", "work-ls", "work-stale",
}

// ChangingVerbs are the verbs that create, change, move or remove something, start or stop a program, or send
// data to another node.
var ChangingVerbs = []string{
	"age-decrypt", "age-encrypt", "backup-detach", "chmod-work", "convert-finish", "convert-kill", "convert-run",
	"convert-start", "copy", "copy-kill", "download", "download-cancel", "guest-customize", "import-convert-kill",
	"import-convert-run", "import-convert-start", "migtls-commit", "migtls-discard", "migtls-mkdir", "migtls-put",
	"migtls-remove", "migtls-rmdir", "mkworkdir", "move", "net-apply", "net-boot", "net-confirm", "net-rollback",
	"net-run", "node-reboot", "nvram-new", "peer-allow", "peer-disallow", "peer-hostkey-set", "peer-hostkey-unset",
	"peer-recv", "peer-send", "qcow2-bitmap-rm", "remove", "repl-commit", "repl-overlay", "repl-rebase", "rmdir",
	"rmtree", "svc-allow", "sysprep", "tar-extract", "tar-unpack", "tpm-place", "tpm-save", "update-run",
	"update-start", "write",
}

var readOnly = func() map[string]bool {
	m := map[string]bool{}
	for _, v := range ReadOnlyVerbs {
		m[v] = true
	}
	return m
}()

// ReadOnlyVerb says whether running verb with args changes nothing on the node. A verb that is not listed (or
// listed as changing) is not read-only. selfcheck only reports unless it is told to repair.
func ReadOnlyVerb(verb string, args []string) bool {
	if verb == "selfcheck" {
		for _, a := range args {
			if a == "repair" {
				return false
			}
		}
	}
	return readOnly[verb]
}

// ClassifiedVerbs returns every listed verb, sorted: the two lists together.
func ClassifiedVerbs() []string {
	all := append(append([]string{}, ReadOnlyVerbs...), ChangingVerbs...)
	sort.Strings(all)
	return all
}
