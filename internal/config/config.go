// Package config reads and validates Orchard's environment configuration (DESIGN §12).
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// minTokenLen refuses a token short enough to be worth guessing (DESIGN §15).
const minTokenLen = 16

const (
	defaultHostname    = "orchard"
	defaultMaxUploadMB = 512
)

// TSNetSubdir is where tsnet keeps its node key and provisioned certificates.
const TSNetSubdir = "tsnet"

type Config struct {
	StateDir   string
	Token      string
	Hostname   string
	BaseURL    string // empty means "derive from the tsnet name once it is up"
	UploadAddr string // empty disables the CI upload listener (DESIGN §4.3)

	MaxUploadBytes int64
	MaxBuildAge    time.Duration // zero disables the age fallback (DESIGN §11)
}

// Load reads the environment. It does not touch the filesystem; call Prepare for that.
func Load() (*Config, error) {
	c := &Config{
		StateDir:   os.Getenv("ORCHARD_STATE_DIR"),
		Token:      os.Getenv("ORCHARD_TOKEN"),
		Hostname:   envOr("ORCHARD_HOSTNAME", defaultHostname),
		BaseURL:    strings.TrimSuffix(os.Getenv("ORCHARD_BASE_URL"), "/"),
		UploadAddr: os.Getenv("ORCHARD_UPLOAD_ADDR"),
	}

	if c.StateDir == "" {
		return nil, errors.New("ORCHARD_STATE_DIR is required")
	}
	abs, err := filepath.Abs(c.StateDir)
	if err != nil {
		return nil, fmt.Errorf("ORCHARD_STATE_DIR: %w", err)
	}
	c.StateDir = abs

	if c.Token == "" {
		return nil, errors.New("ORCHARD_TOKEN is required")
	}
	if len(c.Token) < minTokenLen {
		return nil, fmt.Errorf("ORCHARD_TOKEN must be at least %d characters", minTokenLen)
	}

	if c.BaseURL != "" && !strings.HasPrefix(c.BaseURL, "https://") && !strings.HasPrefix(c.BaseURL, "http://") {
		return nil, errors.New("ORCHARD_BASE_URL must include a scheme")
	}

	mb, err := intOr("ORCHARD_MAX_UPLOAD_MB", defaultMaxUploadMB)
	if err != nil {
		return nil, err
	}
	if mb <= 0 {
		return nil, errors.New("ORCHARD_MAX_UPLOAD_MB must be positive")
	}
	c.MaxUploadBytes = int64(mb) << 20

	days, err := intOr("ORCHARD_MAX_BUILD_AGE_DAYS", 0)
	if err != nil {
		return nil, err
	}
	if days < 0 {
		return nil, errors.New("ORCHARD_MAX_BUILD_AGE_DAYS must not be negative")
	}
	c.MaxBuildAge = time.Duration(days) * 24 * time.Hour

	return c, nil
}

// Prepare creates the state directory if it is absent and proves it is writable now,
// rather than failing later on the first publish (DESIGN §4.4).
func Prepare(stateDir string) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(stateDir, TSNetSubdir), 0o700); err != nil {
		return fmt.Errorf("create tsnet dir: %w", err)
	}
	probe, err := os.CreateTemp(stateDir, ".writable-*")
	if err != nil {
		return fmt.Errorf("state dir %s is not writable: %w", stateDir, err)
	}
	name := probe.Name()
	probe.Close()
	return os.Remove(name)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func intOr(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}
