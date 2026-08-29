package ipa

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"howett.net/plist"

	"github.com/jameshuntnz/orchard/internal/ipa/ipatest"
)

func write(t *testing.T, body []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.ipa")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInspectBothPlistFormats(t *testing.T) {
	info := ipatest.Info{
		BundleID:    "com.example.app",
		Version:     "0.0.57",
		BuildNumber: "57",
		DisplayName: "Example",
	}
	for name, format := range map[string]int{"binary": plist.BinaryFormat, "xml": plist.XMLFormat} {
		t.Run(name, func(t *testing.T) {
			got, err := Inspect(write(t, ipatest.Build(t, info, format)))
			if err != nil {
				t.Fatal(err)
			}
			want := Info{BundleID: "com.example.app", Version: "0.0.57", BuildNumber: "57", Name: "Example"}
			if got != want {
				t.Errorf("Inspect = %+v, want %+v", got, want)
			}
		})
	}
}

func TestInspectFallsBackToCFBundleName(t *testing.T) {
	raw, err := plist.Marshal(map[string]string{
		"CFBundleIdentifier": "com.example.app",
		"CFBundleName":       "Fallback",
	}, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Inspect(write(t, ipatest.Archive(t, map[string][]byte{"Payload/Example.app/Info.plist": raw})))
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Fallback" {
		t.Errorf("Name = %q, want the CFBundleName fallback", got.Name)
	}
}

// Only the app bundle's own Info.plist counts. A framework or extension nested inside it
// has one too, with a different identifier.
func TestInspectIgnoresNestedPlists(t *testing.T) {
	nested, _ := plist.Marshal(map[string]string{"CFBundleIdentifier": "com.example.app.framework"}, plist.XMLFormat)
	app, _ := plist.Marshal(map[string]string{"CFBundleIdentifier": "com.example.app"}, plist.XMLFormat)

	got, err := Inspect(write(t, ipatest.Archive(t, map[string][]byte{
		"Payload/Example.app/Frameworks/Thing.framework/Info.plist": nested,
		"Payload/Example.app/Info.plist":                            app,
	})))
	if err != nil {
		t.Fatal(err)
	}
	if got.BundleID != "com.example.app" {
		t.Errorf("BundleID = %q, want the app bundle's own", got.BundleID)
	}
}

func TestInspectRejections(t *testing.T) {
	tests := []struct {
		name    string
		body    []byte
		wantErr error
	}{
		{
			name:    "no Info.plist at all",
			body:    ipatest.Archive(t, map[string][]byte{"Payload/Example.app/Other": []byte("x")}),
			wantErr: ErrNoInfoPlist,
		},
		{
			name:    "plist outside Payload",
			body:    ipatest.Archive(t, map[string][]byte{"Info.plist": []byte("x")}),
			wantErr: ErrNoInfoPlist,
		},
		{
			name:    "plist not under a .app bundle",
			body:    ipatest.Archive(t, map[string][]byte{"Payload/Example/Info.plist": []byte("x")}),
			wantErr: ErrNoInfoPlist,
		},
		{
			name:    "oversized entry",
			body:    ipatest.Archive(t, map[string][]byte{"Payload/Example.app/Info.plist": make([]byte, maxPlistBytes+1)}),
			wantErr: ErrPlistTooLarge,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Inspect(write(t, tt.body)); !errors.Is(err, tt.wantErr) {
				t.Errorf("Inspect error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestInspectRejectsNonArchive(t *testing.T) {
	if _, err := Inspect(write(t, []byte("this is not a zip"))); err == nil {
		t.Error("Inspect accepted a non-archive")
	}
}

func TestInspectRequiresBundleIdentifier(t *testing.T) {
	raw, _ := plist.Marshal(map[string]string{"CFBundleName": "Example"}, plist.XMLFormat)
	_, err := Inspect(write(t, ipatest.Archive(t, map[string][]byte{"Payload/Example.app/Info.plist": raw})))
	if err == nil || !strings.Contains(err.Error(), "CFBundleIdentifier") {
		t.Errorf("error = %v, want one naming CFBundleIdentifier", err)
	}
}
