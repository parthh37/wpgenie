package site

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/parthh37/wpgenie/internal/jobs"
	"github.com/parthh37/wpgenie/internal/store"
)

// PHP versions and per-site PHP settings. Both change the site's replicas
// through the usual blue/green reconcile (no downtime); a version switch is
// health-checked and switched back if it breaks a working site (a plugin
// using something the new PHP removed).

// ImageBuilder builds the PHP image of a version (runtime.Docker).
type ImageBuilder interface {
	ImageExists(ctx context.Context, image string) bool
	Build(ctx context.Context, tag, dir string, buildArgs ...string) error
}

// PHPInput is a site's PHP version and settings.
type PHPInput struct {
	Version  string            `json:"version"`
	Settings store.PHPSettings `json:"settings"`
}

func (s *Service) validatePHP(st *store.Site, in PHPInput) error {
	if !slices.Contains(s.Cfg.PHPVersions, in.Version) {
		return fmt.Errorf("%w: PHP version must be one of %v", ErrInvalidInput, s.Cfg.PHPVersions)
	}
	p := in.Settings
	checks := []struct {
		v, lo, hi int
		what      string
	}{
		{p.MemoryLimitMB, 64, min(2048, st.MemoryMB), "memory_limit_mb (at most the replica's memory)"},
		{p.UploadMaxMB, 1, 2048, "upload_max_mb"},
		{p.MaxExecutionTime, 10, 600, "max_execution_time"},
		{p.MaxInputVars, 1000, 100000, "max_input_vars"},
	}
	for _, c := range checks {
		if c.v != 0 && (c.v < c.lo || c.v > c.hi) {
			return fmt.Errorf("%w: %s must be 0 (default) or between %d and %d", ErrInvalidInput, c.what, c.lo, c.hi)
		}
	}
	return nil
}

// phpEnv turns settings into the variables images/php/pool.conf reads.
func phpEnv(p store.PHPSettings) []string {
	var env []string
	if p.MemoryLimitMB > 0 {
		env = append(env, "WPG_MEMORY_LIMIT="+strconv.Itoa(p.MemoryLimitMB)+"M")
	}
	if p.UploadMaxMB > 0 {
		// The request carrying the upload is a little larger than the file.
		env = append(env, "WPG_UPLOAD_MAX="+strconv.Itoa(p.UploadMaxMB)+"M",
			"WPG_POST_MAX="+strconv.Itoa(p.UploadMaxMB+8)+"M")
	}
	if p.MaxExecutionTime > 0 {
		// FPM kills a request after request_terminate_timeout whatever PHP's
		// own limit says: keep it above max_execution_time.
		env = append(env, "WPG_MAX_EXECUTION_TIME="+strconv.Itoa(p.MaxExecutionTime),
			"WPG_REQUEST_TIMEOUT="+strconv.Itoa(max(120, p.MaxExecutionTime+30))+"s")
	}
	if p.MaxInputVars > 0 {
		env = append(env, "WPG_MAX_INPUT_VARS="+strconv.Itoa(p.MaxInputVars))
	}
	return env
}

// ensurePHPImage builds a version's image if it isn't there (several
// minutes, once per version).
func (s *Service) ensurePHPImage(ctx context.Context, version string, report Progress) error {
	image := s.Cfg.PHPImageFor(version)
	if s.Images == nil || s.Images.ImageExists(ctx, image) {
		return nil
	}
	m, _ := s.builds.LoadOrStore(version, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	if s.Images.ImageExists(ctx, image) { // built while we waited
		return nil
	}
	report(5, "Building the PHP "+version+" image (a few minutes, only the first time)")
	if err := s.Images.Build(ctx, image, filepath.Join(s.Cfg.ImagesDir, "php"), "PHP_VERSION="+version); err != nil {
		return fmt.Errorf("building the PHP %s image: %w", version, err)
	}
	return nil
}

// PHPResult is recorded with a PHP change job.
type PHPResult struct {
	From       string `json:"from"`
	To         string `json:"to"`
	Before     Health `json:"before"`
	After      Health `json:"after"`
	RolledBack bool   `json:"rolled_back"`
}

// StartPHPChange switches a site's PHP version and/or settings as a job.
func (s *Service) StartPHPChange(ctx context.Context, id string, in PHPInput) (int64, error) {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return 0, err
	}
	if st.Status != store.StatusActive {
		return 0, fmt.Errorf("%w: site is %s", ErrInvalidInput, st.Status)
	}
	if in.Version == "" {
		in.Version = st.PHPVersion
	}
	if err := s.validatePHP(st, in); err != nil {
		return 0, err
	}
	return s.Jobs.Submit(ctx, s.siteJob(id, "php", false), func(ctx context.Context, t *jobs.Task) error {
		return s.changePHP(ctx, id, in, t)
	})
}

// task is what a job function uses of jobs.Task.
type task interface {
	Progress(pct int, step string)
	SetResult(v any)
}

func (s *Service) changePHP(ctx context.Context, id string, in PHPInput, t task) error {
	cur, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return err
	}
	res := &PHPResult{From: cur.PHPVersion, To: in.Version}
	t.SetResult(res)
	if err := s.ensurePHPImage(ctx, in.Version, t.Progress); err != nil {
		return err
	}
	old := PHPInput{Version: cur.PHPVersion, Settings: cur.PHP}
	switched := in.Version != old.Version
	if switched {
		t.Progress(60, "Checking the site before switching")
		res.Before = s.Prober.Probe(ctx, cur.PrimaryDomain)
	}
	t.Progress(70, "Starting PHP "+in.Version+" replicas")
	if err := s.applyPHP(ctx, id, in, old); err != nil {
		return err
	}
	if !switched {
		s.event(id, "php", "PHP settings changed")
		return nil
	}
	t.Progress(90, "Checking the site on PHP "+in.Version)
	res.After = s.Prober.Probe(ctx, cur.PrimaryDomain)
	if res.Before.OK && !res.After.OK {
		t.Progress(95, "The site broke on PHP "+in.Version+": switching back")
		if err := s.applyPHP(context.WithoutCancel(ctx), id, old, in); err != nil {
			return fmt.Errorf("the site failed its health check on PHP %s (%s) and switching back FAILED: %w",
				in.Version, res.After.Detail, err)
		}
		res.RolledBack = true
		msg := fmt.Sprintf("Switched back to PHP %s: on PHP %s the site failed its health check (%s)",
			old.Version, in.Version, res.After.Detail)
		s.event(id, "php", msg)
		return errors.New(msg)
	}
	s.event(id, "php", fmt.Sprintf("PHP %s → %s", old.Version, in.Version))
	return nil
}

// applyPHP records the PHP settings and rolls the replicas onto them; if
// that fails, the previous settings are recorded again.
func (s *Service) applyPHP(ctx context.Context, id string, in, prev PHPInput) error {
	_, err := s.scaleWithUndo(ctx, id, func(st *store.Site) (Resources, error) {
		if err := s.Store.SetPHP(ctx, id, in.Version, in.Settings); err != nil {
			return Resources{}, err
		}
		st.PHPVersion, st.PHP = in.Version, in.Settings
		return Resources{st.MemoryMB, st.CPUs, st.Replicas}, nil
	}, func(c context.Context) error { return s.Store.SetPHP(c, id, prev.Version, prev.Settings) })
	return err
}

// buildImagesInUse rebuilds the images of the non-default PHP versions
// (after WPGenie updated itself, so they get the new sources too): the ones
// sites use, and the ones already built, which ensurePHPImage would hand a
// site switching later as they are. A failed rebuild leaves its sites on the
// previous image; each of them says so.
func (s *Service) buildImagesInUse(ctx context.Context, sites []*store.Site) {
	if s.Images == nil {
		return
	}
	users := map[string][]string{}
	versions := slices.Clone(s.Cfg.PHPVersions)
	for _, st := range sites {
		if st.Status != store.StatusActive || st.PHPVersion == "" {
			continue
		}
		if !slices.Contains(versions, st.PHPVersion) { // dropped from the config, still in use
			versions = append(versions, st.PHPVersion)
		}
		users[st.PHPVersion] = append(users[st.PHPVersion], st.ID)
	}
	for _, v := range versions {
		image := s.Cfg.PHPImageFor(v)
		if v == s.Cfg.DefaultPHPVersion() || (len(users[v]) == 0 && !s.Images.ImageExists(ctx, image)) {
			continue
		}
		if err := s.Images.Build(ctx, image, filepath.Join(s.Cfg.ImagesDir, "php"), "PHP_VERSION="+v); err != nil {
			s.Log.Error("rebuilding a PHP image", "version", v, "err", err)
			for _, id := range users[v] {
				s.event(id, "update", "Rebuilding the PHP "+v+" image failed, so the site stays on the previous one: "+err.Error())
			}
		}
	}
}

// phpSecurityFloor is each PHP line's first release with PHP2Shell fixed
// (CVE-2026-109618, CVE-2026-109619: unauthenticated remote code execution
// and cross-request memory disclosure, in every PHP since 5.4). Lines older
// than these are out of support and never got the fix. images/php and
// images/phpmyadmin refuse to build below the same floors.
var phpSecurityFloor = map[string]string{"8.2": "8.2.35", "8.3": "8.3.36", "8.4": "8.4.27", "8.5": "8.5.12"}

// phpUnpatched reports whether a PHP version (PHP_VERSION, e.g. "8.3.30") is
// below its line's security floor, and the release that fixes it ("" for a
// line too old to get one). Lines newer than the table came out after the
// fix; a version that doesn't parse isn't judged.
func phpUnpatched(v string) (fixedIn string, unpatched bool) {
	n, ok := phpVersionParts(v)
	if !ok {
		return "", false
	}
	if floor, ok := phpSecurityFloor[fmt.Sprintf("%d.%d", n[0], n[1])]; ok {
		f, _ := phpVersionParts(floor)
		return floor, n[2] < f[2]
	}
	for line := range phpSecurityFloor {
		l, _ := phpVersionParts(line + ".0")
		if n[0] < l[0] || (n[0] == l[0] && n[1] < l[1]) {
			return "", true // older than a fixed line and not one itself: out of support
		}
	}
	return "", false // newer than every fixed line
}

// phpVersionParts parses major.minor.patch, ignoring a suffix ("8.5.0RC1",
// "8.3.36-dev").
func phpVersionParts(v string) ([3]int, bool) {
	var n [3]int
	parts := strings.SplitN(v, ".", 3)
	if len(parts) != 3 {
		return n, false
	}
	for i, p := range parts {
		if end := strings.IndexFunc(p, func(r rune) bool { return r < '0' || r > '9' }); end >= 0 {
			p = p[:end]
		}
		x, err := strconv.Atoi(p)
		if err != nil {
			return n, false
		}
		n[i] = x
	}
	return n, true
}
