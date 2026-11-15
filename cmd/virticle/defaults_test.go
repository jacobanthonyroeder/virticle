package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"virticle/internal/store"
	"virticle/internal/webaccess"
)

// -data: the packages' directory when it holds a database or the package is installed, else "data".
func TestPickDataDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	if got := pickDataDir(missing, missing); got != "data" {
		t.Errorf("missing directory: %q", got)
	}
	empty := t.TempDir()
	if got := pickDataDir(empty, missing); got != "data" {
		t.Errorf("directory without a database: %q", got)
	}
	with := t.TempDir()
	if err := os.WriteFile(filepath.Join(with, store.DBName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := pickDataDir(with, missing); got != with {
		t.Errorf("directory with a database: %q", got)
	}
	// The package is installed but its service never ran: setup must work on the service's directory.
	unit := filepath.Join(t.TempDir(), "virticle.service")
	os.WriteFile(unit, nil, 0o644)
	if got := pickDataDir(missing, unit); got != missing {
		t.Errorf("installed package: %q", got)
	}
}

// The listener without flags: stored settings win; an existing installation
// without stored settings keeps loopback HTTP and is told; a new one gets
// HTTPS with a self-signed certificate on all interfaces.
func TestChooseSettings(t *testing.T) {
	stored := webaccess.Settings{Mode: "own", HTTPSListen: "10.0.0.5:443"}
	c := chooseSettings(stored, true, true, "0.0.0.0:8080")
	if c.settings.Mode != "own" || c.source != "stored" || c.notice != "" || c.persist {
		t.Errorf("stored: %+v", c)
	}
	c = chooseSettings(webaccess.Settings{}, false, true, "")
	if c.settings.Mode != webaccess.ModeOff || c.settings.Listen != "127.0.0.1:8080" || c.notice == "" || c.persist {
		t.Errorf("upgrade without stored settings: %+v", c)
	}
	c = chooseSettings(webaccess.Settings{}, false, true, "0.0.0.0:8080")
	if c.settings.Mode != webaccess.ModeSelfSigned || c.settings.HTTPSListen != "0.0.0.0:8080" || c.notice == "" || c.persist || c.source != "default" {
		t.Errorf("container upgrade: %+v", c)
	}
	c = chooseSettings(webaccess.Settings{}, false, false, "0.0.0.0:8080")
	if c.settings.HTTPSListen != webaccess.DefaultHTTPSListen || !c.persist {
		t.Errorf("fresh container install ignores the upgrade address: %+v", c)
	}
	c = chooseSettings(webaccess.Settings{}, false, false, "")
	if c.settings.Mode != webaccess.ModeSelfSigned || c.settings.HTTPSListen != webaccess.DefaultHTTPSListen || c.notice != "" || !c.persist {
		t.Errorf("fresh install: %+v", c)
	}
}

// The port of a fresh install is one constant, and it is not the port that
// many other products use.
func TestDefaultPort(t *testing.T) {
	if webaccess.DefaultHTTPSPort != 8463 || webaccess.DefaultHTTPSListen != ":"+strconv.Itoa(webaccess.DefaultHTTPSPort) {
		t.Errorf("%d %s", webaccess.DefaultHTTPSPort, webaccess.DefaultHTTPSListen)
	}
	if webaccess.Defaults().HTTPSListen != webaccess.DefaultHTTPSListen || webaccess.FirstRunDefaults().HTTPSListen != webaccess.DefaultHTTPSListen {
		t.Error("the defaults do not use the constant")
	}
	// An install that stored another port keeps it, with or without users.
	for _, users := range []bool{false, true} {
		stored := webaccess.FirstRunDefaults()
		stored.HTTPSListen = ":8443"
		if c := chooseSettings(stored, true, users, ""); c.settings.HTTPSListen != ":8443" || c.persist {
			t.Errorf("stored settings moved (users %v): %+v", users, c)
		}
	}
}

func TestSetupHint(t *testing.T) {
	ws := webaccess.FirstRunDefaults()
	if got := setupHint(false, ws, []string{"10.0.0.5", "vm1"}); got != "open https://10.0.0.5:8463 to create the administrator" {
		t.Errorf("%q", got)
	}
	if got := setupHint(true, ws, nil); got != "" {
		t.Errorf("with an administrator: %q", got)
	}
	d := webaccess.Defaults()
	if got := setupHint(false, d, nil); !strings.HasPrefix(got, "open http://127.0.0.1:8080 ") {
		t.Errorf("%q", got)
	}
}
