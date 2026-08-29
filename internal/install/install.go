// Package install bootstraps a host into an Orchard node, and reports on one that
// already is.
//
// Every step is split into check-then-fix. That split is what makes `install` safe to
// re-run after a partial failure or an OS update — it fixes only what is missing — and it
// is why `doctor` can run exactly the same checks while changing nothing. A second code
// path that merely claimed to inspect the same things would drift from the one that acts.
package install

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"
)

// Kind is the outcome of checking one step.
type Kind int

const (
	// KindOK means the step is already in the desired state.
	KindOK Kind = iota
	// KindFixable means it is missing and this step knows how to fix it.
	KindFixable
	// KindManual means only a human can finish it.
	KindManual
	// KindFailed means it is present but broken.
	KindFailed
	// KindUnverified means the check could not run, with the reason.
	//
	// This is its own case because the alternative is lying. A check that needs root
	// and does not have it would otherwise report the desired state whether it held,
	// did not hold, or could not be read — and would keep reporting it throughout an
	// outage. Reporting green on an unknown is worse than reporting nothing, so this
	// does not count as a problem to fix either.
	KindUnverified
)

func (k Kind) label() string {
	switch k {
	case KindOK:
		return "ok"
	case KindFixable:
		return "fix"
	case KindManual:
		return "you"
	case KindFailed:
		return "BAD"
	default:
		return "  ?"
	}
}

// State is what a check found.
type State struct {
	Kind         Kind
	Summary      string
	Instructions string // set when Kind is KindManual
}

func OK(format string, a ...any) State {
	return State{Kind: KindOK, Summary: fmt.Sprintf(format, a...)}
}

func Fixable(format string, a ...any) State {
	return State{Kind: KindFixable, Summary: fmt.Sprintf(format, a...)}
}

func Failed(format string, a ...any) State {
	return State{Kind: KindFailed, Summary: fmt.Sprintf(format, a...)}
}

func Unverified(format string, a ...any) State {
	return State{Kind: KindUnverified, Summary: fmt.Sprintf(format, a...)}
}

func Manual(summary, instructions string) State {
	return State{Kind: KindManual, Summary: summary, Instructions: instructions}
}

// Step is one idempotent unit of an install.
type Step interface {
	Name() string
	Check(ctx context.Context) State
	// Fix is called only when Check returned KindFixable.
	Fix(ctx context.Context) (string, error)
}

// Run checks every step in order, applying fixes when apply is set. It returns the number
// of steps still needing attention, so `doctor` can exit non-zero on a broken node.
func Run(ctx context.Context, steps []Step, apply bool, w io.Writer) int {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	problems := 0
	var manual []Step
	var notes []string

	for _, s := range steps {
		state := s.Check(ctx)

		if state.Kind == KindFixable && apply {
			msg, err := s.Fix(ctx)
			if err != nil {
				fmt.Fprintf(tw, "  %s\t%s\t%s\n", KindFailed.label(), s.Name(), err)
				problems++
				continue
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", KindOK.label(), s.Name(), msg)
			continue
		}

		fmt.Fprintf(tw, "  %s\t%s\t%s\n", state.Kind.label(), s.Name(), state.Summary)
		switch state.Kind {
		case KindFixable, KindFailed:
			problems++
		case KindManual:
			problems++
			manual = append(manual, s)
			notes = append(notes, state.Instructions)
		}
	}
	tw.Flush()

	for i, s := range manual {
		if strings.TrimSpace(notes[i]) == "" {
			continue
		}
		fmt.Fprintf(w, "\n%s — this one is yours:\n%s\n", s.Name(), indent(notes[i]))
	}
	return problems
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}

// Plan is what the installer is being asked to produce.
type Plan struct {
	// Prefix is the install root. The service user owns it, which is what will let a
	// future self-update replace the binary without root.
	Prefix string
	// User is the account the service runs as.
	User string
	// Hostname is the tsnet node name.
	Hostname string
	// UploadAddr binds the CI upload listener; empty leaves it disabled.
	UploadAddr string
}

const (
	DefaultPrefix   = "/usr/local/orchard"
	DefaultHostname = "orchard"
)

func (p Plan) BinDir() string   { return filepath.Join(p.Prefix, "bin") }
func (p Plan) Binary() string   { return filepath.Join(p.BinDir(), "orchard") }
func (p Plan) StateDir() string { return filepath.Join(p.Prefix, "state") }
func (p Plan) EnvFile() string  { return filepath.Join(p.Prefix, "orchard.env") }
func (p Plan) LogFile() string  { return filepath.Join(p.Prefix, "orchard.log") }

// Steps returns the plan's steps in dependency order.
func (p Plan) Steps() []Step {
	return []Step{
		platformStep{},
		userStep{plan: p},
		dirsStep{plan: p},
		configStep{plan: p},
		binaryStep{plan: p},
		serviceStep(p),
		tailnetStep{plan: p},
	}
}

// Supported reports whether this platform has a supervisor integration.
func Supported() bool { return runtime.GOOS == "darwin" || runtime.GOOS == "linux" }

func isRoot() bool { return os.Geteuid() == 0 }

func requireRoot(what string) error {
	if isRoot() {
		return nil
	}
	return fmt.Errorf("%s needs root — re-run with sudo", what)
}

// requirePrivilege is the softer form, for steps that only need root because they hand
// ownership to another account. Installing into a prefix you already own is an ordinary
// file operation, and demanding sudo for it would be theatre — so this permits it, and
// lets the filesystem refuse a genuinely privileged path on its own terms.
func requirePrivilege(targetUser, what string) error {
	if isRoot() {
		return nil
	}
	if cur, err := user.Current(); err == nil && cur.Username == targetUser {
		return nil
	}
	return fmt.Errorf("%s as %s needs root — re-run with sudo", what, targetUser)
}

// chownIfNeeded skips the chown when the file already belongs to the right account,
// which is what lets an unprivileged install into your own prefix succeed.
func chownIfNeeded(path string, uid, gid int) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if owner, ok := ownerUID(info); ok && owner == uid {
		return nil
	}
	return os.Chown(path, uid, gid)
}

// run executes a command, returning its combined output for error messages.
func run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
