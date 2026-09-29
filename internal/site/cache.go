package site

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

var ErrConflict = errors.New("conflict")

type CacheSettings struct {
	PageCache   bool `json:"page_cache"`
	ObjectCache bool `json:"object_cache"`
	// Mobile stores separate mobile and desktop copies of every page, for
	// themes that detect phones themselves (pages that ask wp_is_mobile()
	// always get them). Omitted: unchanged.
	Mobile *bool `json:"mobile,omitempty"`
}

// Paths inside a site's docroot. The page cache layout is shared with
// images/php/page-cache.php and the Caddy config.
const (
	pageCacheWrapperPath = "wp-content/mu-plugins/wpgenie-page-cache.php"
	objectCacheDropIn    = "wp-content/object-cache.php"
	pageCacheDir         = "wp-content/cache/wpgenie"
	pageCacheMarker      = "wp-content/cache/wpgenie.purged"
	managedMarker        = "Managed by WPGenie"
	wwwData              = 82
)

// The sites only get these one-line wrappers; the code they load ships
// read-only in the PHP image, where a compromised plugin can't modify it.
// Both degrade to "no cache" when running on an older image.
const pageCacheWrapper = `<?php
/**
 * Plugin Name: WPGenie Page Cache
 * Description: Full-page cache served straight from Caddy. Managed by WPGenie: toggle it in the WPGenie panel; this file is rewritten on changes.
 */
%sif ( is_file( '/usr/local/share/wpgenie/page-cache.php' ) ) {
	require_once '/usr/local/share/wpgenie/page-cache.php';
}
`

// pageCacheWrapperFor is the wrapper with the site's settings.
func pageCacheWrapperFor(mobile bool) string {
	opts := ""
	if mobile {
		opts = "define( 'WPGENIE_CACHE_MOBILE_ALWAYS', true );\n"
	}
	return fmt.Sprintf(pageCacheWrapper, opts)
}

const objectCacheWrapper = `<?php
/**
 * Redis object cache (redis-cache drop-in). Managed by WPGenie: toggle it in
 * the WPGenie panel; this file is rewritten on changes.
 */
// Valkey is shared by every site on the server:
// - if it is down, keep serving uncached instead of an error page;
// - a flush (WordPress core runs one on updates) must drop only this site's
//   keys (WP_REDIS_PREFIX), never FLUSHDB everyone's cache.
defined( 'WP_REDIS_GRACEFUL' ) || define( 'WP_REDIS_GRACEFUL', true );
defined( 'WP_REDIS_SELECTIVE_FLUSH' ) || define( 'WP_REDIS_SELECTIVE_FLUSH', true );
if ( is_file( '/usr/local/share/wpgenie/object-cache.php' ) ) {
	require_once '/usr/local/share/wpgenie/object-cache.php';
}
`

// SetCache turns the page cache and object cache on or off.
func (s *Service) SetCache(ctx context.Context, id string, c CacheSettings) (*store.Site, error) {
	retire, err := s.setCacheLocked(ctx, id, c)
	if err != nil {
		return nil, err
	}
	retire()
	return s.Store.GetSite(ctx, id)
}

func (s *Service) setCacheLocked(ctx context.Context, id string, c CacheSettings) (func(), error) {
	// A rollback during an update rewrites the cache wrappers; don't race it.
	lock := s.maintLock(id)
	if !lock.TryLock() {
		return nil, fmt.Errorf("%w: an update or scan is running on this site; try again when it finishes", ErrConflict)
	}
	// Released before the caller runs retire, which takes it too.
	defer lock.Unlock()
	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, err
	}
	if st.Status != store.StatusActive {
		return nil, fmt.Errorf("%w: site is %s", ErrInvalidInput, st.Status)
	}
	mobile := st.CacheMobile
	if c.Mobile != nil {
		mobile = *c.Mobile
	}
	if c.PageCache && (!st.PageCache || mobile != st.CacheMobile) {
		// Pages cached before the cache was last turned off are stale, and
		// a change of layout leaves copies the new one doesn't expect.
		if err := s.purgePageCacheFiles(id); err != nil {
			return nil, err
		}
	}
	if err := s.writeCacheFiles(id, c.PageCache, c.ObjectCache, mobile); err != nil {
		return nil, err
	}
	if err := s.Store.SetCache(ctx, id, c.PageCache, c.ObjectCache, mobile); err != nil {
		return nil, err
	}
	if err := s.Sync(ctx); err != nil {
		return nil, err
	}
	if !c.PageCache && st.PageCache {
		if err := s.purgePageCacheFiles(id); err != nil {
			s.Log.Warn("cleaning up page cache", "site", id, "err", err)
		}
	}
	if c.ObjectCache && !st.ObjectCache {
		// Keys written before it was last turned off are stale.
		if err := s.Cache.FlushPrefix(ctx, cachePrefix(id)); err != nil {
			s.Log.Warn("flushing object cache", "site", id, "err", err)
		}
	}
	// Replicas from an image without the cache code would ignore the new
	// wrappers; roll any that are out of date (a no-op when all are current).
	st.PageCache, st.ObjectCache = c.PageCache, c.ObjectCache
	retire, err := s.reconcile(ctx, st)
	if err != nil {
		return nil, fmt.Errorf("cache settings saved, but refreshing the site's PHP containers failed: %w", err)
	}
	return retire, nil
}

// Purge empties the page cache, this site's object cache keys and, with
// the CDN integration on, the CDN's copy of its hostnames.
func (s *Service) Purge(ctx context.Context, id string) error {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return err
	}
	errs := []error{s.purgeLocal(ctx, st)}
	// Last: the CDN refetches from the origin, which must already be fresh.
	if err := s.purgeCDNIfOn(ctx, st); err != nil {
		errs = append(errs, fmt.Errorf("CDN purge: %w", err))
	}
	return errors.Join(errs...)
}

// purgeLocal empties the caches on this server: page cache and object cache.
func (s *Service) purgeLocal(ctx context.Context, st *store.Site) error {
	errs := []error{s.purgePageCacheFiles(st.ID)}
	if st.ObjectCache {
		if err := s.Cache.FlushPrefix(ctx, cachePrefix(st.ID)); err != nil {
			errs = append(errs, fmt.Errorf("object cache flush: %w", err))
		}
	}
	return errors.Join(errs...)
}

func (s *Service) purgeCDNIfOn(ctx context.Context, st *store.Site) error {
	c, err := s.Store.GetCDN(ctx, st.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	return s.purgeCDN(ctx, st, c)
}

// cachePrefix is WP_REDIS_PREFIX from the site's wp-config.php.
func cachePrefix(id string) string { return id + ":" }

// The docroot is writable by the site, while this daemon runs as root. A
// compromised site could plant symlinks (wp-content/mu-plugins -> /etc) to
// turn a root write or delete into one outside its docroot. os.Root refuses
// to resolve any path outside the directory it was opened on, so every file
// operation below goes through one.

func (s *Service) writeCacheFiles(id string, page, object, mobile bool) error {
	root, err := os.OpenRoot(s.Cfg.SiteRoot(id))
	if err != nil {
		return err
	}
	defer root.Close()
	return errors.Join(
		ensureManaged(root, pageCacheWrapperPath, pageCacheWrapperFor(mobile), page),
		ensureManaged(root, objectCacheDropIn, objectCacheWrapper, object),
	)
}

// ensureManaged writes (want) or removes (!want) a file WPGenie owns. A file
// at that path that WPGenie did not write is never touched.
func ensureManaged(root *os.Root, name, content string, want bool) error {
	cur, err := root.ReadFile(name)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if exists && !bytes.Contains(cur, []byte(managedMarker)) {
		if want {
			return fmt.Errorf("%w: %s already exists and is not managed by WPGenie "+
				"(another caching plugin?); remove it first", ErrConflict, name)
		}
		return nil
	}
	if !want {
		if exists {
			return root.Remove(name)
		}
		return nil
	}
	if exists && string(cur) == content {
		return nil
	}
	dir := path.Dir(name)
	if _, err := root.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		// The site must still be able to add its own files there.
		if err := chownToSite(root, dir); err != nil {
			return err
		}
	}
	// Root-owned and not writable by PHP; write-then-rename so PHP never
	// includes a half-written file.
	tmp := name + ".wpgenie-tmp"
	if err := root.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return root.Rename(tmp, name)
}

// purgePageCacheFiles is the daemon-side twin of wpgenie_cache_purge().
func (s *Service) purgePageCacheFiles(id string) error {
	root, err := os.OpenRoot(s.Cfg.SiteRoot(id))
	if err != nil {
		return err
	}
	defer root.Close()
	if _, err := root.Stat(path.Dir(pageCacheDir)); errors.Is(err, fs.ErrNotExist) {
		return nil // nothing was ever cached
	}
	// Touch the marker so a page that was rendering during the purge isn't
	// stored afterwards. PHP touches it too, so it must stay owned by the site.
	now := time.Now()
	if err := root.Chtimes(pageCacheMarker, now, now); errors.Is(err, fs.ErrNotExist) {
		if err := root.WriteFile(pageCacheMarker, nil, 0o644); err != nil {
			return err
		}
		if err := chownToSite(root, pageCacheMarker); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	// Rename first so Caddy stops finding pages at once.
	trash := pageCacheDir + ".trash-" + randString(8, lowerAlnum)
	if err := root.Rename(pageCacheDir, trash); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		trash = pageCacheDir
	}
	return root.RemoveAll(trash)
}

func chownToSite(root *os.Root, name string) error {
	if os.Geteuid() != 0 {
		return nil // development on a workstation; ownership is best effort
	}
	return root.Lchown(name, wwwData, wwwData)
}
