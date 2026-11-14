// Package minver holds the one rule about the software a node must run:
// libvirt 11.0 and QEMU 10.0 or newer. Prepare, the connection test and every
// connection of a node use Check; nothing else in the program compares these
// versions, and there is no way to switch the rule off.
package minver

import (
	"fmt"
	"regexp"
	"strconv"
)

// The oldest versions Virticle works with, as libvirt numbers them:
// major*1000000 + minor*1000 + release.
const (
	Libvirt uint64 = 11000000
	QEMU    uint64 = 10000000
)

// Encode returns the number libvirt uses for a version.
func Encode(major, minor, release uint64) uint64 {
	return major*1000000 + minor*1000 + release
}

// Format writes a libvirt version number as "11.3.0".
func Format(v uint64) string {
	return strconv.FormatUint(v/1000000, 10) + "." + strconv.FormatUint(v/1000%1000, 10) + "." + strconv.FormatUint(v%1000, 10)
}

// short writes a version without its release number: "11.0".
func short(v uint64) string {
	return strconv.FormatUint(v/1000000, 10) + "." + strconv.FormatUint(v/1000%1000, 10)
}

// Needs is the requirement in words.
func Needs() string {
	return "Virticle needs libvirt " + short(Libvirt) + " and QEMU " + short(QEMU) + " or newer."
}

// Describe names two versions: "libvirt 10.0.0 and QEMU 8.2.2".
func Describe(libvirt, qemu uint64) string {
	return "libvirt " + Format(libvirt) + " and QEMU " + Format(qemu)
}

// Check says whether a node with these two versions is supported: the libvirt
// version (ConnectGetLibVersion) and the hypervisor version (ConnectGetVersion).
// The error is a plain sentence that can be shown as it is.
func Check(libvirt, qemu uint64) error {
	if libvirt >= Libvirt && qemu >= QEMU {
		return nil
	}
	return fmt.Errorf("This node has %s. %s", Describe(libvirt, qemu), Needs())
}

var versionText = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)

// Parse reads the first version in a text such as "11.3.0", "1:10.0.2+ds-2"
// (a package version) or "QEMU emulator version 10.0.2 (Debian 1:10.0.2+ds-2)".
func Parse(text string) (uint64, bool) {
	m := versionText.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	var n [3]uint64
	for i := range n {
		if m[i+1] == "" {
			continue
		}
		v, err := strconv.ParseUint(m[i+1], 10, 32)
		if err != nil || (i > 0 && v > 999) {
			return 0, false
		}
		n[i] = v
	}
	return Encode(n[0], n[1], n[2]), true
}
