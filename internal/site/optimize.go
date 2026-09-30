package site

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/parthh37/wpgenie/internal/runtime"
	"github.com/parthh37/wpgenie/internal/store"
)

// WordPress performance tweaks: the small things performance plugins do
// that WPGenie's page cache, object cache and image conversion don't. The
// optimize mu-plugin (images/php/optimize.php, read-only in the image)
// applies the ones a site has on; the wrapper WPGenie writes names them.
// Database cleanup is the daemon's: nightly, through WP-CLI.

const (
	optimizeWrapperPath = "wp-content/mu-plugins/wpgenie-optimize.php"
	cleanupTimeout      = 10 * time.Minute
)

// Optimization keys (store.Site.Optimize).
const (
	OptEmoji         = "emoji"
	OptEmbeds        = "embeds"
	OptHead          = "head"
	OptHeartbeat     = "heartbeat"
	OptSelfPings     = "self_pings"
	OptDashicons     = "dashicons"
	OptJQueryMigrate = "jquery_migrate"
	OptDBCleanup     = "db_cleanup"
)

// Optimization describes one tweak for the panel.
type Optimization struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	Description string `json:"description"`
	// Default: new sites get it, and "apply recommended" turns it on.
	Default bool `json:"default"`
}

// Optimizations is every tweak, in the order the panel lists them.
var Optimizations = []Optimization{
	{OptEmoji, "Remove emoji scripts", "Browsers draw emoji themselves: skip WordPress's emoji script and styles on every page.", true},
	{OptEmbeds, "Disable oEmbed discovery", "Drop the embed discovery links and script that let other sites embed yours (embedding YouTube and others still works).", true},
	{OptHead, "Clean up <head>", "Remove the generator tag (it advertises the WordPress version), RSD, Windows Live Writer and shortlink tags.", true},
	{OptHeartbeat, "Slow the Heartbeat API", "wp-admin polls every 60 s instead of 15 s, and visitors' pages don't load it: fewer PHP requests.", true},
	{OptSelfPings, "No self-pingbacks", "Linking to your own posts no longer sends pingbacks to yourself.", true},
	{OptDashicons, "No Dashicons for visitors", "The admin icon font only loads for signed-in users.", true},
	{OptJQueryMigrate, "Remove jQuery Migrate", "Visitors' pages skip the jQuery compatibility layer. Old themes and plugins may need it: check the site afterwards.", false},
	{OptDBCleanup, "Nightly database cleanup", "Every night: expired transients, auto-drafts and spam older than a week or month, and old revisions beyond the newest five per post.", true},
}

// DefaultOptimizations are the tweaks new sites start with.
func DefaultOptimizations() []string {
	var out []string
	for _, o := range Optimizations {
		if o.Default {
			out = append(out, o.Key)
		}
	}
	return out
}

// normalizeOptimizations validates keys and puts them in catalogue order,
// without duplicates.
func normalizeOptimizations(in []string) ([]string, error) {
	for _, k := range in {
		if !slices.ContainsFunc(Optimizations, func(o Optimization) bool { return o.Key == k }) {
			return nil, fmt.Errorf("%w: unknown optimization %q", ErrInvalidInput, k)
		}
	}
	out := []string{}
	for _, o := range Optimizations {
		if slices.Contains(in, o.Key) {
			out = append(out, o.Key)
		}
	}
	return out, nil
}

// phpOptimizations are the keys the mu-plugin acts on (not db_cleanup).
func phpOptimizations(keys []string) []string {
	return slices.DeleteFunc(slices.Clone(keys), func(k string) bool { return k == OptDBCleanup })
}

const optimizeWrapper = `<?php
/**
 * Plugin Name: WPGenie Optimize
 * Description: Performance tweaks chosen in the WPGenie panel. Managed by WPGenie: change them in the panel; this file is rewritten on changes.
 */
define( 'WPGENIE_OPTIMIZE', '%s' );
if ( is_file( '/usr/local/share/wpgenie/optimize.php' ) ) {
	require_once '/usr/local/share/wpgenie/optimize.php';
}
`

type OptimizeInput struct {
	Optimizations []string `json:"optimizations"`
}

// SetOptimize chooses a site's performance tweaks. The page cache is
// purged: cached pages still carry what the tweaks remove.
func (s *Service) SetOptimize(ctx context.Context, id string, in OptimizeInput) (*store.Site, error) {
	keys, err := normalizeOptimizations(in.Optimizations)
	if err != nil {
		return nil, err
	}
	retire, prev, err := s.setOptimizeLocked(ctx, id, keys)
	if err != nil {
		return nil, err
	}
	retire()
	if !slices.Equal(prev, keys) {
		if err := s.Purge(ctx, id); err != nil {
			s.Log.Warn("purging the cache after changing optimizations", "site", id, "err", err)
		}
		if len(keys) == 0 {
			s.event(id, "optimize", "Performance tweaks turned off")
		} else {
			s.event(id, "optimize", "Performance tweaks: "+strings.Join(keys, ", "))
		}
	}
	return s.Store.GetSite(ctx, id)
}

func (s *Service) setOptimizeLocked(ctx context.Context, id string, keys []string) (func(), []string, error) {
	lock := s.maintLock(id)
	if !lock.TryLock() {
		return nil, nil, fmt.Errorf("%w: an update, scan or job is running on this site; try again when it finishes", ErrConflict)
	}
	defer lock.Unlock()
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if st.Status != store.StatusActive {
		return nil, nil, fmt.Errorf("%w: site is %s", ErrInvalidInput, st.Status)
	}
	prev := st.Optimize
	if err := s.writeOptimizeWrapper(id, keys); err != nil {
		return nil, nil, err
	}
	if err := s.Store.SetOptimize(ctx, id, keys); err != nil {
		return nil, nil, err
	}
	// Replicas from an image without optimize.php would ignore the wrapper:
	// roll any that are out of date.
	st.Optimize = keys
	retire, err := s.reconcile(ctx, st)
	if err != nil {
		return nil, nil, fmt.Errorf("optimizations saved, but refreshing the site's PHP containers failed: %w", err)
	}
	return retire, prev, nil
}

func (s *Service) writeOptimizeWrapper(id string, keys []string) error {
	root, err := os.OpenRoot(s.Cfg.SiteRoot(id))
	if err != nil {
		return err
	}
	defer root.Close()
	php := phpOptimizations(keys)
	// Keys are validated: only [a-z_] and commas reach the PHP string.
	return ensureManaged(root, optimizeWrapperPath, fmt.Sprintf(optimizeWrapper, strings.Join(php, ",")), len(php) > 0)
}

// CleanupResult is what a database cleanup removed.
type CleanupResult struct {
	Transients int `json:"transients"`
	AutoDrafts int `json:"auto_drafts"`
	Spam       int `json:"spam"`
	Revisions  int `json:"revisions"`
}

func (c CleanupResult) total() int { return c.Transients + c.AutoDrafts + c.Spam + c.Revisions }

// cleanupPHP runs inside WordPress (WP-CLI, plugins skipped). Batches are
// bounded so one night never runs for long; what's left goes the next.
// Deleting through WordPress's own functions keeps metadata, comment
// counts and caches consistent (a bare DELETE would orphan postmeta).
const cleanupPHP = `
global $wpdb;
$now = time();
$out = array( 'transients' => 0, 'auto_drafts' => 0, 'spam' => 0, 'revisions' => 0 );
$before = (int) $wpdb->get_var( "SELECT COUNT(*) FROM {$wpdb->options} WHERE option_name LIKE '\\_transient\\_timeout\\_%' OR option_name LIKE '\\_site\\_transient\\_timeout\\_%'" );
delete_expired_transients( true );
$after = (int) $wpdb->get_var( "SELECT COUNT(*) FROM {$wpdb->options} WHERE option_name LIKE '\\_transient\\_timeout\\_%' OR option_name LIKE '\\_site\\_transient\\_timeout\\_%'" );
$out['transients'] = max( 0, $before - $after );
$ids = $wpdb->get_col( $wpdb->prepare( "SELECT ID FROM {$wpdb->posts} WHERE post_status = 'auto-draft' AND post_date_gmt < %s LIMIT 2000", gmdate( 'Y-m-d H:i:s', $now - 7 * DAY_IN_SECONDS ) ) );
foreach ( $ids as $id ) { if ( wp_delete_post( (int) $id, true ) ) { $out['auto_drafts']++; } }
$ids = $wpdb->get_col( $wpdb->prepare( "SELECT comment_ID FROM {$wpdb->comments} WHERE comment_approved = 'spam' AND comment_date_gmt < %s LIMIT 5000", gmdate( 'Y-m-d H:i:s', $now - 30 * DAY_IN_SECONDS ) ) );
foreach ( $ids as $id ) { if ( wp_delete_comment( (int) $id, true ) ) { $out['spam']++; } }
$parents = $wpdb->get_col( "SELECT post_parent FROM {$wpdb->posts} WHERE post_type = 'revision' GROUP BY post_parent HAVING COUNT(*) > 5 LIMIT 500" );
$cut = $now - 30 * DAY_IN_SECONDS;
foreach ( $parents as $parent ) {
	$revs = wp_get_post_revisions( (int) $parent, array( 'order' => 'DESC', 'orderby' => 'date ID', 'check_enabled' => false ) );
	foreach ( array_slice( array_values( $revs ), 5 ) as $rev ) {
		if ( strtotime( $rev->post_modified_gmt . ' UTC' ) < $cut && wp_delete_post_revision( $rev->ID ) ) { $out['revisions']++; }
	}
}
echo wp_json_encode( $out );
`

// CleanupDatabase removes expired transients, stale auto-drafts, old spam
// and old revisions (the newest five per post always stay).
func (s *Service) CleanupDatabase(ctx context.Context, id string) (*CleanupResult, error) {
	lock := s.maintLock(id)
	if !lock.TryLock() {
		return nil, fmt.Errorf("%w: an update, scan or job is running on this site; try again when it finishes", ErrConflict)
	}
	defer lock.Unlock()
	return s.cleanupLocked(ctx, id)
}

func (s *Service) cleanupLocked(ctx context.Context, id string) (*CleanupResult, error) {
	ctx, cancel := context.WithTimeout(ctx, cleanupTimeout)
	defer cancel()
	var out bytes.Buffer
	if err := s.Runtime.Exec(ctx, id, nil, &out, runtime.WPArgs("eval", cleanupPHP)...); err != nil {
		return nil, fmt.Errorf("database cleanup: %w", err)
	}
	var r CleanupResult
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &r); err != nil {
		return nil, fmt.Errorf("database cleanup: unexpected output: %w", err)
	}
	if r.total() > 0 {
		s.event(id, "optimize", fmt.Sprintf("Database cleanup: %d expired transients, %d auto-drafts, %d spam comments, %d old revisions removed",
			r.Transients, r.AutoDrafts, r.Spam, r.Revisions))
	}
	return &r, nil
}
