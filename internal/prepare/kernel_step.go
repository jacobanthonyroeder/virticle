package prepare

import (
	"fmt"
	"strings"
)

// loadModuleScript loads the module $m and says how it went, in lines: "result=ok", or "result=fail" with "err=" (what
// modprobe or the wait for /dev/kvm said), "blocker=" and "blockkind=" (a modprobe.d file with a blacklist or install
// line for the module; then nothing is loaded) and "klog=" (the kernel's lines about KVM). It never fails itself. No
// module option is passed.
const loadModuleScript = `
m2=$(echo "$m" | tr _ -)
fail() {
  echo "result=fail"; echo "err=$1"
  { dmesg 2>/dev/null || journalctl -k -b --no-pager 2>/dev/null; } | sed 's/^\[[^]]*\] *//' | grep -iE '^(kvm[_-]?(intel|amd)?|vmx|svm)[: ]|disabled by bios' | tail -n 3 | sed 's/^/klog=/'
  exit 0
}
# A blacklist line keeps the module from loading at boot (also from modules-load.d); an install line replaces the load.
b=$(grep -rE "^[[:space:]]*(blacklist|install)[[:space:]]+($m|$m2)([[:space:]]|\$)" ` + modprobeDirs + ` 2>/dev/null | grep -v -e '--ignore-install' | head -n 1)
if [ -n "$b" ]; then
  echo "result=fail"; echo "blocker=$(echo "$b" | cut -d: -f1)"; echo "blockkind=$(echo "$b" | cut -d: -f2- | awk '{print $1}')"; exit 0
fi
if ! out=$(modprobe "$m" 2>&1); then fail "$(echo "$out" | head -n 1)"; fi
case "$m" in kvm_*)
  i=0; while [ ! -e /dev/kvm ] && [ $i -lt 25 ]; do sleep 0.2; i=$((i+1)); done
  [ -e /dev/kvm ] || fail "/dev/kvm did not appear after loading $m"
esac
echo "result=ok"`

// stepKernel loads the kernel modules prepare wants (see ModulesWanted) and lists them in the drop-in that Virticle
// owns. It changes no module option, no other file in /etc/modules-load.d or /etc/modprobe.d, and not the kernel
// command line. A module that does not load fails the step with the kernel's reason.
func stepKernel(r *Runner, p Plan, o Options) (string, bool, error) {
	f := p.Facts
	want := ModulesWanted(f)
	if len(want) == 0 {
		if m := kvmModule(f.CPUVirt); m != "" {
			if f.KMod[m] == "builtin" {
				return m + " is built into the kernel", false, nil
			}
			return m + " is loaded by the system", false, nil
		}
		return "No module to load", false, nil
	}
	changed := false
	var parts []string
	for _, m := range want {
		if f.KMod[m] != "absent" {
			continue // loaded already, only the drop-in is kept
		}
		out, err := r.root("m="+shq(m)+"\n"+loadModuleScript, "")
		if err != nil {
			return "", false, fmt.Errorf("loading %s: %w: %s", m, err, firstLines(out, 4))
		}
		res := parseLoad(out)
		if !res.ok {
			return "", false, fmt.Errorf("%s", ModuleFailure(m, res.err, res.klog, res.blocker, res.blockKind, f.KernelInfo()))
		}
		parts = append(parts, "loaded "+m)
		changed = true
	}
	if ModulesFileForeign(f) {
		return "", changed, &statusError{Warning, strings.Join(append(parts, ModulesFile+" exists and was not written by Virticle; it is left alone, so "+strings.Join(want, " ")+" is not loaded at boot unless you list it there"), "; ")}
	}
	d, ch, err := installFile(r, "the module list", ModulesFile, "755", "644", ModuleFile(want), "", "")
	if err != nil {
		return "", false, err
	}
	parts = append(parts, d)
	return strings.Join(parts, "; "), changed || ch, nil
}

type loadResult struct {
	ok                            bool
	err, klog, blocker, blockKind string
}

func parseLoad(out string) loadResult {
	var r loadResult
	var kl []string
	for _, l := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(strings.TrimRight(l, "\r"), "=")
		switch k {
		case "result":
			r.ok = v == "ok"
		case "err":
			r.err = v
		case "blocker":
			r.blocker = v
		case "blockkind":
			r.blockKind = v
		case "klog":
			kl = append(kl, v)
		}
	}
	r.klog = strings.Join(kl, "\n")
	return r
}
