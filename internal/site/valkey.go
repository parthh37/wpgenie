package site

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/parthh37/wpgenie/internal/runtime"
)

// Object cache isolation. Every site connects to the shared Valkey as its
// own ACL user, limited to its own keys (runtime.RenderACL): a site's PHP,
// which a tenant controls, can't read or rewrite another site's cached
// options and users. Credentials derive from a key only this server's
// daemon reads (HMAC of the site ID), so nothing new is stored: a site's
// wp-config.php carries them, the ACL file holds their hashes, and a spread
// site's replicas elsewhere use the home's through the copied wp-config.php.

// ValkeyACL is where the ACL file goes and how Valkey reloads it (nil: no
// isolation, as in tests without a cache server).
type ValkeyACL interface {
	LoadACL(ctx context.Context) error
	Restart(ctx context.Context) error
}

// cacheKeyState is the server's cache key, read (or created) once.
type cacheKeyState struct {
	once sync.Once
	key  []byte
	err  error
}

// cacheSecret is the server's cache key (created once, root only).
func (s *Service) cacheSecret() ([]byte, error) {
	if s.Cfg.ValkeyKey == "" {
		return nil, errors.New("no valkey_key path configured")
	}
	c := &s.cacheKey
	c.once.Do(func() {
		if b, err := os.ReadFile(s.Cfg.ValkeyKey); err == nil {
			c.key, c.err = hex.DecodeString(strings.TrimSpace(string(b)))
			if c.err == nil && len(c.key) < 32 {
				c.err = errors.New("valkey key too short")
			}
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			c.err = err
			return
		}
		k := make([]byte, 32)
		rand.Read(k)
		if err := os.MkdirAll(filepath.Dir(s.Cfg.ValkeyKey), 0o700); err != nil {
			c.err = err
			return
		}
		c.key, c.err = k, os.WriteFile(s.Cfg.ValkeyKey, []byte(hex.EncodeToString(k)+"\n"), 0o600)
	})
	return c.key, c.err
}

// cacheCredentials are a site's object cache user and password; "" when
// isolation isn't configured (the site then uses the default user).
func (s *Service) cacheCredentials(id string) (string, string, error) {
	if s.CacheACL == nil || s.Cfg.ValkeyKey == "" {
		return "", "", nil
	}
	k, err := s.cacheSecret()
	if err != nil {
		return "", "", err
	}
	return "wpg_" + id, derive(k, "site:"+id), nil
}

// CacheAdminPassword is the daemon's own Valkey password.
func (s *Service) CacheAdminPassword() string {
	if s.CacheACL == nil || s.Cfg.ValkeyKey == "" {
		return ""
	}
	k, err := s.cacheSecret()
	if err != nil {
		return ""
	}
	return derive(k, "admin")
}

func derive(key []byte, what string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("valkey:" + what))
	return hex.EncodeToString(m.Sum(nil))
}

var aclMu sync.Mutex

// SyncCacheUsers writes the ACL file for this server's sites and has Valkey
// load it. A Valkey started before the file existed (an upgrade) is
// restarted once to take it: the cache is disposable.
func (s *Service) SyncCacheUsers(ctx context.Context) error {
	if s.CacheACL == nil || s.Cfg.ValkeyACLDir == "" || s.Cfg.ValkeyKey == "" {
		return nil
	}
	aclMu.Lock()
	defer aclMu.Unlock()
	sites, err := s.Store.ListSites(ctx)
	if err != nil {
		return err
	}
	var users []runtime.ACLUser
	for _, st := range sites {
		u, p, err := s.cacheCredentials(st.ID)
		if err != nil {
			return err
		}
		users = append(users, runtime.ACLUser{Name: u, Password: p, Prefix: st.ID + ":"})
	}
	b, err := runtime.RenderACL(s.CacheAdminPassword(), users)
	if err != nil {
		return err
	}
	// Valkey (its own user in the container) reads it: hashes only.
	if err := os.MkdirAll(s.Cfg.ValkeyACLDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(s.Cfg.ValkeyACLDir, "users.acl")
	if cur, err := os.ReadFile(path); err != nil || string(cur) != string(b) {
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, b, 0o644); err != nil {
			return err
		}
		if err := os.Chmod(tmp, 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
	}
	err = s.CacheACL.LoadACL(ctx)
	if errors.Is(err, runtime.ErrNoACLFile) {
		s.Log.Info("restarting Valkey to turn on per-site cache users")
		if err := s.CacheACL.Restart(ctx); err != nil {
			return err
		}
		return nil // it read the file at start
	}
	return err
}

var redisHostLine = regexp.MustCompile(`(?m)^define\( 'WP_REDIS_HOST', '[^']*' \);\n`)

// ensureCacheCredentials gives a site created before cache users its
// credentials in wp-config.php (WPGenie's file; root-owned).
func (s *Service) ensureCacheCredentials(id string) error {
	user, pass, err := s.cacheCredentials(id)
	if err != nil || user == "" {
		return err
	}
	path := filepath.Join(s.Cfg.SiteDir(id), "wp-config.php")
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if strings.Contains(string(b), "WP_REDIS_PASSWORD") {
		return nil
	}
	loc := redisHostLine.FindIndex(b)
	if loc == nil {
		return fmt.Errorf("%s: no WP_REDIS_HOST line", path)
	}
	line := fmt.Sprintf("define( 'WP_REDIS_PASSWORD', [ '%s', '%s' ] ); // this site's own cache user\n", user, pass)
	out := append(append(append([]byte{}, b[:loc[1]]...), line...), b[loc[1]:]...)
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, fi.Mode().Perm()); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(tmp, 0, wwwData); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	return os.Rename(tmp, path)
}

// UpgradeCacheUsers gives every site of this server its cache user (at
// startup; a no-op once done).
func (s *Service) UpgradeCacheUsers(ctx context.Context) error {
	if s.CacheACL == nil {
		return nil
	}
	sites, err := s.Store.ListSites(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, st := range sites {
		if err := s.ensureCacheCredentials(st.ID); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("site %s: %w", st.ID, err))
		}
	}
	errs = append(errs, s.SyncCacheUsers(ctx))
	return errors.Join(errs...)
}
