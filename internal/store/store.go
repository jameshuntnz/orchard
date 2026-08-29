// Package store owns the state directory. The filesystem is the database: no SQL, no
// embedded KV, and nothing derived is ever stored (DESIGN §7).
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

var ErrNotFound = errors.New("not found")

// identRe is the pattern from DESIGN §7. App ids and slugs arrive over the network and
// become path segments, so this is the only thing standing between the API and directory
// traversal — it is validated on every path that touches the filesystem (DESIGN §15).
var identRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// maxIdentLen keeps a legal-but-absurd identifier from producing an unusable path.
const maxIdentLen = 100

// ValidIdent reports whether s may be used as an app id or a build slug.
func ValidIdent(s string) bool {
	return len(s) <= maxIdentLen && identRe.MatchString(s)
}

const (
	appsDir  = "apps"
	buildsIn = "builds"
	tmpDir   = "tmp" // staging and trash, on the same filesystem so renames are atomic

	ipaName  = "app.ipa"
	metaName = "meta.json"
)

type Store struct {
	root  string
	log   *slog.Logger
	locks sync.Map // "app/slug" -> *sync.Mutex
}

func New(root string, log *slog.Logger) *Store {
	return &Store{root: root, log: log}
}

// App is one app and its builds, newest first.
type App struct {
	ID     string  `json:"app"`
	Title  string  `json:"title"`
	Builds []Build `json:"builds"`
}

// LatestAt is the time of the app's most recent build, which orders the root index.
func (a App) LatestAt() time.Time {
	if len(a.Builds) == 0 {
		return time.Time{}
	}
	return a.Builds[0].PublishedAt
}

func (s *Store) appDir(app string) string { return filepath.Join(s.root, appsDir, app) }
func (s *Store) buildDir(app, slug string) string {
	return filepath.Join(s.appDir(app), buildsIn, slug)
}

// IPAPath is where a build's payload lives. The caller must have validated the identifiers.
func (s *Store) IPAPath(app, slug string) string {
	return filepath.Join(s.buildDir(app, slug), ipaName)
}

func (s *Store) lockFor(app, slug string) *sync.Mutex {
	v, _ := s.locks.LoadOrStore(app+"/"+slug, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// ---------------------------------------------------------------- publishing

// Staging is a build being written. The upload streams into it without holding any lock;
// only the swap into place is serialised, so two concurrent publishes to one slug both
// complete and the last to finish wins (DESIGN §4.4).
type Staging struct {
	store *Store
	app   string
	slug  string
	dir   string
}

// BeginPublish creates a staging directory for app/slug. Call Commit or Abort.
func (s *Store) BeginPublish(app, slug string) (*Staging, error) {
	if !ValidIdent(app) || !ValidIdent(slug) {
		return nil, fmt.Errorf("invalid identifier")
	}
	if err := os.MkdirAll(filepath.Join(s.root, tmpDir), 0o755); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(filepath.Join(s.root, tmpDir), "stage-")
	if err != nil {
		return nil, err
	}
	return &Staging{store: s, app: app, slug: slug, dir: dir}, nil
}

// IPAPath is where the caller should stream the uploaded payload.
func (st *Staging) IPAPath() string { return filepath.Join(st.dir, ipaName) }

// Abort discards a staged build. It is safe to call after Commit.
func (st *Staging) Abort() {
	if st.dir == "" {
		return
	}
	_ = os.RemoveAll(st.dir)
	st.dir = ""
}

// Commit writes meta.json and swaps the staged directory into place atomically. A reader
// sees one build or the other, never a mixture (DESIGN §6).
func (st *Staging) Commit(m Meta) error {
	m.SchemaVersion = CurrentSchema
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(st.dir, metaName), append(raw, '\n'), 0o644); err != nil {
		return err
	}

	s := st.store
	target := s.buildDir(st.app, st.slug)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}

	mu := s.lockFor(st.app, st.slug)
	mu.Lock()

	var trash string
	if _, err := os.Stat(target); err == nil {
		trash, err = os.MkdirTemp(filepath.Join(s.root, tmpDir), "trash-")
		if err != nil {
			mu.Unlock()
			return err
		}
		// MkdirTemp made the directory; rename needs the name to be free.
		if err := os.Remove(trash); err != nil {
			mu.Unlock()
			return err
		}
		if err := os.Rename(target, trash); err != nil {
			mu.Unlock()
			return fmt.Errorf("displace existing build: %w", err)
		}
	}

	if err := os.Rename(st.dir, target); err != nil {
		if trash != "" {
			// Put the previous build back rather than leaving the slug empty.
			_ = os.Rename(trash, target)
		}
		mu.Unlock()
		return fmt.Errorf("install build: %w", err)
	}
	mu.Unlock()

	st.dir = "" // ownership has moved; Abort must not remove the published build
	if trash != "" {
		_ = os.RemoveAll(trash)
	}
	return nil
}

// ---------------------------------------------------------------- reading

// Get returns one build.
func (s *Store) Get(app, slug string) (Build, error) {
	if !ValidIdent(app) || !ValidIdent(slug) {
		return Build{}, ErrNotFound
	}
	raw, err := os.ReadFile(filepath.Join(s.buildDir(app, slug), metaName))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Build{}, ErrNotFound
		}
		return Build{}, err
	}
	m, err := decodeMeta(raw)
	if err != nil {
		return Build{}, err
	}
	return Build{Meta: m, App: app, Slug: slug}, nil
}

// ListBuilds returns an app's builds, newest first. A build directory that cannot be read
// is skipped and logged; one malformed directory never takes down an index (DESIGN §14.2).
func (s *Store) ListBuilds(app string) ([]Build, error) {
	if !ValidIdent(app) {
		return nil, ErrNotFound
	}
	entries, err := os.ReadDir(filepath.Join(s.appDir(app), buildsIn))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var builds []Build
	for _, e := range entries {
		if !e.IsDir() || !ValidIdent(e.Name()) {
			continue
		}
		b, err := s.Get(app, e.Name())
		if err != nil {
			s.log.Warn("skipping unreadable build", "app", app, "slug", e.Name(), "err", err)
			continue
		}
		builds = append(builds, b)
	}
	sort.Slice(builds, func(i, j int) bool {
		return builds[i].PublishedAt.After(builds[j].PublishedAt)
	})
	return builds, nil
}

// ListApps returns every app with at least one build, ordered by most recent build.
// An app is created implicitly on first publish and does not exist without one (DESIGN §5).
func (s *Store) ListApps() ([]App, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, appsDir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var apps []App
	for _, e := range entries {
		if !e.IsDir() || !ValidIdent(e.Name()) {
			continue
		}
		builds, err := s.ListBuilds(e.Name())
		if err != nil {
			s.log.Warn("skipping unreadable app", "app", e.Name(), "err", err)
			continue
		}
		if len(builds) == 0 {
			continue
		}
		// The display title comes from the most recent build, so it stays current
		// without a separate record to maintain (DESIGN §5).
		apps = append(apps, App{ID: e.Name(), Title: builds[0].Title, Builds: builds})
	}
	sort.Slice(apps, func(i, j int) bool {
		return apps[i].LatestAt().After(apps[j].LatestAt())
	})
	return apps, nil
}

// ---------------------------------------------------------------- removal

// Delete removes one branch's build.
func (s *Store) Delete(app, slug string) error {
	if !ValidIdent(app) || !ValidIdent(slug) {
		return ErrNotFound
	}
	mu := s.lockFor(app, slug)
	mu.Lock()
	defer mu.Unlock()

	dir := s.buildDir(app, slug)
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrNotFound
		}
		return err
	}
	return os.RemoveAll(dir)
}

// Sweep removes every build in this app whose slug is absent from keep. Apps are fully
// isolated: a sweep for one app can never touch another's builds (DESIGN §5).
func (s *Store) Sweep(app string, keep []string) (removed []string, kept int, err error) {
	if !ValidIdent(app) {
		return nil, 0, ErrNotFound
	}
	set := make(map[string]bool, len(keep))
	for _, k := range keep {
		set[k] = true
	}

	builds, err := s.ListBuilds(app)
	if err != nil {
		return nil, 0, err
	}
	removed = []string{}
	for _, b := range builds {
		if set[b.Slug] {
			kept++
			continue
		}
		if err := s.Delete(app, b.Slug); err != nil && !errors.Is(err, ErrNotFound) {
			return nil, 0, err
		}
		removed = append(removed, b.Slug)
	}
	sort.Strings(removed)
	return removed, kept, nil
}

// SweepAge drops builds older than max, across every app. This is the backstop for a
// consumer that stops calling in, and it logs loudly when it acts (DESIGN §11).
func (s *Store) SweepAge(max time.Duration, now time.Time) int {
	if max <= 0 {
		return 0
	}
	apps, err := s.ListApps()
	if err != nil {
		s.log.Error("age sweep could not list apps", "err", err)
		return 0
	}
	n := 0
	for _, a := range apps {
		for _, b := range a.Builds {
			age := now.Sub(b.PublishedAt)
			if age <= max {
				continue
			}
			if err := s.Delete(a.ID, b.Slug); err != nil {
				s.log.Error("age sweep could not delete build", "app", a.ID, "slug", b.Slug, "err", err)
				continue
			}
			s.log.Warn("age sweep removed a build",
				"app", a.ID, "slug", b.Slug, "branch", b.Branch,
				"ageDays", int(age.Hours()/24), "maxAgeDays", int(max.Hours()/24))
			n++
		}
	}
	return n
}

// CleanTmp removes staging and trash left behind by a process that died mid-publish.
func (s *Store) CleanTmp() {
	entries, err := os.ReadDir(filepath.Join(s.root, tmpDir))
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(s.root, tmpDir, e.Name()))
	}
}
