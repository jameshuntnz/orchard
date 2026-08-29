package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"howett.net/plist"

	"github.com/jameshuntnz/orchard/internal/ipa/ipatest"
	"github.com/jameshuntnz/orchard/internal/store"
	"github.com/jameshuntnz/orchard/internal/web"
)

const (
	token = "test-token-long-enough"
	// baseURL is deliberately not the test server's address: absolute URLs always come
	// from configuration, never from the request (DESIGN §4.3).
	baseURL = "https://orchard.example.ts.net"
)

type harness struct {
	*Server
	tailnet *httptest.Server
	upload  *httptest.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	renderer, err := web.New()
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	s := &Server{
		Store:     store.New(t.TempDir(), log),
		Web:       renderer,
		Log:       log,
		Token:     token,
		BaseURL:   baseURL,
		MaxUpload: 8 << 20,
		Version:   "test",
	}
	h := &harness{
		Server:  s,
		tailnet: httptest.NewServer(s.TailnetMux()),
		upload:  httptest.NewServer(s.UploadMux()),
	}
	t.Cleanup(h.tailnet.Close)
	t.Cleanup(h.upload.Close)
	return h
}

type meta map[string]any

func defaultMeta(bundleID string) meta {
	return meta{
		"branch":      "feature/new-checkout",
		"commit":      "abcdef1234567890abcdef1234567890abcdef12",
		"version":     "0.0.57",
		"buildNumber": "57",
		"bundleId":    bundleID,
		"title":       "Example",
		"notes":       "TEST BUILD - do not release.",
		"runUrl":      "https://ci.example.invalid/runs/123",
	}
}

func multipartBody(t *testing.T, ipaBytes []byte, m meta) (string, io.Reader) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	if m != nil {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		w, _ := mw.CreateFormField("metadata")
		w.Write(raw)
	}
	if ipaBytes != nil {
		w, _ := mw.CreateFormFile("ipa", "app.ipa")
		w.Write(ipaBytes)
	}
	mw.Close()
	return mw.FormDataContentType(), &buf
}

func (h *harness) do(t *testing.T, srv *httptest.Server, method, path, auth string, contentType string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (h *harness) publish(t *testing.T, app, slug, bundleID string) *http.Response {
	t.Helper()
	ct, body := multipartBody(t, ipatest.Binary(t, bundleID), defaultMeta(bundleID))
	return h.do(t, h.tailnet, http.MethodPost, "/api/v1/apps/"+app+"/builds/"+slug, token, ct, body)
}

func (h *harness) get(t *testing.T, path string) *http.Response {
	t.Helper()
	return h.do(t, h.tailnet, http.MethodGet, path, "", "", nil)
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return v
}

func wantStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("%s %s: status %d, want %d\n%s", resp.Request.Method, resp.Request.URL.Path,
			resp.StatusCode, want, body(t, resp))
	}
}

func wantError(t *testing.T, resp *http.Response, status int, code string) {
	t.Helper()
	if resp.StatusCode != status {
		t.Errorf("status %d, want %d", resp.StatusCode, status)
	}
	got := decode[errorBody](t, resp)
	if got.Error != code {
		t.Errorf("error code = %q, want %q (message: %s)", got.Error, code, got.Message)
	}
	if got.Message == "" {
		t.Error("error body has no message")
	}
}

// TestLifecycle walks the whole flow in one place: the design's claim is that this is
// small enough to test end to end in process (DESIGN §16).
func TestLifecycle(t *testing.T) {
	h := newHarness(t)

	resp := h.publish(t, "example", "feature-new-checkout", "com.example.app")
	wantStatus(t, resp, http.StatusOK)

	published := decode[struct {
		URL  string `json:"url"`
		App  string `json:"app"`
		Slug string `json:"slug"`
	}](t, resp)
	wantURL := baseURL + "/a/example/b/feature-new-checkout"
	if published.URL != wantURL {
		t.Errorf("publish url = %q, want %q", published.URL, wantURL)
	}
	if published.App != "example" || published.Slug != "feature-new-checkout" {
		t.Errorf("publish echoed %+v", published)
	}

	t.Run("install page", func(t *testing.T) {
		resp := h.get(t, "/a/example/b/feature-new-checkout")
		wantStatus(t, resp, http.StatusOK)
		page := body(t, resp)

		// The bundle identifier is what determines which installed app this replaces,
		// so it must be on the page (DESIGN §5.1).
		if !strings.Contains(page, "com.example.app") {
			t.Error("install page does not show the bundle identifier")
		}
		if !strings.Contains(page, "do not release") {
			t.Error("install page has no do-not-release marker")
		}
		// html/template only trusts http, https and mailto in an href; if the scheme is
		// stripped the install button silently does nothing.
		if strings.Contains(page, "ZgotmplZ") {
			t.Error("html/template stripped the itms-services scheme from the install link")
		}
		if !strings.Contains(page, "itms-services://?action=download-manifest") {
			t.Error("install page has no itms-services link")
		}
		if !strings.Contains(page, "<svg class=\"qr\"") {
			t.Error("install page has no QR code")
		}
	})

	t.Run("manifest", func(t *testing.T) {
		resp := h.get(t, "/a/example/b/feature-new-checkout/manifest.plist")
		wantStatus(t, resp, http.StatusOK)

		var doc struct {
			Items []struct {
				Assets []struct {
					URL string `plist:"url"`
				} `plist:"assets"`
				Metadata struct {
					BundleIdentifier string `plist:"bundle-identifier"`
					BundleVersion    string `plist:"bundle-version"`
				} `plist:"metadata"`
			} `plist:"items"`
		}
		raw := []byte(body(t, resp))
		if _, err := plist.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("manifest does not parse: %v\n%s", err, raw)
		}
		// The URL the phone will fetch must be the configured external one, not the
		// address this request happened to arrive on (DESIGN §4.3).
		want := baseURL + "/a/example/b/feature-new-checkout/app.ipa"
		if got := doc.Items[0].Assets[0].URL; got != want {
			t.Errorf("manifest ipa url = %q, want %q", got, want)
		}
		if doc.Items[0].Metadata.BundleIdentifier != "com.example.app" {
			t.Errorf("manifest bundle-identifier = %q", doc.Items[0].Metadata.BundleIdentifier)
		}
	})

	t.Run("ipa download", func(t *testing.T) {
		resp := h.get(t, "/a/example/b/feature-new-checkout/app.ipa")
		wantStatus(t, resp, http.StatusOK)
		if len(body(t, resp)) == 0 {
			t.Error("ipa download was empty")
		}
	})

	t.Run("api list", func(t *testing.T) {
		resp := h.get(t, "/api/v1/apps")
		wantStatus(t, resp, http.StatusOK)

		got := decode[struct {
			Apps []appJSON `json:"apps"`
		}](t, resp)
		if len(got.Apps) != 1 || len(got.Apps[0].Builds) != 1 {
			t.Fatalf("apps = %+v", got.Apps)
		}
		b := got.Apps[0].Builds[0]
		if b.Slug != "feature-new-checkout" || b.Branch != "feature/new-checkout" ||
			b.BundleID != "com.example.app" || b.Version != "0.0.57" || b.IPASize == 0 {
			t.Errorf("build = %+v", b)
		}
		if b.URL != wantURL {
			t.Errorf("build url = %q, want %q", b.URL, wantURL)
		}
		if got.Apps[0].Title != "Example" {
			t.Errorf("app title = %q", got.Apps[0].Title)
		}
	})

	t.Run("indexes", func(t *testing.T) {
		root := body(t, h.get(t, "/"))
		if !strings.Contains(root, "Example") {
			t.Error("root index does not list the app")
		}
		app := body(t, h.get(t, "/a/example"))
		if !strings.Contains(app, "feature/new-checkout") {
			t.Error("app index does not list the branch")
		}
	})

	t.Run("delete then gone", func(t *testing.T) {
		resp := h.do(t, h.tailnet, http.MethodDelete, "/api/v1/apps/example/builds/feature-new-checkout", token, "", nil)
		wantStatus(t, resp, http.StatusOK)

		wantError(t, h.get(t, "/a/example/b/feature-new-checkout"), http.StatusNotFound, "not_found")
		// An app with no builds does not exist (DESIGN §5).
		wantError(t, h.get(t, "/a/example"), http.StatusNotFound, "not_found")

		// A repeat delete is a 404; treating it as success is the consumer's convention
		// (DESIGN §11), not something the status code hides.
		resp = h.do(t, h.tailnet, http.MethodDelete, "/api/v1/apps/example/builds/feature-new-checkout", token, "", nil)
		wantError(t, resp, http.StatusNotFound, "not_found")
	})
}

// Apps are fully isolated in storage, in URLs and in sweeps (DESIGN §5).
func TestSweepIsolatesApps(t *testing.T) {
	h := newHarness(t)
	for _, app := range []string{"alpha", "beta"} {
		for _, slug := range []string{"main", "stale"} {
			wantStatus(t, h.publish(t, app, slug, "com.example."+app), http.StatusOK)
		}
	}

	resp := h.do(t, h.tailnet, http.MethodPost, "/api/v1/apps/alpha/sweep", token,
		"application/json", strings.NewReader(`{"keep":["main"]}`))
	wantStatus(t, resp, http.StatusOK)

	swept := decode[struct {
		Removed []string `json:"removed"`
		Kept    int      `json:"kept"`
	}](t, resp)
	if len(swept.Removed) != 1 || swept.Removed[0] != "stale" || swept.Kept != 1 {
		t.Errorf("sweep = %+v", swept)
	}

	if resp := h.get(t, "/a/beta/b/stale"); resp.StatusCode != http.StatusOK {
		t.Error("sweeping alpha removed a build from beta")
	}
	if resp := h.get(t, "/a/alpha/b/stale"); resp.StatusCode != http.StatusNotFound {
		t.Error("sweep did not remove alpha's stale build")
	}
}

func TestSweepRejectsEmptyKeep(t *testing.T) {
	h := newHarness(t)
	wantStatus(t, h.publish(t, "example", "main", "com.example.app"), http.StatusOK)

	for _, b := range []string{`{"keep":[]}`, `{}`} {
		resp := h.do(t, h.tailnet, http.MethodPost, "/api/v1/apps/example/sweep", token,
			"application/json", strings.NewReader(b))
		wantError(t, resp, http.StatusBadRequest, "empty_keep")
	}
	// Nothing was removed.
	if resp := h.get(t, "/a/example/b/main"); resp.StatusCode != http.StatusOK {
		t.Error("a rejected sweep still removed a build")
	}
}

func TestSweepRejectsMalformedKeep(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, h.tailnet, http.MethodPost, "/api/v1/apps/example/sweep", token,
		"application/json", strings.NewReader(`{"keep":["feature/new-checkout"]}`))
	wantError(t, resp, http.StatusBadRequest, "invalid_slug")
}

// The IPA's own identifier is checked against the metadata, turning a confusing
// device-side failure into a clear CI failure (DESIGN §8).
func TestPublishRejectsBundleIDMismatch(t *testing.T) {
	h := newHarness(t)
	ct, b := multipartBody(t, ipatest.Binary(t, "com.example.actual"), defaultMeta("com.example.claimed"))
	resp := h.do(t, h.tailnet, http.MethodPost, "/api/v1/apps/example/builds/main", token, ct, b)
	wantError(t, resp, http.StatusConflict, "bundle_id_mismatch")

	if resp := h.get(t, "/a/example/b/main"); resp.StatusCode != http.StatusNotFound {
		t.Error("a rejected publish still created a build")
	}
}

func TestWritesRequireToken(t *testing.T) {
	h := newHarness(t)
	ct, b := multipartBody(t, ipatest.Binary(t, "com.example.app"), defaultMeta("com.example.app"))

	for _, tc := range []struct{ name, auth string }{
		{"missing", ""},
		{"wrong", "not-the-token"},
		{"prefix of the real token", token[:8]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.do(t, h.tailnet, http.MethodPost, "/api/v1/apps/example/builds/main", tc.auth, ct, b)
			wantError(t, resp, http.StatusUnauthorized, "unauthorized")
		})
	}

	// Reads are open: tailnet membership is the boundary (DESIGN §10).
	wantStatus(t, h.get(t, "/api/v1/apps"), http.StatusOK)
	wantStatus(t, h.get(t, "/healthz"), http.StatusOK)
}

func TestPublishRejectsBadIdentifiers(t *testing.T) {
	h := newHarness(t)
	ct, b := multipartBody(t, ipatest.Binary(t, "com.example.app"), defaultMeta("com.example.app"))

	resp := h.do(t, h.tailnet, http.MethodPost, "/api/v1/apps/Bad_App/builds/main", token, ct, b)
	wantError(t, resp, http.StatusBadRequest, "invalid_app")

	resp = h.do(t, h.tailnet, http.MethodPost, "/api/v1/apps/example/builds/feature%2Fx", token, ct, b)
	wantError(t, resp, http.StatusBadRequest, "invalid_slug")
}

func TestPublishRejectsBadMetadata(t *testing.T) {
	h := newHarness(t)
	ipaBytes := ipatest.Binary(t, "com.example.app")

	t.Run("not json", func(t *testing.T) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		w, _ := mw.CreateFormField("metadata")
		w.Write([]byte("{not json"))
		f, _ := mw.CreateFormFile("ipa", "app.ipa")
		f.Write(ipaBytes)
		mw.Close()
		resp := h.do(t, h.tailnet, http.MethodPost, "/api/v1/apps/example/builds/main", token, mw.FormDataContentType(), &buf)
		wantError(t, resp, http.StatusBadRequest, "invalid_metadata")
	})

	t.Run("missing required field", func(t *testing.T) {
		m := defaultMeta("com.example.app")
		delete(m, "title")
		ct, b := multipartBody(t, ipaBytes, m)
		resp := h.do(t, h.tailnet, http.MethodPost, "/api/v1/apps/example/builds/main", token, ct, b)
		wantError(t, resp, http.StatusBadRequest, "invalid_metadata")
	})

	t.Run("no ipa part", func(t *testing.T) {
		ct, b := multipartBody(t, nil, defaultMeta("com.example.app"))
		resp := h.do(t, h.tailnet, http.MethodPost, "/api/v1/apps/example/builds/main", token, ct, b)
		wantError(t, resp, http.StatusBadRequest, "invalid_metadata")
	})

	t.Run("unreadable ipa", func(t *testing.T) {
		ct, b := multipartBody(t, []byte("not a zip archive"), defaultMeta("com.example.app"))
		resp := h.do(t, h.tailnet, http.MethodPost, "/api/v1/apps/example/builds/main", token, ct, b)
		wantError(t, resp, http.StatusBadRequest, "invalid_metadata")
	})

	t.Run("not multipart", func(t *testing.T) {
		resp := h.do(t, h.tailnet, http.MethodPost, "/api/v1/apps/example/builds/main", token,
			"application/json", strings.NewReader(`{}`))
		wantError(t, resp, http.StatusBadRequest, "invalid_metadata")
	})
}

// An unbounded multipart read on a shared host is a trivial disk-fill (DESIGN §15).
func TestPublishOverSizeCap(t *testing.T) {
	h := newHarness(t)
	h.MaxUpload = 1 << 10

	ct, b := multipartBody(t, bytes.Repeat([]byte("x"), 64<<10), defaultMeta("com.example.app"))
	resp := h.do(t, h.tailnet, http.MethodPost, "/api/v1/apps/example/builds/main", token, ct, b)
	wantError(t, resp, http.StatusRequestEntityTooLarge, "payload_too_large")
}

// The upload listener is the widest surface: writes only, and nothing that serves a build
// or a page (DESIGN §4.3, §15).
func TestUploadListenerServesWritesOnly(t *testing.T) {
	h := newHarness(t)
	wantStatus(t, h.publish(t, "example", "main", "com.example.app"), http.StatusOK)

	for _, path := range []string{
		"/",
		"/a/example",
		"/a/example/b/main",
		"/a/example/b/main/manifest.plist",
		"/a/example/b/main/app.ipa",
		"/api/v1/apps",
	} {
		resp := h.do(t, h.upload, http.MethodGet, path, token, "", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("upload listener served %s with status %d; it must serve writes only", path, resp.StatusCode)
		}
	}

	// Writes do work there, and still require the token.
	ct, b := multipartBody(t, ipatest.Binary(t, "com.example.app"), defaultMeta("com.example.app"))
	resp := h.do(t, h.upload, http.MethodPost, "/api/v1/apps/example/builds/via-gateway", token, ct, b)
	wantStatus(t, resp, http.StatusOK)

	// And the URL it hands back is the tailnet one, because that is where the phone will
	// fetch it — which is what makes the two-listener split invisible (DESIGN §4.3).
	got := decode[struct {
		URL string `json:"url"`
	}](t, resp)
	if got.URL != baseURL+"/a/example/b/via-gateway" {
		t.Errorf("url = %q, want the configured base URL", got.URL)
	}

	ct, b = multipartBody(t, ipatest.Binary(t, "com.example.app"), defaultMeta("com.example.app"))
	resp = h.do(t, h.upload, http.MethodPost, "/api/v1/apps/example/builds/nope", "", ct, b)
	wantError(t, resp, http.StatusUnauthorized, "unauthorized")
}

func TestHealthz(t *testing.T) {
	h := newHarness(t)
	h.SetSelfUpdate(SelfUpdateStatus{Enabled: true, Channel: "dev"})
	h.tailnet.Close()
	h.tailnet = httptest.NewServer(h.Server.TailnetMux())
	t.Cleanup(h.tailnet.Close)

	resp := h.get(t, "/healthz")
	wantStatus(t, resp, http.StatusOK)

	got := decode[struct {
		Status      string   `json:"status"`
		Version     string   `json:"version"`
		APIVersions []string `json:"apiVersions"`
		Deprecated  []string `json:"deprecated"`
		SelfUpdate  struct {
			Enabled bool   `json:"enabled"`
			Channel string `json:"channel"`
			Reason  string `json:"reason"`
		} `json:"selfUpdate"`
	}](t, resp)

	// Whether the node moves on its own is half of "what is running here", now that
	// dev builds publish per green commit.
	if !got.SelfUpdate.Enabled || got.SelfUpdate.Channel != "dev" {
		t.Errorf("selfUpdate = %+v", got.SelfUpdate)
	}
	if got.Status != "ok" || got.Version != "test" {
		t.Errorf("healthz = %+v", got)
	}
	if len(got.APIVersions) != 1 || got.APIVersions[0] != "v1" {
		t.Errorf("apiVersions = %v", got.APIVersions)
	}
	if got.Deprecated == nil {
		t.Error("deprecated must be an empty array, not null")
	}
}

// Branch names, titles and notes arrive over the network and are rendered into HTML
// (DESIGN §15).
func TestUntrustedMetadataIsEscaped(t *testing.T) {
	h := newHarness(t)
	m := defaultMeta("com.example.app")
	m["branch"] = `<script>alert(1)</script>`
	m["title"] = `Ex"&<ample>`
	m["notes"] = `</style><img src=x onerror=alert(1)>`

	ct, b := multipartBody(t, ipatest.Binary(t, "com.example.app"), m)
	wantStatus(t, h.do(t, h.tailnet, http.MethodPost, "/api/v1/apps/example/builds/main", token, ct, b), http.StatusOK)

	for _, path := range []string{"/", "/a/example", "/a/example/b/main"} {
		page := body(t, h.get(t, path))
		if strings.Contains(page, "<script>alert(1)</script>") || strings.Contains(page, "<img src=x") {
			t.Errorf("%s rendered untrusted metadata unescaped", path)
		}
	}
}

// The upload listener is bound beyond the tailnet, so a source allowlist gates it
// before routing or token comparison (DESIGN §15).
func TestUploadListenerSourceAllowlist(t *testing.T) {
	h := newHarness(t)
	// httptest connects over loopback, so this is what a permitted source looks like.
	h.UploadAllowed = func(a netip.Addr) bool { return a.Unmap().IsLoopback() }
	h.upload.Close()
	h.upload = httptest.NewServer(h.Server.UploadMux())
	t.Cleanup(h.upload.Close)

	ct, b := multipartBody(t, ipatest.Binary(t, "com.example.app"), defaultMeta("com.example.app"))
	resp := h.do(t, h.upload, http.MethodPost, "/api/v1/apps/example/builds/allowed", token, ct, b)
	wantStatus(t, resp, http.StatusOK)
}

func TestUploadListenerRefusesDisallowedSource(t *testing.T) {
	h := newHarness(t)
	h.UploadAllowed = func(netip.Addr) bool { return false }
	h.upload.Close()
	h.upload = httptest.NewServer(h.Server.UploadMux())
	t.Cleanup(h.upload.Close)

	ct, b := multipartBody(t, ipatest.Binary(t, "com.example.app"), defaultMeta("com.example.app"))
	resp := h.do(t, h.upload, http.MethodPost, "/api/v1/apps/example/builds/blocked", token, ct, b)
	wantError(t, resp, http.StatusUnauthorized, "unauthorized")

	// Nothing was written.
	if _, err := h.Store.Get("example", "blocked"); err == nil {
		t.Error("a refused source still published a build")
	}

	// Even /healthz is behind the allowlist on this listener: a source that may not
	// publish has no business enumerating the service either.
	resp = h.do(t, h.upload, http.MethodGet, "/healthz", token, "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("healthz on the upload listener = %d, want 401 for a refused source", resp.StatusCode)
	}
}

// The refusal must not tell an unexpected source why it was refused — that hands it the
// one fact it did not already have.
func TestUploadRefusalIsIndistinguishableFromABadToken(t *testing.T) {
	h := newHarness(t)
	ct, b := multipartBody(t, ipatest.Binary(t, "com.example.app"), defaultMeta("com.example.app"))
	badToken := h.do(t, h.upload, http.MethodPost, "/api/v1/apps/example/builds/x", "wrong-token", ct, b)
	fromBadToken := decode[errorBody](t, badToken)

	h.UploadAllowed = func(netip.Addr) bool { return false }
	h.upload.Close()
	h.upload = httptest.NewServer(h.Server.UploadMux())
	t.Cleanup(h.upload.Close)

	ct, b = multipartBody(t, ipatest.Binary(t, "com.example.app"), defaultMeta("com.example.app"))
	blocked := h.do(t, h.upload, http.MethodPost, "/api/v1/apps/example/builds/x", token, ct, b)
	fromBlocked := decode[errorBody](t, blocked)

	if fromBlocked != fromBadToken {
		t.Errorf("a blocked source gets %+v but a bad token gets %+v; the two must be indistinguishable",
			fromBlocked, fromBadToken)
	}
}

// The tailnet listener is not gated by the allowlist: there, membership of the tailnet
// is already the boundary (DESIGN §10).
func TestAllowlistDoesNotAffectTheTailnetListener(t *testing.T) {
	h := newHarness(t)
	h.UploadAllowed = func(netip.Addr) bool { return false }

	wantStatus(t, h.publish(t, "example", "main", "com.example.app"), http.StatusOK)
	wantStatus(t, h.get(t, "/a/example/b/main"), http.StatusOK)
	wantStatus(t, h.get(t, "/api/v1/apps"), http.StatusOK)
}

// When self-update is off, /healthz says why. "Not moving" and "not moving because
// nobody configured it" are different situations to be looking at during an incident.
func TestHealthzReportsWhySelfUpdateIsOff(t *testing.T) {
	h := newHarness(t)
	h.SetSelfUpdate(SelfUpdateStatus{Reason: "container: the image tag is the unit of deployment"})
	h.tailnet.Close()
	h.tailnet = httptest.NewServer(h.Server.TailnetMux())
	t.Cleanup(h.tailnet.Close)

	got := decode[struct {
		SelfUpdate SelfUpdateStatus `json:"selfUpdate"`
	}](t, h.get(t, "/healthz"))

	if got.SelfUpdate.Enabled {
		t.Error("selfUpdate.enabled is true")
	}
	if !strings.Contains(got.SelfUpdate.Reason, "image tag") {
		t.Errorf("reason = %q", got.SelfUpdate.Reason)
	}
}

// Enabled says what the node intends; lastCheck says whether it is happening. The two
// came apart in practice — configured to update, reporting itself enabled, every check
// failing on authentication — and a node that has silently stopped tracking releases
// looks exactly like one that is already up to date.
func TestHealthzReportsTheLastCheckOutcome(t *testing.T) {
	h := newHarness(t)
	h.tailnet.Close()
	h.tailnet = httptest.NewServer(h.Server.TailnetMux())
	t.Cleanup(h.tailnet.Close)

	type health struct {
		SelfUpdate SelfUpdateStatus `json:"selfUpdate"`
	}

	t.Run("before any check has completed", func(t *testing.T) {
		h.SetSelfUpdate(SelfUpdateStatus{Enabled: true, Channel: "dev"})
		got := decode[health](t, h.get(t, "/healthz")).SelfUpdate
		if got.LastCheck != nil {
			t.Error("lastCheck present before any check ran")
		}
	})

	t.Run("a failing check is visible despite enabled", func(t *testing.T) {
		now := time.Now().UTC()
		h.SetSelfUpdate(SelfUpdateStatus{
			Enabled:   true,
			Channel:   "dev",
			LastCheck: &now,
			LastError: "listing releases: 404 Not Found",
		})
		got := decode[health](t, h.get(t, "/healthz")).SelfUpdate
		if !got.Enabled {
			t.Error("enabled should still be true — the intent has not changed")
		}
		if got.LastCheck == nil || got.LastError == "" {
			t.Fatalf("a failing check is not visible: %+v", got)
		}
		if !strings.Contains(got.LastError, "404") {
			t.Errorf("lastError = %q", got.LastError)
		}
	})

	t.Run("a successful check with something newer", func(t *testing.T) {
		now := time.Now().UTC()
		h.SetSelfUpdate(SelfUpdateStatus{
			Enabled: true, Channel: "dev", LastCheck: &now, Available: "0.2.0",
		})
		got := decode[health](t, h.get(t, "/healthz")).SelfUpdate
		if got.LastError != "" {
			t.Errorf("lastError = %q, want empty on success", got.LastError)
		}
		if got.Available != "0.2.0" {
			t.Errorf("available = %q", got.Available)
		}
	})
}

// The update loop writes this while /healthz reads it, so it has to be safe under -race.
func TestSelfUpdateStatusIsSafeConcurrently(t *testing.T) {
	h := newHarness(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 50 {
			now := time.Now().UTC()
			h.SetSelfUpdate(SelfUpdateStatus{Enabled: true, LastCheck: &now, Available: string(rune('a' + i%26))})
		}
	}()
	for range 50 {
		_ = h.Server.selfUpdateStatus()
	}
	<-done
}
