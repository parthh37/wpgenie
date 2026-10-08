package site

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestNormalizeHardening(t *testing.T) {
	if _, err := normalizeHardening([]string{HardenUserEnum, "'); system('id"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown key: %v", err)
	}
	got, err := normalizeHardening([]string{HardenPingbacks, HardenAdminLock, HardenUserEnum, HardenPingbacks})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{HardenUserEnum, HardenAdminLock, HardenPingbacks}; !slices.Equal(got, want) {
		t.Errorf("normalized %v, want %v (catalogue order, no duplicates)", got, want)
	}
	if got, _ := normalizeHardening(nil); got == nil || len(got) != 0 {
		t.Errorf("none: %#v, want an empty list", got)
	}
}

func TestHardeningDefaults(t *testing.T) {
	want := []string{HardenUserEnum, HardenLoginErrors, HardenAppPasswords, HardenShortSessions, HardenStrongPasswords, HardenPingbacks}
	if got := DefaultHardening(); !slices.Equal(got, want) {
		t.Errorf("defaults %v, want %v", got, want)
	}
	// They change how people work in wp-admin: chosen, never imposed.
	for _, k := range []string{HardenAdminLock, HardenFileMods} {
		if slices.Contains(DefaultHardening(), k) {
			t.Errorf("%s is on by default", k)
		}
	}
	if st := newSite("a.test", "A"); !slices.Equal(st.Harden, want) {
		t.Errorf("new site hardening %v", st.Harden)
	}
	for _, o := range HardeningOptions {
		if !regexp.MustCompile(`^[a-z_]+$`).MatchString(o.Key) || o.Title == "" || o.Description == "" {
			t.Errorf("catalogue entry %+v", o)
		}
		// The panel says "Protection", never "Shield".
		if strings.Contains(strings.ToLower(o.Description), "shield") {
			t.Errorf("%s mentions the shield", o.Key)
		}
	}
}

func TestHardeningWrapper(t *testing.T) {
	w := hardeningWrapperFor([]string{HardenUserEnum, HardenAdminLock})
	for _, want := range []string{
		"define( 'WPGENIE_HARDEN', 'user_enum,admin_lock' );",
		"require_once '/usr/local/share/wpgenie/hardening.php';",
		managedMarker,
	} {
		if !strings.Contains(w, want) {
			t.Errorf("wrapper missing %q:\n%s", want, w)
		}
	}
	// Every key the catalogue has is one the PHP side acts on.
	php, err := os.ReadFile("../../images/php/hardening.php")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range HardeningOptions {
		if !strings.Contains(string(php), "$wpgenie_harden['"+o.Key+"']") {
			t.Errorf("hardening.php doesn't handle %s", o.Key)
		}
	}
}

func TestSetHardening(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	wrapper := filepath.Join(h.svc.Cfg.SiteRoot("s1"), hardeningWrapperPath)

	if _, err := h.svc.SetHardening(ctx, "s1", HardeningInput{Hardening: []string{"nope"}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown key: %v", err)
	}
	st, err := h.svc.SetHardening(ctx, "s1", HardeningInput{Hardening: []string{HardenFileMods, HardenUserEnum}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{HardenUserEnum, HardenFileMods}; !slices.Equal(st.Harden, want) {
		t.Errorf("stored %v, want %v", st.Harden, want)
	}
	got, err := os.ReadFile(wrapper)
	if err != nil || !strings.Contains(string(got), "define( 'WPGENIE_HARDEN', 'user_enum,file_mods' );") {
		t.Fatalf("wrapper: %v\n%s", err, got)
	}
	// The rest of the record is untouched (the store writes one column).
	if st.PrimaryDomain != "a.test" || st.Replicas != 1 {
		t.Errorf("site record changed: %+v", st)
	}

	// None: no wrapper at all.
	if _, err := h.svc.SetHardening(ctx, "s1", HardeningInput{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wrapper); !os.IsNotExist(err) {
		t.Errorf("wrapper kept with nothing on: %v", err)
	}

	// A file WPGenie didn't write is never replaced.
	if err := os.MkdirAll(filepath.Dir(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wrapper, []byte("<?php // someone's own"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.SetHardening(ctx, "s1", HardeningInput{Hardening: DefaultHardening()}); !errors.Is(err, ErrConflict) {
		t.Errorf("over a foreign file: %v", err)
	}
}

func TestRotateSaltsKeepsTheRest(t *testing.T) {
	cfg, err := renderWPConfig(wpConfigData{SiteID: "s1", DBName: "wp_s1", DBUser: "u_s1", DBPassword: "pw",
		DBHost: "mariadb", RedisHost: "valkey", TablePrefix: "wp_x_"})
	if err != nil {
		t.Fatal(err)
	}
	// The site's own lines below WPGenie's, one of them a salt-like define.
	mine := "define( 'WP_DEBUG', true );\ndefine( 'AUTH_KEY', 'not ours' );\n"
	cfg = []byte(strings.Replace(string(cfg), "if ( ! defined( 'WP_DEBUG' ) ) {", mine+"if ( ! defined( 'WP_DEBUG' ) ) {", 1))

	out, err := rotateSalts(cfg)
	if err != nil {
		t.Fatal(err)
	}
	saltRe := regexp.MustCompile(`(?m)^define\( '([A-Z_]+)', '([^']*)' \);$`)
	salts := func(b []byte) map[string]string {
		m := map[string]string{}
		end := strings.Index(string(b), wpConfigEnd)
		for _, x := range saltRe.FindAllStringSubmatch(string(b)[:end], -1) {
			if slices.Contains(saltNames, x[1]) {
				m[x[1]] = x[2]
			}
		}
		return m
	}
	before, after := salts(cfg), salts(out)
	if len(before) != 8 || len(after) != 8 {
		t.Fatalf("salts before %d, after %d", len(before), len(after))
	}
	for n, v := range before {
		if after[n] == v || len(after[n]) != 64 || strings.ContainsAny(after[n], `'\`) {
			t.Errorf("%s: %q -> %q", n, v, after[n])
		}
	}
	// Everything but the salt values is byte for byte the same.
	blank := func(b []byte) string {
		end := strings.Index(string(b), wpConfigEnd)
		head := saltRe.ReplaceAllStringFunc(string(b)[:end], func(l string) string {
			if m := saltRe.FindStringSubmatch(l); slices.Contains(saltNames, m[1]) {
				return "SALT " + m[1]
			}
			return l
		})
		return head + string(b)[end:]
	}
	if blank(cfg) != blank(out) {
		t.Errorf("more than the salts changed:\n%s\n---\n%s", cfg, out)
	}
	if !strings.Contains(string(out), mine) {
		t.Error("the site's own lines changed")
	}

	if _, err := rotateSalts([]byte("<?php\ndefine( 'AUTH_KEY', 'x' );\n")); err == nil {
		t.Error("a wp-config.php WPGenie didn't write was rewritten")
	}
	dup := strings.Replace(string(cfg), "define( 'NONCE_SALT',", "define( 'NONCE_SALT', 'a' );\ndefine( 'NONCE_SALT',", 1)
	if _, err := rotateSalts([]byte(dup)); err == nil {
		t.Error("a salt defined twice wasn't refused")
	}
}

func TestSignOutEveryone(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	cfg, err := renderWPConfig(wpConfigData{SiteID: "s1", DBName: "wp_s1", DBUser: "u", DBPassword: "p",
		DBHost: "db", RedisHost: "r", TablePrefix: "wp_"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(h.svc.Cfg.SiteDir("s1"), "wp-config.php")
	if err := os.WriteFile(path, cfg, 0o640); err != nil {
		t.Fatal(err)
	}
	var evals []string
	h.rt.exec = func(args []string, _ io.Reader, _ io.Writer) error {
		evals = append(evals, strings.Join(args, " "))
		return nil
	}
	if err := h.svc.SignOutEveryone(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) == string(cfg) {
		t.Error("salts weren't replaced")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %v, want 0640 (PHP reads it, can't write it)", fi.Mode().Perm())
	}
	if len(evals) != 1 || !strings.Contains(evals[0], "destroy_all_for_all_users") || !strings.Contains(evals[0], "--skip-plugins") {
		t.Errorf("sessions weren't forgotten through WP-CLI with plugins skipped: %v", evals)
	}

	// A site without WPGenie's wp-config.php is left alone.
	if err := os.WriteFile(path, []byte("<?php // someone else's"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.SignOutEveryone(ctx, "s1"); err == nil {
		t.Error("rewrote a wp-config.php without WPGenie's section")
	}
}
