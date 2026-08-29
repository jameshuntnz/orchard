package config

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setEnv clears every Orchard variable, then applies the given ones, so a test never
// inherits the developer's own environment.
func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, k := range []string{
		"ORCHARD_STATE_DIR", "ORCHARD_TOKEN", "ORCHARD_HOSTNAME", "ORCHARD_BASE_URL",
		"ORCHARD_UPLOAD_ADDR", "ORCHARD_UPLOAD_ALLOW", "ORCHARD_MAX_UPLOAD_MB",
		"ORCHARD_MAX_BUILD_AGE_DAYS",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestLoadDefaults(t *testing.T) {
	setEnv(t, map[string]string{
		"ORCHARD_STATE_DIR": t.TempDir(),
		"ORCHARD_TOKEN":     strings.Repeat("t", minTokenLen),
	})

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Hostname != defaultHostname {
		t.Errorf("Hostname = %q, want %q", c.Hostname, defaultHostname)
	}
	if c.MaxUploadBytes != defaultMaxUploadMB<<20 {
		t.Errorf("MaxUploadBytes = %d", c.MaxUploadBytes)
	}
	// The age fallback and the upload listener are both off unless asked for
	// (DESIGN §11, §4.3).
	if c.MaxBuildAge != 0 {
		t.Errorf("MaxBuildAge = %v, want the fallback disabled", c.MaxBuildAge)
	}
	if c.UploadAddr != "" {
		t.Errorf("UploadAddr = %q, want the upload listener disabled", c.UploadAddr)
	}
	// An unset base URL is derived from the tsnet name once it is up.
	if c.BaseURL != "" {
		t.Errorf("BaseURL = %q, want empty", c.BaseURL)
	}
}

func TestLoadRejections(t *testing.T) {
	dir := t.TempDir()
	good := strings.Repeat("t", minTokenLen)

	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"no state dir", map[string]string{"ORCHARD_TOKEN": good}, "ORCHARD_STATE_DIR"},
		{"no token", map[string]string{"ORCHARD_STATE_DIR": dir}, "ORCHARD_TOKEN"},
		// Refuse to start if the token is trivially short (DESIGN §15).
		{"short token", map[string]string{"ORCHARD_STATE_DIR": dir, "ORCHARD_TOKEN": "short"}, "at least"},
		{"schemeless base url", map[string]string{"ORCHARD_STATE_DIR": dir, "ORCHARD_TOKEN": good, "ORCHARD_BASE_URL": "orchard.example.ts.net"}, "scheme"},
		{"bad upload cap", map[string]string{"ORCHARD_STATE_DIR": dir, "ORCHARD_TOKEN": good, "ORCHARD_MAX_UPLOAD_MB": "nope"}, "ORCHARD_MAX_UPLOAD_MB"},
		{"zero upload cap", map[string]string{"ORCHARD_STATE_DIR": dir, "ORCHARD_TOKEN": good, "ORCHARD_MAX_UPLOAD_MB": "0"}, "positive"},
		{"negative age", map[string]string{"ORCHARD_STATE_DIR": dir, "ORCHARD_TOKEN": good, "ORCHARD_MAX_BUILD_AGE_DAYS": "-1"}, "negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setEnv(t, tt.env)
			_, err := Load()
			if err == nil {
				t.Fatal("Load accepted the configuration")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestLoadNormalises(t *testing.T) {
	setEnv(t, map[string]string{
		"ORCHARD_STATE_DIR":          "./relative-state",
		"ORCHARD_TOKEN":              strings.Repeat("t", minTokenLen),
		"ORCHARD_BASE_URL":           "https://orchard.example.ts.net/",
		"ORCHARD_MAX_BUILD_AGE_DAYS": "14",
	})

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(c.StateDir) {
		t.Errorf("StateDir = %q, want an absolute path", c.StateDir)
	}
	if c.BaseURL != "https://orchard.example.ts.net" {
		t.Errorf("BaseURL = %q, want the trailing slash trimmed", c.BaseURL)
	}
	if c.MaxBuildAge != 14*24*time.Hour {
		t.Errorf("MaxBuildAge = %v", c.MaxBuildAge)
	}
}

// The state directory is created on start if absent, with the tsnet subdirectory 0700
// (DESIGN §4.4).
func TestPrepare(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "state")
	if err := Prepare(dir); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dir, TSNetSubdir))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("tsnet dir mode = %o, want 700", perm)
	}

	// The probe file must not be left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("state dir holds %d entries, want just tsnet", len(entries))
	}
}

// Refuse to start on an unwritable state directory, rather than failing later on the
// first publish (DESIGN §4.4).
func TestPrepareRefusesUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	dir := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	if err := Prepare(dir); err == nil {
		t.Error("Prepare accepted an unwritable state directory")
	}
}

// The upload listener is plain HTTP carrying a bearer token in cleartext. Binding it
// without saying who may reach it is the omission this refuses to make silently.
func TestUploadAllowRequiredWithListener(t *testing.T) {
	setEnv(t, map[string]string{
		"ORCHARD_STATE_DIR":   t.TempDir(),
		"ORCHARD_TOKEN":       strings.Repeat("t", minTokenLen),
		"ORCHARD_UPLOAD_ADDR": "0.0.0.0:8477",
	})
	_, err := Load()
	if err == nil {
		t.Fatal("Load accepted an upload listener with no allowlist")
	}
	if !strings.Contains(err.Error(), "ORCHARD_UPLOAD_ALLOW") {
		t.Errorf("error = %q, want it to name the variable", err)
	}
}

func TestUploadAllowRejectedWithoutListener(t *testing.T) {
	setEnv(t, map[string]string{
		"ORCHARD_STATE_DIR":    t.TempDir(),
		"ORCHARD_TOKEN":        strings.Repeat("t", minTokenLen),
		"ORCHARD_UPLOAD_ALLOW": "127.0.0.1",
	})
	if _, err := Load(); err == nil {
		t.Error("Load accepted an allowlist with no listener to apply it to")
	}
}

func TestUploadAllowMatching(t *testing.T) {
	setEnv(t, map[string]string{
		"ORCHARD_STATE_DIR":    t.TempDir(),
		"ORCHARD_TOKEN":        strings.Repeat("t", minTokenLen),
		"ORCHARD_UPLOAD_ADDR":  "0.0.0.0:8477",
		"ORCHARD_UPLOAD_ALLOW": "127.0.0.1, 192.168.64.0/18",
	})
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}

	tests := map[string]bool{
		"127.0.0.1":           true,
		"192.168.64.1":        true,  // a bridge gateway
		"192.168.64.7":        true,  // a guest on it
		"192.168.99.3":        true,  // a bridge that renumbered
		"::ffff:192.168.64.7": true,  // the same guest, v4-mapped
		"192.168.0.145":       false, // the host's own LAN address
		"192.168.0.50":        false, // anything else on the LAN
		"100.66.217.76":       false, // a tailnet node
		"::1":                 false, // not listed, so not allowed
	}
	for s, want := range tests {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if got := c.AllowsUpload(addr); got != want {
			t.Errorf("AllowsUpload(%s) = %v, want %v", s, got, want)
		}
	}
}

// An unrestricted listener must be a decision, and a visible one.
func TestUploadAllowAny(t *testing.T) {
	setEnv(t, map[string]string{
		"ORCHARD_STATE_DIR":    t.TempDir(),
		"ORCHARD_TOKEN":        strings.Repeat("t", minTokenLen),
		"ORCHARD_UPLOAD_ADDR":  "0.0.0.0:8477",
		"ORCHARD_UPLOAD_ALLOW": "any",
	})
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.UploadAllowAny {
		t.Fatal("UploadAllowAny not set")
	}
	if !c.AllowsUpload(netip.MustParseAddr("203.0.113.9")) {
		t.Error("any did not allow an arbitrary source")
	}
}

func TestUploadAllowRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"not-an-address", "192.168.0.0/99", "192.168.0.1/", ","} {
		setEnv(t, map[string]string{
			"ORCHARD_STATE_DIR":    t.TempDir(),
			"ORCHARD_TOKEN":        strings.Repeat("t", minTokenLen),
			"ORCHARD_UPLOAD_ADDR":  "0.0.0.0:8477",
			"ORCHARD_UPLOAD_ALLOW": bad,
		})
		if _, err := Load(); err == nil {
			t.Errorf("Load accepted ORCHARD_UPLOAD_ALLOW=%q", bad)
		}
	}
}
