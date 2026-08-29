package api

import (
	"bytes"
	"errors"
	"net/http"
	"time"

	"github.com/jameshuntnz/orchard/internal/manifest"
	"github.com/jameshuntnz/orchard/internal/store"
	"github.com/jameshuntnz/orchard/internal/web"
)

func (s *Server) pageURL(app, slug string) string { return s.BaseURL + "/a/" + app + "/b/" + slug }
func (s *Server) ipaURL(app, slug string) string  { return s.pageURL(app, slug) + "/app.ipa" }
func (s *Server) manifestURL(a, sl string) string { return s.pageURL(a, sl) + "/manifest.plist" }

// ---------------------------------------------------------------- machine-readable

type buildJSON struct {
	Slug        string    `json:"slug"`
	Branch      string    `json:"branch"`
	Commit      string    `json:"commit"`
	Version     string    `json:"version"`
	BuildNumber string    `json:"buildNumber"`
	BundleID    string    `json:"bundleId"`
	PublishedAt time.Time `json:"publishedAt"`
	IPASize     int64     `json:"ipaSize"`
	URL         string    `json:"url"`
}

type appJSON struct {
	App    string      `json:"app"`
	Title  string      `json:"title"`
	Builds []buildJSON `json:"builds"`
}

// handleListApps is the endpoint a consumer uses to reconcile state without scraping HTML.
// Builds are newest first within an app; apps are ordered by their most recent build
// (DESIGN §6). The wire shape is declared here rather than marshalled from the storage
// structs, so the v1 contract cannot drift as a side effect of a storage change.
func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	apps, err := s.Store.ListApps()
	if err != nil {
		s.internal(w, "could not list apps", err)
		return
	}

	out := make([]appJSON, 0, len(apps))
	for _, a := range apps {
		builds := make([]buildJSON, 0, len(a.Builds))
		for _, b := range a.Builds {
			builds = append(builds, buildJSON{
				Slug:        b.Slug,
				Branch:      b.Branch,
				Commit:      b.Commit,
				Version:     b.Version,
				BuildNumber: b.BuildNumber,
				BundleID:    b.BundleID,
				PublishedAt: b.PublishedAt,
				IPASize:     b.IPASize,
				URL:         s.pageURL(a.ID, b.Slug),
			})
		}
		out = append(out, appJSON{App: a.ID, Title: a.Title, Builds: builds})
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": out})
}

// ---------------------------------------------------------------- pages

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	apps, err := s.Store.ListApps()
	if err != nil {
		s.internal(w, "could not list apps", err)
		return
	}
	s.renderPage(w, func(b *bytes.Buffer) error {
		return s.Web.Root(b, web.RootData{Apps: apps})
	})
}

func (s *Server) handleApp(w http.ResponseWriter, r *http.Request) {
	app, _, ok := idents(w, r, false)
	if !ok {
		return
	}
	builds, err := s.Store.ListBuilds(app)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			notFound(w)
			return
		}
		s.internal(w, "could not list builds", err)
		return
	}
	if len(builds) == 0 {
		notFound(w)
		return
	}
	a := store.App{ID: app, Title: builds[0].Title, Builds: builds}
	s.renderPage(w, func(b *bytes.Buffer) error {
		return s.Web.App(b, web.AppData{App: a})
	})
}

func (s *Server) handleInstall(w http.ResponseWriter, r *http.Request) {
	app, slug, build, ok := s.lookup(w, r)
	if !ok {
		return
	}
	d := web.InstallData{
		App:         app,
		Build:       build,
		PageURL:     s.pageURL(app, slug),
		IPAURL:      s.ipaURL(app, slug),
		ManifestURL: s.manifestURL(app, slug),
	}
	s.renderPage(w, func(b *bytes.Buffer) error {
		return s.Web.Install(b, d)
	})
}

func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	app, slug, build, ok := s.lookup(w, r)
	if !ok {
		return
	}
	// bundle-identifier is the one read out of the archive at publish time, so it cannot
	// disagree with what is actually installed (DESIGN §8).
	out, err := manifest.Render(build.BundleID, build.Version, build.Title, s.ipaURL(app, slug))
	if err != nil {
		s.internal(w, "could not render manifest", err)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(out)
}

func (s *Server) handleIPA(w http.ResponseWriter, r *http.Request) {
	app, slug, _, ok := s.lookup(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	// ServeFile handles the range requests iOS's install daemon makes.
	http.ServeFile(w, r, s.Store.IPAPath(app, slug))
}

// lookup validates the identifiers and loads the build, writing the error response itself.
func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (app, slug string, b store.Build, ok bool) {
	app, slug, ok = idents(w, r, true)
	if !ok {
		return "", "", store.Build{}, false
	}
	b, err := s.Store.Get(app, slug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			notFound(w)
		} else {
			s.internal(w, "could not read build", err)
		}
		return "", "", store.Build{}, false
	}
	return app, slug, b, true
}

// renderPage buffers so a template failure produces a 500 rather than a truncated page.
func (s *Server) renderPage(w http.ResponseWriter, render func(*bytes.Buffer) error) {
	var buf bytes.Buffer
	if err := render(&buf); err != nil {
		s.internal(w, "could not render page", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}
