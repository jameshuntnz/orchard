package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jameshuntnz/orchard/internal/ipa"
	"github.com/jameshuntnz/orchard/internal/store"
)

// maxMetadataBytes caps the JSON part. The IPA streams to disk; only this is held in memory.
const maxMetadataBytes = 64 << 10

// uploadMeta is the metadata part of a publish (DESIGN §6).
type uploadMeta struct {
	Branch      string `json:"branch"`
	Commit      string `json:"commit"`
	Version     string `json:"version"`
	BuildNumber string `json:"buildNumber"`
	BundleID    string `json:"bundleId"`
	Title       string `json:"title"`
	Notes       string `json:"notes"`
	RunURL      string `json:"runUrl"`
}

func (m uploadMeta) validate() error {
	switch {
	case m.Branch == "":
		return errors.New("branch is required")
	case m.Version == "":
		return errors.New("version is required")
	case m.BundleID == "":
		return errors.New("bundleId is required")
	case m.Title == "":
		return errors.New("title is required")
	}
	return nil
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	app, slug, ok := idents(w, r, true)
	if !ok {
		return
	}

	// An unbounded multipart read on a shared host is a trivial disk-fill (DESIGN §15).
	r.Body = http.MaxBytesReader(w, r.Body, s.MaxUpload)

	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_metadata", "expected multipart/form-data with ipa and metadata parts")
		return
	}

	staging, err := s.Store.BeginPublish(app, slug)
	if err != nil {
		s.internal(w, "could not stage build", err)
		return
	}
	defer staging.Abort()

	var (
		meta    uploadMeta
		gotMeta bool
		gotIPA  bool
		ipaSize int64
	)

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.publishReadErr(w, err)
			return
		}

		// Parts are accepted in either order; validation happens once both have arrived.
		switch part.FormName() {
		case "metadata":
			raw, err := io.ReadAll(io.LimitReader(part, maxMetadataBytes+1))
			part.Close()
			if err != nil {
				s.publishReadErr(w, err)
				return
			}
			if len(raw) > maxMetadataBytes {
				writeErr(w, http.StatusBadRequest, "invalid_metadata", "metadata part is too large")
				return
			}
			if err := json.Unmarshal(raw, &meta); err != nil {
				writeErr(w, http.StatusBadRequest, "invalid_metadata", "metadata is not valid JSON: "+err.Error())
				return
			}
			gotMeta = true

		case "ipa":
			n, err := writePart(part, staging.IPAPath())
			part.Close()
			if err != nil {
				s.publishReadErr(w, err)
				return
			}
			gotIPA, ipaSize = true, n

		default:
			_, _ = io.Copy(io.Discard, part)
			part.Close()
		}
	}

	if !gotMeta || !gotIPA {
		writeErr(w, http.StatusBadRequest, "invalid_metadata", "both an ipa part and a metadata part are required")
		return
	}
	if err := meta.validate(); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_metadata", err.Error())
		return
	}

	// Reading Info.plist out of the archive turns a confusing device-side failure into a
	// clear CI failure. It is the one piece of real validation the service performs
	// (DESIGN §8).
	info, err := ipa.Inspect(staging.IPAPath())
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_metadata", "could not read the uploaded ipa: "+err.Error())
		return
	}
	if info.BundleID != meta.BundleID {
		writeErr(w, http.StatusConflict, "bundle_id_mismatch",
			"the ipa contains bundle identifier "+info.BundleID+" but the metadata says "+meta.BundleID)
		return
	}

	published := time.Now().UTC().Truncate(time.Second)
	err = staging.Commit(store.Meta{
		Branch:      meta.Branch,
		Commit:      meta.Commit,
		Version:     meta.Version,
		BuildNumber: meta.BuildNumber,
		BundleID:    meta.BundleID,
		Title:       meta.Title,
		Notes:       meta.Notes,
		RunURL:      meta.RunURL,
		PublishedAt: published,
		IPASize:     ipaSize,
	})
	if err != nil {
		s.internal(w, "could not publish build", err)
		return
	}

	s.Log.Info("published build", "app", app, "slug", slug, "branch", meta.Branch,
		"bundleId", meta.BundleID, "version", meta.Version, "bytes", ipaSize)

	writeJSON(w, http.StatusOK, map[string]any{
		"url":         s.pageURL(app, slug),
		"app":         app,
		"slug":        slug,
		"publishedAt": published,
	})
}

// publishReadErr distinguishes a client that sent too much from everything else.
func (s *Server) publishReadErr(w http.ResponseWriter, err error) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) || strings.Contains(err.Error(), "http: request body too large") {
		writeErr(w, http.StatusRequestEntityTooLarge, "payload_too_large", "the upload exceeds the configured size cap")
		return
	}
	writeErr(w, http.StatusBadRequest, "invalid_metadata", "could not read the upload: "+err.Error())
}

func writePart(part *multipart.Part, path string) (int64, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n, err := io.Copy(f, part)
	if err != nil {
		return 0, err
	}
	return n, f.Close()
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	app, slug, ok := idents(w, r, true)
	if !ok {
		return
	}
	switch err := s.Store.Delete(app, slug); {
	case err == nil:
		s.Log.Info("deleted build", "app", app, "slug", slug)
		writeJSON(w, http.StatusOK, map[string]any{"app": app, "slug": slug, "deleted": true})
	case errors.Is(err, store.ErrNotFound):
		notFound(w)
	default:
		s.internal(w, "could not delete build", err)
	}
}

func (s *Server) handleSweep(w http.ResponseWriter, r *http.Request) {
	app, _, ok := idents(w, r, false)
	if !ok {
		return
	}

	var body struct {
		Keep []string `json:"keep"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxMetadataBytes)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_metadata", "body must be JSON with a keep array")
		return
	}
	// Almost certainly a caller bug rather than a genuine instruction to delete every
	// build in the app (DESIGN §6).
	if len(body.Keep) == 0 {
		writeErr(w, http.StatusBadRequest, "empty_keep", "keep must list at least one slug")
		return
	}
	// A malformed slug in keep means the caller's publish and cleanup paths disagree
	// about which directory belongs to which branch (DESIGN §11). Fail loudly.
	for _, k := range body.Keep {
		if !store.ValidIdent(k) {
			writeErr(w, http.StatusBadRequest, "invalid_slug", "keep contains "+k+", which does not match ^[a-z0-9][a-z0-9-]*$")
			return
		}
	}

	removed, kept, err := s.Store.Sweep(app, body.Keep)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			notFound(w)
			return
		}
		s.internal(w, "could not sweep app", err)
		return
	}
	if len(removed) > 0 {
		s.Log.Info("swept app", "app", app, "removed", removed, "kept", kept)
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed, "kept": kept})
}
