package site

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/parthh37/wpgenie/internal/cluster"
	"github.com/parthh37/wpgenie/internal/jobs"
	"github.com/parthh37/wpgenie/internal/sftp"
	"github.com/parthh37/wpgenie/internal/shield"
	"github.com/parthh37/wpgenie/internal/store"
)

// Moving a site to another server. The control plane runs the move as a
// job, talking to the two servers (either may be its own):
//
//  1. The target creates the site with the same ID and settings, not yet
//     served (status importing).
//  2. First pass, the site live: files (as the site user on both ends,
//     two-phase extract as for restores) and the database.
//  3. The source goes into maintenance (WordPress's .maintenance), then a
//     second pass copies the database again and the files changed since the
//     first pass began: the only downtime.
//  4. The target finishes (wrappers, connection limit, live in its Caddy,
//     which gets certificates right away: see below), and the registry
//     points at it.
//  5. The source stops its replicas and forwards the site's domains to the
//     target through the cluster tunnel, so visitors DNS still sends to the
//     old server reach the new one, with their own address (PROXY protocol
//     into a loopback-only Caddy listener on the target), until DNS
//     follows. Let's Encrypt's HTTP challenges for the target are forwarded
//     too, so the target has certificates before DNS moves. After
//     forwardDays (or when the move is finished from the panel) the old
//     copy is deleted.
//
// Anything failing before step 4 aborts the import and takes the source out
// of maintenance: the site stays where it was.

// Site statuses during a move.
const (
	StatusImporting = store.StatusImporting
	StatusMoved     = store.StatusMoved
)

// forwardDays: how long an old server forwards a moved site (DNS TTLs are
// usually hours; resolvers holding on for days are rare).
const forwardDays = 7

// MigrationMeta is what moves with a site besides files and database.
type MigrationMeta struct {
	Site        *store.Site         `json:"site"`
	TablePrefix string              `json:"table_prefix"`
	Tables      []string            `json:"tables"`
	CertPEM     string              `json:"cert_pem,omitempty"`
	KeyPEM      string              `json:"key_pem,omitempty"`
	Backup      *store.BackupPolicy `json:"backup,omitempty"`
	CDN         *store.CDN          `json:"cdn,omitempty"`
	SFTP        []migratedSFTP      `json:"sftp,omitempty"`
	SMTP        *SMTPCreds          `json:"smtp,omitempty"`
	// Offload moves with its secret key: uploads whose local copies were
	// removed exist only in the bucket, and the new server serves them
	// from there as the old one did.
	Offload *store.Offload `json:"offload,omitempty"`
}

type migratedSFTP struct {
	Username   string   `json:"username"`
	Password   string   `json:"password"` // SHA-512 crypt hash, as stored
	PublicKeys []string `json:"public_keys"`
}

// ---- Source side ----

// ExportMeta describes a site for a move (only sites that can move).
func (s *Service) ExportMeta(ctx context.Context, id string) (*MigrationMeta, error) {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, err
	}
	if st.Status != store.StatusActive {
		return nil, fmt.Errorf("%w: only an active site can be moved (this one is %s)", ErrInvalidInput, st.Status)
	}
	if st.ParentID != "" {
		return nil, fmt.Errorf("%w: staging sites move with their live site: delete this staging copy, move the live site, and clone it again", ErrInvalidInput)
	}
	if kids, err := s.Store.StagingOf(ctx, id); err != nil {
		return nil, err
	} else if len(kids) > 0 {
		return nil, fmt.Errorf("%w: delete this site's staging copy first (it can be cloned again after the move)", ErrInvalidInput)
	}
	if len(st.SpreadNodes) > 0 || len(st.RemoteUpstreams) > 0 {
		return nil, fmt.Errorf("%w: stop spreading this site over several servers first", ErrInvalidInput)
	}
	m := &MigrationMeta{Site: st}
	if m.TablePrefix, err = s.tablePrefix(id); err != nil {
		return nil, err
	}
	if m.Tables, err = s.DB.Tables(ctx, st.DBName); err != nil {
		return nil, err
	}
	if b, err := os.ReadFile(filepath.Join(s.certDir(id), "cert.pem")); err == nil {
		k, err := os.ReadFile(filepath.Join(s.certDir(id), "key.pem"))
		if err != nil {
			return nil, err
		}
		m.CertPEM, m.KeyPEM = string(b), string(k)
	}
	if p, err := s.Store.BackupPolicy(ctx, id); err == nil {
		m.Backup = p
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if c, err := s.Store.GetCDN(ctx, id); err == nil {
		m.CDN = c
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if o, err := s.Store.GetOffload(ctx, id); err == nil {
		m.Offload = o
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	users, err := s.Store.SFTPUsers(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		m.SFTP = append(m.SFTP, migratedSFTP{Username: u.Username, Password: u.Password, PublicKeys: u.PublicKeys})
	}
	if st.SMTP {
		if b, err := os.ReadFile(filepath.Join(s.Cfg.SiteDir(id), smtpCredsFile)); err == nil {
			if c, ok := parseSMTPCreds(b); ok {
				m.SMTP = &c
			}
		}
	}
	return m, nil
}

// ExportFiles writes the site's install as a tar stream (as the site
// user, inside its container). With since > 0 (unix seconds), only files
// and symlinks whose inode changed since then (the second pass of a move):
// the change time, which no client can set back the way SFTP uploads keep
// an old modification time.
func (s *Service) ExportFiles(ctx context.Context, id string, since int64, w io.Writer) error {
	root := s.Cfg.SiteRoot(id)
	if since <= 0 {
		return s.Runtime.Exec(ctx, id, nil, w, installTar(root, true)...)
	}
	script := `set -e; cd "$1"; find .` + findPrunes() + ` \( -type f -o -type l \) -exec stat -c '%Z %n' {} + |
		awk -v t="$2" '$1 >= t { sub(/^[0-9]+ /, ""); print }' | tar -cf - -T -`
	return s.Runtime.Exec(ctx, id, nil, w, "sh", "-c", script, "sh", root, strconv.FormatInt(since, 10))
}

// findPrunes leaves out of a find what installTar leaves out of a copy.
func findPrunes() string {
	out := ""
	for _, e := range copyExcludes {
		out += " -path ./" + e + " -prune -o"
	}
	return out
}

// ExportManifest lists every path of the install (one per line), for the
// target to delete what the source no longer has after the second pass.
func (s *Service) ExportManifest(ctx context.Context, id string, w io.Writer) error {
	script := `set -e; cd "$1"; find .` + findPrunes() + ` -mindepth 1 -print`
	return s.Runtime.Exec(ctx, id, nil, w, "sh", "-c", script, "sh", s.Cfg.SiteRoot(id))
}

// ExportDB writes the site's database as SQL.
func (s *Service) ExportDB(ctx context.Context, id string, w io.Writer) error {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return err
	}
	return s.Dumper.Dump(ctx, st.DBName, w)
}

// maintenanceFile: WordPress shows "Briefly unavailable for scheduled
// maintenance" while it exists and $upgrading is less than 10 minutes in
// the past. A time a day ahead keeps it on however long the copy takes;
// the marker lets the cleanup loop take away a page a move left behind
// (a panel that died mid-move) after maintenanceStale.
const (
	maintenanceFile   = ".maintenance"
	maintenanceMarker = "// wpgenie: move in progress"
	maintenanceStale  = 3 * time.Hour
)

// SetMaintenance puts a site into (or out of) WordPress's maintenance mode
// for the final copy of a move; its logins (SFTP, phpMyAdmin) are off
// meanwhile, so nothing is written that the copy would miss.
func (s *Service) SetMaintenance(ctx context.Context, id string, on bool) error {
	root := s.Cfg.SiteRoot(id)
	if err := s.Store.FreezeSite(ctx, id, on); err != nil {
		return err
	}
	if s.SiteSuspended != nil {
		s.SiteSuspended(ctx, id, on)
	}
	if !on {
		return s.Runtime.Exec(ctx, id, nil, nil, "rm", "-f", filepath.Join(root, maintenanceFile))
	}
	content := fmt.Sprintf("<?php %s\n$upgrading = %d;\n", maintenanceMarker, time.Now().Add(24*time.Hour).Unix())
	return s.Runtime.Exec(ctx, id, strings.NewReader(content), nil, "sh", "-c", `cat > "$1"`, "sh",
		filepath.Join(root, maintenanceFile))
}

// MovedTo retires the site here after a move: its replicas stop, its
// domains are forwarded to the new server (see the file comment), and the
// data stays until the forward ends, so a move can still be undone.
func (s *Service) MovedTo(ctx context.Context, id string, to cluster.Endpoint, days int) error {
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return err
	}
	if days <= 0 {
		days = forwardDays
	}
	domains := slices.Concat([]string{st.PrimaryDomain}, st.Domains, st.RedirectDomains)
	slices.Sort(domains)
	domains = slices.Compact(domains)
	ports, err := s.Store.AllocatePorts(ctx, s.Cfg.SitePortBase, 2, s.heldPorts(ctx)...)
	if err != nil {
		return err
	}
	f := store.SiteForward{SiteID: id, NodeID: to.ID, Address: to.Address, Domains: domains, Port: ports[0],
		HTTPPort: ports[1], ExpiresAt: time.Now().Add(time.Duration(days) * 24 * time.Hour)}
	if err := s.Store.PutSiteForward(ctx, f); err != nil {
		return err
	}
	if err := s.Store.SetSiteStatus(ctx, id, StatusMoved); err != nil {
		return err
	}
	if err := s.openTunnels(ctx); err != nil {
		s.Log.Warn("opening the forward to a moved site's new server", "site", id, "err", err)
	}
	if err := s.Sync(ctx); err != nil {
		return err
	}
	// Its logins go (the copy here is no longer the site); the freeze of
	// the final copy isn't needed any more.
	if err := s.Store.FreezeSite(ctx, id, false); err != nil {
		s.Log.Warn("unfreezing a moved site", "site", id, "err", err)
	}
	if s.SiteSuspended != nil {
		s.SiteSuspended(ctx, id, true)
	}
	// Nothing routes to the replicas any more. The maintenance file stays
	// in the old copy: it must never serve again by accident.
	if err := s.Runtime.RemoveSite(context.WithoutCancel(ctx), id); err != nil {
		s.Log.Warn("stopping a moved site's replicas", "site", id, "err", err)
	}
	s.event(id, "migrate", "Moved to server "+to.ID+"; visitors reaching this server are forwarded there until DNS follows")
	return nil
}

// Reactivate undoes MovedTo (a move rolled back from the panel).
func (s *Service) Reactivate(ctx context.Context, id string) error {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return err
	}
	if st.Status != StatusMoved && st.Status != store.StatusActive {
		return fmt.Errorf("%w: the site is %s", ErrInvalidInput, st.Status)
	}
	if err := s.Store.DeleteSiteForward(ctx, id); err != nil {
		return err
	}
	if err := s.Store.SetSiteStatus(ctx, id, store.StatusActive); err != nil {
		return err
	}
	s.opsMu.Lock()
	st.Status = store.StatusActive
	retire, err := s.reconcile(ctx, st)
	s.opsMu.Unlock()
	if err != nil {
		return err
	}
	go retire()
	if err := s.SetMaintenance(ctx, id, false); err != nil {
		return err
	}
	s.closeUnusedTunnels(ctx)
	s.event(id, "migrate", "Serving here again (the move was undone)")
	return nil
}

// EndForward stops forwarding a moved site; with purge the old copy
// (files, database, settings) goes too.
func (s *Service) EndForward(ctx context.Context, id string, purge bool) error {
	if err := s.Store.DeleteSiteForward(ctx, id); err != nil {
		return err
	}
	s.closeUnusedTunnels(ctx)
	st, err := s.Store.GetSite(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return s.Sync(ctx)
	}
	if err != nil {
		return err
	}
	if purge && st.Status == StatusMoved {
		// Delete as a site that no longer lives here: the mail sender and
		// anything else the site owns elsewhere moved with it.
		return s.deleteLocal(ctx, id, false)
	}
	return s.Sync(ctx)
}

// RunForwards ends forwards that expired, deleting the old copies.
func (s *Service) RunForwards(ctx context.Context) {
	if err := s.openTunnels(ctx); err != nil {
		s.Log.Warn("cluster: opening tunnels", "err", err)
	}
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		fwds, err := s.Store.SiteForwards(ctx)
		if err != nil {
			s.Log.Warn("listing forwards", "err", err)
			continue
		}
		s.cleanupMoves(ctx)
		for _, f := range fwds {
			if time.Now().After(f.ExpiresAt) {
				if err := s.EndForward(ctx, f.SiteID, true); err != nil {
					s.Log.Warn("ending an expired forward", "site", f.SiteID, "err", err)
				} else {
					s.Log.Info("moved site's old copy deleted", "site", f.SiteID)
				}
			}
		}
		if err := s.openTunnels(ctx); err != nil {
			s.Log.Warn("cluster: reopening tunnels", "err", err)
		}
	}
}

// cleanupMoves undoes what a move the panel stopped following left here
// (it died mid-move, or the network did): an import that went quiet is
// removed, and a maintenance page a move put up and never took down is.
func (s *Service) cleanupMoves(ctx context.Context) {
	sites, err := s.Store.ListSites(ctx)
	if err != nil {
		return
	}
	for _, st := range sites {
		switch {
		case st.Status == StatusImporting && time.Since(st.UpdatedAt) > time.Hour:
			if err := s.AbortImport(ctx, st.ID); err != nil {
				s.Log.Warn("removing an abandoned import", "site", st.ID, "err", err)
			} else {
				s.Log.Info("removed an import that stopped arriving", "site", st.ID)
			}
		case st.Status == store.StatusActive:
			root, err := os.OpenRoot(s.Cfg.SiteRoot(st.ID))
			if err != nil {
				continue
			}
			fi, err := root.Lstat(maintenanceFile)
			stale := err == nil && fi.Mode().IsRegular() && fi.Size() < 4096 && time.Since(fi.ModTime()) > maintenanceStale
			var b []byte
			if stale {
				b, _ = root.ReadFile(maintenanceFile)
			}
			root.Close()
			if stale && strings.Contains(string(b), maintenanceMarker) {
				if err := s.SetMaintenance(ctx, st.ID, false); err != nil {
					s.Log.Warn("taking down a move's leftover maintenance page", "site", st.ID, "err", err)
				} else {
					s.event(st.ID, "migrate", "A move that didn't finish left the maintenance page on; taken down")
				}
			}
		}
	}
}

// ---- Target side ----

var prefixOK = regexp.MustCompile(`^[A-Za-z0-9_]{1,32}$`)

// ImportSite creates a site arriving from another server: same ID and
// settings, a fresh database account, its first replica, not served yet.
func (s *Service) ImportSite(ctx context.Context, m *MigrationMeta) (*store.Site, error) {
	if m == nil || m.Site == nil || !siteIDRe.MatchString(m.Site.ID) || !prefixOK.MatchString(m.TablePrefix) {
		return nil, fmt.Errorf("%w: incomplete site description", ErrInvalidInput)
	}
	in := m.Site
	if _, err := s.Store.GetSite(ctx, in.ID); err == nil {
		return nil, fmt.Errorf("%w: site %s already exists here", ErrConflict, in.ID)
	}
	st := *in
	// The description comes from another server (compromised, maybe):
	// every setting is checked as if typed here before it reaches Caddy's
	// config, Docker or the shield.
	if err := s.sanitizeImport(&st); err != nil {
		return nil, err
	}
	st.Status, st.SMTP = StatusImporting, false
	st.SpreadNodes, st.RemoteUpstreams, st.Node = nil, nil, ""
	b, err := s.reserveAs(ctx, &st)
	if err != nil {
		return nil, err
	}
	if err := b.start(ctx, m.TablePrefix, envOf(&st), noProgress); err != nil {
		b.rollback(err)
		return nil, err
	}
	b.undo = nil // from here on, an abort deletes the site as a whole
	return s.Store.GetSite(ctx, st.ID)
}

func envOf(st *store.Site) string {
	if st.ParentID != "" {
		return "staging"
	}
	return ""
}

// reserveAs is reserve for a site that already has its ID (a move): the
// record keeps its settings; ports are this server's.
func (s *Service) reserveAs(ctx context.Context, st *store.Site) (*siteBuild, error) {
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	for _, d := range slices.Concat([]string{st.PrimaryDomain}, st.Domains, st.RedirectDomains) {
		if err := s.domainFreeExcept(ctx, d, st.ID); err != nil {
			return nil, fmt.Errorf("%s: %w", d, err)
		}
	}
	ports, err := s.Store.AllocatePorts(ctx, s.Cfg.SitePortBase, 1, s.heldPorts(ctx)...)
	if err != nil {
		return nil, err
	}
	st.FPMPort, st.Upstreams, st.DBName = ports[0], []int{ports[0]}, "wp_"+st.ID
	redirects := st.RedirectDomains
	st.Domains = slices.DeleteFunc(slices.Clone(st.Domains), func(d string) bool { return slices.Contains(redirects, d) })
	if !slices.Contains(st.Domains, st.PrimaryDomain) {
		st.Domains = append([]string{st.PrimaryDomain}, st.Domains...)
	}
	if err := s.Store.CreateSite(ctx, st); err != nil {
		return nil, err
	}
	for _, d := range redirects {
		if err := s.Store.AddDomain(ctx, st.ID, d, true); err != nil {
			s.Store.DeleteSite(ctx, st.ID)
			return nil, err
		}
	}
	b := &siteBuild{s: s, st: st, dbUser: "u_" + st.ID, dbPass: randString(32, passAlphabet)}
	b.undo = append(b.undo, func() { c, cancel := bgCtx(); defer cancel(); s.Store.DeleteSite(c, st.ID) })
	return b, nil
}

var siteIDRe = regexp.MustCompile(`^s[a-z0-9]{7}$`)

// sanitizeImport checks and normalises a site description from another
// server with the validation the panel applies to what people type.
func (s *Service) sanitizeImport(st *store.Site) error {
	if !slices.Contains(s.Cfg.PHPVersions, st.PHPVersion) {
		return fmt.Errorf("%w: PHP %s is not available on this server", ErrInvalidInput, st.PHPVersion)
	}
	var err error
	if st.PrimaryDomain, err = NormalizeDomain(st.PrimaryDomain); err != nil {
		return err
	}
	for _, l := range []*[]string{&st.Domains, &st.RedirectDomains} {
		for i, d := range *l {
			if (*l)[i], err = NormalizeDomain(d); err != nil {
				return err
			}
		}
	}
	if validName(st.Name) != nil {
		st.Name = st.PrimaryDomain
	}
	if err := s.validateResources(Resources{MemoryMB: st.MemoryMB, CPUs: st.CPUs, Replicas: st.Replicas}); err != nil {
		return err
	}
	if st.MinReplicas < 1 || st.MaxReplicas < st.MinReplicas || st.MaxReplicas > s.Cfg.MaxReplicas {
		st.Autoscale, st.MinReplicas, st.MaxReplicas = false, 1, 1
	}
	if st.TargetCPU < MinTargetCPU || st.TargetCPU > MaxTargetCPU {
		st.TargetCPU = 70
	}
	if !slices.Contains([]string{BurstOff, BurstAuto, BurstOn}, st.BurstMode) || !st.Autoscale {
		// From an older version (autoscaling only), or burst without the
		// autoscaling it runs on.
		st.BurstMode, st.BurstUntil = BurstOff, time.Time{}
		if st.Autoscale {
			st.BurstMode = BurstAuto
		}
	}
	if st.TargetWorkers != 0 && (st.TargetWorkers < MinTargetWorkers || st.TargetWorkers > MaxTargetWorkers) {
		st.TargetWorkers = 0
	}
	if st.TargetResponseMS != 0 && (st.TargetResponseMS < MinTargetMS || st.TargetResponseMS > MaxTargetMS) {
		st.TargetResponseMS = 0
	}
	if err := s.validatePHP(st, PHPInput{Version: st.PHPVersion, Settings: st.PHP}); err != nil {
		return err
	}
	if !shield.Mode(st.ShieldMode).Valid() {
		return fmt.Errorf("%w: shield mode %q", ErrInvalidInput, st.ShieldMode)
	}
	in := ShieldInput{Mode: shield.Mode(st.ShieldMode), BlockAIBots: st.BlockAIBots, WAF: &st.WAF, XMLRPC: &st.XMLRPC,
		AdminAllow: &st.AdminAllow, TrustedIPs: &st.TrustedIPs, DenyIPs: &st.DenyIPs, RateRPS: &st.RateRPS,
		RateBurst: &st.RateBurst, LoginPerMin: &st.LoginPerMin, ChallengeBits: &st.ChallengeBits,
		Reputation: &st.Reputation, CountryMode: &st.CountryMode, Countries: &st.Countries,
		CountryAction: &st.CountryAction, BodyWAF: &st.BodyWAF}
	var c store.ShieldSettings
	if err := applyShieldInput(&c, in); err != nil {
		return err
	}
	st.AdminAllow, st.TrustedIPs, st.DenyIPs, st.Countries = c.AdminAllow, c.TrustedIPs, c.DenyIPs, c.Countries
	st.ImageFormats = slices.DeleteFunc(slices.Clone(st.ImageFormats), func(f string) bool {
		return f != "avif" && f != "webp"
	})
	// From another version of WPGenie: keep only the tweaks this one knows.
	st.Optimize = slices.DeleteFunc(slices.Clone(st.Optimize), func(k string) bool {
		_, err := normalizeOptimizations([]string{k})
		return err != nil
	})
	if !slices.Contains([]string{"off", "security", "all"}, st.AutoUpdate) {
		st.AutoUpdate = "security"
	}
	if st.ParentID != "" {
		return fmt.Errorf("%w: staging sites don't move", ErrInvalidInput)
	}
	return nil
}

// touchImport marks an import as alive every few minutes while a long
// transfer runs (the cleanup loop removes imports that went quiet).
func (s *Service) touchImport(ctx context.Context, id string) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			s.Store.SetSiteStatus(context.WithoutCancel(ctx), id, StatusImporting)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return cancel
}

// ImportPrune deletes what the source no longer has (its manifest on r),
// after the second pass: a plugin removed during the first copy doesn't
// come back on the new server. As the site user, in its container.
func (s *Service) ImportPrune(ctx context.Context, id string, r io.Reader) error {
	if err := s.importing(ctx, id); err != nil {
		return err
	}
	script := `set -e; cd "$1"; want=$(mktemp); have=$(mktemp); trap 'rm -f "$want" "$have"' EXIT
		sort > "$want"
		find .` + findPrunes() + ` -mindepth 1 -print | sort > "$have"
		comm -23 "$have" "$want" | while IFS= read -r p; do rm -rf -- "$p"; done`
	return s.Runtime.Exec(ctx, id, r, nil, "sh", "-c", script, "sh", s.Cfg.SiteRoot(id))
}

// ImportFiles replaces the install with a tar stream (first pass), or
// adds the changed files over it (second pass, overlay).
func (s *Service) ImportFiles(ctx context.Context, id string, overlay bool, r io.Reader) error {
	if err := s.importing(ctx, id); err != nil {
		return err
	}
	defer s.touchImport(ctx, id)()
	if !overlay {
		return s.replaceInstall(ctx, id, 0, keepNone, func(w io.Writer) error {
			_, err := io.Copy(w, r)
			return err
		})
	}
	// Written by the site user in its container, like every restore.
	return s.Runtime.Exec(ctx, id, r, nil, "tar", "-xf", "-", "--no-same-owner", "-C", s.Cfg.SiteRoot(id))
}

// ImportDB loads the site's database (tables it had here and not in the
// source are dropped at FinishImport). The SQL comes from another server,
// which could have been compromised: it is loaded as a temporary account
// with rights on this site's database only (and in the client's sandbox),
// never as root, like a staging push. Triggers or routines another account
// defined make it fail, which a move then reports rather than elevates.
func (s *Service) ImportDB(ctx context.Context, id string, r io.Reader) error {
	if err := s.importing(ctx, id); err != nil {
		return err
	}
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return err
	}
	defer s.touchImport(ctx, id)()
	user, pass := "wpga_"+randString(12, lowerAlnum), randString(32, passAlphabet)
	if err := s.DB.CreateTempUser(ctx, st.DBName, user, pass); err != nil {
		return fmt.Errorf("loading the database: %w", err)
	}
	defer func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if err := s.DB.DropTempUser(c, user); err != nil {
			s.Log.Warn("dropping the import's temporary database account", "user", user, "err", err)
		}
	}()
	return s.Dumper.RestoreAs(ctx, st.DBName, user, pass, r)
}

func (s *Service) importing(ctx context.Context, id string) error {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return err
	}
	if st.Status != StatusImporting {
		return fmt.Errorf("%w: site %s is not being imported", ErrConflict, id)
	}
	return nil
}

// FinishImport puts an imported site live here: its settings, SFTP logins,
// certificate, CDN and backups carried over, and the old server allowed to
// pass visitors on until DNS moves.
func (s *Service) FinishImport(ctx context.Context, m *MigrationMeta, from string) (*store.Site, error) {
	id := m.Site.ID
	if err := s.importing(ctx, id); err != nil {
		return nil, err
	}
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, err
	}
	now, err := s.DB.Tables(ctx, st.DBName)
	if err != nil {
		return nil, err
	}
	var extra []string
	for _, t := range now {
		if !slices.Contains(m.Tables, t) {
			extra = append(extra, t)
		}
	}
	if len(extra) > 0 {
		if err := s.DB.DropTables(ctx, st.DBName, extra); err != nil {
			return nil, err
		}
	}
	for _, u := range m.SFTP {
		// From the old server, checked as if typed here: a login name is a
		// file name and a passwd entry, its keys an authorized_keys file.
		keys, err := sftp.NormalizeKeys(u.PublicKeys)
		if err != nil || !sftp.ValidLogin(id, u.Username) || !sftp.ValidPasswordHash(u.Password) {
			s.event(id, "sftp", fmt.Sprintf("An SFTP login from the old server was not valid here and was left out (%q)", u.Username))
			continue
		}
		if err := s.Store.CreateSFTPUser(ctx, &store.SFTPUser{Username: u.Username, SiteID: id, Password: u.Password,
			PublicKeys: keys}); err != nil {
			return nil, fmt.Errorf("SFTP login %s: %w", u.Username, err)
		}
	}
	if m.CDN != nil {
		c := *m.CDN
		c.SiteID = id
		if err := s.Store.SetCDN(ctx, &c); err != nil {
			return nil, err
		}
	}
	if m.Backup != nil {
		p := *m.Backup
		p.SiteID = id
		if _, err := s.Store.GetRepo(ctx, p.RepoID); err != nil {
			// A destination this server doesn't have (another server's
			// local repository): back up locally instead.
			s.event(id, "backup", "The backup destination didn't move with the site; backing up to this server instead")
			p.RepoID = LocalRepoID
			if _, err := s.ensureLocalRepo(ctx); err != nil {
				s.Log.Warn("creating the local backup repository", "err", err)
			}
		}
		p.LastBackupAt, p.LastAttemptAt, p.LastError = time.Time{}, time.Time{}, ""
		if err := s.Store.SetBackupPolicy(ctx, &p); err != nil {
			return nil, err
		}
	}
	if m.Offload != nil {
		o := *m.Offload
		o.SiteID = id
		// The sync history stays valid: the bucket already holds what it
		// says. Its wrapper is written by finish, with the other wrappers.
		if err := s.Store.SetOffload(ctx, &o, false); err != nil {
			return nil, err
		}
	}
	b := &siteBuild{s: s, st: st, dbUser: "u_" + id}
	if err := b.finish(ctx, noProgress); err != nil {
		return nil, err
	}
	if m.SMTP != nil {
		if err := s.installSMTP(ctx, id, m.SMTP.Host, m.SMTP.Address, m.SMTP.Password); err != nil {
			s.event(id, "mail", "WordPress mail settings didn't carry over: "+err.Error())
		}
	}
	if m.CertPEM != "" {
		if _, err := s.SetCert(ctx, id, CertInput{Certificate: m.CertPEM, Key: m.KeyPEM}); err != nil {
			s.event(id, "certificate", "The site's own certificate didn't carry over: "+err.Error())
		}
	}
	if from != "" {
		if err := s.AllowIngress(ctx, id, from, (forwardDays+1)*24*time.Hour); err != nil {
			return nil, err
		}
		if err := s.Sync(ctx); err != nil {
			return nil, err
		}
	}
	if s.AccessChanged != nil {
		s.AccessChanged(ctx)
	}
	s.event(id, "migrate", "Moved here from server "+from)
	return s.Store.GetSite(ctx, id)
}

// AbortImport deletes a half-imported site.
func (s *Service) AbortImport(ctx context.Context, id string) error {
	st, err := s.Store.GetSite(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.Status != StatusImporting {
		return fmt.Errorf("%w: site %s is not being imported", ErrConflict, id)
	}
	return s.deleteLocal(ctx, id, false)
}

// ---- The move itself (on the control plane) ----

// siteEnd is one side of a move: this server or another one.
type siteEnd interface {
	name() string
	export(ctx context.Context, id string) (*MigrationMeta, error)
	exportFiles(ctx context.Context, id string, since int64, w io.Writer) error
	manifest(ctx context.Context, id string, w io.Writer) error
	prune(ctx context.Context, id string, r io.Reader) error
	remove(ctx context.Context, id string) error
	reactivate(ctx context.Context, id string) error
	exportDB(ctx context.Context, id string, w io.Writer) error
	maintenance(ctx context.Context, id string, on bool) error
	movedTo(ctx context.Context, id string, to cluster.Endpoint) error
	importSite(ctx context.Context, m *MigrationMeta) error
	importFiles(ctx context.Context, id string, overlay bool, r io.Reader) error
	importDB(ctx context.Context, id string, r io.Reader) error
	finish(ctx context.Context, m *MigrationMeta, from string) (*store.Site, error)
	abort(ctx context.Context, id string) error
}

type localEnd struct{ s *Service }

func (l localEnd) name() string { return cluster.LocalNode }
func (l localEnd) export(ctx context.Context, id string) (*MigrationMeta, error) {
	return l.s.ExportMeta(ctx, id)
}
func (l localEnd) exportFiles(ctx context.Context, id string, since int64, w io.Writer) error {
	return l.s.ExportFiles(ctx, id, since, w)
}
func (l localEnd) manifest(ctx context.Context, id string, w io.Writer) error {
	return l.s.ExportManifest(ctx, id, w)
}
func (l localEnd) prune(ctx context.Context, id string, r io.Reader) error {
	return l.s.ImportPrune(ctx, id, r)
}
func (l localEnd) remove(ctx context.Context, id string) error {
	return l.s.deleteLocal(ctx, id, false)
}
func (l localEnd) reactivate(ctx context.Context, id string) error { return l.s.Reactivate(ctx, id) }
func (l localEnd) exportDB(ctx context.Context, id string, w io.Writer) error {
	return l.s.ExportDB(ctx, id, w)
}
func (l localEnd) maintenance(ctx context.Context, id string, on bool) error {
	return l.s.SetMaintenance(ctx, id, on)
}
func (l localEnd) movedTo(ctx context.Context, id string, to cluster.Endpoint) error {
	return l.s.MovedTo(ctx, id, to, forwardDays)
}
func (l localEnd) importSite(ctx context.Context, m *MigrationMeta) error {
	_, err := l.s.ImportSite(ctx, m)
	return err
}
func (l localEnd) importFiles(ctx context.Context, id string, overlay bool, r io.Reader) error {
	return l.s.ImportFiles(ctx, id, overlay, r)
}
func (l localEnd) importDB(ctx context.Context, id string, r io.Reader) error {
	return l.s.ImportDB(ctx, id, r)
}
func (l localEnd) finish(ctx context.Context, m *MigrationMeta, from string) (*store.Site, error) {
	return l.s.FinishImport(ctx, m, from)
}
func (l localEnd) abort(ctx context.Context, id string) error { return l.s.AbortImport(ctx, id) }

// remoteEnd drives another server's cluster endpoints.
type remoteEnd struct {
	c    *cluster.Controller
	node string
}

func (r remoteEnd) name() string { return r.node }

func (r remoteEnd) call(ctx context.Context, method, path string, body, out any) error {
	return r.c.Call(ctx, r.node, method, path, body, out)
}

// stream sends body (may be nil) and copies the answer into w (may be nil).
func (r remoteEnd) stream(ctx context.Context, method, path string, body io.Reader, w io.Writer) error {
	cl := r.c.Client()
	if cl == nil {
		return cluster.ErrNoCluster
	}
	ep, err := r.c.Endpoint(ctx, r.node)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+ep.Address+path, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.ContentLength = -1
	}
	resp, err := cl.Do(ep, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return &cluster.StatusError{Code: resp.StatusCode, Msg: string(msg)}
	}
	if w == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	// A stream the node cut half-way ends without its trailer: report it
	// rather than pass a truncated archive on as complete.
	if _, err := io.Copy(w, resp.Body); err != nil {
		return err
	}
	if e := resp.Trailer.Get(trailerError); e != "" {
		return errors.New(e)
	}
	if resp.Trailer.Get(trailerDone) != "1" {
		return errors.New("the transfer ended before the node finished it")
	}
	return nil
}

func (r remoteEnd) export(ctx context.Context, id string) (*MigrationMeta, error) {
	var m MigrationMeta
	return &m, r.call(ctx, http.MethodGet, "/cluster/v1/sites/"+id+"/export", nil, &m)
}
func (r remoteEnd) exportFiles(ctx context.Context, id string, since int64, w io.Writer) error {
	return r.stream(ctx, http.MethodGet, "/cluster/v1/sites/"+id+"/export/files?since="+strconv.FormatInt(since, 10), nil, w)
}
func (r remoteEnd) manifest(ctx context.Context, id string, w io.Writer) error {
	return r.stream(ctx, http.MethodGet, "/cluster/v1/sites/"+id+"/export/manifest", nil, w)
}
func (r remoteEnd) prune(ctx context.Context, id string, body io.Reader) error {
	return r.stream(ctx, http.MethodPut, "/cluster/v1/sites/"+id+"/import/prune", body, nil)
}
func (r remoteEnd) remove(ctx context.Context, id string) error {
	err := r.call(ctx, http.MethodDelete, "/cluster/v1/sites/"+id, nil, nil)
	var se *cluster.StatusError
	if errors.As(err, &se) && se.Code == http.StatusNotFound {
		return nil
	}
	return err
}
func (r remoteEnd) reactivate(ctx context.Context, id string) error {
	return r.call(ctx, http.MethodPost, "/cluster/v1/sites/"+id+"/reactivate", nil, nil)
}
func (r remoteEnd) exportDB(ctx context.Context, id string, w io.Writer) error {
	return r.stream(ctx, http.MethodGet, "/cluster/v1/sites/"+id+"/export/db", nil, w)
}
func (r remoteEnd) maintenance(ctx context.Context, id string, on bool) error {
	return r.call(ctx, http.MethodPut, "/cluster/v1/sites/"+id+"/maintenance", map[string]bool{"on": on}, nil)
}
func (r remoteEnd) movedTo(ctx context.Context, id string, to cluster.Endpoint) error {
	return r.call(ctx, http.MethodPut, "/cluster/v1/sites/"+id+"/moved", movedInput{Node: to.ID, Address: to.Address, Days: forwardDays}, nil)
}
func (r remoteEnd) importSite(ctx context.Context, m *MigrationMeta) error {
	return r.call(ctx, http.MethodPost, "/cluster/v1/import", m, nil)
}
func (r remoteEnd) importFiles(ctx context.Context, id string, overlay bool, body io.Reader) error {
	q := ""
	if overlay {
		q = "?overlay=1"
	}
	return r.stream(ctx, http.MethodPut, "/cluster/v1/sites/"+id+"/import/files"+q, body, nil)
}
func (r remoteEnd) importDB(ctx context.Context, id string, body io.Reader) error {
	return r.stream(ctx, http.MethodPut, "/cluster/v1/sites/"+id+"/import/db", body, nil)
}
func (r remoteEnd) finish(ctx context.Context, m *MigrationMeta, from string) (*store.Site, error) {
	var st store.Site
	err := r.call(ctx, http.MethodPost, "/cluster/v1/sites/"+m.Site.ID+"/import/finish?from="+url.QueryEscape(from), m, &st)
	return &st, err
}
func (r remoteEnd) abort(ctx context.Context, id string) error {
	return r.call(ctx, http.MethodDelete, "/cluster/v1/sites/"+id+"/import", nil, nil)
}

type movedInput struct {
	Node    string `json:"node"`
	Address string `json:"address"`
	Days    int    `json:"days"`
}

func (s *Service) end(node string) siteEnd {
	if node == "" || node == cluster.LocalNode {
		return localEnd{s}
	}
	return remoteEnd{c: s.Cluster, node: node}
}

// endpointOf is how the source reaches the target for forwarding.
func (s *Service) endpointOf(ctx context.Context, node string) (cluster.Endpoint, error) {
	if node == cluster.LocalNode {
		addr, err := s.Store.Setting(ctx, "cluster_control_address")
		if err != nil {
			return cluster.Endpoint{}, err
		}
		if addr == "" {
			return cluster.Endpoint{}, fmt.Errorf("%w: set cluster_address in the panel's config.json (the address nodes reach it at) to move sites to it", ErrInvalidInput)
		}
		return cluster.Endpoint{ID: cluster.LocalNode, Address: addr}, nil
	}
	return s.Cluster.Endpoint(ctx, node)
}

// StartMigration moves a site to another server as a job (see the file
// comment). The panel's own server is "local".
func (s *Service) StartMigration(ctx context.Context, id, target string) (int64, error) {
	if s.Cluster == nil || !s.Cluster.Enabled() {
		return 0, fmt.Errorf("%w: no other servers have been added", ErrInvalidInput)
	}
	source := cluster.LocalNode
	if node, remote, err := s.Cluster.SiteNode(ctx, id); err != nil {
		return 0, err
	} else if remote {
		source = node
	} else if _, err := s.Store.GetSite(ctx, id); err != nil {
		return 0, err
	}
	if target == "" || target == source {
		return 0, fmt.Errorf("%w: choose another server to move the site to", ErrInvalidInput)
	}
	if target != cluster.LocalNode {
		n, err := s.Store.GetNode(ctx, target)
		if err != nil {
			return 0, err
		}
		if n.Status != store.NodeActive {
			return 0, fmt.Errorf("%w: server %s is %s", ErrInvalidInput, n.ID, n.Status)
		}
	}
	src, dst := s.end(source), s.end(target)
	// Checked before the job: an obvious refusal is an immediate error.
	meta, err := src.export(ctx, id)
	if err != nil {
		return 0, err
	}
	to, err := s.endpointOf(ctx, target)
	if err != nil {
		return 0, err
	}
	release, err := s.claimMove(id)
	if err != nil {
		return 0, err
	}
	// Big sites take hours to copy; the default job timeout is two.
	spec := jobs.Spec{SiteID: id, Kind: "migrate", Heavy: true, Timeout: 24 * time.Hour}
	if source == cluster.LocalNode {
		spec.Lock = jobs.LockFunc(s.maintLock(id))
	}
	jobID, err := s.Jobs.Submit(ctx, spec, func(ctx context.Context, t *jobs.Task) error {
		defer release()
		return s.migrate(ctx, t, id, meta, src, dst, to)
	})
	if err != nil {
		release()
	}
	return jobID, err
}

// moving: site ID -> a move in progress (on the panel), so two moves of a
// site (a double click, a drain and a manual move) never run at once: both
// would import it and the registry would flip between two live copies.
var moving sync.Map

func (s *Service) claimMove(id string) (release func(), err error) {
	if _, busy := moving.LoadOrStore(id, true); busy {
		return nil, fmt.Errorf("%w: this site is already being moved", ErrConflict)
	}
	return func() { moving.Delete(id) }, nil
}

// progressTask is what a move reports to: its own job, or a drain's.
type progressTask interface {
	Progress(pct int, step string)
	SetResult(v any)
}

// quietTask keeps a move inside a drain from overwriting the drain's
// progress and result.
type quietTask struct{ *jobs.Task }

func (quietTask) Progress(int, string) {}
func (quietTask) SetResult(any)        {}

func (s *Service) migrate(ctx context.Context, t progressTask, id string, meta *MigrationMeta, src, dst siteEnd, to cluster.Endpoint) (err error) {
	// How far the move got, for the rollback: each step undoes what came
	// before it, so a failure anywhere leaves the site where it was.
	const (
		nothing  = iota
		imported // the target has an importing copy
		live     // the target copy is live
		switched // the registry points at the target
		retiring // the source was asked to retire (may have, partly)
	)
	phase := nothing
	maint := false
	source := meta.Site
	defer func() {
		if err == nil {
			return
		}
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
		var rerrs []error
		if phase >= retiring {
			if rerr := src.reactivate(c, id); rerr != nil {
				rerrs = append(rerrs, fmt.Errorf("serving again on %s: %w", src.name(), rerr))
			}
		}
		if phase >= switched {
			if rerr := s.recordHome(c, id, src.name(), source); rerr != nil {
				rerrs = append(rerrs, fmt.Errorf("recording the site back on %s: %w", src.name(), rerr))
			}
		}
		switch {
		case phase >= live:
			if rerr := dst.remove(c, id); rerr != nil {
				rerrs = append(rerrs, fmt.Errorf("removing the copy on %s: %w", dst.name(), rerr))
			}
		case phase >= imported:
			if rerr := dst.abort(c, id); rerr != nil {
				rerrs = append(rerrs, fmt.Errorf("removing the half-imported copy on %s: %w", dst.name(), rerr))
			}
		}
		if maint && phase < retiring {
			if merr := src.maintenance(c, id, false); merr != nil {
				rerrs = append(rerrs, fmt.Errorf("the site is still in maintenance mode on %s: %w", src.name(), merr))
			}
		}
		if len(rerrs) > 0 {
			s.Log.Error("migration: rolling back", "site", id, "err", errors.Join(rerrs...))
			err = errors.Join(err, errors.Join(rerrs...))
		}
	}()
	t.Progress(5, "Creating the site on "+dst.name())
	if err := dst.importSite(ctx, meta); err != nil {
		return fmt.Errorf("creating the site on %s: %w", dst.name(), err)
	}
	phase = imported
	passStart := time.Now().Add(-time.Minute) // clock skew between servers: copy a little more
	t.Progress(15, "Copying files")
	if err := pipe(func(w io.Writer) error { return src.exportFiles(ctx, id, 0, w) },
		func(r io.Reader) error { return dst.importFiles(ctx, id, false, r) }); err != nil {
		return fmt.Errorf("copying files: %w", err)
	}
	t.Progress(50, "Copying the database")
	if err := pipe(func(w io.Writer) error { return src.exportDB(ctx, id, w) },
		func(r io.Reader) error { return dst.importDB(ctx, id, r) }); err != nil {
		return fmt.Errorf("copying the database: %w", err)
	}

	t.Progress(65, "Final copy (the site shows a maintenance page for this step)")
	if err := src.maintenance(ctx, id, true); err != nil {
		return fmt.Errorf("maintenance mode: %w", err)
	}
	maint = true
	// Keep the page fresh: the source's cleanup takes down a move's page
	// that went stale (a panel that died mid-move).
	refresh, stopRefresh := context.WithCancel(ctx)
	go func() {
		tk := time.NewTicker(30 * time.Minute)
		defer tk.Stop()
		for {
			select {
			case <-refresh.Done():
				return
			case <-tk.C:
				src.maintenance(refresh, id, true)
			}
		}
	}()
	defer stopRefresh()
	if meta, err = src.export(ctx, id); err != nil { // tables may have changed
		return err
	}
	if err := pipe(func(w io.Writer) error { return src.exportDB(ctx, id, w) },
		func(r io.Reader) error { return dst.importDB(ctx, id, r) }); err != nil {
		return fmt.Errorf("copying the database again: %w", err)
	}
	if err := pipe(func(w io.Writer) error { return src.exportFiles(ctx, id, passStart.Unix(), w) },
		func(r io.Reader) error { return dst.importFiles(ctx, id, true, r) }); err != nil {
		return fmt.Errorf("copying changed files: %w", err)
	}
	if err := pipe(func(w io.Writer) error { return src.manifest(ctx, id, w) },
		func(r io.Reader) error { return dst.prune(ctx, id, r) }); err != nil {
		return fmt.Errorf("removing files deleted meanwhile: %w", err)
	}

	t.Progress(85, "Going live on "+dst.name())
	st, err := dst.finish(ctx, meta, src.name())
	if err != nil {
		phase = live // it may have got as far as serving
		return fmt.Errorf("finishing on %s: %w", dst.name(), err)
	}
	phase = live
	if err := s.recordHome(ctx, id, dst.name(), st); err != nil {
		return fmt.Errorf("recording the new server: %w", err)
	}
	phase = switched
	t.Progress(95, "Forwarding visitors from the old server")
	phase = retiring
	if err := src.movedTo(ctx, id, to); err != nil {
		return fmt.Errorf("retiring the old copy on %s: %w", src.name(), err)
	}
	stopRefresh()
	post := context.WithoutCancel(ctx)
	ip := ""
	if n, err := s.Store.GetNode(post, dst.name()); err == nil {
		ip = n.PublicIP
	}
	mv, _ := json.Marshal(Move{From: src.name(), To: dst.name(), At: time.Now().UTC(), PointDNSTo: ip})
	if err := s.Store.SetSetting(post, moveKey(id), string(mv)); err != nil {
		s.Log.Warn("recording a move", "site", id, "err", err)
	}
	t.SetResult(map[string]string{"site_id": id, "node": dst.name(), "point_dns_to": ip})
	return nil
}

// recordHome points the registry at the server a site lives on (the
// panel's own: no entry).
func (s *Service) recordHome(ctx context.Context, id, node string, st *store.Site) error {
	if node == cluster.LocalNode {
		return s.Store.DeleteClusterSite(ctx, id)
	}
	if s.Cluster == nil {
		return errNotClustered
	}
	return s.Cluster.Register(ctx, node, st)
}

// Move is the last move of a site, as the panel remembers it: the old
// server forwards visitors until the move is finished (or forwardDays).
type Move struct {
	From       string    `json:"from"`
	To         string    `json:"to"`
	At         time.Time `json:"at"`
	PointDNSTo string    `json:"point_dns_to,omitempty"`
}

func moveKey(id string) string { return "cluster_move:" + id }

// LastMove is a site's last move, if the old copy may still exist.
func (s *Service) LastMove(ctx context.Context, id string) (*Move, error) {
	v, err := s.Store.Setting(ctx, moveKey(id))
	if err != nil || v == "" {
		return nil, err
	}
	var m Move
	if err := json.Unmarshal([]byte(v), &m); err != nil {
		return nil, err
	}
	if time.Since(m.At) > (forwardDays+1)*24*time.Hour {
		return nil, nil
	}
	return &m, nil
}

// FinishMove ends the old server's forward and deletes the old copy now
// (DNS points at the new server).
func (s *Service) FinishMove(ctx context.Context, id string) error {
	m, err := s.LastMove(ctx, id)
	if err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("%w: this site has no move in progress", store.ErrNotFound)
	}
	if m.From == cluster.LocalNode {
		err = s.EndForward(ctx, id, true)
	} else {
		err = s.Cluster.Call(ctx, m.From, http.MethodDelete, "/cluster/v1/sites/"+id+"/moved?purge=1", nil, nil)
	}
	if err != nil {
		return err
	}
	return s.Store.SetSetting(ctx, moveKey(id), "")
}

// StartDrain moves every site off a server (set to draining, so placement
// skips it) to wherever placement puts each, one after the other, as a job.
func (s *Service) StartDrain(ctx context.Context, node string) (int64, error) {
	if s.Cluster == nil || !s.Cluster.Enabled() {
		return 0, fmt.Errorf("%w: no other servers have been added", ErrInvalidInput)
	}
	if node == cluster.LocalNode {
		return 0, fmt.Errorf("%w: the panel's own server can't be drained from here: move its sites one by one", ErrInvalidInput)
	}
	n, err := s.Store.GetNode(ctx, node)
	if err != nil {
		return 0, err
	}
	if n.Status != store.NodeDraining {
		if err := s.Store.UpdateNode(ctx, n.ID, n.Name, n.Address, n.PublicIP, store.NodeDraining); err != nil {
			return 0, err
		}
	}
	return s.Jobs.Submit(ctx, jobs.Spec{Kind: "drain", Heavy: true, Timeout: 7 * 24 * time.Hour}, func(ctx context.Context, t *jobs.Task) error {
		sites, err := s.Store.ClusterSites(ctx, node)
		if err != nil {
			return err
		}
		var failed []string
		for i, cs := range sites {
			t.Progress(i*100/max(len(sites), 1), fmt.Sprintf("Moving %s (%d of %d)", cs.PrimaryDomain, i+1, len(sites)))
			target, err := s.Cluster.Place(ctx)
			if err == nil && target == node {
				err = errors.New("no other server can take it")
			}
			if err == nil {
				err = s.migrateNow(ctx, t, cs.SiteID, node, target)
			}
			if err != nil {
				failed = append(failed, cs.PrimaryDomain+": "+err.Error())
				s.Log.Warn("drain: moving a site", "site", cs.SiteID, "err", err)
			}
		}
		if len(failed) > 0 {
			return fmt.Errorf("%d of %d sites couldn't be moved: %s", len(failed), len(sites), strings.Join(failed, "; "))
		}
		return nil
	})
}

// migrateNow runs a move inside another job (a drain).
func (s *Service) migrateNow(ctx context.Context, t *jobs.Task, id, source, target string) error {
	release, err := s.claimMove(id)
	if err != nil {
		return err
	}
	defer release()
	src, dst := s.end(source), s.end(target)
	meta, err := src.export(ctx, id)
	if err != nil {
		return err
	}
	to, err := s.endpointOf(ctx, target)
	if err != nil {
		return err
	}
	return s.migrate(ctx, quietTask{t}, id, meta, src, dst, to)
}

// ---- HTTP (cluster endpoints) ----

// Streams carry trailers: a node that fails half-way through a download
// says so instead of ending what looks like a complete archive.
const (
	trailerDone  = "X-Wpgenie-Done"
	trailerError = "X-Wpgenie-Error"
)

func streamOut(w http.ResponseWriter, produce func(io.Writer) error) {
	w.Header().Set("Trailer", trailerDone+", "+trailerError)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	if err := produce(w); err != nil {
		w.Header().Set(trailerError, strings.ReplaceAll(err.Error(), "\n", " "))
		return
	}
	w.Header().Set(trailerDone, "1")
}

func (s *Service) migrationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /cluster/v1/sites/{id}/export", func(w http.ResponseWriter, r *http.Request) {
		m, err := s.ExportMeta(r.Context(), r.PathValue("id"))
		if err != nil {
			clusterError(w, err)
			return
		}
		writeClusterJSON(w, http.StatusOK, m)
	})
	mux.HandleFunc("GET /cluster/v1/sites/{id}/export/files", func(w http.ResponseWriter, r *http.Request) {
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		streamOut(w, func(out io.Writer) error { return s.ExportFiles(r.Context(), r.PathValue("id"), since, out) })
	})
	mux.HandleFunc("GET /cluster/v1/sites/{id}/export/manifest", func(w http.ResponseWriter, r *http.Request) {
		streamOut(w, func(out io.Writer) error { return s.ExportManifest(r.Context(), r.PathValue("id"), out) })
	})
	mux.HandleFunc("PUT /cluster/v1/sites/{id}/import/prune", func(w http.ResponseWriter, r *http.Request) {
		if err := s.ImportPrune(r.Context(), r.PathValue("id"), r.Body); err != nil {
			clusterError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /cluster/v1/sites/{id}/export/db", func(w http.ResponseWriter, r *http.Request) {
		streamOut(w, func(out io.Writer) error { return s.ExportDB(r.Context(), r.PathValue("id"), out) })
	})
	mux.HandleFunc("PUT /cluster/v1/sites/{id}/maintenance", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			On bool `json:"on"`
		}
		if decodeCluster(w, r, &in) != nil {
			return
		}
		if err := s.SetMaintenance(r.Context(), r.PathValue("id"), in.On); err != nil {
			clusterError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PUT /cluster/v1/sites/{id}/moved", func(w http.ResponseWriter, r *http.Request) {
		var in movedInput
		if decodeCluster(w, r, &in) != nil {
			return
		}
		if in.Node != cluster.LocalNode && !cluster.ValidNodeID(in.Node) {
			clusterError(w, fmt.Errorf("%w: node", ErrInvalidInput))
			return
		}
		if err := s.MovedTo(r.Context(), r.PathValue("id"), cluster.Endpoint{ID: in.Node, Address: in.Address}, in.Days); err != nil {
			clusterError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /cluster/v1/sites/{id}/moved", func(w http.ResponseWriter, r *http.Request) {
		if err := s.EndForward(r.Context(), r.PathValue("id"), r.URL.Query().Get("purge") == "1"); err != nil {
			clusterError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /cluster/v1/sites/{id}/reactivate", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Reactivate(r.Context(), r.PathValue("id")); err != nil {
			clusterError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /cluster/v1/import", func(w http.ResponseWriter, r *http.Request) {
		var m MigrationMeta
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&m); err != nil {
			clusterError(w, fmt.Errorf("%w: %v", ErrInvalidInput, err))
			return
		}
		st, err := s.ImportSite(r.Context(), &m)
		if err != nil {
			clusterError(w, err)
			return
		}
		writeClusterJSON(w, http.StatusCreated, st)
	})
	mux.HandleFunc("PUT /cluster/v1/sites/{id}/import/files", func(w http.ResponseWriter, r *http.Request) {
		if err := s.ImportFiles(r.Context(), r.PathValue("id"), r.URL.Query().Get("overlay") == "1", r.Body); err != nil {
			clusterError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PUT /cluster/v1/sites/{id}/import/db", func(w http.ResponseWriter, r *http.Request) {
		if err := s.ImportDB(r.Context(), r.PathValue("id"), r.Body); err != nil {
			clusterError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /cluster/v1/sites/{id}/import/finish", func(w http.ResponseWriter, r *http.Request) {
		var m MigrationMeta
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&m); err != nil || m.Site == nil ||
			m.Site.ID != r.PathValue("id") {
			clusterError(w, fmt.Errorf("%w: site description", ErrInvalidInput))
			return
		}
		st, err := s.FinishImport(r.Context(), &m, r.URL.Query().Get("from"))
		if err != nil {
			clusterError(w, err)
			return
		}
		writeClusterJSON(w, http.StatusOK, st)
	})
	mux.HandleFunc("DELETE /cluster/v1/sites/{id}/import", func(w http.ResponseWriter, r *http.Request) {
		if err := s.AbortImport(r.Context(), r.PathValue("id")); err != nil {
			clusterError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
