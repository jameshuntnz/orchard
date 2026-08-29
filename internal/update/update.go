// Package update implements the binary's self-update (DESIGN §14.1).
//
// What this trusts, stated plainly because the service downloads something and then
// executes it: the release is fetched over HTTPS from the configured repository only,
// the archive is checked against checksums published alongside it, and a release without
// checksums is refused rather than installed unverified. What that does not cover is a
// compromised repository — the checksums come from the same place as the archive, so
// this detects corruption and interrupted downloads, not substitution. Signing the
// artifacts and verifying a pinned public key is the next step and is not this.
package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	// DefaultRepo is compiled in. Overriding it changes what the service will execute,
	// so it is a trust setting rather than a convenience (DESIGN §12).
	DefaultRepo     = "jameshuntnz/orchard"
	DefaultChannel  = "stable"
	DefaultInterval = 6 * time.Hour

	// maxArchive bounds what a download may expand to. The archive is verified before
	// it is opened, but a bound costs nothing and the alternative is trusting a length
	// field.
	maxArchive = 256 << 20

	checksumFile = "SHA256SUMS"
	binaryName   = "orchard"
)

var ErrNoUpdate = errors.New("already up to date")

type Config struct {
	Enabled  bool
	Repo     string
	Channel  string
	Interval time.Duration
	// Token authenticates against the release API. A public repository needs none;
	// a private one serves its assets only to a credentialed caller.
	Token string
}

// Release is a candidate to update to.
type Release struct {
	Version Version
	Assets  map[string]string // asset name -> API URL
}

// Updater checks for, verifies and installs new versions.
type Updater struct {
	cfg     Config
	current Version
	binary  string // path of the running binary, which is what gets replaced
	client  *http.Client
	// api is the release API base, overridden in tests.
	api string
}

func New(cfg Config, currentVersion, binaryPath string) (*Updater, error) {
	v, err := ParseVersion(currentVersion)
	if err != nil {
		return nil, fmt.Errorf("running version %q is not comparable, so nothing can be judged newer: %w", currentVersion, err)
	}
	if cfg.Repo == "" {
		cfg.Repo = DefaultRepo
	}
	if cfg.Channel == "" {
		cfg.Channel = DefaultChannel
	}
	return &Updater{
		cfg:     cfg,
		current: v,
		binary:  binaryPath,
		client:  &http.Client{Timeout: 10 * time.Minute},
		api:     "https://api.github.com",
	}, nil
}

// Current is the running version.
func (u *Updater) Current() Version { return u.current }

// AssetName is the artifact this platform needs.
func AssetName(v Version) string {
	return fmt.Sprintf("orchard_%s_%s_%s.tar.gz", v, runtime.GOOS, runtime.GOARCH)
}

// Releases lists every release this channel is eligible to install.
func (u *Updater) Releases(ctx context.Context) ([]Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/repos/%s/releases?per_page=30", u.api, u.cfg.Repo), nil)
	if err != nil {
		return nil, err
	}
	u.authorize(req)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound && u.cfg.Token == "" {
		return nil, fmt.Errorf("listing releases for %s: %s (a private repository serves them only to a credentialed caller; set ORCHARD_UPDATE_TOKEN)", u.cfg.Repo, resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing releases for %s: %s", u.cfg.Repo, resp.Status)
	}

	var raw []struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&raw); err != nil {
		return nil, err
	}

	var out []Release
	for _, r := range raw {
		if r.Draft {
			continue
		}
		v, err := ParseVersion(r.TagName)
		if err != nil {
			continue // a tag that is not a version is not a release we can order
		}
		// Prereleases are ignored unless the channel says otherwise (DESIGN §12).
		if (r.Prerelease || v.IsPrerelease()) && u.cfg.Channel == DefaultChannel {
			continue
		}
		assets := make(map[string]string, len(r.Assets))
		for _, a := range r.Assets {
			assets[a.Name] = a.URL
		}
		out = append(out, Release{Version: v, Assets: assets})
	}
	return out, nil
}

// Latest returns the newest eligible release, or ErrNoUpdate when nothing is newer than
// what is running.
func (u *Updater) Latest(ctx context.Context) (Release, error) {
	releases, err := u.Releases(ctx)
	if err != nil {
		return Release{}, err
	}
	var best Release
	found := false
	for _, r := range releases {
		if !found || r.Version.Compare(best.Version) > 0 {
			best, found = r, true
		}
	}
	if !found || best.Version.Compare(u.current) <= 0 {
		return Release{}, ErrNoUpdate
	}
	return best, nil
}

// Find returns one specific release, for `orchard update --version`. It does not require
// the version to be newer: pinning to an older one on purpose is a legitimate thing to
// want when a release turns out badly.
func (u *Updater) Find(ctx context.Context, want Version) (Release, error) {
	releases, err := u.Releases(ctx)
	if err != nil {
		return Release{}, err
	}
	for _, r := range releases {
		if r.Version.Compare(want) == 0 {
			return r, nil
		}
	}
	return Release{}, fmt.Errorf("no release %s in %s on the %s channel", want, u.cfg.Repo, u.cfg.Channel)
}

// Fetch downloads the release's artifact for this platform and verifies it against the
// published checksums, returning the verified binary.
func (u *Updater) Fetch(ctx context.Context, rel Release) ([]byte, error) {
	name := AssetName(rel.Version)
	assetURL, ok := rel.Assets[name]
	if !ok {
		return nil, fmt.Errorf("release %s has no artifact for %s/%s", rel.Version, runtime.GOOS, runtime.GOARCH)
	}
	sumsURL, ok := rel.Assets[checksumFile]
	if !ok {
		// Not a warning and not a fallback: without checksums there is nothing to
		// verify against, and installing anyway would make the verification decorative.
		return nil, fmt.Errorf("release %s publishes no %s; refusing to install it unverified", rel.Version, checksumFile)
	}

	sums, err := u.get(ctx, sumsURL)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", checksumFile, err)
	}
	want, err := checksumFor(string(sums), name)
	if err != nil {
		return nil, err
	}

	archive, err := u.get(ctx, assetURL)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", name, err)
	}
	got := sha256.Sum256(archive)
	if hex.EncodeToString(got[:]) != want {
		return nil, fmt.Errorf("%s does not match its published checksum", name)
	}

	return extractBinary(archive)
}

func (u *Updater) authorize(req *http.Request) {
	if u.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+u.cfg.Token)
	}
}

func (u *Updater) get(ctx context.Context, url string) ([]byte, error) {
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(u.api, "http://127.0.0.1") {
		return nil, fmt.Errorf("refusing a non-HTTPS download URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	u.authorize(req)
	// The asset API returns metadata unless the caller asks for the bytes; this is
	// also the only form that works for a private repository.
	req.Header.Set("Accept", "application/octet-stream")

	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxArchive))
}

// checksumFor finds one file's digest in a sha256sum-format listing.
func checksumFor(sums, name string) (string, error) {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == name {
			if len(fields[0]) != 64 {
				return "", fmt.Errorf("%s: %q is not a sha256 digest", checksumFile, fields[0])
			}
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("%s does not list %s", checksumFile, name)
}

// extractBinary pulls the single expected file out of the archive. Nothing else in it is
// read, and nothing is written to disk here.
func extractBinary(archive []byte) ([]byte, error) {
	zr, err := gzip.NewReader(strings.NewReader(string(archive)))
	if err != nil {
		return nil, fmt.Errorf("archive is not gzip: %w", err)
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		// Only the binary, at the archive root, and only a regular file. A path with
		// a directory component is not something this archive should contain.
		if filepath.Base(h.Name) != binaryName || h.Name != binaryName || h.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(tr, maxArchive))
		if err != nil {
			return nil, err
		}
		if len(body) == 0 {
			return nil, errors.New("archive contains an empty binary")
		}
		return body, nil
	}
	return nil, fmt.Errorf("archive contains no %q at its root", binaryName)
}

// osExecutable is indirected for tests.
var osExecutable = os.Executable
