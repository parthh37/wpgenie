package site

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/parthh37/wpgenie/internal/config"
	"github.com/parthh37/wpgenie/internal/store"
)

func TestPHPUnpatched(t *testing.T) {
	for v, want := range map[string]struct {
		fixedIn   string
		unpatched bool
	}{
		"8.2.34": {"8.2.35", true}, "8.2.35": {"8.2.35", false},
		"8.3.30": {"8.3.36", true}, "8.3.36": {"8.3.36", false}, "8.3.40": {"8.3.36", false},
		"8.4.0": {"8.4.27", true}, "8.4.27": {"8.4.27", false},
		"8.5.0RC1": {"8.5.12", true}, "8.5.12": {"8.5.12", false}, "8.3.36-dev": {"8.3.36", false},
		// Out of support: no fixed release.
		"8.1.31": {"", true}, "7.4.33": {"", true}, "5.4.0": {"", true},
		// Released after the fix.
		"8.6.0": {"", false}, "9.0.1": {"", false},
		// Not judged.
		"": {"", false}, "8.3": {"", false}, "garbage": {"", false},
	} {
		if fixed, bad := phpUnpatched(v); fixed != want.fixedIn || bad != want.unpatched {
			t.Errorf("phpUnpatched(%q) = %q, %v; want %q, %v", v, fixed, bad, want.fixedIn, want.unpatched)
		}
	}
}

func TestAnalyseReportsUnpatchedPHP(t *testing.T) {
	now := time.Date(2026, 10, 22, 3, 0, 0, 0, time.UTC)
	st := &store.Site{ID: "s1", ShieldMode: "standard", WAF: true}
	find := func(php string) (Finding, bool) {
		fs := analyse(st, &SiteFacts{PHPVersion: php}, nil, nil, now)
		if i := slices.IndexFunc(fs, func(f Finding) bool { return f.ID == "php-unpatched" }); i >= 0 {
			return fs[i], true
		}
		return Finding{}, false
	}
	if f, ok := find("8.3.30"); !ok || f.Severity != SevCritical || f.Category != CatSecurity || !strings.Contains(f.Detail, "8.3.36") {
		t.Errorf("PHP 8.3.30: %+v (reported %v)", f, ok)
	}
	if f, ok := find("8.1.31"); !ok || !strings.Contains(f.Detail, "no more fixes") {
		t.Errorf("PHP 8.1.31: %+v (reported %v)", f, ok)
	}
	if _, ok := find("8.3.36"); ok {
		t.Error("patched PHP reported")
	}
}

// The images refuse to build below the floors the site report checks, and
// every PHP version sites may run has a base pinned by digest (a bare tag is
// never re-pulled, so servers would keep rebuilding on an old PHP).
func TestImagesEnforceTheSecurityFloor(t *testing.T) {
	php, err := os.ReadFile("../../images/php/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	pma, err := os.ReadFile("../../images/phpmyadmin/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for line, floor := range phpSecurityFloor {
		entry := `"` + line + `" => "` + floor + `"`
		for name, src := range map[string][]byte{"images/php": php, "images/phpmyadmin": pma} {
			if !strings.Contains(string(src), entry) {
				t.Errorf("%s/Dockerfile lacks the floor %s", name, entry)
			}
		}
	}
	for _, v := range config.Default().PHPVersions {
		if !strings.Contains(string(php), "FROM wordpress:php"+v+"-fpm-alpine@sha256:") || !strings.Contains(string(php), " AS base-"+v+"\n") {
			t.Errorf("images/php/Dockerfile has no digest-pinned base-%s stage", v)
		}
	}
	if !strings.Contains(string(pma), "FROM phpmyadmin:") || !strings.Contains(string(pma), "-fpm-alpine@sha256:") {
		t.Error("images/phpmyadmin/Dockerfile's base isn't pinned by digest")
	}
	if !strings.Contains(config.Default().WebmailImage, "@sha256:") {
		t.Error("the webmail image isn't pinned by digest")
	}
}

type fakeImages struct {
	have  map[string]bool
	fail  map[string]bool
	built []string
}

func (f *fakeImages) ImageExists(_ context.Context, image string) bool { return f.have[image] }

func (f *fakeImages) Build(_ context.Context, tag, _ string, buildArgs ...string) error {
	f.built = append(f.built, tag+" "+strings.Join(buildArgs, " "))
	if f.fail[tag] {
		return errors.New("pull access denied")
	}
	f.have[tag] = true
	return nil
}

func TestBuildImagesInUseRebuildsEveryStaleImage(t *testing.T) {
	h := newHarness(t) // s1: active on 8.3, the default
	ctx := context.Background()
	for _, s := range []*store.Site{
		{ID: "s2", Name: "s2", PrimaryDomain: "b.test", PHPVersion: "8.4", FPMPort: 19010, DBName: "wp_s2",
			Status: store.StatusActive, ShieldMode: "standard", MemoryMB: 512, CPUs: 1, Replicas: 1},
		{ID: "s3", Name: "s3", PrimaryDomain: "c.test", PHPVersion: "8.2", FPMPort: 19020, DBName: "wp_s3",
			Status: store.StatusSuspended, ShieldMode: "standard", MemoryMB: 512, CPUs: 1, Replicas: 1},
	} {
		if err := h.svc.Store.CreateSite(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	sites, err := h.svc.Store.ListSites(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// 8.4 is in use; 8.2's image exists though only a suspended site uses it
	// (a site switching later, or s3 resuming, would get it as it is).
	img := &fakeImages{have: map[string]bool{"wpgenie/php:8.2": true, "wpgenie/php:8.3": true}}
	h.svc.Images = img
	h.svc.buildImagesInUse(ctx, sites)
	slices.Sort(img.built)
	if want := []string{"wpgenie/php:8.2 PHP_VERSION=8.2", "wpgenie/php:8.4 PHP_VERSION=8.4"}; !slices.Equal(img.built, want) {
		t.Fatalf("built %v, want %v (the default's image is the updater's to build)", img.built, want)
	}

	// A failed rebuild is shown on the sites left on the old image.
	img = &fakeImages{have: map[string]bool{}, fail: map[string]bool{"wpgenie/php:8.4": true}}
	h.svc.Images = img
	h.svc.buildImagesInUse(ctx, sites)
	if !slices.Equal(img.built, []string{"wpgenie/php:8.4 PHP_VERSION=8.4"}) {
		t.Fatalf("built %v: only versions in use or already built", img.built)
	}
	events, _ := h.svc.Store.Events(ctx, "s2", 5)
	if len(events) == 0 || !strings.Contains(events[0].Message, "PHP 8.4 image failed") {
		t.Fatalf("s2 events: %+v", events)
	}
}
