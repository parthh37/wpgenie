package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Valkey clears a site's object cache in the shared Valkey container, and
// keeps its users.
//
// The daemon deletes the keys itself rather than running `wp cache flush`:
// WP-CLI loads the site's wp-content/object-cache.php (a file the site can
// replace) with no open_basedir and exec() enabled, i.e. outside the jail.
//
// Every site connects as its own ACL user that can only touch keys under
// its prefix (and run no server-wide command): without that, PHP on one
// site could read or rewrite another's cached options and users (a tenant
// making itself administrator elsewhere). The default user is off; the
// daemon is the "wpgenie" user.
type Valkey struct {
	Docker
	Container string
	// AdminPassword is the "wpgenie" user's ("": the default user, before
	// ACLs are on).
	AdminPassword func() string
}

// AdminUser is the daemon's ACL user.
const AdminUser = "wpgenie"

// flushPrefixScript deletes every key matching ARGV[1] server-side, so any
// key name (spaces, quotes, binary) is handled, in batches that keep each
// UNLINK well under Lua's argument limit.
const flushPrefixScript = `local cursor, n = "0", 0
repeat
  local r = redis.call("SCAN", cursor, "MATCH", ARGV[1], "COUNT", 1000)
  cursor = r[1]
  if #r[2] > 0 then n = n + redis.call("UNLINK", unpack(r[2])) end
until cursor == "0"
return n`

// Prefixes are site IDs plus ":"; nothing that is a glob metacharacter, so
// the MATCH pattern can't reach beyond the site's own keys.
var prefixRe = regexp.MustCompile(`^[a-z0-9]{1,32}:$`)

// cli runs valkey-cli in the container as the daemon's user. The password
// goes in the exec's environment, never in argv (ps inside the container).
func (v *Valkey) cli(ctx context.Context, args ...string) ([]byte, error) {
	full := []string{"exec"}
	if v.AdminPassword != nil {
		if pw := v.AdminPassword(); pw != "" {
			full = append(full, "-e", "REDISCLI_AUTH="+pw)
			args = append([]string{"--user", AdminUser}, args...)
		}
	}
	full = append(full, v.Container, "valkey-cli")
	return v.run(ctx, nil, append(full, args...)...)
}

func (v *Valkey) FlushPrefix(ctx context.Context, prefix string) error {
	if !prefixRe.MatchString(prefix) {
		return fmt.Errorf("refusing to flush unsafe cache prefix %q", prefix)
	}
	out, err := v.cli(ctx, "--no-raw", "EVAL", flushPrefixScript, "0", prefix+"*")
	if err == nil && bytes.Contains(out, []byte("ERR")) {
		err = fmt.Errorf("flush: %s", bytes.TrimSpace(out))
	}
	return err
}

// ErrNoACLFile: Valkey was started without an ACL file (an install from
// before site users, until the container restarts with it).
var ErrNoACLFile = errors.New("valkey runs without an ACL file")

// LoadACL makes Valkey read its ACL file again.
func (v *Valkey) LoadACL(ctx context.Context) error {
	out, err := v.cli(ctx, "ACL", "LOAD")
	s := strings.TrimSpace(string(out))
	switch {
	case strings.Contains(s, "not configured to use an ACL file"):
		return ErrNoACLFile
	case err != nil:
		return err
	case s != "OK":
		return fmt.Errorf("ACL LOAD: %s", s)
	}
	return nil
}

// Restart restarts the container (it picks up the ACL file at start).
func (v *Valkey) Restart(ctx context.Context) error {
	_, err := v.run(ctx, nil, "restart", v.Container)
	return err
}

// ACLUser is a site's object cache user.
type ACLUser struct {
	Name     string
	Password string
	// Prefix is the only key prefix it may touch (the site ID and ":").
	Prefix string
}

var aclNameRe = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// RenderACL is the ACL file: the default user off, the daemon's user, and
// one user per site. Passwords are stored hashed. Site users get every
// command but the dangerous and administrative ones (KEYS, FLUSHALL,
// CONFIG, CLIENT, DEBUG, ...), no functions, no killing or flushing others'
// scripts, no pub/sub channels, keys under their prefix only; INFO is
// allowed (the object-cache drop-in reads the server version). SCAN still
// lists other sites' key names (not values); that is the price of the
// drop-in's own flush, which needs it.
func RenderACL(adminPassword string, sites []ACLUser) ([]byte, error) {
	if adminPassword == "" {
		return nil, errors.New("no admin password")
	}
	var b bytes.Buffer
	b.WriteString("user default off resetkeys resetchannels -@all\n")
	fmt.Fprintf(&b, "user %s on #%s ~* &* +@all\n", AdminUser, hashPassword(adminPassword))
	for _, u := range sites {
		if !aclNameRe.MatchString(u.Name) || u.Name == AdminUser || u.Name == "default" {
			return nil, fmt.Errorf("invalid cache user %q", u.Name)
		}
		if !prefixRe.MatchString(u.Prefix) {
			return nil, fmt.Errorf("invalid cache prefix %q", u.Prefix)
		}
		if u.Password == "" {
			return nil, fmt.Errorf("cache user %s has no password", u.Name)
		}
		fmt.Fprintf(&b, "user %s on #%s resetkeys ~%s* resetchannels +@all -@dangerous -@admin +info -function -script|flush -script|kill\n",
			u.Name, hashPassword(u.Password), u.Prefix)
	}
	return b.Bytes(), nil
}

func hashPassword(p string) string {
	sum := sha256.Sum256([]byte(p))
	return hex.EncodeToString(sum[:])
}
