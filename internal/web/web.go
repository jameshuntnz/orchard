// Package web renders Orchard's three human pages: an index of apps, an index of one
// app's branches, and the install page (DESIGN §9).
//
// Everything is rendered on read from these templates, so a template change takes effect
// without republishing anything (DESIGN §7).
package web

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"time"

	"github.com/jameshuntnz/orchard/internal/manifest"
	"github.com/jameshuntnz/orchard/internal/store"
)

//go:embed templates/*.html
var files embed.FS

// pages each combine layout.html with their own "content" block.
var pages = []string{"root", "app", "install"}

type Renderer struct {
	t map[string]*template.Template
}

func New() (*Renderer, error) {
	r := &Renderer{t: make(map[string]*template.Template, len(pages))}
	for _, name := range pages {
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(files,
			"templates/layout.html", "templates/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		r.t[name] = t
	}
	return r, nil
}

type RootData struct {
	Apps []store.App
}

type AppData struct {
	App store.App
}

type InstallData struct {
	App         string
	Build       store.Build
	PageURL     string
	IPAURL      string
	ManifestURL string
}

func (r *Renderer) Root(w io.Writer, d RootData) error       { return r.t["root"].Execute(w, d) }
func (r *Renderer) App(w io.Writer, d AppData) error         { return r.t["app"].Execute(w, d) }
func (r *Renderer) Install(w io.Writer, d InstallData) error { return r.t["install"].Execute(w, d) }

var funcs = template.FuncMap{
	// installURL is returned as template.URL deliberately: html/template only trusts
	// http, https and mailto in an href and would otherwise replace the itms-services
	// scheme with #ZgotmplZ, leaving an install button that does nothing.
	"installURL": func(manifestURL string) template.URL {
		return template.URL(manifest.InstallURL(manifestURL))
	},
	"qr":    qrSVG,
	"since": since,
	"bytes": humanBytes,
}

// since renders an approximate age. The install page is read on a phone, where "2 hours
// ago" is more useful than a timestamp in an unknown timezone.
func since(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute")
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour")
	case d < 30*24*time.Hour:
		return plural(int(d.Hours()/24), "day")
	default:
		return t.UTC().Format("2 Jan 2006")
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit + " ago"
	}
	return fmt.Sprintf("%d %ss ago", n, unit)
}

func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGT"[exp])
}
