package site

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/parthh37/wpgenie/internal/jobs"
	"github.com/parthh37/wpgenie/internal/runtime"
	"github.com/parthh37/wpgenie/internal/store"
)

// Staging: a full copy of a live site on its own domain, as a site of its
// own (container, database, settings), linked to the live one. What makes
// it a staging site:
//
//   - WP_ENVIRONMENT_TYPE is "staging" (plugins like Jetpack and WooCommerce
//     Subscriptions stop acting as the live site) and search engines are
//     asked not to index it (blog_public 0, X-Robots-Tag from Caddy);
//   - it sends no mail through the mail server and WPGenie runs no cron for
//     it, so customers get no e-mails or renewals from a copy;
//   - no automatic updates or scheduled backups.
//
// Changes go back with a push: code (core, plugins, themes), all files
// and/or the database (or chosen tables). The live site is backed up first,
// and the database arrives with its links already rewritten: WP-CLI
// exports it from the staging site with the replacement applied, so the
// live database never holds staging links, not even for a moment.

// StagingInput creates a staging site.
type StagingInput struct {
	Domain string `json:"domain"` // default staging.<live primary domain>
}

// StartStaging clones a live site into a new staging site, as a job.
func (s *Service) StartStaging(ctx context.Context, liveID string, in StagingInput) (*store.Site, int64, error) {
	live, err := s.Store.GetSite(ctx, liveID)
	if err != nil {
		return nil, 0, err
	}
	if live.Status != store.StatusActive {
		return nil, 0, fmt.Errorf("%w: site is %s", ErrInvalidInput, live.Status)
	}
	if live.ParentID != "" {
		return nil, 0, fmt.Errorf("%w: this is a staging site; stage the live site instead", ErrInvalidInput)
	}
	if ids, err := s.Store.StagingOf(ctx, liveID); err != nil {
		return nil, 0, err
	} else if len(ids) > 0 {
		return nil, 0, fmt.Errorf("%w: the site already has a staging site (%s)", ErrConflict, ids[0])
	}
	if in.Domain == "" {
		in.Domain = "staging." + strings.TrimPrefix(live.PrimaryDomain, "www.")
	}
	domain, err := NormalizeDomain(in.Domain)
	if err != nil {
		return nil, 0, err
	}
	name := "Staging: " + live.Name
	if validName(name) != nil {
		name = domain
	}
	st := newSite(domain, name)
	st.ParentID = live.ID
	st.PHPVersion, st.PHP = live.PHPVersion, live.PHP
	st.MemoryMB, st.CPUs = live.MemoryMB, live.CPUs
	st.PageCache, st.ObjectCache = live.PageCache, live.ObjectCache
	st.Optimize = slices.Clone(live.Optimize)
	st.Harden = slices.Clone(live.Harden)
	st.ShieldMode, st.BlockAIBots, st.WAF, st.BodyWAF, st.XMLRPC = live.ShieldMode, live.BlockAIBots, live.WAF, live.BodyWAF, live.XMLRPC
	st.AdminAllow, st.TrustedIPs, st.DenyIPs = live.AdminAllow, live.TrustedIPs, live.DenyIPs
	st.AutoUpdate = AutoUpdateOff
	bld, err := s.reserve(ctx, st)
	if err != nil {
		return nil, 0, err
	}
	out := *bld.st
	// The live site is locked too (the copy is one moment of it, and no
	// update runs on it meanwhile), both before the job takes a heavy slot.
	spec := s.siteJob(st.ID, "staging", true)
	spec.Lock = jobs.LockFunc(s.maintLock(st.ID), s.maintLock(liveID))
	id, err := s.Jobs.Submit(ctx, spec, func(ctx context.Context, t *jobs.Task) (err error) {
		defer func() {
			if err != nil {
				bld.rollback(err)
			}
		}()
		live, err := s.Store.GetSite(ctx, liveID)
		if err != nil {
			return err
		}
		prefix, err := s.tablePrefix(liveID)
		if err != nil {
			return err
		}
		if err := bld.start(ctx, prefix, "staging", t.Progress); err != nil {
			return err
		}
		t.Progress(20, "Copying files")
		if err := s.copyInstall(ctx, liveID, st.ID, true); err != nil {
			return err
		}
		t.Progress(55, "Copying the database")
		if err := s.copyDB(ctx, live.DBName, st.DBName); err != nil {
			return err
		}
		t.Progress(70, "Rewriting links to "+domain)
		if err := s.searchReplace(ctx, st.ID, live.PrimaryDomain, domain); err != nil {
			return err
		}
		// Discourage search engines (WordPress's own setting; Caddy adds the
		// header too).
		if err := s.Runtime.Exec(ctx, st.ID, nil, nil, runtime.WPArgs("option", "update", "blog_public", "0")...); err != nil {
			return err
		}
		if err := bld.finish(ctx, t.Progress); err != nil {
			return err
		}
		t.SetResult(map[string]string{"site_id": st.ID, "url": "https://" + domain})
		s.event(liveID, "staging", fmt.Sprintf("Staging site %s created (%s)", domain, st.ID))
		s.event(st.ID, "staging", fmt.Sprintf("Cloned from %s", live.PrimaryDomain))
		return nil
	})
	if err != nil {
		bld.rollback(err)
		return nil, 0, err
	}
	return &out, id, nil
}

// Push file scopes.
const (
	PushFilesNone = ""
	PushFilesCode = "code" // core, plugins, themes, mu-plugins: everything but uploads
	PushFilesAll  = "all"  // uploads too
)

// PushInput selects what a push copies to the live site.
type PushInput struct {
	Files    string `json:"files"`    // "", code or all
	Database bool   `json:"database"` // push the database
	// Tables limits the database push to these tables (empty = all). With
	// all tables, live tables that don't exist on staging are dropped.
	Tables []string `json:"tables"`
}

// PushResult is recorded with a push job.
type PushResult struct {
	SafetyBackup string `json:"safety_backup"`
	Before       Health `json:"before"`
	After        Health `json:"after"`
}

// StartPush copies a staging site's changes to its live site, as a job.
func (s *Service) StartPush(ctx context.Context, stagingID string, in PushInput) (int64, error) {
	stg, err := s.Store.GetSite(ctx, stagingID)
	if err != nil {
		return 0, err
	}
	if stg.ParentID == "" {
		return 0, fmt.Errorf("%w: %s is not a staging site", ErrInvalidInput, stagingID)
	}
	if stg.Status != store.StatusActive {
		return 0, fmt.Errorf("%w: site is %s", ErrInvalidInput, stg.Status)
	}
	if in.Files != PushFilesNone && in.Files != PushFilesCode && in.Files != PushFilesAll {
		return 0, fmt.Errorf("%w: files must be empty, code or all", ErrInvalidInput)
	}
	if in.Files == PushFilesNone && !in.Database {
		return 0, fmt.Errorf("%w: nothing selected to push", ErrInvalidInput)
	}
	if len(in.Tables) > 0 {
		if !in.Database {
			return 0, fmt.Errorf("%w: tables need database", ErrInvalidInput)
		}
		have, err := s.DB.Tables(ctx, stg.DBName)
		if err != nil {
			return 0, err
		}
		for _, t := range in.Tables {
			if !slices.Contains(have, t) {
				return 0, fmt.Errorf("%w: the staging site has no table %q", ErrInvalidInput, t)
			}
		}
	}
	if s.Backups == nil {
		return 0, fmt.Errorf("%w: pushing needs backups (the live site is backed up first)", ErrInvalidInput)
	}
	liveID := stg.ParentID
	spec := s.siteJob(liveID, "push", true)
	spec.Lock = jobs.LockFunc(s.maintLock(liveID), s.maintLock(stagingID))
	return s.Jobs.Submit(ctx, spec, func(ctx context.Context, t *jobs.Task) error {
		return s.push(ctx, liveID, stagingID, in, t)
	})
}

func (s *Service) push(ctx context.Context, liveID, stagingID string, in PushInput, t *jobs.Task) error {
	live, err := s.Store.GetSite(ctx, liveID)
	if err != nil {
		return err
	}
	stg, err := s.Store.GetSite(ctx, stagingID)
	if err != nil {
		return err
	}
	if live.Status != store.StatusActive {
		return fmt.Errorf("the live site is %s", live.Status)
	}
	res := &PushResult{}
	t.SetResult(res)
	res.Before = s.Prober.Probe(ctx, live.PrimaryDomain)

	repo, err := s.siteRepo(ctx, liveID)
	if err != nil {
		return err
	}
	safety, err := s.backupLocked(ctx, live, repo, BackupSafety, func(pct int, step string) {
		t.Progress(pct*40/100, "Backing up the live site first: "+strings.ToLower(step))
	})
	if err != nil {
		return fmt.Errorf("backing up the live site failed, nothing was pushed: %w", err)
	}
	res.SafetyBackup = safety.ShortID
	fail := func(err error) error {
		s.event(liveID, "staging", fmt.Sprintf("Push from %s FAILED: %v. The live site before the push is backup %s.",
			stg.PrimaryDomain, err, safety.ShortID))
		return fmt.Errorf("%w (the live site before the push is backup %s)", err, safety.ShortID)
	}

	if in.Files != PushFilesNone {
		t.Progress(45, "Pushing files")
		if err := s.copyInstall(ctx, stagingID, liveID, in.Files == PushFilesAll); err != nil {
			return fail(err)
		}
	}
	if in.Database {
		t.Progress(70, "Pushing the database")
		// Staging asks search engines to stay away (blog_public 0): that
		// setting must not reach the live site with wp_options.
		var public strings.Builder
		if err := s.Runtime.Exec(ctx, liveID, nil, &public, runtime.WPArgs("option", "get", "blog_public")...); err != nil {
			return fail(fmt.Errorf("reading the live site's search engine setting: %w", err))
		}
		if err := s.pushDB(ctx, stg, live, in.Tables); err != nil {
			return fail(err)
		}
		// Now, not with the purge below: the option update next reads the
		// cached value, and WordPress skips writing one that looks unchanged.
		if err := s.flushObjectCache(ctx, liveID); err != nil {
			return fail(err)
		}
		v := strings.TrimSpace(public.String())
		if v != "0" {
			v = "1"
		}
		if err := s.Runtime.Exec(ctx, liveID, nil, nil, runtime.WPArgs("option", "update", "blog_public", v)...); err != nil {
			return fail(fmt.Errorf("restoring the live site's search engine setting: %w", err))
		}
	}
	t.Progress(92, "Purging caches")
	if err := s.purgeLocal(ctx, live); err != nil {
		s.Log.Warn("purging after a push", "site", liveID, "err", err)
	}
	if err := s.purgeCDNIfOn(ctx, live); err != nil {
		s.Log.Warn("purging the CDN after a push", "site", liveID, "err", err)
	}
	res.After = s.Prober.Probe(context.WithoutCancel(ctx), live.PrimaryDomain)
	what := []string{}
	if in.Files != PushFilesNone {
		what = append(what, map[string]string{PushFilesCode: "code", PushFilesAll: "all files"}[in.Files])
	}
	if in.Database {
		if len(in.Tables) > 0 {
			what = append(what, fmt.Sprintf("%d table(s)", len(in.Tables)))
		} else {
			what = append(what, "the database")
		}
	}
	msg := fmt.Sprintf("Pushed %s from %s (undo: backup %s)", strings.Join(what, " and "), stg.PrimaryDomain, safety.ShortID)
	if res.Before.OK && !res.After.OK {
		msg += ". WARNING: the site now fails its health check (" + res.After.Detail + "): restore backup " + safety.ShortID + " to undo"
	}
	s.event(liveID, "staging", msg)
	s.event(stagingID, "staging", msg)
	return nil
}

// pushDB exports the staging tables with links rewritten to the live
// domain (WP-CLI search-replace --export: staging itself is untouched) and
// loads the SQL into the live database, each table replaced whole.
//
// That SQL comes out of the staging site's container, whose WordPress a
// compromised plugin could have altered: it is loaded as a temporary
// account with rights on the live site's database only (and in the
// client's sandbox), never as root, so the worst it can do is what a push
// may do anyway.
func (s *Service) pushDB(ctx context.Context, stg, live *store.Site, tables []string) error {
	user, pass := "wpga_"+randString(12, lowerAlnum), randString(32, passAlphabet)
	if err := s.DB.CreateTempUser(ctx, live.DBName, user, pass); err != nil {
		return fmt.Errorf("pushing the database: %w", err)
	}
	defer func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if err := s.DB.DropTempUser(c, user); err != nil {
			s.Log.Warn("dropping the push's temporary database account", "user", user, "err", err)
		}
	}()
	err := pipe(func(w io.Writer) error {
		return s.Runtime.Exec(ctx, stg.ID, nil, w, searchReplaceArgs(stg.PrimaryDomain, live.PrimaryDomain, tables, true)...)
	}, func(r io.Reader) error { return s.Dumper.RestoreAs(ctx, live.DBName, user, pass, r) })
	if err != nil {
		return fmt.Errorf("pushing the database: %w", err)
	}
	if len(tables) > 0 {
		return nil
	}
	// A whole-database push makes live match staging: tables staging
	// doesn't have (a plugin removed there) go.
	have, err := s.DB.Tables(ctx, stg.DBName)
	if err != nil {
		return err
	}
	now, err := s.DB.Tables(ctx, live.DBName)
	if err != nil {
		return err
	}
	var extra []string
	for _, t := range now {
		if !slices.Contains(have, t) {
			extra = append(extra, t)
		}
	}
	if len(extra) > 0 {
		return s.DB.DropTables(ctx, live.DBName, extra)
	}
	return nil
}

// SiteTables lists a site's database tables (to choose what to push).
func (s *Service) SiteTables(ctx context.Context, id string) ([]string, error) {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.DB.Tables(ctx, st.DBName)
}

// waitFor is a small helper for tests and the CLI: wait for a job.
func (s *Service) waitFor(ctx context.Context, id int64) error {
	c, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	j, err := s.Jobs.WaitJob(c, id)
	if err != nil {
		return err
	}
	if j.Status != store.JobSucceeded {
		return errors.New(j.Error)
	}
	return nil
}
