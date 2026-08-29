package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// MarkerName records an update that has been installed but not yet proven to work.
//
// It is what makes rollback automatic: the replacement binary finds it on startup, runs
// a self-check, and puts the previous binary back if that fails. A crash loop in a bad
// release therefore corrects itself rather than waiting for someone to notice.
const MarkerName = ".orchard-update"

// PreviousSuffix is appended to the outgoing binary. Keeping it beside the new one is
// what lets a rollback happen without the network, a package manager, or root.
const PreviousSuffix = ".prev"

type Marker struct {
	// PreviousVersion is what to expect back if the self-check fails.
	PreviousVersion string    `json:"previousVersion"`
	NewVersion      string    `json:"newVersion"`
	InstalledAt     time.Time `json:"installedAt"`
}

func MarkerPath(binary string) string   { return filepath.Join(filepath.Dir(binary), MarkerName) }
func PreviousPath(binary string) string { return binary + PreviousSuffix }

// Install replaces the running binary, leaving the previous one alongside and a marker
// recording what changed. The caller then drains and exits; the supervisor starts the
// new binary (DESIGN §14.1).
//
// Nothing here needs privilege beyond writing to the install directory the service
// already owns, which is the reason there is no sudo, no root daemon and no launchctl
// permission to arrange.
func (u *Updater) Install(binaryBody []byte, newVersion Version) error {
	target := u.binary
	dir := filepath.Dir(target)

	staged := filepath.Join(dir, binaryName+".new")
	if err := os.WriteFile(staged, binaryBody, 0o755); err != nil {
		return fmt.Errorf("staging the new binary: %w", err)
	}
	// WriteFile respects umask, and a binary the service cannot execute would be a
	// rollback loop rather than an update.
	if err := os.Chmod(staged, 0o755); err != nil {
		os.Remove(staged)
		return err
	}

	marker := Marker{
		PreviousVersion: u.current.String(),
		NewVersion:      newVersion.String(),
		InstalledAt:     time.Now().UTC(),
	}
	raw, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		os.Remove(staged)
		return err
	}
	// The marker is written before anything moves. If the process dies mid-update, a
	// marker with no replacement is harmless — the self-check passes and clears it —
	// whereas a replacement with no marker would never be checked at all.
	if err := os.WriteFile(MarkerPath(target), append(raw, '\n'), 0o644); err != nil {
		os.Remove(staged)
		return err
	}

	previous := PreviousPath(target)
	os.Remove(previous)
	if err := os.Rename(target, previous); err != nil {
		os.Remove(staged)
		os.Remove(MarkerPath(target))
		return fmt.Errorf("moving the running binary aside: %w", err)
	}
	if err := os.Rename(staged, target); err != nil {
		// Put it back rather than leaving no binary at all for the supervisor.
		_ = os.Rename(previous, target)
		os.Remove(MarkerPath(target))
		return fmt.Errorf("installing the new binary: %w", err)
	}
	return nil
}

// PendingMarker reports an update awaiting its self-check.
func PendingMarker(binary string) (Marker, bool) {
	raw, err := os.ReadFile(MarkerPath(binary))
	if err != nil {
		return Marker{}, false
	}
	var m Marker
	if err := json.Unmarshal(raw, &m); err != nil {
		// An unreadable marker still means an update happened, and the safe reading
		// of "something changed and cannot be described" is to check it.
		return Marker{}, true
	}
	return m, true
}

func ClearMarker(binary string) error {
	err := os.Remove(MarkerPath(binary))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Rollback restores the previous binary. It leaves the failed one as orchard.failed
// rather than deleting it, because a binary that could not start is the only evidence of
// why.
func Rollback(binary string) error {
	previous := PreviousPath(binary)
	if _, err := os.Stat(previous); err != nil {
		return fmt.Errorf("no %s to roll back to: %w", previous, err)
	}
	failed := binary + ".failed"
	os.Remove(failed)
	if err := os.Rename(binary, failed); err != nil {
		return err
	}
	if err := os.Rename(previous, binary); err != nil {
		_ = os.Rename(failed, binary)
		return err
	}
	return ClearMarker(binary)
}
