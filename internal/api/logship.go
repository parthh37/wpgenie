package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/parthh37/wpgenie/internal/auth"
	"github.com/parthh37/wpgenie/internal/cluster"
	"github.com/parthh37/wpgenie/internal/logship"
	"github.com/parthh37/wpgenie/internal/store"
)

// Log shipping to S3-compatible storage (see internal/logship). Staff
// only: logs name visitors, clients and staff. The secret key is
// write-only. On a cluster every server ships its own logs with the
// panel's settings: saving them here sends them to every server (and a
// server that comes back, or is added, gets them; see ConfigureNodeLogs).

func init() {
	registerErrorStatus(logship.ErrInvalid, http.StatusBadRequest)
	registerErrorStatus(logship.ErrNotFound, http.StatusNotFound)
	registerErrorStatus(logship.ErrStorage, http.StatusBadGateway)
}

// logshipRoutes registers this feature's routes (mux: its public, unsigned
// endpoints: none).
func (s *Server) logshipRoutes(mux *http.ServeMux, r func(string, string, handlerFunc)) {
	if s.Logship == nil {
		return
	}
	r("GET /api/v1/logs/status", viewer, s.logStatus)
	r("GET /api/v1/logs/settings", admin, s.logSettings)
	r("PUT /api/v1/logs/settings", admin, s.setLogSettings)
	r("POST /api/v1/logs/test", admin, s.testLogDestination)
	r("GET /api/v1/logs/archives", admin, s.logArchives)
	r("GET /api/v1/logs/archives/object", admin, s.logArchiveObject)
}

// logSettingsView is the settings without the secret, with what the form
// needs to know about this server.
type logSettingsView struct {
	logship.Settings
	Server    string          `json:"server"`
	Available map[string]bool `json:"available"`
	Catalog   []logship.Type  `json:"catalog"`
	Providers []string        `json:"providers"`
}

func (s *Server) logSettingsView(set logship.Settings) logSettingsView {
	v := logSettingsView{Settings: set.Redacted(), Server: s.Logship.ServerName(), Available: map[string]bool{},
		Catalog: logship.Types, Providers: logship.Providers}
	for _, t := range logship.Types {
		v.Available[t.Name] = s.Logship.Available(t.Name)
	}
	return v
}

func (s *Server) logSettings(w http.ResponseWriter, r *http.Request) error {
	set, err := s.Logship.Settings(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, s.logSettingsView(set))
}

// setLogSettings saves the settings and, on the panel of a cluster, sends
// them (the secret included, over the cluster's mutual TLS) to every
// server.
func (s *Server) setLogSettings(w http.ResponseWriter, r *http.Request) error {
	var in logship.Settings
	if err := decode(w, r, &in); err != nil {
		return err
	}
	set, err := s.Logship.SetSettings(r.Context(), in)
	if err != nil {
		return err
	}
	if s.Cluster != nil && !s.Node {
		s.broadcast(r, http.MethodPut, "/api/v1/logs/settings", set)
	}
	return writeJSON(w, http.StatusOK, s.logSettingsView(set))
}

// ConfigureNodeLogs sends a server the panel's log shipping settings
// (called with ConfigureNode: after pairing, and when a server is back).
func (s *Server) ConfigureNodeLogs(ctx context.Context, n *store.Node) error {
	if s.Logship == nil {
		return nil
	}
	set, err := s.Logship.Settings(ctx)
	if err != nil {
		return err
	}
	panel := cluster.Identity{Name: "panel", Role: auth.RoleAdmin, Owner: "api-token"}
	if err := s.nodeAPI(ctx, n.ID, panel, http.MethodPut, "/api/v1/logs/settings", set, nil); err != nil {
		var se *cluster.StatusError
		if errors.As(err, &se) && se.Code == http.StatusNotFound {
			return nil // a server running a version without log shipping
		}
		return fmt.Errorf("log shipping: %w", err)
	}
	return nil
}

// testLogDestination writes and deletes a small object at the destination
// in the form (not saved yet). The storage's refusal is a result, not an
// error: {ok: false, error: "…"}.
func (s *Server) testLogDestination(w http.ResponseWriter, r *http.Request) error {
	var in logship.Destination
	if err := decode(w, r, &in); err != nil {
		return err
	}
	res, err := s.Logship.Test(r.Context(), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, res)
}

// serverLogStatus is another server's status (or why it's missing).
type serverLogStatus struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Status *logship.Status `json:"status,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type logStatusView struct {
	*logship.Status
	// Servers are the cluster's other servers (on the panel).
	Servers []serverLogStatus `json:"servers,omitempty"`
}

// logStatus is this server's shipping status and, on the panel of a
// cluster, every other server's (?local=1: only this one's).
func (s *Server) logStatus(w http.ResponseWriter, r *http.Request) error {
	st, err := s.Logship.Status(r.Context())
	if err != nil {
		return err
	}
	out := logStatusView{Status: st}
	if s.Cluster != nil && !s.Node && r.URL.Query().Get("local") != "1" && s.Cluster.Enabled() {
		nodes, err := s.Store.ListNodes(r.Context())
		if err != nil {
			return err
		}
		names := map[string]string{}
		for _, n := range nodes {
			names[n.ID] = n.Name
		}
		var mu sync.Mutex
		panel := cluster.Identity{Name: "panel", Role: auth.RoleAdmin, Owner: "api-token"}
		errs := s.eachNode(r.Context(), 10*time.Second, func(ctx context.Context, n *store.Node) error {
			var ns logship.Status
			if err := s.nodeAPI(ctx, n.ID, panel, http.MethodGet, "/api/v1/logs/status?local=1", nil, &ns); err != nil {
				return err
			}
			mu.Lock()
			out.Servers = append(out.Servers, serverLogStatus{ID: n.ID, Name: n.Name, Status: &ns})
			mu.Unlock()
			return nil
		})
		for id, err := range errs {
			out.Servers = append(out.Servers, serverLogStatus{ID: id, Name: names[id], Error: err.Error()})
		}
		slices.SortFunc(out.Servers, func(a, b serverLogStatus) int { return strings.Compare(a.Name+a.ID, b.Name+b.ID) })
	}
	return writeJSON(w, http.StatusOK, out)
}

// logArchives lists the archive's objects of a server, type and day
// (?server=&type=&date=YYYY-MM-DD; defaults: this server, today in UTC).
func (s *Server) logArchives(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	server := q.Get("server")
	if server == "" {
		server = s.Logship.ServerName()
	}
	day := s.now().UTC()
	if v := q.Get("date"); v != "" {
		d, err := time.Parse(time.DateOnly, v)
		if err != nil {
			return fmt.Errorf("%w: date must be YYYY-MM-DD", errBadRequest)
		}
		day = d
	}
	typ := q.Get("type")
	if typ == "" {
		typ = logship.TypeAccess
	}
	servers := []string{s.Logship.ServerName()}
	if s.Cluster != nil && !s.Node {
		nodes, err := s.Store.ListNodes(r.Context())
		if err != nil {
			return err
		}
		for _, n := range nodes {
			servers = append(servers, n.ID)
		}
	}
	objs, err := s.Logship.Archives(r.Context(), server, typ, day)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"server": server, "type": typ, "date": day.Format(time.DateOnly),
		"servers": servers, "objects": objs})
}

// maxArchiveView bounds what one archive shows in the panel.
const maxArchiveView = 20 << 20

var safeFileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,200}$`)

// logArchiveObject returns one object of the archive (?key=, validated
// against the archive's prefix), decompressed, as text (?download=1: as a
// file). At most 20 MB are shown. A zstd object downloads as it is.
func (s *Server) logArchiveObject(w http.ResponseWriter, r *http.Request) error {
	ar, err := s.Logship.OpenArchive(r.Context(), r.URL.Query().Get("key"))
	if err != nil {
		return err
	}
	defer ar.Close()
	name := ar.Name
	if !safeFileName.MatchString(name) {
		name = "archive.log"
	}
	h := w.Header()
	// What the logs hold (a visitor's request line…) is never markup here.
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cache-Control", "no-store")
	if ar.Compressed {
		h.Set("Content-Type", "application/zstd")
		h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
		_, err := io.Copy(w, ar)
		s.logCopyErr(r, err)
		return nil
	}
	h.Set("Content-Type", "text/plain; charset=utf-8")
	if r.URL.Query().Get("download") == "1" {
		h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	}
	n, err := io.Copy(w, io.LimitReader(ar, maxArchiveView))
	if err != nil {
		s.logCopyErr(r, err)
		return nil
	}
	if n == maxArchiveView {
		var one [1]byte
		if k, _ := ar.Read(one[:]); k > 0 {
			fmt.Fprintf(w, "\n… cut at %d MB: the rest is in the bucket (%s).\n", maxArchiveView>>20, r.URL.Query().Get("key"))
		}
	}
	return nil
}

// logCopyErr: once the body has started, an error can't be reported
// (the client went away, or the object is corrupt): it's only logged.
func (s *Server) logCopyErr(r *http.Request, err error) {
	if err != nil && r.Context().Err() == nil {
		s.Log.Warn("sending a log archive", "key", r.URL.Query().Get("key"), "err", err)
	}
}
