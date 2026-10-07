package api

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/parthh37/wpgenie/internal/billing"
	"github.com/parthh37/wpgenie/internal/cluster"
	"github.com/parthh37/wpgenie/internal/sftp"
	"github.com/parthh37/wpgenie/internal/site"
	"github.com/parthh37/wpgenie/internal/store"
)

// Phase 2 endpoints: jobs, backups, staging, domains and certificates, PHP,
// SFTP and phpMyAdmin. Long operations answer 202 with a job ID; GET
// /jobs/{id} follows them.

func jobAccepted(w http.ResponseWriter, id int64, extra map[string]any) error {
	out := map[string]any{"job_id": id}
	for k, v := range extra {
		out[k] = v
	}
	return writeJSON(w, http.StatusAccepted, out)
}

// ---- Jobs ----

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) error {
	limit, err := limitParam(r, 50, 500)
	if err != nil {
		return err
	}
	siteID, active := r.URL.Query().Get("site"), r.URL.Query().Get("active") == "1"
	if tenantOf(r) != nil {
		// Their sites' jobs (shared sites' too), and those they started (a
		// failed create).
		p := principalFrom(r.Context())
		owned, _, err := s.visibleSites(r.Context(), p)
		if err != nil {
			return err
		}
		ids, owner := sortedKeys(owned), p.owner()
		if siteID != "" {
			if _, ok := owned[siteID]; !ok {
				return store.ErrNotFound
			}
			ids, owner = []string{siteID}, ""
		}
		list, err := s.Store.VisibleJobs(r.Context(), ids, owner, active, limit)
		if err != nil {
			return err
		}
		return writeJSON(w, http.StatusOK, list)
	}
	list, err := s.Store.Jobs(r.Context(), siteID, active, limit)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, s.clusterJobs(r, list, limit))
}

func jobID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return 0, errBadRequest
	}
	return id, nil
}

// getJob returns a job, with its secret result (a new site's password)
// for the user who started it.
func (s *Server) getJob(w http.ResponseWriter, r *http.Request) error {
	if remote, err := s.remoteJob(w, r); remote || err != nil {
		return err
	}
	id, err := jobID(r)
	if err != nil {
		return err
	}
	j, err := s.Store.GetJob(r.Context(), id)
	if err != nil {
		return err
	}
	out := map[string]any{"job": j}
	if v, ok := s.Jobs.Secret(id, principalFrom(r.Context()).owner()); ok {
		out["secret"] = v
	}
	return writeJSON(w, http.StatusOK, out)
}

func (s *Server) dropJobSecret(w http.ResponseWriter, r *http.Request) error {
	if remote, err := s.remoteJob(w, r); remote || err != nil {
		return err
	}
	id, err := jobID(r)
	if err != nil {
		return err
	}
	s.Jobs.DropSecret(id, principalFrom(r.Context()).owner())
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// ---- Backups ----

type repoView struct {
	*store.BackupRepo
	HostKeyFingerprint string `json:"host_key_fingerprint,omitempty"`
}

func viewRepo(r *store.BackupRepo) repoView {
	return repoView{r, site.HostKeyFingerprint(r.HostKey)}
}

func (s *Server) listRepos(w http.ResponseWriter, r *http.Request) error {
	repos, err := s.Sites.Repos(r.Context())
	if err != nil {
		return err
	}
	out := make([]repoView, len(repos))
	for i, rp := range repos {
		out[i] = viewRepo(rp)
	}
	return writeJSON(w, http.StatusOK, out)
}

func (s *Server) addRepo(w http.ResponseWriter, r *http.Request) error {
	var in site.RepoInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	generated := in.Password == ""
	repo, err := s.Sites.AddRepo(r.Context(), in)
	if err != nil {
		return err
	}
	// Every server backs up its own sites: they all get the destination.
	s.pushRepos(r.Context())
	out := map[string]any{"repo": viewRepo(repo)}
	if generated {
		// Shown once here (and on an admin's request later): without it the
		// backups can't be restored if this server is lost.
		out["password"] = repo.Password
	}
	return writeJSON(w, http.StatusCreated, out)
}

func (s *Server) deleteRepo(w http.ResponseWriter, r *http.Request) error {
	if err := s.repoUnusedElsewhere(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	if err := s.Sites.RemoveRepo(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	s.eachNode(context.WithoutCancel(r.Context()), 20*time.Second, func(ctx context.Context, n *store.Node) error {
		return s.Cluster.Call(ctx, n.ID, http.MethodDelete, "/cluster/v1/repos/"+r.PathValue("id"), nil, nil)
	})
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) checkRepo(w http.ResponseWriter, r *http.Request) error {
	repo, err := s.Sites.CheckRepo(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, viewRepo(repo))
}

// repoPassword is a POST so that revealing it is in the audit log.
func (s *Server) repoPassword(w http.ResponseWriter, r *http.Request) error {
	pw, err := s.Sites.RepoPassword(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]string{"password": pw})
}

func (s *Server) repoBackups(w http.ResponseWriter, r *http.Request) error {
	list, err := s.Sites.RepoBackups(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, list)
}

func (s *Server) deleteRepoBackup(w http.ResponseWriter, r *http.Request) error {
	if err := s.Sites.DeleteBackup(r.Context(), "", r.PathValue("id"), r.PathValue("backup")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) restoreAsNew(w http.ResponseWriter, r *http.Request) error {
	var in site.RestoreAsNewInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	st, id, err := s.Sites.StartRestoreAsNew(r.Context(), in)
	if err != nil {
		return err
	}
	return jobAccepted(w, id, map[string]any{"site": st})
}

func (s *Server) siteBackups(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if _, err := s.Store.GetSite(r.Context(), id); err != nil {
		return err
	}
	var policy *store.BackupPolicy
	if p, err := s.Store.BackupPolicy(r.Context(), id); err == nil {
		policy = p
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	list, errs, err := s.Sites.SiteBackups(r.Context(), id)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"policy": policy, "backups": list, "errors": errs})
}

func (s *Server) setBackupPolicy(w http.ResponseWriter, r *http.Request) error {
	var in site.PolicyInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if t := tenantOf(r); t != nil && in.RepoID != "" && !slices.Contains(t.SiteLimits.BackupRepos, in.RepoID) {
		return fmt.Errorf("%w: your plan doesn't include that backup destination", errForbidden)
	}
	if done, err := s.forwardAfterChecks(w, r, in); done || err != nil {
		return err
	}
	p, err := s.Sites.SetBackupPolicy(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, p)
}

func (s *Server) startBackup(w http.ResponseWriter, r *http.Request) error {
	id, err := s.Sites.StartBackup(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return jobAccepted(w, id, nil)
}

func (s *Server) startRestore(w http.ResponseWriter, r *http.Request) error {
	var in site.RestoreInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	id, err := s.Sites.StartRestore(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	return jobAccepted(w, id, nil)
}

var safeName = regexp.MustCompile(`[^a-z0-9.-]+`)

// downloadBackup streams a backup as .tar.gz straight from restic.
func (s *Server) downloadBackup(w http.ResponseWriter, r *http.Request) error {
	// A whole site can take a while: the server's write timeout would cut it.
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(12 * time.Hour))
	var zw *gzip.Writer
	started := false
	err := s.Sites.DownloadBackup(r.Context(), r.PathValue("id"), r.PathValue("repo"), r.PathValue("backup"),
		func(b *site.BackupInfo) io.Writer {
			started = true
			name := fmt.Sprintf("%s-%s-%s.tar.gz", safeName.ReplaceAllString(b.Domain, "-"),
				b.Time.UTC().Format("20060102-1504"), b.ShortID)
			w.Header().Set("Content-Type", "application/gzip")
			w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			zw, _ = gzip.NewWriterLevel(w, gzip.BestSpeed)
			return zw
		})
	if !started {
		return err
	}
	if err == nil {
		err = zw.Close()
	}
	if err != nil {
		// Headers are gone: abort the connection so the download fails
		// visibly (no gzip trailer) instead of looking complete.
		s.Log.Error("backup download failed", "site", r.PathValue("id"), "backup", r.PathValue("backup"), "err", err)
		panic(http.ErrAbortHandler)
	}
	return nil
}

func (s *Server) deleteBackup(w http.ResponseWriter, r *http.Request) error {
	if err := s.Sites.DeleteBackup(r.Context(), r.PathValue("id"), r.PathValue("repo"), r.PathValue("backup")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// ---- Staging ----

func (s *Server) createStaging(w http.ResponseWriter, r *http.Request) error {
	var in site.StagingInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	// A staging copy belongs to the live site's account and counts as one
	// of its sites.
	ctx := r.Context()
	if err := s.checkTenantDomain(r, in.Domain); err != nil {
		return err
	}
	var acctID int64
	if o, err := s.Store.SiteOwnerOf(ctx, r.PathValue("id")); err == nil && s.Billing != nil {
		acctID = o.AccountID
		unlock := s.Billing.LockQuota()
		defer unlock()
		if tenantOf(r) != nil {
			if err := s.Billing.CheckNewSiteLocked(ctx, acctID); err != nil {
				return err
			}
		}
	}
	st, id, remote, err := s.createStagingOnNode(r, in)
	if err != nil {
		return err
	}
	if !remote {
		if st, id, err = s.Sites.StartStaging(ctx, r.PathValue("id"), in); err != nil {
			return err
		}
	}
	// The copy is the owner's, like the live site, and shared like it
	// (siteAccess).
	view := siteView{Site: st, AccountID: acctID}
	if t := tenantOf(r); t != nil {
		view.Access = t.Access
	}
	if acctID != 0 {
		if err := s.assignNewSite(ctx, st.ID, acctID); err != nil {
			return err
		}
	}
	s.announceSite(id, st.ID, acctID)
	return jobAccepted(w, id, map[string]any{"site": view})
}

func (s *Server) pushStaging(w http.ResponseWriter, r *http.Request) error {
	var in site.PushInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if tenantOf(r) != nil {
		// The push writes into the live site: it must be theirs too (an
		// administrator may have moved one of the two).
		stg, err := s.siteRecord(r.Context(), r.PathValue("id"))
		if err != nil {
			return err
		}
		// Shared with them, it needs the level the push needs here.
		_, access, ok := s.siteAccess(r.Context(), principalFrom(r.Context()), stg.ParentID)
		if !ok || !accessAllows(access, requiredAccess("POST /api/v1/sites/{id}/push")) {
			return fmt.Errorf("%w: the live site isn't yours", errForbidden)
		}
	}
	if done, err := s.forwardAfterChecks(w, r, in); done || err != nil {
		return err
	}
	id, err := s.Sites.StartPush(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	return jobAccepted(w, id, nil)
}

func (s *Server) siteTables(w http.ResponseWriter, r *http.Request) error {
	t, err := s.Sites.SiteTables(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	if t == nil {
		t = []string{}
	}
	return writeJSON(w, http.StatusOK, t)
}

// ---- Domains and certificates ----

func (s *Server) addDomain(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Domain   string `json:"domain"`
		Redirect bool   `json:"redirect"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if err := s.checkTenantDomain(r, in.Domain); err != nil {
		return err
	}
	if t := tenantOf(r); t != nil && t.SiteLimits.MaxDomains > 0 {
		// Counted and added under the quota lock: parallel adds can't
		// overshoot the plan.
		unlock := s.Billing.LockQuota()
		defer unlock()
		st, err := s.siteRecord(r.Context(), r.PathValue("id"))
		if err != nil {
			return err
		}
		if len(st.Domains)+len(st.RedirectDomains) >= t.SiteLimits.MaxDomains {
			return fmt.Errorf("%w: your plan allows %d domains per site", billing.ErrQuota, t.SiteLimits.MaxDomains)
		}
	}
	if s.Cluster != nil {
		// A site on another server: that server only knows its own sites, so
		// the domain is checked against every server here first.
		if node, remote, err := s.Cluster.SiteNode(r.Context(), r.PathValue("id")); err != nil {
			return err
		} else if remote {
			mu := s.Cluster.CreateLock()
			mu.Lock()
			defer mu.Unlock()
			if err := s.Sites.DomainFree(r.Context(), in.Domain); err != nil {
				return err
			}
			rebody(r, in)
			return s.forwardSite(w, r, node, r.PathValue("id"))
		}
		mu := s.Cluster.CreateLock()
		mu.Lock()
		defer mu.Unlock()
	}
	st, err := s.Sites.AddDomain(r.Context(), r.PathValue("id"), in.Domain, in.Redirect)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}

// dnsCheck tells where a domain points before a site is created for it:
// at the chosen server (admins), or at any server that may get the site.
func (s *Server) dnsCheck(w http.ResponseWriter, r *http.Request) error {
	node := r.URL.Query().Get("node")
	tenant := tenantOf(r) != nil
	if tenant {
		node = "" // the provider places tenants' sites
	}
	if node == "" && (s.Cluster == nil || !s.Cluster.Enabled()) {
		node = cluster.LocalNode
	}
	res, err := s.Sites.CheckDNS(r.Context(), r.URL.Query().Get("domain"), node)
	if err != nil {
		return err
	}
	if tenant {
		res.Server = "" // server names are the provider's business
	}
	return writeJSON(w, http.StatusOK, res)
}

// siteDNSCheck tells whether a domain points at the server a site is on,
// before it's added to the site. Answered here, not by that server: the
// control plane knows each server's public address (a server behind NAT
// doesn't).
func (s *Server) siteDNSCheck(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	node := cluster.LocalNode
	if s.Cluster != nil {
		n, remote, err := s.Cluster.SiteNode(r.Context(), id)
		if err != nil {
			return err
		}
		if remote {
			node = n
		}
	}
	if node == cluster.LocalNode {
		if _, err := s.Store.GetSite(r.Context(), id); err != nil {
			return err
		}
	}
	res, err := s.Sites.CheckDNS(r.Context(), r.URL.Query().Get("domain"), node)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, res)
}

func (s *Server) setDomain(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Redirect bool `json:"redirect"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	st, err := s.Sites.SetDomainRedirect(r.Context(), r.PathValue("id"), r.PathValue("domain"), in.Redirect)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}

func (s *Server) removeDomain(w http.ResponseWriter, r *http.Request) error {
	st, err := s.Sites.RemoveDomain(r.Context(), r.PathValue("id"), r.PathValue("domain"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}

func (s *Server) setPrimaryDomain(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Domain string `json:"domain"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	id, err := s.Sites.StartPrimaryDomain(r.Context(), r.PathValue("id"), in.Domain)
	if err != nil {
		return err
	}
	return jobAccepted(w, id, nil)
}

func (s *Server) getCert(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.Store.GetSite(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	c, err := s.Store.SiteCert(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		return writeJSON(w, http.StatusOK, nil) // automatic certificates
	}
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, c)
}

func (s *Server) setCert(w http.ResponseWriter, r *http.Request) error {
	var in site.CertInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	c, err := s.Sites.SetCert(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, c)
}

func (s *Server) deleteCert(w http.ResponseWriter, r *http.Request) error {
	if err := s.Sites.RemoveCert(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// ---- PHP ----

func (s *Server) phpVersions(w http.ResponseWriter, _ *http.Request) error {
	return writeJSON(w, http.StatusOK, map[string]any{"versions": s.Sites.Cfg.PHPVersions,
		"default": s.Sites.Cfg.DefaultPHPVersion()})
}

func (s *Server) setPHP(w http.ResponseWriter, r *http.Request) error {
	var in site.PHPInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	id, err := s.Sites.StartPHPChange(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	return jobAccepted(w, id, nil)
}

// ---- SFTP and phpMyAdmin ----

func (s *Server) listSFTP(w http.ResponseWriter, r *http.Request) error {
	st, err := s.Store.GetSite(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	users, err := s.SFTP.Users(r.Context(), st.ID)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"users": users, "server": s.SFTP.Info(r.Context()),
		"host": st.PrimaryDomain, "phpmyadmin_sessions": s.PHPMyAdmin.Sessions(st.ID)})
}

func (s *Server) addSFTP(w http.ResponseWriter, r *http.Request) error {
	var in sftp.UserInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	// Recorded so a login goes with its adder's access to a shared site
	// (sharing.go); on a node, the panel's user it forwarded for.
	p := principalFrom(r.Context())
	in.AddedBy, in.AddedByName = p.owner(), p.Name
	u, pw, err := s.SFTP.Add(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, map[string]any{"user": u, "password": pw})
}

func (s *Server) setSFTPKeys(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		PublicKeys []string `json:"public_keys"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	u, err := s.SFTP.SetKeys(r.Context(), r.PathValue("id"), r.PathValue("user"), in.PublicKeys)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, u)
}

func (s *Server) setSFTPPassword(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	pw, err := s.SFTP.SetPassword(r.Context(), r.PathValue("id"), r.PathValue("user"), in.Enabled)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]string{"password": pw})
}

func (s *Server) deleteSFTP(w http.ResponseWriter, r *http.Request) error {
	if err := s.SFTP.Delete(r.Context(), r.PathValue("id"), r.PathValue("user")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) openPHPMyAdmin(w http.ResponseWriter, r *http.Request) error {
	u, exp, err := s.PHPMyAdmin.Open(r.Context(), r.PathValue("id"), principalFrom(r.Context()).Name)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"url": u, "expires_at": exp})
}
