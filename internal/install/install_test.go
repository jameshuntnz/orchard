package install

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func testPlan(t *testing.T) Plan {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return Plan{
		Prefix:     t.TempDir(),
		User:       u.Username,
		Hostname:   "orchard",
		UploadAddr: "0.0.0.0:8477",
	}
}

// ---------------------------------------------------------------- the runner

type fakeStep struct {
	name     string
	state    State
	fixErr   error
	fixCalls *int
}

func (f fakeStep) Name() string                { return f.name }
func (f fakeStep) Check(context.Context) State { return f.state }
func (f fakeStep) Fix(context.Context) (string, error) {
	if f.fixCalls != nil {
		*f.fixCalls++
	}
	if f.fixErr != nil {
		return "", f.fixErr
	}
	return "fixed", nil
}

// doctor must not touch anything: the whole point of one check path is that reading is
// safe.
func TestRunWithoutApplyNeverFixes(t *testing.T) {
	calls := 0
	steps := []Step{fakeStep{name: "a", state: Fixable("missing"), fixCalls: &calls}}

	var out bytes.Buffer
	problems := Run(context.Background(), steps, false, &out)

	if calls != 0 {
		t.Errorf("Fix called %d times with apply=false", calls)
	}
	if problems != 1 {
		t.Errorf("problems = %d, want 1", problems)
	}
}

func TestRunApplyFixes(t *testing.T) {
	calls := 0
	steps := []Step{fakeStep{name: "a", state: Fixable("missing"), fixCalls: &calls}}

	var out bytes.Buffer
	if problems := Run(context.Background(), steps, true, &out); problems != 0 {
		t.Errorf("problems = %d, want 0", problems)
	}
	if calls != 1 {
		t.Errorf("Fix called %d times, want 1", calls)
	}
}

func TestRunApplyCountsAFailedFix(t *testing.T) {
	steps := []Step{fakeStep{name: "a", state: Fixable("missing"), fixErr: errors.New("nope")}}
	var out bytes.Buffer
	if problems := Run(context.Background(), steps, true, &out); problems != 1 {
		t.Errorf("problems = %d, want 1", problems)
	}
	if !strings.Contains(out.String(), "nope") {
		t.Errorf("the failure reason was not reported: %s", out.String())
	}
}

// Reporting green on an unknown is worse than reporting nothing — but an unknown is not
// a problem to fix either.
func TestRunUnverifiedIsNotAProblem(t *testing.T) {
	steps := []Step{fakeStep{name: "a", state: Unverified("needs root to read")}}
	var out bytes.Buffer
	if problems := Run(context.Background(), steps, true, &out); problems != 0 {
		t.Errorf("problems = %d, want 0", problems)
	}
	if !strings.Contains(out.String(), "needs root to read") {
		t.Error("the reason was not reported")
	}
}

func TestRunPrintsManualInstructions(t *testing.T) {
	steps := []Step{fakeStep{name: "tailnet", state: Manual("not joined", "open the login URL")}}
	var out bytes.Buffer
	if problems := Run(context.Background(), steps, true, &out); problems != 1 {
		t.Errorf("problems = %d, want 1", problems)
	}
	if !strings.Contains(out.String(), "open the login URL") {
		t.Errorf("instructions missing: %s", out.String())
	}
}

// ---------------------------------------------------------------- supervisors

func TestLaunchdPlistIsWellFormed(t *testing.T) {
	p := testPlan(t)
	p.User = `Ann & "Bob" <admin>` // names are not sanitised anywhere upstream
	body, err := launchdStep{plan: p}.plist()
	if err != nil {
		t.Fatal(err)
	}

	dec := xml.NewDecoder(strings.NewReader(body))
	for {
		_, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("plist is not well-formed XML: %v\n%s", err, body)
		}
	}

	for _, want := range []string{
		"<string>net.orchard</string>",
		p.EnvFile(),
		p.Binary(),
		"<key>KeepAlive</key>",
		"Ann &amp; &#34;Bob&#34; &lt;admin&gt;",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("plist missing %q", want)
		}
	}
}

// The plist embeds a shell command, so a prefix that could break out of it is refused
// rather than quietly rendered.
func TestLaunchdPlistRejectsUnsafePrefix(t *testing.T) {
	p := testPlan(t)
	p.Prefix = "/tmp/orchard'; rm -rf /; '"
	if _, err := (launchdStep{plan: p}).plist(); err == nil {
		t.Error("plist accepted a prefix with shell metacharacters")
	}
}

func TestSystemdUnit(t *testing.T) {
	p := testPlan(t)
	unit := systemdStep{plan: p}.unit()

	for _, want := range []string{
		"User=" + p.User,
		// systemd reads an environment file natively, so no shell is in the way.
		"EnvironmentFile=" + p.EnvFile(),
		"ExecStart=" + p.Binary() + " serve",
		"Restart=always",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit missing %q\n%s", want, unit)
		}
	}
}

// ---------------------------------------------------------------- steps

func TestDirsStepCreatesAndIsIdempotent(t *testing.T) {
	p := testPlan(t)
	step := dirsStep{plan: p}

	if state := step.Check(context.Background()); state.Kind != KindFixable {
		// t.TempDir already exists, so only bin and state are missing.
		if state.Kind != KindFixable {
			t.Fatalf("Check = %v (%s), want fixable", state.Kind, state.Summary)
		}
	}
	if _, err := step.Fix(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state := step.Check(context.Background()); state.Kind != KindOK {
		t.Fatalf("after Fix, Check = %v (%s)", state.Kind, state.Summary)
	}
	for _, d := range []string{p.BinDir(), p.StateDir()} {
		if info, err := os.Stat(d); err != nil || !info.IsDir() {
			t.Errorf("%s was not created", d)
		}
	}
}

func TestConfigStepWritesLockedDownFile(t *testing.T) {
	p := testPlan(t)
	if _, err := (dirsStep{plan: p}).Fix(context.Background()); err != nil {
		t.Fatal(err)
	}
	step := configStep{plan: p}
	if _, err := step.Fix(context.Background()); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(p.EnvFile())
	if err != nil {
		t.Fatal(err)
	}
	// It holds the write token.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}

	raw, err := os.ReadFile(p.EnvFile())
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.Contains(body, "ORCHARD_UPLOAD_ADDR=0.0.0.0:8477") {
		t.Error("upload address not written")
	}
	if !strings.Contains(body, "ORCHARD_STATE_DIR="+p.StateDir()) {
		t.Error("state dir not written")
	}
	// A token short enough to guess is refused at startup, so generate a real one.
	for _, line := range strings.Split(body, "\n") {
		if after, ok := strings.CutPrefix(line, "ORCHARD_TOKEN="); ok && len(after) < 32 {
			t.Errorf("generated token is only %d characters", len(after))
		}
	}

	if state := step.Check(context.Background()); state.Kind != KindOK {
		t.Errorf("Check after Fix = %v (%s)", state.Kind, state.Summary)
	}
}

// Re-running the installer must not invalidate the token CI is already using.
func TestConfigStepNeverOverwrites(t *testing.T) {
	p := testPlan(t)
	if _, err := (dirsStep{plan: p}).Fix(context.Background()); err != nil {
		t.Fatal(err)
	}
	step := configStep{plan: p}
	if _, err := step.Fix(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(p.EnvFile())

	if _, err := step.Fix(context.Background()); err == nil {
		t.Error("Fix overwrote an existing env file")
	}
	after, _ := os.ReadFile(p.EnvFile())
	if string(before) != string(after) {
		t.Error("the env file changed")
	}
}

func TestConfigStepRejectsLoosePermissions(t *testing.T) {
	p := testPlan(t)
	if err := os.WriteFile(p.EnvFile(), []byte("ORCHARD_TOKEN=x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	state := configStep{plan: p}.Check(context.Background())
	if state.Kind != KindFailed {
		t.Errorf("Check = %v (%s), want failed on a world-readable token file", state.Kind, state.Summary)
	}
}

func TestBinaryStepInstallsAndDetectsDrift(t *testing.T) {
	p := testPlan(t)
	if _, err := (dirsStep{plan: p}).Fix(context.Background()); err != nil {
		t.Fatal(err)
	}
	step := binaryStep{plan: p}

	if state := step.Check(context.Background()); state.Kind != KindFixable {
		t.Fatalf("Check = %v (%s), want fixable", state.Kind, state.Summary)
	}
	if _, err := step.Fix(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state := step.Check(context.Background()); state.Kind != KindOK {
		t.Fatalf("after Fix, Check = %v (%s)", state.Kind, state.Summary)
	}

	// A different binary at the target is an upgrade, not a no-op.
	if err := os.WriteFile(p.Binary(), []byte("an older release"), 0o755); err != nil {
		t.Fatal(err)
	}
	if state := step.Check(context.Background()); state.Kind != KindFixable {
		t.Errorf("Check = %v (%s), want fixable when the installed binary differs", state.Kind, state.Summary)
	}
}

func TestPlanPaths(t *testing.T) {
	p := Plan{Prefix: "/usr/local/orchard"}
	want := map[string]string{
		p.BinDir():   "/usr/local/orchard/bin",
		p.Binary():   "/usr/local/orchard/bin/orchard",
		p.StateDir(): "/usr/local/orchard/state",
		p.EnvFile():  "/usr/local/orchard/orchard.env",
		p.LogFile():  "/usr/local/orchard/orchard.log",
	}
	for got, expected := range want {
		if got != expected {
			t.Errorf("path = %q, want %q", got, expected)
		}
	}
	if filepath.Dir(p.Binary()) != p.BinDir() {
		t.Error("binary is not in the bin directory")
	}
}

func TestStepsCoverThePlan(t *testing.T) {
	steps := testPlan(t).Steps()
	names := make([]string, len(steps))
	for i, s := range steps {
		names[i] = s.Name()
	}
	for _, want := range []string{"platform", "service user", "directories", "configuration", "binary", "tailnet"} {
		if !slicesContains(names, want) {
			t.Errorf("no %q step; got %v", want, names)
		}
	}
}

func slicesContains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
