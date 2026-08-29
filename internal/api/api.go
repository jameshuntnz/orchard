// Package api serves Orchard's HTTP surface: the versioned API under /api/v1 and the
// human browse pages (DESIGN §6).
package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jameshuntnz/orchard/internal/store"
	"github.com/jameshuntnz/orchard/internal/web"
)

// APIVersions is what /healthz reports as served. A consumer pins a path prefix and can
// assert compatibility rather than assume it (DESIGN §6).
var APIVersions = []string{"v1"}

// Deprecated lists API versions still served but scheduled for removal.
var Deprecated = []string{}

type Server struct {
	Store *store.Store
	Web   *web.Renderer
	Log   *slog.Logger

	// Token authorises every write. It grants access to every app (DESIGN §10).
	Token string
	// BaseURL is the external base for every absolute URL the service emits. It never
	// comes from the request: a build published through the upload listener still gets
	// a manifest addressed at the ts.net name, because that is where the phone will
	// fetch it (DESIGN §4.3).
	BaseURL   string
	MaxUpload int64
	Version   string
}

// TailnetMux serves everything: browse pages, reads and writes. Reads are unauthenticated
// because tailnet membership is the boundary (DESIGN §10).
func (s *Server) TailnetMux() http.Handler {
	mux := http.NewServeMux()
	s.routeWrites(mux)

	mux.HandleFunc("GET /api/v1/apps", s.handleListApps)
	mux.HandleFunc("GET /healthz", s.handleHealth)

	mux.HandleFunc("GET /{$}", s.handleRoot)
	mux.HandleFunc("GET /a/{app}", s.handleApp)
	mux.HandleFunc("GET /a/{app}/b/{slug}", s.handleInstall)
	mux.HandleFunc("GET /a/{app}/b/{slug}/manifest.plist", s.handleManifest)
	mux.HandleFunc("GET /a/{app}/b/{slug}/app.ipa", s.handleIPA)
	return mux
}

// UploadMux is the CI-facing listener. It is plain HTTP bound beyond the tailnet, so it
// carries writes only: no browse page, no IPA download, nothing to read, and a bearer
// token required on every route (DESIGN §4.3, §15).
func (s *Server) UploadMux() http.Handler {
	mux := http.NewServeMux()
	s.routeWrites(mux)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	return mux
}

func (s *Server) routeWrites(mux *http.ServeMux) {
	mux.Handle("POST /api/v1/apps/{app}/builds/{slug}", s.requireToken(s.handlePublish))
	mux.Handle("DELETE /api/v1/apps/{app}/builds/{slug}", s.requireToken(s.handleDelete))
	mux.Handle("POST /api/v1/apps/{app}/sweep", s.requireToken(s.handleSweep))
}

// requireToken compares the bearer token in constant time (DESIGN §15). Both sides are
// hashed first so the comparison does not also leak the token's length.
func (s *Server) requireToken(next http.HandlerFunc) http.Handler {
	want := sha256.Sum256([]byte(s.Token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, prefix) {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		got := sha256.Sum256([]byte(strings.TrimPrefix(h, prefix)))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid bearer token")
			return
		}
		next(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "ok",
		"version":     s.Version,
		"apiVersions": APIVersions,
		"deprecated":  Deprecated,
	})
}

// ---------------------------------------------------------------- responses

// errorBody is the shape every non-2xx from /api/v1 returns, so a caller can log
// something useful without special-casing each endpoint (DESIGN §6).
type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorBody{Error: code, Message: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// idents validates the app and slug path values before anything touches the filesystem
// (DESIGN §15). It writes the error response itself and reports whether to continue.
func idents(w http.ResponseWriter, r *http.Request, wantSlug bool) (app, slug string, ok bool) {
	app = r.PathValue("app")
	if !store.ValidIdent(app) {
		writeErr(w, http.StatusBadRequest, "invalid_app", "app must match ^[a-z0-9][a-z0-9-]*$")
		return "", "", false
	}
	if wantSlug {
		slug = r.PathValue("slug")
		if !store.ValidIdent(slug) {
			writeErr(w, http.StatusBadRequest, "invalid_slug", "slug must match ^[a-z0-9][a-z0-9-]*$")
			return "", "", false
		}
	}
	return app, slug, true
}

func (s *Server) internal(w http.ResponseWriter, msg string, err error) {
	s.Log.Error(msg, "err", err)
	writeErr(w, http.StatusInternalServerError, "internal", msg)
}

func notFound(w http.ResponseWriter) {
	writeErr(w, http.StatusNotFound, "not_found", "no such build")
}

var errBuildMissing = errors.New("build missing")

// ---------------------------------------------------------------- logging

// Logging emits one structured line per request — method, path, status, bytes, duration,
// which listener it arrived on, and the tsnet peer where there is one — on stdout, for
// the supervisor to capture. No log files, no rotation to configure (DESIGN §4.4).
func Logging(next http.Handler, log *slog.Logger, listener string, peer func(*http.Request) string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		attrs := []any{
			"listener", listener,
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"dur", time.Since(start).Round(time.Millisecond).String(),
		}
		if peer != nil {
			if who := peer(r); who != "" {
				attrs = append(attrs, "peer", who)
			}
		}
		log.Info("request", attrs...)
	})
}

type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (r *recorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wrote = true
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}
