package manifest

import (
	"strings"
	"testing"

	"howett.net/plist"
)

func TestRenderShape(t *testing.T) {
	out, err := Render("com.example.app", "0.0.57", "Example", "https://orchard.example.ts.net/a/example/b/main/app.ipa")
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Items []struct {
			Assets []struct {
				Kind string `plist:"kind"`
				URL  string `plist:"url"`
			} `plist:"assets"`
			Metadata struct {
				BundleIdentifier string `plist:"bundle-identifier"`
				BundleVersion    string `plist:"bundle-version"`
				Kind             string `plist:"kind"`
				Title            string `plist:"title"`
			} `plist:"metadata"`
		} `plist:"items"`
	}
	if _, err := plist.Unmarshal(out, &doc); err != nil {
		t.Fatalf("rendered manifest does not parse: %v\n%s", err, out)
	}
	if len(doc.Items) != 1 || len(doc.Items[0].Assets) != 1 {
		t.Fatalf("unexpected structure: %+v", doc)
	}

	a, m := doc.Items[0].Assets[0], doc.Items[0].Metadata
	if a.Kind != "software-package" {
		t.Errorf("asset kind = %q", a.Kind)
	}
	// All URLs must be absolute, which is why the service needs its own base URL (§8).
	if !strings.HasPrefix(a.URL, "https://") {
		t.Errorf("asset url = %q, want absolute", a.URL)
	}
	if m.BundleIdentifier != "com.example.app" || m.BundleVersion != "0.0.57" ||
		m.Kind != "software" || m.Title != "Example" {
		t.Errorf("metadata = %+v", m)
	}
}

// Titles arrive over the network. Marshalling from a struct rather than templating means
// markup characters cannot break the document.
func TestRenderEscapesTitle(t *testing.T) {
	title := `Ex & <ample> "quoted" </plist>`
	out, err := Render("com.example.app", "1.0", title, "https://example.invalid/app.ipa")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Items []struct {
			Metadata struct {
				Title string `plist:"title"`
			} `plist:"metadata"`
		} `plist:"items"`
	}
	if _, err := plist.Unmarshal(out, &doc); err != nil {
		t.Fatalf("manifest broken by title: %v\n%s", err, out)
	}
	if got := doc.Items[0].Metadata.Title; got != title {
		t.Errorf("title round-tripped as %q, want %q", got, title)
	}
}

func TestInstallURL(t *testing.T) {
	got := InstallURL("https://orchard.example.ts.net/a/example/b/feature-x/manifest.plist")
	want := "itms-services://?action=download-manifest&url=" +
		"https%3A%2F%2Forchard.example.ts.net%2Fa%2Fexample%2Fb%2Ffeature-x%2Fmanifest.plist"
	if got != want {
		t.Errorf("InstallURL =\n%s\nwant\n%s", got, want)
	}
}
