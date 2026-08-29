// Package ipa reads an uploaded .ipa well enough to learn what it claims to be.
//
// The archive comes from an authenticated but not necessarily careful client, so the
// entry count and the decompressed size are both bounded and nothing is ever extracted
// to disk (DESIGN §15).
package ipa

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"regexp"

	"howett.net/plist"
)

var (
	ErrNoInfoPlist    = errors.New("no Payload/*.app/Info.plist in archive")
	ErrTooManyEntries = errors.New("archive has too many entries")
	ErrPlistTooLarge  = errors.New("Info.plist is implausibly large")
)

const (
	maxEntries    = 50_000
	maxPlistBytes = 10 << 20
)

// infoPlistRe matches the app bundle's own Info.plist and nothing nested deeper —
// a framework or extension inside the bundle has its own, with a different identifier.
var infoPlistRe = regexp.MustCompile(`^Payload/[^/]+\.app/Info\.plist$`)

// Info is what Orchard needs from a build's Info.plist.
type Info struct {
	BundleID    string // CFBundleIdentifier
	Version     string // CFBundleShortVersionString
	BuildNumber string // CFBundleVersion
	Name        string // CFBundleDisplayName, falling back to CFBundleName
}

// Inspect opens the .ipa at path and reads the app bundle's Info.plist.
func Inspect(path string) (Info, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return Info{}, fmt.Errorf("not a readable zip archive: %w", err)
	}
	defer zr.Close()

	if len(zr.File) > maxEntries {
		return Info{}, ErrTooManyEntries
	}

	for _, f := range zr.File {
		if !infoPlistRe.MatchString(f.Name) {
			continue
		}
		raw, err := readBounded(f)
		if err != nil {
			return Info{}, err
		}
		return parse(raw)
	}
	return Info{}, ErrNoInfoPlist
}

// readBounded reads one entry into memory under a hard cap. The declared uncompressed
// size is a claim by the archive, so the limit is enforced on what is actually read.
func readBounded(f *zip.File) ([]byte, error) {
	if f.UncompressedSize64 > maxPlistBytes {
		return nil, ErrPlistTooLarge
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	raw, err := io.ReadAll(io.LimitReader(rc, maxPlistBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxPlistBytes {
		return nil, ErrPlistTooLarge
	}
	return raw, nil
}

// parse decodes either format: Xcode writes a binary plist for anything shipped, and a
// hand-assembled archive may carry XML.
func parse(raw []byte) (Info, error) {
	var d struct {
		BundleID    string `plist:"CFBundleIdentifier"`
		Version     string `plist:"CFBundleShortVersionString"`
		BuildNumber string `plist:"CFBundleVersion"`
		DisplayName string `plist:"CFBundleDisplayName"`
		Name        string `plist:"CFBundleName"`
	}
	if _, err := plist.Unmarshal(raw, &d); err != nil {
		return Info{}, fmt.Errorf("Info.plist: %w", err)
	}
	if d.BundleID == "" {
		return Info{}, errors.New("Info.plist has no CFBundleIdentifier")
	}

	name := d.DisplayName
	if name == "" {
		name = d.Name
	}
	return Info{
		BundleID:    d.BundleID,
		Version:     d.Version,
		BuildNumber: d.BuildNumber,
		Name:        name,
	}, nil
}
