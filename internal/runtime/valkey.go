package runtime

import (
	"context"
	"fmt"
	"regexp"
)

// Valkey clears a site's object cache in the shared Valkey container.
//
// The daemon deletes the keys itself rather than running `wp cache flush`:
// WP-CLI loads the site's wp-content/object-cache.php (a file the site can
// replace) with no open_basedir and exec() enabled, i.e. outside the jail.
type Valkey struct {
	Docker
	Container string
}

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

func (v *Valkey) FlushPrefix(ctx context.Context, prefix string) error {
	if !prefixRe.MatchString(prefix) {
		return fmt.Errorf("refusing to flush unsafe cache prefix %q", prefix)
	}
	_, err := v.run(ctx, nil, "exec", v.Container, "valkey-cli", "--no-raw", "EVAL", flushPrefixScript, "0", prefix+"*")
	return err
}
