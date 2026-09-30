package api

import (
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/parthh37/wpgenie/internal/files"
)

// The file manager (internal/files). Paths travel in the query (?path=,
// ?to=), so the audit log, which records the query, says which files every
// change touched.

func (s *Server) fileRoutes(r func(pattern, role string, h handlerFunc)) {
	r("GET /api/v1/sites/{id}/files", viewer, s.listFiles)
	r("GET /api/v1/sites/{id}/files/content", viewer, s.readFile)
	r("GET /api/v1/sites/{id}/files/download", viewer, s.downloadFile)
	r("PUT /api/v1/sites/{id}/files/content", operator, s.writeFile)
	r("POST /api/v1/sites/{id}/files/folder", operator, s.makeFolder)
	r("POST /api/v1/sites/{id}/files/move", operator, s.moveFile)
	r("POST /api/v1/sites/{id}/files/copy", operator, s.copyFile)
	r("PUT /api/v1/sites/{id}/files/mode", operator, s.chmodFile)
	r("POST /api/v1/sites/{id}/files/extract", operator, s.extractFile)
	r("DELETE /api/v1/sites/{id}/files", operator, s.deleteFile)
}

func (s *Server) files() (*files.Service, error) {
	if s.Files == nil {
		return nil, fmt.Errorf("%w: the file manager isn't available on this server", errBadRequest)
	}
	return s.Files, nil
}

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) error {
	fm, err := s.files()
	if err != nil {
		return err
	}
	l, err := fm.List(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, l)
}

func (s *Server) readFile(w http.ResponseWriter, r *http.Request) error {
	fm, err := s.files()
	if err != nil {
		return err
	}
	t, err := fm.Read(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, t)
}

// writeFile stores the request body, as is, in a file: an upload, a save
// from the editor (?version= as it was read), a new empty file.
// ?overwrite=1 replaces an existing file.
func (s *Server) writeFile(w http.ResponseWriter, r *http.Request) error {
	fm, err := s.files()
	if err != nil {
		return err
	}
	// A large upload on a slow line outlasts the server's write timeout,
	// which counts from the request's headers.
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(2 * time.Hour))
	rc.SetWriteDeadline(time.Now().Add(2 * time.Hour))
	q := r.URL.Query()
	e, err := fm.Write(r.Context(), r.PathValue("id"), q.Get("path"), r.Body,
		files.WriteOptions{Overwrite: q.Get("overwrite") == "1", Version: q.Get("version")})
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, e)
}

func (s *Server) makeFolder(w http.ResponseWriter, r *http.Request) error {
	fm, err := s.files()
	if err != nil {
		return err
	}
	if err := fm.Mkdir(r.Context(), r.PathValue("id"), r.URL.Query().Get("path")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) moveFile(w http.ResponseWriter, r *http.Request) error {
	fm, err := s.files()
	if err != nil {
		return err
	}
	q := r.URL.Query()
	if err := fm.Move(r.Context(), r.PathValue("id"), q.Get("path"), q.Get("to")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) copyFile(w http.ResponseWriter, r *http.Request) error {
	fm, err := s.files()
	if err != nil {
		return err
	}
	q := r.URL.Query()
	res, err := fm.Copy(r.Context(), r.PathValue("id"), q.Get("path"), q.Get("to"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, res)
}

func (s *Server) chmodFile(w http.ResponseWriter, r *http.Request) error {
	fm, err := s.files()
	if err != nil {
		return err
	}
	q := r.URL.Query()
	m, err := strconv.ParseUint(q.Get("mode"), 8, 32)
	if err != nil {
		return fmt.Errorf("%w: permissions are 3 octal digits, like 644", errBadRequest)
	}
	if err := fm.Chmod(r.Context(), r.PathValue("id"), q.Get("path"), fs.FileMode(m)); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// extractFile unpacks a zip archive into ?to= (by default, the folder it's
// in).
func (s *Server) extractFile(w http.ResponseWriter, r *http.Request) error {
	fm, err := s.files()
	if err != nil {
		return err
	}
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Hour))
	q := r.URL.Query()
	to := q.Get("to")
	if !q.Has("to") {
		to = path.Dir(path.Clean("/" + q.Get("path")))
	}
	res, err := fm.Extract(r.Context(), r.PathValue("id"), q.Get("path"), to, q.Get("overwrite") == "1")
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, res)
}

func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request) error {
	fm, err := s.files()
	if err != nil {
		return err
	}
	if err := fm.Delete(r.Context(), r.PathValue("id"), r.URL.Query().Get("path")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// previewTypes are the images the dashboard may show inline (?inline=1).
// Never SVG, HTML or anything else a browser would run script in.
var previewTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".webp": "image/webp", ".avif": "image/avif", ".ico": "image/x-icon", ".bmp": "image/bmp",
}

// downloadFile sends a file, or a folder as a zip archive.
//
// The files are the site's, anyone's who could upload to it, and they're
// served from the panel's origin: an HTML or SVG file shown as a page here
// would run its script as the signed-in user (staff looking at a tenant's
// site, say). So a file is always an attachment of an opaque type, the
// browser mustn't guess another (nosniff), and the response carries a
// sandbox CSP in case anything renders it anyway. Only raster images are
// shown inline, as images.
func (s *Server) downloadFile(w http.ResponseWriter, r *http.Request) error {
	fm, err := s.files()
	if err != nil {
		return err
	}
	ctx, id, p := r.Context(), r.PathValue("id"), r.URL.Query().Get("path")
	dir, err := fm.IsDir(ctx, id, p)
	if err != nil {
		return err
	}
	hdr := w.Header()
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	hdr.Set("Cache-Control", "private, no-store")
	name := path.Base(path.Clean("/" + p))
	if dir {
		if name == "/" {
			name = id
		}
		http.NewResponseController(w).SetWriteDeadline(time.Now().Add(12 * time.Hour))
		hdr.Set("Content-Type", "application/zip")
		hdr.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name + ".zip"}))
		// Streamed as it's made: once it has started, a failure can only cut
		// the archive short (the client sees a truncated download).
		if err := fm.Zip(ctx, id, p, w); err != nil {
			s.Log.Warn("files: zipping a folder", "site", id, "path", p, "err", err)
		}
		return nil
	}
	f, fi, err := fm.Download(ctx, id, p)
	if err != nil {
		return err
	}
	defer f.Close()
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(12 * time.Hour))
	disposition, ctype := "attachment", "application/octet-stream"
	if t, ok := previewTypes[strings.ToLower(path.Ext(name))]; ok && r.URL.Query().Get("inline") == "1" {
		disposition, ctype = "inline", t
	}
	hdr.Set("Content-Type", ctype)
	hdr.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": name}))
	http.ServeContent(w, r, "", fi.ModTime(), f)
	return nil
}
