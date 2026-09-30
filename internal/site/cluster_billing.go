package site

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/parthh37/wpgenie/internal/cluster"
	"github.com/parthh37/wpgenie/internal/store"
)

// ClusterOps is what billing does to sites (suspend, unsuspend, delete,
// measure, count bandwidth), sent to the server each site lives on: a
// tenant's plan applies to all of its sites, wherever they run.
type ClusterOps struct {
	S *Service
}

func (c ClusterOps) node(ctx context.Context, id string) (string, bool, error) {
	if c.S.Cluster == nil {
		return "", false, nil
	}
	return c.S.Cluster.SiteNode(ctx, id)
}

func (c ClusterOps) Suspend(ctx context.Context, id string) error {
	return c.op(ctx, id, "suspend", c.S.Suspend)
}

func (c ClusterOps) Unsuspend(ctx context.Context, id string) error {
	return c.op(ctx, id, "unsuspend", c.S.Unsuspend)
}

func (c ClusterOps) op(ctx context.Context, id, verb string, local func(context.Context, string) error) error {
	node, remote, err := c.node(ctx, id)
	if err != nil {
		return err
	}
	if !remote {
		return local(ctx, id)
	}
	if err := c.S.Cluster.Call(ctx, node, http.MethodPost, "/cluster/v1/sites/"+id+"/"+verb, nil, nil); err != nil {
		return err
	}
	return c.S.Cluster.RefreshSite(ctx, node, id)
}

// Delete deletes a site wherever it lives (an account terminated).
func (c ClusterOps) Delete(ctx context.Context, id string) error {
	node, remote, err := c.node(ctx, id)
	if err != nil {
		return err
	}
	if !remote {
		return c.S.Delete(ctx, id)
	}
	st, _ := c.S.Store.ClusterSite(ctx, id)
	err = c.S.Cluster.Call(ctx, node, http.MethodDelete, "/cluster/v1/sites/"+id, nil, nil)
	var se *cluster.StatusError
	if err != nil && !(errors.As(err, &se) && se.Code == http.StatusNotFound) {
		return err
	}
	if st != nil && st.Site.SMTP && c.S.Mailer != nil {
		c.S.Mailer.RemoveSender(ctx, id, st.PrimaryDomain)
	}
	return c.S.Store.DeleteClusterSite(ctx, id)
}

// MeasureDisk measures a site's files and database where they are.
func (c ClusterOps) MeasureDisk(ctx context.Context, id string) (store.SiteUsage, error) {
	node, remote, err := c.node(ctx, id)
	if err != nil || !remote {
		if err != nil {
			return store.SiteUsage{}, err
		}
		return c.S.MeasureDisk(ctx, id)
	}
	var u store.SiteUsage
	err = c.S.Cluster.Call(ctx, node, http.MethodGet, "/cluster/v1/sites/"+id+"/disk", nil, &u)
	u.SiteID = id
	return u, err
}

// Bandwidth adds up what each server served for the sites it holds.
func (c ClusterOps) Bandwidth(ctx context.Context, ids []string, from, to time.Time) (map[string]int64, error) {
	byNode := map[string][]string{}
	var local []string
	for _, id := range ids {
		node, remote, err := c.node(ctx, id)
		if err != nil {
			return nil, err
		}
		if remote {
			byNode[node] = append(byNode[node], id)
		} else {
			local = append(local, id)
		}
	}
	out, err := c.S.Store.Bandwidth(ctx, local, from, to)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]int64{}
	}
	for node, list := range byNode {
		q := url.Values{"from": {strconv.FormatInt(from.Unix(), 10)}, "to": {strconv.FormatInt(to.Unix(), 10)},
			"sites": {strings.Join(list, ",")}}
		var got map[string]int64
		if err := c.S.Cluster.Call(ctx, node, http.MethodGet, "/cluster/v1/bandwidth?"+q.Encode(), nil, &got); err != nil {
			// A node that can't answer now is counted on the next pass; its
			// sites' usage isn't taken as zero.
			return nil, err
		}
		for k, v := range got {
			out[k] += v
		}
	}
	return out, nil
}

// Disk is the last measurement of each site (measured where it lives,
// recorded here).
func (c ClusterOps) Disk(ctx context.Context, ids []string) (map[string]store.SiteUsage, error) {
	return c.S.Store.SiteUsages(ctx, ids)
}

// BurstMinutes returns every site's burst minutes of a month, wherever
// they were counted: each other server reports the minutes sites ran
// there, kept here per server (a site that moved has several), and a
// server that can't answer now is counted from its own record next time.
func (c ClusterOps) BurstMinutes(ctx context.Context, month time.Time) (map[string]int64, error) {
	if c.S.Cluster != nil {
		nodes, err := c.S.Store.ListNodes(ctx)
		if err != nil {
			return nil, err
		}
		for _, n := range nodes {
			var got map[string]int64
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.S.Cluster.Call(cctx, n.ID, http.MethodGet,
				"/cluster/v1/burst?month="+strconv.FormatInt(month.Unix(), 10), nil, &got)
			cancel()
			if err != nil {
				c.S.Log.Debug("burst: a server's minutes", "node", n.ID, "err", err)
				continue
			}
			for id, m := range got {
				if !siteIDRe.MatchString(id) {
					continue
				}
				if err := c.S.Store.SetBurstReport(ctx, id, n.ID, month, m); err != nil {
					return nil, err
				}
			}
		}
	}
	return c.S.Store.BurstMinutes(ctx, month)
}

// BurstPaused lists the sites whose burst is paused, wherever they run
// (other servers' as the registry last saw them).
func (c ClusterOps) BurstPaused(ctx context.Context) (map[string]bool, error) {
	sites, err := c.S.Store.ListSites(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, st := range sites {
		if st.BurstPaused {
			out[st.ID] = true
		}
	}
	if c.S.Cluster != nil {
		reg, err := c.S.Store.ClusterSites(ctx, "")
		if err != nil {
			return nil, err
		}
		for _, cs := range reg {
			if cs.Site != nil && cs.Site.BurstPaused {
				out[cs.SiteID] = true
			}
		}
	}
	return out, nil
}

// SetBurstPaused pauses or resumes a site's burst where it runs.
func (c ClusterOps) SetBurstPaused(ctx context.Context, id string, paused bool) error {
	node, remote, err := c.node(ctx, id)
	if err != nil {
		return err
	}
	if !remote {
		return c.S.SetBurstPaused(ctx, id, paused)
	}
	body := map[string]bool{"paused": paused}
	if err := c.S.Cluster.Call(ctx, node, http.MethodPost, "/cluster/v1/sites/"+id+"/burst-paused", body, nil); err != nil {
		return err
	}
	return c.S.Cluster.RefreshSite(ctx, node, id)
}

// billingRoutes are the node side of ClusterOps.
func (s *Service) billingRoutes(mux *http.ServeMux) {
	for verb, fn := range map[string]func(context.Context, string) error{"suspend": s.Suspend, "unsuspend": s.Unsuspend} {
		mux.HandleFunc("POST /cluster/v1/sites/{id}/"+verb, func(w http.ResponseWriter, r *http.Request) {
			if err := fn(r.Context(), r.PathValue("id")); err != nil {
				clusterError(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
	}
	mux.HandleFunc("DELETE /cluster/v1/sites/{id}", func(w http.ResponseWriter, r *http.Request) {
		// Deleted as a site whose account went: its mail sender lives on the
		// panel, which removes it.
		if err := s.deleteLocal(r.Context(), r.PathValue("id"), false); err != nil {
			clusterError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /cluster/v1/burst", func(w http.ResponseWriter, r *http.Request) {
		month, err := strconv.ParseInt(r.URL.Query().Get("month"), 10, 64)
		if err != nil {
			clusterError(w, ErrInvalidInput)
			return
		}
		got, err := s.Store.LocalBurstMinutes(r.Context(), time.Unix(month, 0))
		if err != nil {
			clusterError(w, err)
			return
		}
		writeClusterJSON(w, http.StatusOK, got)
	})
	mux.HandleFunc("POST /cluster/v1/sites/{id}/burst-paused", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Paused bool `json:"paused"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&in); err != nil {
			clusterError(w, ErrInvalidInput)
			return
		}
		if err := s.SetBurstPaused(r.Context(), r.PathValue("id"), in.Paused); err != nil {
			clusterError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /cluster/v1/sites/{id}/disk", func(w http.ResponseWriter, r *http.Request) {
		u, err := s.MeasureDisk(r.Context(), r.PathValue("id"))
		if err != nil {
			clusterError(w, err)
			return
		}
		writeClusterJSON(w, http.StatusOK, u)
	})
	mux.HandleFunc("GET /cluster/v1/bandwidth", func(w http.ResponseWriter, r *http.Request) {
		from, err1 := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
		to, err2 := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64)
		if err1 != nil || err2 != nil {
			clusterError(w, ErrInvalidInput)
			return
		}
		var ids []string
		for _, id := range strings.Split(r.URL.Query().Get("sites"), ",") {
			if siteIDRe.MatchString(id) {
				ids = append(ids, id)
			}
		}
		got, err := s.Store.Bandwidth(r.Context(), ids, time.Unix(from, 0), time.Unix(to, 0))
		if err != nil {
			clusterError(w, err)
			return
		}
		if got == nil {
			got = map[string]int64{}
		}
		writeClusterJSON(w, http.StatusOK, got)
	})
}
