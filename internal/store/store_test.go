package store

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	return New(t.TempDir(), slog.New(slog.DiscardHandler))
}

func publish(t *testing.T, s *Store, app, slug, payload string, m Meta) {
	t.Helper()
	st, err := s.BeginPublish(app, slug)
	if err != nil {
		t.Fatalf("BeginPublish(%q, %q): %v", app, slug, err)
	}
	if err := os.WriteFile(st.IPAPath(), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	if m.PublishedAt.IsZero() {
		m.PublishedAt = time.Now().UTC()
	}
	if err := st.Commit(m); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	st.Abort() // must be a no-op after Commit
}

func TestValidIdent(t *testing.T) {
	ok := []string{"a", "0", "example", "feature-new-checkout", "app2", strings.Repeat("a", maxIdentLen)}
	for _, s := range ok {
		if !ValidIdent(s) {
			t.Errorf("ValidIdent(%q) = false, want true", s)
		}
	}

	// Traversal is the highest-risk surface in the service (DESIGN §15).
	bad := []string{
		"", "..", ".", "../etc", "a/b", "a\\b", "/abs", "a/../b",
		"-lead", "Upper", "MiXeD", "under_score", "dot.ted", "space bar",
		"café", "аpp", // Cyrillic а, a homoglyph
		"a\x00b", "a\nb", "app%2e%2e",
		strings.Repeat("a", maxIdentLen+1),
	}
	for _, s := range bad {
		if ValidIdent(s) {
			t.Errorf("ValidIdent(%q) = true, want false", s)
		}
	}
}

func TestPublishAndGet(t *testing.T) {
	s := newStore(t)
	publish(t, s, "example", "main", "payload", Meta{
		Branch: "main", Version: "1.0", BundleID: "com.example.app", Title: "Example", IPASize: 7,
	})

	b, err := s.Get("example", "main")
	if err != nil {
		t.Fatal(err)
	}
	if b.App != "example" || b.Slug != "main" || b.Branch != "main" {
		t.Fatalf("unexpected build: %+v", b)
	}
	if b.SchemaVersion != CurrentSchema {
		t.Errorf("SchemaVersion = %d, want %d", b.SchemaVersion, CurrentSchema)
	}
	if got, _ := os.ReadFile(s.IPAPath("example", "main")); string(got) != "payload" {
		t.Errorf("ipa = %q, want %q", got, "payload")
	}
}

func TestPublishReplacesInPlace(t *testing.T) {
	s := newStore(t)
	publish(t, s, "example", "main", "first", Meta{Branch: "main", Version: "1", BundleID: "x", Title: "E"})
	publish(t, s, "example", "main", "second", Meta{Branch: "main", Version: "2", BundleID: "x", Title: "E"})

	b, err := s.Get("example", "main")
	if err != nil {
		t.Fatal(err)
	}
	if b.Version != "2" {
		t.Errorf("Version = %q, want the replacement", b.Version)
	}
	if got, _ := os.ReadFile(s.IPAPath("example", "main")); string(got) != "second" {
		t.Errorf("ipa = %q, want the replacement", got)
	}

	// One build per branch: no history is kept (DESIGN §11).
	builds, _ := s.ListBuilds("example")
	if len(builds) != 1 {
		t.Errorf("got %d builds, want 1", len(builds))
	}
	// The displaced build and its staging directory are cleaned up.
	entries, _ := os.ReadDir(filepath.Join(s.root, tmpDir))
	if len(entries) != 0 {
		t.Errorf("tmp still holds %d entries", len(entries))
	}
}

// Two concurrent publishes to one slug must both complete, with the last to finish
// winning, and a reader must never see an ipa and a meta.json from different builds
// (DESIGN §4.4).
func TestConcurrentPublishNeverMixes(t *testing.T) {
	s := newStore(t)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v := fmt.Sprint(i)
			st, err := s.BeginPublish("example", "main")
			if err != nil {
				t.Error(err)
				return
			}
			if err := os.WriteFile(st.IPAPath(), []byte(v), 0o644); err != nil {
				t.Error(err)
				return
			}
			if err := st.Commit(Meta{Branch: "main", Version: v, BundleID: "x", Title: "E", PublishedAt: time.Now()}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	b, err := s.Get("example", "main")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(s.IPAPath("example", "main"))
	if err != nil {
		t.Fatal(err)
	}
	if b.Version != string(payload) {
		t.Fatalf("mixed build: meta says version %q but the ipa is from %q", b.Version, payload)
	}
}

func TestListAppsOrderingAndTitle(t *testing.T) {
	s := newStore(t)
	now := time.Now().UTC()
	publish(t, s, "alpha", "main", "p", Meta{Branch: "main", Title: "Alpha", BundleID: "x", Version: "1", PublishedAt: now.Add(-3 * time.Hour)})
	publish(t, s, "beta", "main", "p", Meta{Branch: "main", Title: "Beta old", BundleID: "x", Version: "1", PublishedAt: now.Add(-2 * time.Hour)})
	publish(t, s, "beta", "fix", "p", Meta{Branch: "fix", Title: "Beta new", BundleID: "x", Version: "2", PublishedAt: now.Add(-1 * time.Hour)})

	apps, err := s.ListApps()
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 2 {
		t.Fatalf("got %d apps, want 2", len(apps))
	}
	// Apps are ordered by their most recent build (DESIGN §6).
	if apps[0].ID != "beta" || apps[1].ID != "alpha" {
		t.Errorf("order = %s, %s; want beta, alpha", apps[0].ID, apps[1].ID)
	}
	// Builds are newest first within an app.
	if apps[0].Builds[0].Slug != "fix" {
		t.Errorf("beta's first build = %s, want fix", apps[0].Builds[0].Slug)
	}
	// The display title comes from the most recent build (DESIGN §5).
	if apps[0].Title != "Beta new" {
		t.Errorf("title = %q, want %q", apps[0].Title, "Beta new")
	}
}

func TestSweepIsolatesApps(t *testing.T) {
	s := newStore(t)
	for _, app := range []string{"alpha", "beta"} {
		for _, slug := range []string{"main", "stale", "keep-me"} {
			publish(t, s, app, slug, "p", Meta{Branch: slug, Title: app, BundleID: "x", Version: "1"})
		}
	}

	removed, kept, err := s.Sweep("alpha", []string{"main", "keep-me"})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "stale" {
		t.Errorf("removed = %v, want [stale]", removed)
	}
	if kept != 2 {
		t.Errorf("kept = %d, want 2", kept)
	}

	// A sweep for one app can never touch another's builds (DESIGN §5).
	other, _ := s.ListBuilds("beta")
	if len(other) != 3 {
		t.Errorf("beta has %d builds, want 3 untouched", len(other))
	}
}

func TestDeleteMissingIsNotFound(t *testing.T) {
	s := newStore(t)
	if err := s.Delete("example", "nope"); err != ErrNotFound {
		t.Errorf("Delete of a missing build = %v, want ErrNotFound", err)
	}
}

func TestSweepAge(t *testing.T) {
	s := newStore(t)
	now := time.Now().UTC()
	publish(t, s, "example", "fresh", "p", Meta{Branch: "fresh", Title: "E", BundleID: "x", Version: "1", PublishedAt: now.Add(-2 * 24 * time.Hour)})
	publish(t, s, "example", "ancient", "p", Meta{Branch: "ancient", Title: "E", BundleID: "x", Version: "1", PublishedAt: now.Add(-40 * 24 * time.Hour)})

	if n := s.SweepAge(30*24*time.Hour, now); n != 1 {
		t.Fatalf("SweepAge removed %d, want 1", n)
	}
	if _, err := s.Get("example", "ancient"); err != ErrNotFound {
		t.Errorf("ancient build survived: %v", err)
	}
	if _, err := s.Get("example", "fresh"); err != nil {
		t.Errorf("fresh build was removed: %v", err)
	}

	// Disabled by default: a zero max must never remove anything (DESIGN §11).
	if n := s.SweepAge(0, now); n != 0 {
		t.Errorf("SweepAge(0) removed %d, want 0", n)
	}
}

// One malformed directory should never take down the index (DESIGN §14.2).
func TestListBuildsSkipsUnreadable(t *testing.T) {
	s := newStore(t)
	publish(t, s, "example", "good", "p", Meta{Branch: "good", Title: "E", BundleID: "x", Version: "1"})

	bad := s.buildDir("example", "broken")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, metaName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	builds, err := s.ListBuilds("example")
	if err != nil {
		t.Fatalf("ListBuilds failed instead of skipping: %v", err)
	}
	if len(builds) != 1 || builds[0].Slug != "good" {
		t.Errorf("builds = %v, want just the readable one", builds)
	}
}

// Fixtures for every retired schema version live in the repo permanently (DESIGN §14.2).
func TestDecodeMetaFixtures(t *testing.T) {
	dirs, err := os.ReadDir("testdata/schema")
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) == 0 {
		t.Fatal("no schema fixtures")
	}
	for _, d := range dirs {
		t.Run(d.Name(), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata/schema", d.Name(), "meta.json"))
			if err != nil {
				t.Fatal(err)
			}
			m, err := decodeMeta(raw)
			if err != nil {
				t.Fatalf("decodeMeta: %v", err)
			}
			if m.BundleID == "" || m.Branch == "" || m.PublishedAt.IsZero() {
				t.Errorf("fixture decoded incompletely: %+v", m)
			}
			if m.SchemaVersion != CurrentSchema {
				t.Errorf("SchemaVersion = %d, want it migrated to %d", m.SchemaVersion, CurrentSchema)
			}
		})
	}
}

func TestDecodeMetaSchemaVersions(t *testing.T) {
	// A file written before the field existed reads as version 1.
	m, err := decodeMeta([]byte(`{"branch":"main","bundleId":"x"}`))
	if err != nil {
		t.Fatalf("version 0: %v", err)
	}
	if m.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d, want 1", m.SchemaVersion)
	}

	// Anything newer than this binary understands is an error, so the build is skipped
	// rather than guessed at — which is what makes a rollback safe.
	raw, _ := json.Marshal(map[string]any{"schemaVersion": CurrentSchema + 1})
	if _, err := decodeMeta(raw); err == nil {
		t.Error("decodeMeta accepted a newer schemaVersion")
	}
}

func TestBeginPublishRejectsBadIdents(t *testing.T) {
	s := newStore(t)
	if _, err := s.BeginPublish("../evil", "main"); err == nil {
		t.Error("BeginPublish accepted a traversal app id")
	}
	if _, err := s.BeginPublish("example", ".."); err == nil {
		t.Error("BeginPublish accepted a traversal slug")
	}
}

func TestShortCommit(t *testing.T) {
	if got := (Build{Meta: Meta{Commit: "abcdef1234567890"}}).ShortCommit(); got != "abcdef1" {
		t.Errorf("ShortCommit = %q", got)
	}
	if got := (Build{Meta: Meta{Commit: "abc"}}).ShortCommit(); got != "abc" {
		t.Errorf("ShortCommit = %q", got)
	}
}
