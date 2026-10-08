package site

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/parthh37/wpgenie/internal/jobs"
)

// Themes: list what's installed, switch, delete unused ones and install
// from wordpress.org, through WP-CLI. Updates go through the update
// manager (StartUpdate: snapshot, health check, rollback) like plugins'.

// Theme is one installed theme.
type Theme struct {
	Slug    string `json:"slug"`
	Title   string `json:"title"`
	Version string `json:"version"`
	// Status: active, parent (of the active child theme) or inactive.
	Status        string `json:"status"`
	UpdateVersion string `json:"update_version,omitempty"`
}

var (
	// installedThemeRe: a theme's directory name as WP-CLI lists it.
	installedThemeRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,99}$`)
	// themeDirectoryRe: a wordpress.org theme slug. No leading dash: WP-CLI
	// would read the slug as an option.
	themeDirectoryRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,99}$`)
)

const themeInstallTimeout = 10 * time.Minute

// parseThemes reads `wp theme list --format=json` (fields name, title,
// status, version, update, update_version): active first, then its
// parent, then by title.
func parseThemes(items []wpThemeItem) []Theme {
	out := make([]Theme, 0, len(items))
	for _, it := range items {
		if !installedThemeRe.MatchString(it.Name) {
			continue // not something the panel could act on
		}
		t := Theme{Slug: it.Name, Title: it.Title, Version: it.Version, Status: it.Status}
		if t.Title == "" {
			t.Title = it.Name
		}
		if it.Update == "available" {
			t.UpdateVersion = it.UpdateVersion
		}
		out = append(out, t)
	}
	rank := map[string]int{"active": 0, "parent": 1}
	slices.SortStableFunc(out, func(a, b Theme) int {
		ra, ok := rank[a.Status]
		if !ok {
			ra = 2
		}
		rb, ok := rank[b.Status]
		if !ok {
			rb = 2
		}
		if c := cmp.Compare(ra, rb); c != 0 {
			return c
		}
		return cmp.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title))
	})
	return out
}

type wpThemeItem struct {
	Name          string   `json:"name"`
	Title         string   `json:"title"`
	Status        string   `json:"status"`
	Version       string   `json:"version"`
	Update        wpUpdate `json:"update"`
	UpdateVersion string   `json:"update_version"`
}

// Themes lists the site's installed themes.
func (s *Service) Themes(ctx context.Context, id string) ([]Theme, error) {
	if err := s.requireActive(ctx, id); err != nil {
		return nil, err
	}
	return s.themes(ctx, id)
}

func (s *Service) themes(ctx context.Context, id string) ([]Theme, error) {
	var items []wpThemeItem
	if err := s.wpJSON(ctx, id, &items, "theme", "list", "--format=json",
		"--fields=name,title,status,version,update,update_version"); err != nil {
		return nil, fmt.Errorf("listing themes: %w", err)
	}
	return parseThemes(items), nil
}

// installedTheme finds an installed theme by slug.
func (s *Service) installedTheme(ctx context.Context, id, slug string) (*Theme, error) {
	if !installedThemeRe.MatchString(slug) {
		return nil, fmt.Errorf("%w: theme %q", ErrInvalidInput, slug)
	}
	list, err := s.themes(ctx, id)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(list, func(t Theme) bool { return t.Slug == slug })
	if i < 0 {
		return nil, fmt.Errorf("%w: no theme %q is installed", ErrInvalidInput, slug)
	}
	return &list[i], nil
}

// themeLocked runs fn under the site's maintenance lock: theme changes
// must not overlap an update (whose rollback would undo them) or a backup.
func (s *Service) themeLocked(ctx context.Context, id string, fn func() error) error {
	if err := s.requireActive(ctx, id); err != nil {
		return err
	}
	lock := s.maintLock(id)
	if !lock.TryLock() {
		return fmt.Errorf("%w: an update, scan or job is running on this site; try again when it finishes", ErrConflict)
	}
	defer lock.Unlock()
	return fn()
}

// ActivateTheme switches the site to an installed theme. The page cache
// (and the CDN's copy) is purged: every page changes.
func (s *Service) ActivateTheme(ctx context.Context, id, slug string) (*Theme, error) {
	var t *Theme
	err := s.themeLocked(ctx, id, func() error {
		var err error
		if t, err = s.installedTheme(ctx, id, slug); err != nil {
			return err
		}
		if t.Status == "active" {
			return nil
		}
		if _, err := s.Runtime.WP(ctx, id, nil, "theme", "activate", t.Slug); err != nil {
			return fmt.Errorf("switching theme: %w", err)
		}
		t.Status = "active"
		if err := s.Purge(ctx, id); err != nil {
			s.Log.Warn("purging the cache after switching theme", "site", id, "err", err)
		}
		s.event(id, "tools", fmt.Sprintf("Theme switched to %s %s", t.Title, t.Version))
		return nil
	})
	return t, err
}

// DeleteTheme deletes an inactive theme: never the active one, nor the
// parent the active child theme is built on.
func (s *Service) DeleteTheme(ctx context.Context, id, slug string) error {
	return s.themeLocked(ctx, id, func() error {
		t, err := s.installedTheme(ctx, id, slug)
		if err != nil {
			return err
		}
		switch t.Status {
		case "active":
			return fmt.Errorf("%w: %s is the active theme; switch to another one first", ErrInvalidInput, t.Title)
		case "parent":
			return fmt.Errorf("%w: the active theme is built on %s (it's the parent theme)", ErrInvalidInput, t.Title)
		case "inactive":
		default:
			return fmt.Errorf("%w: %s can't be deleted (%s)", ErrInvalidInput, t.Title, t.Status)
		}
		if _, err := s.Runtime.WP(ctx, id, nil, "theme", "delete", t.Slug); err != nil {
			return fmt.Errorf("deleting theme: %w", err)
		}
		s.event(id, "tools", fmt.Sprintf("Theme %s %s deleted", t.Title, t.Version))
		return nil
	})
}

// ThemeInstallInput installs a theme from wordpress.org.
type ThemeInstallInput struct {
	Slug string `json:"slug"`
}

func validThemeSlug(slug string) error {
	if !themeDirectoryRe.MatchString(slug) {
		return fmt.Errorf("%w: a theme's WordPress.org name is lowercase letters, digits and dashes (as in its address, wordpress.org/themes/<name>)", ErrInvalidInput)
	}
	return nil
}

// StartThemeInstall installs a theme from wordpress.org as a job (not
// activated).
func (s *Service) StartThemeInstall(ctx context.Context, id string, in ThemeInstallInput) (int64, error) {
	slug := strings.TrimSpace(in.Slug)
	if err := validThemeSlug(slug); err != nil {
		return 0, err
	}
	if err := s.requireActive(ctx, id); err != nil {
		return 0, err
	}
	spec := s.siteJob(id, "theme-install", false)
	spec.Timeout = themeInstallTimeout
	return s.Jobs.Submit(ctx, spec, func(ctx context.Context, t *jobs.Task) error {
		if err := s.requireActive(ctx, id); err != nil {
			return err
		}
		t.Progress(10, "Checking what's installed")
		list, err := s.themes(ctx, id)
		if err != nil {
			return err
		}
		if slices.ContainsFunc(list, func(th Theme) bool { return th.Slug == slug }) {
			return fmt.Errorf("%s is already installed", slug)
		}
		t.Progress(30, "Downloading "+slug+" from WordPress.org")
		if _, err := s.Runtime.WP(ctx, id, nil, "theme", "install", slug); err != nil {
			if msg := err.Error(); strings.Contains(msg, "Couldn't find") || strings.Contains(msg, "could not be found") ||
				strings.Contains(msg, "not found") {
				return fmt.Errorf("WordPress.org has no theme called %q", slug)
			}
			return fmt.Errorf("installing %s: %w", slug, err)
		}
		list, err = s.themes(ctx, id)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(list, func(th Theme) bool { return th.Slug == slug })
		if i < 0 {
			return fmt.Errorf("%s was not installed", slug)
		}
		t.SetResult(list[i])
		s.event(id, "tools", fmt.Sprintf("Theme %s %s installed from WordPress.org", list[i].Title, list[i].Version))
		return nil
	})
}
