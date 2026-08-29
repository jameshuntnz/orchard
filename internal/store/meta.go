package store

import (
	"encoding/json"
	"fmt"
	"time"
)

// CurrentSchema is the schemaVersion written by this binary. A newer binary must read
// every older version it has ever shipped with, and migration happens lazily on read,
// in memory — nothing rewrites the state directory (DESIGN §14.2).
const CurrentSchema = 1

// Meta is the contents of meta.json: the metadata from the upload plus what the service
// derives at publish time.
type Meta struct {
	SchemaVersion int `json:"schemaVersion"`

	Branch      string `json:"branch"`
	Commit      string `json:"commit"`
	Version     string `json:"version"`
	BuildNumber string `json:"buildNumber"`
	BundleID    string `json:"bundleId"`
	Title       string `json:"title"`
	Notes       string `json:"notes,omitempty"`
	RunURL      string `json:"runUrl,omitempty"`

	PublishedAt time.Time `json:"publishedAt"`
	IPASize     int64     `json:"ipaSize"`
}

// Build is one branch build: its metadata plus the identifiers that locate it.
type Build struct {
	Meta
	App  string `json:"-"`
	Slug string `json:"slug"`
}

// ShortCommit is the abbreviated SHA the web UI shows.
func (b Build) ShortCommit() string {
	if len(b.Commit) > 7 {
		return b.Commit[:7]
	}
	return b.Commit
}

// decodeMeta parses meta.json, migrating older schema versions in memory.
//
// A version of 0 predates the field and is read as 1; anything newer than this binary
// understands is an error, so the build is skipped and logged rather than guessed at.
func decodeMeta(raw []byte) (Meta, error) {
	var probe struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return Meta{}, fmt.Errorf("meta.json: %w", err)
	}
	if probe.SchemaVersion > CurrentSchema {
		return Meta{}, fmt.Errorf("meta.json: schemaVersion %d is newer than this binary understands (%d)", probe.SchemaVersion, CurrentSchema)
	}

	var m Meta
	if err := json.Unmarshal(raw, &m); err != nil {
		return Meta{}, fmt.Errorf("meta.json: %w", err)
	}

	switch probe.SchemaVersion {
	case 0, 1:
		m.SchemaVersion = 1
	}
	return m, nil
}
