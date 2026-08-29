// Package ipatest builds minimal .ipa archives for tests.
//
// Xcode writes a binary Info.plist for anything it ships, so tests that only exercise the
// XML form would miss the case that actually occurs.
package ipatest

import (
	"archive/zip"
	"bytes"
	"testing"

	"howett.net/plist"
)

// Info is the subset of Info.plist keys Orchard reads.
type Info struct {
	BundleID    string `plist:"CFBundleIdentifier"`
	Version     string `plist:"CFBundleShortVersionString"`
	BuildNumber string `plist:"CFBundleVersion"`
	DisplayName string `plist:"CFBundleDisplayName,omitempty"`
}

// Build returns a zip archive containing Payload/Example.app/Info.plist and nothing else.
func Build(t *testing.T, info Info, format int) []byte {
	t.Helper()
	raw, err := plist.Marshal(info, format)
	if err != nil {
		t.Fatalf("marshal Info.plist: %v", err)
	}
	return Archive(t, map[string][]byte{"Payload/Example.app/Info.plist": raw})
}

// Binary returns an archive whose Info.plist is in the binary format Xcode produces.
func Binary(t *testing.T, bundleID string) []byte {
	t.Helper()
	return Build(t, Info{BundleID: bundleID, Version: "0.0.57", BuildNumber: "57", DisplayName: "Example"}, plist.BinaryFormat)
}

// Archive zips arbitrary named entries, for the malformed cases.
func Archive(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}
