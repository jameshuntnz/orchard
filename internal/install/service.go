package install

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// serviceStep returns the supervisor integration for this platform.
func serviceStep(p Plan) Step {
	if runtime.GOOS == "darwin" {
		return launchdStep{plan: p}
	}
	return systemdStep{plan: p}
}

func supervisorName() string {
	if runtime.GOOS == "darwin" {
		return "launchd"
	}
	return "systemd"
}

func ownerUID(info fs.FileInfo) (int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// ---------------------------------------------------------------- launchd

const launchdLabel = "net.orchard"

type launchdStep struct{ plan Plan }

func (launchdStep) Name() string { return "launchd daemon" }

func (s launchdStep) plistPath() string {
	return filepath.Join("/Library/LaunchDaemons", launchdLabel+".plist")
}

func (s launchdStep) Check(ctx context.Context) State {
	if _, err := os.Stat(s.plistPath()); err != nil {
		return Fixable("not registered")
	}
	want, err := s.plist()
	if err == nil {
		if have, err := os.ReadFile(s.plistPath()); err == nil && string(have) != want {
			return Fixable("registered, but the plist differs from this plan")
		}
	}
	out, err := run(ctx, "launchctl", "print", "system/"+launchdLabel)
	if err != nil {
		return Fixable("plist exists but the job is not loaded")
	}
	if strings.Contains(out, "state = running") {
		return OK("%s loaded and running", launchdLabel)
	}
	return OK("%s loaded (not currently running)", launchdLabel)
}

func (s launchdStep) Fix(ctx context.Context) (string, error) {
	if err := requireRoot("registering a LaunchDaemon"); err != nil {
		return "", err
	}
	body, err := s.plist()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(s.plistPath(), []byte(body), 0o644); err != nil {
		return "", err
	}
	// launchd requires the plist to be owned by root and not group or world writable.
	if err := os.Chown(s.plistPath(), 0, 0); err != nil {
		return "", err
	}

	// bootout first so re-running picks up a changed plist; it fails harmlessly when
	// nothing is loaded.
	_, _ = run(ctx, "launchctl", "bootout", "system/"+launchdLabel)
	if out, err := run(ctx, "launchctl", "bootstrap", "system", s.plistPath()); err != nil {
		return "", fmt.Errorf("launchctl bootstrap failed: %s", out)
	}
	return fmt.Sprintf("registered %s (starts on boot)", launchdLabel), nil
}

// plist renders the daemon definition.
//
// launchd has no EnvironmentFile, so the token would otherwise have to live in this
// plist — which must be root-owned and world-readable. Sourcing a 0600 file from a shell
// keeps the secret out of it and lets the token be rotated without touching launchd
// configuration. exec means launchd supervises orchard itself, not the shell.
func (s launchdStep) plist() (string, error) {
	if strings.ContainsAny(s.plan.Prefix, "'\"$`\\") {
		return "", fmt.Errorf("prefix %q contains characters that are unsafe to embed in a shell command", s.plan.Prefix)
	}
	command := fmt.Sprintf("set -a; . %s; set +a; exec %s serve", s.plan.EnvFile(), s.plan.Binary())

	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>UserName</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/sh</string>
		<string>-c</string>
		<string>%s</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>WorkingDirectory</key>
	<string>%s</string>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
	<key>ProcessType</key>
	<string>Background</string>
</dict>
</plist>
`,
		xmlText(launchdLabel),
		xmlText(s.plan.User),
		xmlText(command),
		xmlText(s.plan.Prefix),
		xmlText(s.plan.LogFile()),
		xmlText(s.plan.LogFile()),
	), nil
}

// ---------------------------------------------------------------- systemd

const systemdUnit = "orchard.service"

type systemdStep struct{ plan Plan }

func (systemdStep) Name() string { return "systemd unit" }

func (s systemdStep) unitPath() string {
	return filepath.Join("/etc/systemd/system", systemdUnit)
}

func (s systemdStep) Check(ctx context.Context) State {
	if _, err := os.Stat(s.unitPath()); err != nil {
		return Fixable("not installed")
	}
	if have, err := os.ReadFile(s.unitPath()); err == nil && string(have) != s.unit() {
		return Fixable("installed, but the unit differs from this plan")
	}
	enabled, err := run(ctx, "systemctl", "is-enabled", systemdUnit)
	if err != nil {
		return Fixable("unit exists but is not enabled")
	}
	active, _ := run(ctx, "systemctl", "is-active", systemdUnit)
	return OK("%s %s, %s", systemdUnit, strings.TrimSpace(enabled), strings.TrimSpace(active))
}

func (s systemdStep) Fix(ctx context.Context) (string, error) {
	if err := requireRoot("installing a systemd unit"); err != nil {
		return "", err
	}
	if err := os.WriteFile(s.unitPath(), []byte(s.unit()), 0o644); err != nil {
		return "", err
	}
	if out, err := run(ctx, "systemctl", "daemon-reload"); err != nil {
		return "", fmt.Errorf("systemctl daemon-reload failed: %s", out)
	}
	if out, err := run(ctx, "systemctl", "enable", "--now", systemdUnit); err != nil {
		return "", fmt.Errorf("systemctl enable failed: %s", out)
	}
	return fmt.Sprintf("installed and started %s", systemdUnit), nil
}

// unit renders the service definition. Unlike launchd, systemd reads an environment file
// natively, so there is no shell in the way.
func (s systemdStep) unit() string {
	return fmt.Sprintf(`[Unit]
Description=Orchard — internal build distribution over a tailnet
Documentation=https://github.com/jameshuntnz/orchard
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=%s
WorkingDirectory=%s
EnvironmentFile=%s
ExecStart=%s serve
# The supervisor half of the update story: on exit, bring it back.
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`, s.plan.User, s.plan.Prefix, s.plan.EnvFile(), s.plan.Binary())
}
