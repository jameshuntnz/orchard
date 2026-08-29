package install

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- platform

type platformStep struct{}

func (platformStep) Name() string { return "platform" }

func (platformStep) Check(context.Context) State {
	target := runtime.GOOS + "/" + runtime.GOARCH
	if !Supported() {
		return Failed("%s has no supervisor integration; run orchard serve under your own supervisor", target)
	}
	return OK("%s, %s", target, supervisorName())
}

func (platformStep) Fix(context.Context) (string, error) { return "", fmt.Errorf("unreachable") }

// ---------------------------------------------------------------- service user

type userStep struct{ plan Plan }

func (userStep) Name() string { return "service user" }

// Check does not offer to create the account. Creating a user differs enough between
// macOS and every Linux distribution that guessing would be worse than saying so.
func (s userStep) Check(context.Context) State {
	u, err := user.Lookup(s.plan.User)
	if err != nil {
		return Manual(
			fmt.Sprintf("%s does not exist", s.plan.User),
			createUserInstructions(s.plan.User),
		)
	}
	if u.Uid == "0" {
		return Failed("%s is root; the service should own nothing else on this host", s.plan.User)
	}
	return OK("%s (uid %s)", u.Username, u.Uid)
}

func (userStep) Fix(context.Context) (string, error) { return "", fmt.Errorf("unreachable") }

func lookupIDs(name string) (uid, gid int, err error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, err
	}
	uid, err = strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, err
	}
	gid, err = strconv.Atoi(u.Gid)
	if err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

// ---------------------------------------------------------------- directories

type dirsStep struct{ plan Plan }

func (dirsStep) Name() string { return "directories" }

func (s dirsStep) dirs() []string {
	return []string{s.plan.Prefix, s.plan.BinDir(), s.plan.StateDir()}
}

func (s dirsStep) Check(context.Context) State {
	uid, _, err := lookupIDs(s.plan.User)
	if err != nil {
		return Unverified("cannot resolve %s", s.plan.User)
	}
	for _, d := range s.dirs() {
		info, err := os.Stat(d)
		if err != nil {
			return Fixable("%s is missing", d)
		}
		if !info.IsDir() {
			return Failed("%s is not a directory", d)
		}
		owner, ok := ownerUID(info)
		if !ok {
			return Unverified("cannot read ownership of %s", d)
		}
		// The service user owning the install directory is the whole reason a later
		// self-update needs no privilege (DESIGN §13.1).
		if owner != uid {
			return Fixable("%s is not owned by %s", d, s.plan.User)
		}
	}
	return OK("%s owned by %s", s.plan.Prefix, s.plan.User)
}

func (s dirsStep) Fix(context.Context) (string, error) {
	if err := requirePrivilege(s.plan.User, "creating "+s.plan.Prefix); err != nil {
		return "", err
	}
	uid, gid, err := lookupIDs(s.plan.User)
	if err != nil {
		return "", err
	}
	for _, d := range s.dirs() {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", err
		}
		if err := chownIfNeeded(d, uid, gid); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("created %s, owned by %s", s.plan.Prefix, s.plan.User), nil
}

// ---------------------------------------------------------------- configuration

type configStep struct{ plan Plan }

func (configStep) Name() string { return "configuration" }

// Check never reads the file's contents beyond the keys it must confirm, and never
// reports the token itself.
func (s configStep) Check(context.Context) State {
	info, err := os.Stat(s.plan.EnvFile())
	if err != nil {
		return Fixable("%s is missing", s.plan.EnvFile())
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return Failed("%s is mode %o; it holds the write token and must not be group or world readable", s.plan.EnvFile(), perm)
	}
	raw, err := os.ReadFile(s.plan.EnvFile())
	if err != nil {
		return Unverified("cannot read %s", s.plan.EnvFile())
	}
	if !strings.Contains(string(raw), "ORCHARD_TOKEN=") {
		return Failed("%s has no ORCHARD_TOKEN", s.plan.EnvFile())
	}
	return OK("%s (0600)", s.plan.EnvFile())
}

// Fix writes a fresh environment file. It never overwrites an existing one, so
// re-running the installer cannot invalidate the token CI is already using.
func (s configStep) Fix(context.Context) (string, error) {
	if _, err := os.Stat(s.plan.EnvFile()); err == nil {
		return "", fmt.Errorf("%s already exists; not overwriting it", s.plan.EnvFile())
	}
	uid, gid, err := lookupIDs(s.plan.User)
	if err != nil {
		return "", err
	}
	token, err := newToken()
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "ORCHARD_STATE_DIR=%s\n", s.plan.StateDir())
	fmt.Fprintf(&b, "ORCHARD_TOKEN=%s\n", token)
	fmt.Fprintf(&b, "ORCHARD_HOSTNAME=%s\n", s.plan.Hostname)
	if s.plan.UploadAddr != "" {
		allow := s.plan.UploadAllow
		if allow == "" {
			allow = DefaultUploadAllow
		}
		b.WriteString("\n# Writes only, bearer token required, nothing that serves a build or a page.\n")
		b.WriteString("# It is plain HTTP, so the token crosses the wire in cleartext: the allowlist\n")
		b.WriteString("# below is what keeps it to guests on this host rather than the whole LAN and\n")
		b.WriteString("# the whole tailnet. Set it to \"any\" only deliberately.\n")
		fmt.Fprintf(&b, "ORCHARD_UPLOAD_ADDR=%s\n", s.plan.UploadAddr)
		fmt.Fprintf(&b, "ORCHARD_UPLOAD_ALLOW=%s\n", allow)
	}
	b.WriteString("\n# ORCHARD_BASE_URL is unset, so it is derived from the tsnet name.\n")
	b.WriteString("# TS_AUTHKEY is unset: on first run orchard logs a login URL instead.\n")

	if err := os.WriteFile(s.plan.EnvFile(), []byte(b.String()), 0o600); err != nil {
		return "", err
	}
	if err := chownIfNeeded(s.plan.EnvFile(), uid, gid); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %s with a new token", s.plan.EnvFile()), nil
}

func newToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// ---------------------------------------------------------------- binary

type binaryStep struct{ plan Plan }

func (binaryStep) Name() string { return "binary" }

// Check compares the installed binary with the one being run, so re-running a newer
// release's installer upgrades in place and re-running the same one is a no-op.
func (s binaryStep) Check(context.Context) State {
	target := s.plan.Binary()
	self, err := os.Executable()
	if err != nil {
		return Unverified("cannot determine the running binary's path")
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}

	info, err := os.Stat(target)
	if err != nil {
		return Fixable("not installed at %s", target)
	}
	if self == target {
		return OK("%s", target)
	}
	same, err := sameContents(self, target)
	if err != nil {
		return Unverified("cannot compare %s with %s", self, target)
	}
	if !same {
		return Fixable("%s differs from the binary you are running", target)
	}
	_ = info
	return OK("%s", target)
}

func (s binaryStep) Fix(context.Context) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	uid, gid, err := lookupIDs(s.plan.User)
	if err != nil {
		return "", err
	}
	target := s.plan.Binary()
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}

	// Write beside the target and rename, so a replaced binary is never half-copied —
	// and so replacing the running executable does not fail with ETXTBSY.
	tmp := target + ".new"
	if err := copyFile(self, tmp, 0o755); err != nil {
		return "", err
	}
	if err := chownIfNeeded(tmp, uid, gid); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return fmt.Sprintf("installed %s", target), nil
}

func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Chmod(mode); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func sameContents(a, b string) (bool, error) {
	ai, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	if ai.Size() != bi.Size() {
		return false, nil
	}
	x, err := os.ReadFile(a)
	if err != nil {
		return false, err
	}
	y, err := os.ReadFile(b)
	if err != nil {
		return false, err
	}
	return string(x) == string(y), nil
}

// ---------------------------------------------------------------- tailnet

type tailnetStep struct{ plan Plan }

func (tailnetStep) Name() string { return "tailnet" }

// Check looks for tsnet's own state rather than asking the network, so it works on a
// node that is merely stopped. Joining cannot be automated without a credential, so an
// unjoined node is reported as the human's job.
func (s tailnetStep) Check(context.Context) State {
	dir := filepath.Join(s.plan.StateDir(), "tsnet")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Manual("not joined yet", joinInstructions(s.plan))
	}
	if len(entries) == 0 {
		return Manual("no node key yet", joinInstructions(s.plan))
	}
	return OK("node key present in %s", dir)
}

func (tailnetStep) Fix(context.Context) (string, error) { return "", fmt.Errorf("unreachable") }

func joinInstructions(p Plan) string {
	return fmt.Sprintf(
		"Start the service and open the login URL it logs:\n\n"+
			"  tail -f %s\n\n"+
			"Or set TS_AUTHKEY in %s for an unattended join.",
		p.LogFile(), p.EnvFile())
}

func createUserInstructions(name string) string {
	if runtime.GOOS == "darwin" {
		return fmt.Sprintf(
			"Either pass --user with an existing account, or create a dedicated one:\n\n"+
				"  sudo sysadminctl -addUser %s -fullName \"Orchard\" -home /var/empty -shell /usr/bin/false\n\n"+
				"An ordinary login account works too; it only needs to own the install directory.",
			name)
	}
	return fmt.Sprintf(
		"Either pass --user with an existing account, or create a system one:\n\n"+
			"  sudo useradd --system --home-dir /nonexistent --shell /usr/sbin/nologin %s",
		name)
}
