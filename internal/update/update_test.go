package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// ---------------------------------------------------------------- versions

func TestVersionOrdering(t *testing.T) {
	// A prerelease sorts below the release it precedes, which is what stops a release
	// candidate looking like an upgrade from the version it is a candidate for.
	ordered := []string{
		"0.0.1", "0.1.0", "0.9.9", "1.0.0-alpha", "1.0.0-alpha.1",
		"1.0.0-beta", "1.0.0-rc.1", "1.0.0-rc.2", "1.0.0", "1.0.1", "1.2.0", "2.0.0",
	}
	for i := 0; i < len(ordered)-1; i++ {
		a, err := ParseVersion(ordered[i])
		if err != nil {
			t.Fatalf("%s: %v", ordered[i], err)
		}
		b, err := ParseVersion(ordered[i+1])
		if err != nil {
			t.Fatalf("%s: %v", ordered[i+1], err)
		}
		if a.Compare(b) != -1 {
			t.Errorf("%s should sort below %s", a, b)
		}
		if b.Compare(a) != 1 {
			t.Errorf("%s should sort above %s", b, a)
		}
		if a.Compare(a) != 0 {
			t.Errorf("%s should equal itself", a)
		}
	}
}

func TestParseVersion(t *testing.T) {
	if v, err := ParseVersion("v1.4.0"); err != nil || v.String() != "1.4.0" {
		t.Errorf("leading v not handled: %v %v", v, err)
	}
	// Build metadata does not affect ordering, so it is dropped rather than compared.
	if v, err := ParseVersion("1.4.0+abc123"); err != nil || v.String() != "1.4.0" {
		t.Errorf("build metadata not dropped: %v %v", v, err)
	}
	for _, bad := range []string{"", "1.2", "1.2.3.4", "one.two.three", "-1.0.0", "latest"} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("ParseVersion(%q) succeeded", bad)
		}
	}
}

// ---------------------------------------------------------------- archives

func tarGz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func TestExtractBinary(t *testing.T) {
	body, err := extractBinary(tarGz(t, map[string][]byte{"orchard": []byte("ELF-ish")}))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "ELF-ish" {
		t.Errorf("got %q", body)
	}
}

func TestExtractBinaryRejections(t *testing.T) {
	tests := map[string][]byte{
		"no binary at all":       tarGz(t, map[string][]byte{"README": []byte("x")}),
		"binary nested in a dir": tarGz(t, map[string][]byte{"bin/orchard": []byte("x")}),
		"empty binary":           tarGz(t, map[string][]byte{"orchard": {}}),
		"not gzip":               []byte("this is not an archive"),
	}
	for name, archive := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := extractBinary(archive); err == nil {
				t.Error("extractBinary accepted it")
			}
		})
	}
}

func TestChecksumFor(t *testing.T) {
	digest := "a" + string(bytes.Repeat([]byte("b"), 63))
	sums := fmt.Sprintf("%s  orchard_1.0.0_linux_amd64.tar.gz\n%s  SHA256SUMS.other\n", digest, digest)

	got, err := checksumFor(sums, "orchard_1.0.0_linux_amd64.tar.gz")
	if err != nil || got != digest {
		t.Errorf("checksumFor = %q, %v", got, err)
	}
	if _, err := checksumFor(sums, "orchard_1.0.0_darwin_arm64.tar.gz"); err == nil {
		t.Error("checksumFor found an entry that is not there")
	}
	if _, err := checksumFor("tooshort  orchard.tar.gz", "orchard.tar.gz"); err == nil {
		t.Error("checksumFor accepted a digest that is not sha256-length")
	}
}

// ---------------------------------------------------------------- the release feed

type fakeRelease struct {
	tag        string
	prerelease bool
	draft      bool
	assets     map[string][]byte
}

// serve stands in for the release API and its asset downloads.
func serve(t *testing.T, releases []fakeRelease) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	type asset struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	type rel struct {
		TagName    string  `json:"tag_name"`
		Draft      bool    `json:"draft"`
		Prerelease bool    `json:"prerelease"`
		Assets     []asset `json:"assets"`
	}

	var out []rel
	for _, r := range releases {
		entry := rel{TagName: r.tag, Draft: r.draft, Prerelease: r.prerelease}
		for name, body := range r.assets {
			path := "/assets/" + r.tag + "/" + name
			entry.Assets = append(entry.Assets, asset{Name: name, URL: srv.URL + path})
			mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) { w.Write(body) })
		}
		out = append(out, entry)
	}
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(out)
	})
	return srv
}

func newTestUpdater(t *testing.T, current string, srv *httptest.Server) *Updater {
	t.Helper()
	u, err := New(Config{Repo: "example/orchard", Channel: DefaultChannel}, current, filepath.Join(t.TempDir(), "orchard"))
	if err != nil {
		t.Fatal(err)
	}
	u.api = srv.URL
	return u
}

// A release published for this platform, correctly checksummed.
func goodRelease(t *testing.T, version, payload string) fakeRelease {
	t.Helper()
	v, err := ParseVersion(version)
	if err != nil {
		t.Fatal(err)
	}
	archive := tarGz(t, map[string][]byte{"orchard": []byte(payload)})
	sum := sha256.Sum256(archive)
	name := AssetName(v)
	return fakeRelease{
		tag: "v" + version,
		assets: map[string][]byte{
			name:         archive,
			checksumFile: []byte(fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), name)),
		},
	}
}

func TestLatestPicksTheNewest(t *testing.T) {
	srv := serve(t, []fakeRelease{
		goodRelease(t, "1.0.0", "a"),
		goodRelease(t, "1.2.0", "b"),
		goodRelease(t, "1.1.0", "c"),
	})
	rel, err := newTestUpdater(t, "1.0.0", srv).Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version.String() != "1.2.0" {
		t.Errorf("Latest = %s, want 1.2.0", rel.Version)
	}
}

func TestLatestIgnoresDraftsAndPrereleases(t *testing.T) {
	draft := goodRelease(t, "2.0.0", "draft")
	draft.draft = true
	pre := goodRelease(t, "1.9.0", "pre")
	pre.prerelease = true

	srv := serve(t, []fakeRelease{draft, pre, goodRelease(t, "1.1.0", "real")})
	rel, err := newTestUpdater(t, "1.0.0", srv).Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version.String() != "1.1.0" {
		t.Errorf("Latest = %s, want 1.1.0 — a draft or prerelease was treated as installable", rel.Version)
	}
}

func TestLatestReportsNothingNewer(t *testing.T) {
	srv := serve(t, []fakeRelease{goodRelease(t, "1.0.0", "a")})
	_, err := newTestUpdater(t, "1.0.0", srv).Latest(context.Background())
	if !errors.Is(err, ErrNoUpdate) {
		t.Errorf("err = %v, want ErrNoUpdate", err)
	}
	_, err = newTestUpdater(t, "2.0.0", srv).Latest(context.Background())
	if !errors.Is(err, ErrNoUpdate) {
		t.Errorf("a newer running version should also be ErrNoUpdate, got %v", err)
	}
}

// ---------------------------------------------------------------- verification

func TestFetchVerifies(t *testing.T) {
	srv := serve(t, []fakeRelease{goodRelease(t, "1.1.0", "the new binary")})
	u := newTestUpdater(t, "1.0.0", srv)
	rel, err := u.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	body, err := u.Fetch(context.Background(), rel)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "the new binary" {
		t.Errorf("got %q", body)
	}
}

// A release without checksums is refused, not installed unverified — otherwise the
// verification is decorative.
func TestFetchRefusesAReleaseWithoutChecksums(t *testing.T) {
	r := goodRelease(t, "1.1.0", "x")
	delete(r.assets, checksumFile)
	srv := serve(t, []fakeRelease{r})

	u := newTestUpdater(t, "1.0.0", srv)
	rel, err := u.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = u.Fetch(context.Background(), rel)
	if err == nil {
		t.Fatal("Fetch installed a release with no checksums")
	}
	if !bytes.Contains([]byte(err.Error()), []byte(checksumFile)) {
		t.Errorf("error = %q, want it to name %s", err, checksumFile)
	}
}

// The case the checksums exist for: a corrupted or substituted archive.
func TestFetchRefusesAChecksumMismatch(t *testing.T) {
	r := goodRelease(t, "1.1.0", "x")
	v, _ := ParseVersion("1.1.0")
	r.assets[AssetName(v)] = tarGz(t, map[string][]byte{"orchard": []byte("something else entirely")})
	srv := serve(t, []fakeRelease{r})

	u := newTestUpdater(t, "1.0.0", srv)
	rel, err := u.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.Fetch(context.Background(), rel); err == nil {
		t.Fatal("Fetch accepted an archive that does not match its checksum")
	}
}

func TestFetchRefusesAReleaseWithNoArtifactForThisPlatform(t *testing.T) {
	r := goodRelease(t, "1.1.0", "x")
	v, _ := ParseVersion("1.1.0")
	delete(r.assets, AssetName(v))
	srv := serve(t, []fakeRelease{r})

	u := newTestUpdater(t, "1.0.0", srv)
	rel, err := u.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = u.Fetch(context.Background(), rel)
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte(runtime.GOARCH)) {
		t.Errorf("error = %v, want it to name the platform", err)
	}
}

// ---------------------------------------------------------------- install and rollback

func TestInstallAndRollback(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "orchard")
	if err := os.WriteFile(binary, []byte("the old binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	u, err := New(Config{}, "1.0.0", binary)
	if err != nil {
		t.Fatal(err)
	}
	newVersion, _ := ParseVersion("1.1.0")
	if err := u.Install([]byte("the new binary"), newVersion); err != nil {
		t.Fatal(err)
	}

	if got, _ := os.ReadFile(binary); string(got) != "the new binary" {
		t.Errorf("binary = %q, want the new one", got)
	}
	if got, _ := os.ReadFile(PreviousPath(binary)); string(got) != "the old binary" {
		t.Errorf("previous = %q, want the old one kept alongside", got)
	}
	if info, err := os.Stat(binary); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed binary is not executable: %v %v", info, err)
	}

	marker, pending := PendingMarker(binary)
	if !pending {
		t.Fatal("no marker after install; the new binary would never be self-checked")
	}
	if marker.PreviousVersion != "1.0.0" || marker.NewVersion != "1.1.0" {
		t.Errorf("marker = %+v", marker)
	}

	// The rollback path: put the old one back and keep the failed one as evidence.
	if err := Rollback(binary); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(binary); string(got) != "the old binary" {
		t.Errorf("after rollback binary = %q, want the old one", got)
	}
	if got, _ := os.ReadFile(binary + ".failed"); string(got) != "the new binary" {
		t.Errorf("the failed binary was not kept: %q", got)
	}
	if _, pending := PendingMarker(binary); pending {
		t.Error("marker survived the rollback; the next start would check again")
	}
}

func TestClearMarkerIsIdempotent(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "orchard")
	if err := ClearMarker(binary); err != nil {
		t.Errorf("ClearMarker with no marker = %v, want nil", err)
	}
}

func TestRollbackWithoutAPreviousBinary(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "orchard")
	os.WriteFile(binary, []byte("x"), 0o755)
	if err := Rollback(binary); err == nil {
		t.Error("Rollback succeeded with nothing to roll back to")
	}
}

func TestNewRejectsAnUncomparableRunningVersion(t *testing.T) {
	// "dev" is what an unstamped build reports, and nothing can be judged newer than
	// something unorderable.
	if _, err := New(Config{}, "dev", "/tmp/orchard"); err == nil {
		t.Error("New accepted a running version it cannot compare")
	}
}
