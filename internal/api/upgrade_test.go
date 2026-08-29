package api

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jameshuntnz/orchard/internal/store"
	"github.com/jameshuntnz/orchard/internal/web"
)

// A state directory written by an older release must still render through the current
// binary (DESIGN §16). This is the check most likely to break silently: everything
// derived is rendered on read, so a schema change that nothing migrates shows up as a
// blank page or a 500 on an install link a tester already has.
//
// The fixtures under internal/store/testdata/schema are permanent. When a schemaVersion
// is retired, its fixture stays and this test keeps proving that version still reads.
func TestBuildsFromEveryRetiredSchemaStillRender(t *testing.T) {
	fixtures, err := os.ReadDir("../store/testdata/schema")
	if err != nil {
		t.Fatal(err)
	}
	if len(fixtures) == 0 {
		t.Fatal("no schema fixtures to upgrade from")
	}

	for _, f := range fixtures {
		t.Run(f.Name(), func(t *testing.T) {
			root := t.TempDir()

			// Lay the state out the way that version's binary would have.
			buildDir := filepath.Join(root, "apps", "example", "builds", "feature-new-checkout")
			if err := os.MkdirAll(buildDir, 0o755); err != nil {
				t.Fatal(err)
			}
			meta, err := os.ReadFile(filepath.Join("../store/testdata/schema", f.Name(), "meta.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(buildDir, "meta.json"), meta, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(buildDir, "app.ipa"), []byte("payload"), 0o644); err != nil {
				t.Fatal(err)
			}

			renderer, err := web.New()
			if err != nil {
				t.Fatal(err)
			}
			log := slog.New(slog.DiscardHandler)
			srv := &Server{
				Store:   store.New(root, log),
				Web:     renderer,
				Log:     log,
				Token:   token,
				BaseURL: baseURL,
				Version: "test",
			}
			ts := httptest.NewServer(srv.TailnetMux())
			t.Cleanup(ts.Close)

			// Nothing rewrites state on disk during an upgrade, so every page has to
			// come off the old file as it stands.
			for _, path := range []string{
				"/",
				"/a/example",
				"/a/example/b/feature-new-checkout",
				"/a/example/b/feature-new-checkout/manifest.plist",
				"/a/example/b/feature-new-checkout/app.ipa",
				"/api/v1/apps",
			} {
				resp, err := ts.Client().Get(ts.URL + path)
				if err != nil {
					t.Fatalf("%s: %v", path, err)
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Errorf("%s returned %d from a %s state directory", path, resp.StatusCode, f.Name())
				}
			}

			// And the metadata has to survive the read, not merely not crash.
			page, err := ts.Client().Get(ts.URL + "/a/example/b/feature-new-checkout")
			if err != nil {
				t.Fatal(err)
			}
			defer page.Body.Close()
			buf := make([]byte, 64<<10)
			n, _ := page.Body.Read(buf)
			body := string(buf[:n])
			for _, want := range []string{"com.example.app", "feature/new-checkout", "0.0.57"} {
				if !strings.Contains(body, want) {
					t.Errorf("install page from a %s state directory is missing %q", f.Name(), want)
				}
			}
		})
	}
}
