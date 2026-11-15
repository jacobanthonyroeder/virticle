package optrace

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Redaction. Everything a trace keeps passes through here before it is stored: a password, key or token never
// reaches the database or the API. The rules lean to removing too much: a value that may be a secret is removed.

// Redacted replaces a removed value.
const Redacted = "[redacted]"

// secretWord is a name that marks its value as a secret: an XML attribute or element, a command-line flag, an
// environment variable, a JSON key.
const secretWord = `pass(?:word|wd|phrase)?|secret|token|psk|private[-_]?key|privkey|api[-_]?key|credentials?|auth[-_]?key|crypt(?:ed)?[-_]?pass(?:word)?`

var (
	// PEM blocks: SSH and TLS private keys, certificates with their keys.
	pemRE = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*-----.*?(?:-----END [A-Z0-9 ]*-----|$)`)
	// A password in a URL: scheme://user:password@host.
	urlCredRE = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^\s/:@'"]*):[^\s/@'"]*@`)
	// Query parameters that carry a secret: ?token=..., &sig=...
	queryRE = regexp.MustCompile(`(?i)([?&](?:` + secretWord + `|key|sig|signature|access_token|x-amz-[a-z-]+))=[^&\s'"]*`)

	// XML attributes: passwd='...', secret="...", key='...'.
	xmlAttrRE = regexp.MustCompile(`(?i)(\s(?:[a-z0-9_-]+:)?(?:` + secretWord + `|passwd|key|dhcert|session)\s*=\s*)('[^']*'|"[^"]*")`)
	// XML elements whose text is a secret: <password>..</password>, <dhCert>..</dhCert>.
	xmlElemRE = regexp.MustCompile(`(?is)(<((?:[a-z0-9_-]+:)?(?:` + secretWord + `|passwd|dhcert|session|key-data|keydata|userdata|user-data))(?:\s[^>]*)?>)([^<]*)(</\s*([^\s>]+)\s*>)`)
	// What the firmware hands to the guest (OEM strings, fw_cfg entries) can be cloud-init or Ignition data
	// with passwords in it.
	sysinfoRE = regexp.MustCompile(`(?is)<oemStrings\b.*?</oemStrings>|<sysinfo\s+type=['"]fwcfg['"].*?</sysinfo>`)
	entryRE   = regexp.MustCompile(`(?is)(<entry\b[^>]*>)([^<]*)(</entry>)`)
	// QEMU pass-through arguments and environment: <qemu:arg value='-object secret,id=s0,data=...'/>,
	// <qemu:env name='X' value='...'/>. Environment values are always removed.
	qemuArgRE = regexp.MustCompile(`(?is)(<qemu:(arg|env)\b[^>]*\bvalue\s*=\s*)('[^']*'|"[^"]*")`)
	secretish = regexp.MustCompile(`(?i)pass|secret|key|token|cred|auth`)

	// Command lines: --password VALUE, --root-password=VALUE, -p VALUE after sshpass.
	flagRE    = regexp.MustCompile(`(?i)((?:^|\s)--?(?:[a-z0-9-]*-)?(?:` + secretWord + `|key|keyfile|key-file|pw)(?:-file)?(?:=|\s+))('[^']*'|"[^"]*"|\S+)`)
	sshpassRE = regexp.MustCompile(`(\bsshpass\s+-p\s*)('[^']*'|"[^"]*"|\S+)`)
	// Environment: DB_PASSWORD=..., AWS_SECRET_ACCESS_KEY=...
	envRE = regexp.MustCompile(`(\b[A-Za-z_][A-Za-z0-9_]*(?i:pass|secret|token|key|psk|cred)[A-Za-z0-9_]*=)('[^']*'|"[^"]*"|\S+)`)
	// password:VALUE, token=VALUE anywhere (virt-customize takes --root-password password:VALUE).
	pairRE = regexp.MustCompile(`(?i)(\b(?:` + secretWord + `)\s*[:=]\s*)('[^']*'|"[^"]*"|[^\s,;&'"]+)`)
)

// RedactText removes the secrets any text can carry: private keys, passwords in URLs and query strings, and
// name=value pairs with a secret name.
func RedactText(s string) string {
	if s == "" {
		return s
	}
	s = pemRE.ReplaceAllString(s, Redacted)
	s = urlCredRE.ReplaceAllString(s, "$1:"+Redacted+"@")
	s = queryRE.ReplaceAllString(s, "$1="+Redacted)
	s = pairRE.ReplaceAllStringFunc(s, func(m string) string {
		p := pairRE.FindStringSubmatch(m)
		if p[2] == Redacted || strings.HasPrefix(p[2], "'"+Redacted) || strings.HasPrefix(p[2], `"`+Redacted) {
			return m
		}
		return p[1] + quoteLike(p[2])
	})
	return s
}

// RedactCommand is RedactText for a command line, plus the values after secret flags (--password VALUE) and of
// secret environment variables (DB_PASSWORD=VALUE).
func RedactCommand(s string) string {
	s = sshpassRE.ReplaceAllStringFunc(s, func(m string) string {
		p := sshpassRE.FindStringSubmatch(m)
		return p[1] + quoteLike(p[2])
	})
	s = flagRE.ReplaceAllStringFunc(s, func(m string) string {
		p := flagRE.FindStringSubmatch(m)
		return p[1] + quoteLike(p[2])
	})
	s = envRE.ReplaceAllStringFunc(s, func(m string) string {
		p := envRE.FindStringSubmatch(m)
		return p[1] + quoteLike(p[2])
	})
	return RedactText(s)
}

// RedactXML removes secrets from a libvirt XML document: VNC and SPICE passwords, attributes and elements with
// a secret name, what the firmware passes to the guest, QEMU pass-through arguments that look like secrets, and
// whatever RedactText finds.
func RedactXML(s string) string {
	if s == "" {
		return s
	}
	s = xmlAttrRE.ReplaceAllStringFunc(s, func(m string) string {
		p := xmlAttrRE.FindStringSubmatch(m)
		return p[1] + quoteLike(p[2])
	})
	s = xmlElemRE.ReplaceAllStringFunc(s, func(m string) string {
		p := xmlElemRE.FindStringSubmatch(m)
		if strings.TrimSpace(p[3]) == "" || !strings.EqualFold(p[5], p[2]) {
			return m
		}
		return p[1] + Redacted + p[4]
	})
	s = sysinfoRE.ReplaceAllStringFunc(s, func(m string) string {
		return entryRE.ReplaceAllString(m, "${1}"+Redacted+"${3}")
	})
	s = qemuArgRE.ReplaceAllStringFunc(s, func(m string) string {
		p := qemuArgRE.FindStringSubmatch(m)
		if p[2] == "arg" && !secretish.MatchString(p[3]) {
			return m
		}
		return p[1] + quoteLike(p[3])
	})
	return RedactText(s)
}

// secretKeyRE matches the JSON keys of guest agent commands that carry a secret or data written into the guest:
// guest-set-user-password's "password", guest-exec's "input-data", guest-file-write's "buf-b64".
var secretKeyRE = regexp.MustCompile(`(?i)^(?:` + secretWord + `|input-data|buf-b64|data|keys?)$`)

// RedactJSON removes the values of secret keys from a JSON document (a guest agent or monitor command). Text
// that is not JSON goes through RedactText.
func RedactJSON(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return RedactText(s)
	}
	v = redactValue(v)
	b, err := json.Marshal(v)
	if err != nil {
		return Redacted
	}
	return RedactText(string(b))
}

func redactValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if secretKeyRE.MatchString(k) {
				if _, isBool := e.(bool); !isBool {
					x[k] = Redacted
					continue
				}
			}
			x[k] = redactValue(e)
		}
	case []any:
		for i := range x {
			x[i] = redactValue(x[i])
		}
	}
	return v
}

// quoteLike is Redacted in the quotes the value had.
func quoteLike(v string) string {
	if len(v) >= 2 && (v[0] == '\'' || v[0] == '"') && v[len(v)-1] == v[0] {
		return string(v[0]) + Redacted + string(v[0])
	}
	return Redacted
}
