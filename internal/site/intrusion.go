package site

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/parthh37/wpgenie/internal/store"
)

// Intrusion detection, part of every scan: what changed since the previous
// scan that nobody asked for. New administrator accounts (the first thing
// an attacker makes), and PHP files in plugins, themes and must-use
// plugins that changed without an update (a backdoor added to a plugin
// wordpress.org doesn't know, which checksums can't catch).
//
// The file baseline is a manifest of hashes kept in the site's directory,
// next to wp-config.php: outside the docroot (never served), root-only
// (PHP can't read or rewrite it), deleted with the site. Not in the panel
// database: a big site's manifest is megabytes, rewritten every night.

// AdminAccount is a WordPress account with administrator powers: a role
// that can manage users, settings or plugins, or a network administrator.
type AdminAccount struct {
	ID         int    `json:"id"`
	Login      string `json:"login"`
	Email      string `json:"email,omitempty"`
	Registered string `json:"registered,omitempty"` // as WordPress stores it (UTC)
	Role       string `json:"role,omitempty"`
	Super      bool   `json:"super,omitempty"` // a multisite network administrator
}

// FileChange is a PHP file that is new or different since the last scan.
type FileChange struct {
	Path      string `json:"path"`      // under wp-content
	Change    string `json:"change"`    // changed | added
	Component string `json:"component"` // "plugin akismet", "theme astra", "must-use plugins"
}

const (
	ChangeChanged = "changed"
	ChangeAdded   = "added"
)

// Intrusion is what a scan found compared with the previous one.
type Intrusion struct {
	// Admins are the administrator accounts at this scan: the next scan
	// compares with them.
	Admins []AdminAccount `json:"admins"`
	// NewAdmins weren't there at the previous scan, nor added from the
	// panel since.
	NewAdmins []AdminAccount `json:"new_admins"`
	// AdminsBaseline says why administrators weren't compared ("" when
	// they were): this scan only records them.
	AdminsBaseline string `json:"admins_baseline,omitempty"`

	// FileChanges are PHP files no update explains (at most maxListed;
	// FileChangesTotal counts them all).
	FileChanges      []FileChange `json:"file_changes"`
	FileChangesTotal int          `json:"file_changes_total"`
	// Updated are plugins and themes whose files changed because they were
	// updated or installed: not reported.
	Updated []string `json:"updated,omitempty"`
	// FilesChecked counts the PHP files hashed; FilesTruncated: the site
	// has more than a scan hashes, so new files can't be told apart.
	FilesChecked   int  `json:"files_checked"`
	FilesTruncated bool `json:"files_truncated,omitempty"`
	// FilesBaseline says why files weren't compared ("" when they were).
	FilesBaseline string `json:"files_baseline,omitempty"`
}

// Bounds of the file manifest: PHP files under this size are hashed, at
// most this many (a large WooCommerce site has ~20,000).
const (
	manifestMaxFiles = 60000
	manifestMaxBytes = manifestMaxFiles * 160 // a sha256sum line is ~70 bytes plus the path
	manifestName     = "integrity-manifest.json"
)

// listAdminsPHP prints the accounts with administrator powers: users of
// any role that can manage options, users or plugins (custom roles
// included), and network administrators.
const listAdminsPHP = `
$powerful = array( 'manage_options', 'promote_users', 'edit_users', 'create_users', 'delete_users', 'install_plugins', 'activate_plugins', 'edit_plugins', 'unfiltered_upload' );
$roles = array( 'administrator' );
foreach ( wp_roles()->roles as $name => $def ) {
	$caps = isset( $def['capabilities'] ) && is_array( $def['capabilities'] ) ? $def['capabilities'] : array();
	foreach ( $powerful as $cap ) {
		if ( ! empty( $caps[ $cap ] ) ) { $roles[] = $name; break; }
	}
}
$out = array();
$seen = array();
foreach ( get_users( array( 'role__in' => array_values( array_unique( $roles ) ), 'orderby' => 'ID', 'order' => 'ASC', 'number' => 500 ) ) as $u ) {
	$r = array_values( array_intersect( (array) $u->roles, $roles ) );
	$out[] = array( 'id' => $u->ID, 'login' => $u->user_login, 'email' => $u->user_email, 'registered' => $u->user_registered, 'role' => $r ? $r[0] : '' );
	$seen[ $u->ID ] = true;
}
if ( is_multisite() && function_exists( 'get_super_admins' ) ) {
	foreach ( get_super_admins() as $login ) {
		$u = get_user_by( 'login', $login );
		if ( $u && empty( $seen[ $u->ID ] ) ) {
			$out[] = array( 'id' => $u->ID, 'login' => $u->user_login, 'email' => $u->user_email, 'registered' => $u->user_registered, 'super' => true );
			$seen[ $u->ID ] = true;
		}
	}
}
echo wp_json_encode( $out );
`

func (s *Service) listAdmins(ctx context.Context, id string) ([]AdminAccount, error) {
	var raw []struct {
		ID         flexInt `json:"id"`
		Login      string  `json:"login"`
		Email      string  `json:"email"`
		Registered string  `json:"registered"`
		Role       string  `json:"role"`
		Super      bool    `json:"super"`
	}
	if err := s.wpJSON(ctx, id, &raw, "eval", listAdminsPHP); err != nil {
		return nil, err
	}
	out := make([]AdminAccount, 0, len(raw))
	for _, u := range raw {
		out = append(out, AdminAccount{ID: int(u.ID), Login: u.Login, Email: u.Email, Registered: u.Registered, Role: u.Role, Super: u.Super})
	}
	return out, nil
}

// newAdmins are the accounts in cur that prev didn't have (by ID: a
// renamed login is the same account).
func newAdmins(prev, cur []AdminAccount) []AdminAccount {
	out := []AdminAccount{}
	for _, a := range cur {
		if !slices.ContainsFunc(prev, func(p AdminAccount) bool { return p.ID == a.ID }) && len(out) < maxListed {
			out = append(out, a)
		}
	}
	return out
}

// manifest is the hashes of a site's plugin, theme and must-use plugin
// PHP files at a scan.
type manifest struct {
	ScannedAt time.Time         `json:"scanned_at"`
	Truncated bool              `json:"truncated,omitempty"`
	Files     map[string]string `json:"files"` // path under wp-content -> sha256
}

var sha256LineRe = regexp.MustCompile(`^([0-9a-f]{64})  (.+)$`)

// parseManifest reads sha256sum output (paths relative to wp-content).
// Lines that aren't one (a path with a newline in it) are skipped.
func parseManifest(out []byte) map[string]string {
	files := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 64<<10)
	for sc.Scan() {
		m := sha256LineRe.FindStringSubmatch(sc.Text())
		if m == nil || len(files) >= manifestMaxFiles {
			continue
		}
		p := path.Clean(strings.TrimPrefix(m[2], "./"))
		if strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/") {
			continue
		}
		files[p] = m[1]
	}
	return files
}

// manifestScript hashes PHP files in plugins, themes and mu-plugins,
// paths relative to wp-content. busybox find and sha256sum, in the site's
// own view of its docroot (no symlinks followed out of it).
const manifestScript = `cd "$1" || exit 0
for d in plugins themes mu-plugins; do
	[ -d "$d" ] || continue
	find "$d" -type f \( -iname '*.php' -o -iname '*.phtml' -o -iname '*.php[0-9]' -o -iname '*.phar' -o -iname '*.inc' \) -size -2048k -exec sha256sum {} +
done
exit 0`

// collectManifest hashes the site's code now.
func (s *Service) collectManifest(ctx context.Context, st *store.Site) (*manifest, error) {
	var out bytes.Buffer
	lw := &limitWriter{w: &out, n: manifestMaxBytes}
	if err := s.Runtime.Exec(ctx, st.ID, nil, lw, "sh", "-c", manifestScript, "sh",
		s.Cfg.SiteRoot(st.ID)+"/wp-content"); err != nil {
		return nil, fmt.Errorf("hashing plugin and theme files: %w", err)
	}
	m := &manifest{ScannedAt: time.Now().UTC(), Files: parseManifest(out.Bytes())}
	m.Truncated = lw.n == 0 || len(m.Files) >= manifestMaxFiles
	return m, nil
}

func (s *Service) manifestPath(id string) string {
	return filepath.Join(s.Cfg.SiteDir(id), manifestName)
}

// loadManifest returns the previous scan's manifest; nil if there's none.
func (s *Service) loadManifest(id string) (*manifest, error) {
	b, err := os.ReadFile(s.manifestPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, nil // unreadable: start over
	}
	return &m, nil
}

// saveManifest replaces the manifest whole (root-only; see above).
func (s *Service) saveManifest(id string, m *manifest) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	p := s.manifestPath(id)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, p)
}

// componentOf names what a file under wp-content belongs to: kind is
// "plugin", "theme" or "mu-plugin", slug the plugin or theme ("" for
// must-use plugins, which nothing updates).
func componentOf(p string) (kind, slug string) {
	dir, rest, _ := strings.Cut(p, "/")
	first, _, nested := strings.Cut(rest, "/")
	switch dir {
	case "plugins":
		if !nested {
			first = strings.TrimSuffix(first, path.Ext(first)) // a single-file plugin (hello.php)
		}
		return "plugin", first
	case "themes":
		return "theme", first
	}
	return "mu-plugin", ""
}

func componentLabel(kind, slug string) string {
	if kind == "mu-plugin" {
		return "must-use plugins"
	}
	return kind + " " + slug
}

// managedFiles are WPGenie's own wrappers: rewritten whenever the panel's
// settings change, so never a finding.
var managedFiles = []string{pageCacheWrapperPath, smtpWrapperPath, imagesWrapperPath, cdnWrapperPath,
	offloadWrapperPath, optimizeWrapperPath, brandWrapperPath, hardeningWrapperPath}

func managedFile(p string) bool {
	return slices.Contains(managedFiles, "wp-content/"+p)
}

// explainedChanges are the plugins and themes whose files were meant to
// change since the previous scan: their version changed (an update from
// anywhere), they're new (installed), or WPGenie updated them. Keys are
// "plugin slug" / "theme slug"; values say what happened.
func explainedChanges(prev, cur *Inventory, updated []UpdateResult) map[string]string {
	out := map[string]string{}
	if prev == nil || cur == nil {
		return out
	}
	find := func(list []Component, slug string) *Component {
		i := slices.IndexFunc(list, func(c Component) bool { return c.Slug == slug })
		if i < 0 {
			return nil
		}
		return &list[i]
	}
	for _, pair := range []struct {
		kind      string
		prev, cur []Component
	}{{"plugin", prev.Plugins, cur.Plugins}, {"theme", prev.Themes, cur.Themes}} {
		for _, c := range pair.cur {
			key := pair.kind + " " + c.Slug
			switch p := find(pair.prev, c.Slug); {
			case p == nil:
				out[key] = key + " installed"
			case p.Version != c.Version:
				out[key] = fmt.Sprintf("%s updated %s → %s", key, p.Version, c.Version)
			}
		}
	}
	for _, r := range updated {
		if r.Status == UpdateUpdated && (r.Type == "plugin" || r.Type == "theme") {
			key := r.Type + " " + r.Slug
			if _, ok := out[key]; !ok {
				out[key] = fmt.Sprintf("%s updated %s → %s", key, r.From, r.To)
			}
		}
	}
	return out
}

// diffManifests lists the files changed or added from prev to cur that
// explained doesn't cover (at most maxListed; total counts them all), in
// path order. Added files aren't reported when either manifest was cut
// short: a file past the cut isn't new.
func diffManifests(prev, cur *manifest, explained map[string]string) (changes []FileChange, total int) {
	changes = []FileChange{}
	paths := make([]string, 0, len(cur.Files))
	for p := range cur.Files {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, p := range paths {
		if managedFile(p) {
			continue
		}
		was, existed := prev.Files[p]
		var change string
		switch {
		case existed && was != cur.Files[p]:
			change = ChangeChanged
		case !existed && !prev.Truncated && !cur.Truncated:
			change = ChangeAdded
		default:
			continue
		}
		kind, slug := componentOf(p)
		if _, ok := explained[kind+" "+slug]; ok {
			continue
		}
		total++
		if len(changes) < maxListed {
			changes = append(changes, FileChange{Path: p, Change: change, Component: componentLabel(kind, slug)})
		}
	}
	return changes, total
}

// replacedFiles says why the site's files were replaced wholesale since
// t, by WPGenie ("" if they weren't): a restore or a staging push. Then
// the comparison starts over.
func (s *Service) replacedFiles(ctx context.Context, id string, t time.Time) string {
	jobs, err := s.Store.Jobs(ctx, id, false, 100)
	if err != nil {
		return ""
	}
	for _, j := range jobs {
		if j.Status != store.JobSucceeded || !j.FinishedAt.After(t) {
			continue
		}
		switch j.Kind {
		case "restore":
			return "the site was restored from a backup since the last scan"
		case "push":
			return "a staging copy was pushed to the site since the last scan"
		}
	}
	return ""
}

// updatesSince are the components WPGenie updated after t.
func (s *Service) updatesSince(ctx context.Context, id string, t time.Time) []UpdateResult {
	runs, err := s.Store.Updates(ctx, id, 50)
	if err != nil {
		return nil
	}
	var out []UpdateResult
	for _, r := range runs {
		if !r.StartedAt.After(t) || r.Details == "" {
			continue
		}
		var d UpdateDetails
		if json.Unmarshal([]byte(r.Details), &d) == nil {
			out = append(out, d.Results...)
		}
	}
	return out
}

// checkIntrusion compares the site with the previous scan (prev: nil if
// none) and records this one's baseline.
func (s *Service) checkIntrusion(ctx context.Context, st *store.Site, prev *ScanReport, inv *Inventory) (*Intrusion, error) {
	in := &Intrusion{Admins: []AdminAccount{}, NewAdmins: []AdminAccount{}, FileChanges: []FileChange{}}
	var errs []string
	var since time.Time
	if prev != nil {
		since = prev.ScannedAt
	}
	replaced := ""
	if prev != nil {
		replaced = s.replacedFiles(ctx, st.ID, since)
	}

	// The previous administrators, if that scan knew them.
	var prevAdmins []AdminAccount
	known := prev != nil && prev.Intrusion != nil && prev.Intrusion.AdminsBaseline != adminsUnknown
	if known {
		prevAdmins = prev.Intrusion.Admins
	}
	admins, err := s.listAdmins(ctx, st.ID)
	switch {
	case err != nil:
		errs = append(errs, "administrators: "+err.Error())
		// The previous list stays the baseline rather than be lost.
		in.AdminsBaseline = adminsUnknown
		if known {
			in.Admins = append(in.Admins, prevAdmins...)
			in.AdminsBaseline = "the administrators couldn't be listed"
		}
	case !known:
		in.Admins = admins
		in.AdminsBaseline = "first check: administrators recorded, new ones are reported from the next scan"
	case replaced != "":
		in.Admins = admins
		in.AdminsBaseline = replaced
	default:
		in.Admins = admins
		in.NewAdmins = newAdmins(prevAdmins, admins)
	}

	cur, err := s.collectManifest(ctx, st)
	if err != nil {
		errs = append(errs, err.Error())
		in.FilesBaseline = "the files couldn't be checked"
	} else {
		in.FilesChecked, in.FilesTruncated = len(cur.Files), cur.Truncated
		old, lerr := s.loadManifest(st.ID)
		switch {
		case lerr != nil:
			errs = append(errs, "previous file list: "+lerr.Error())
			in.FilesBaseline = "the previous file list couldn't be read"
		case old == nil || prev == nil:
			in.FilesBaseline = "first check: files recorded, changes are reported from the next scan"
		case replaced != "":
			in.FilesBaseline = replaced
		default:
			var prevInv *Inventory
			if prev != nil {
				prevInv = prev.Inventory
			}
			explained := explainedChanges(prevInv, inv, s.updatesSince(ctx, st.ID, old.ScannedAt))
			in.FileChanges, in.FileChangesTotal = diffManifests(old, cur, explained)
			for _, v := range explained {
				in.Updated = append(in.Updated, v)
			}
			slices.Sort(in.Updated)
			if len(in.Updated) > maxListed {
				in.Updated = in.Updated[:maxListed]
			}
		}
		if serr := s.saveManifest(st.ID, cur); serr != nil {
			errs = append(errs, "saving the file list: "+serr.Error())
		}
	}
	if len(errs) > 0 {
		return in, errors.New(strings.Join(errs, "; "))
	}
	return in, nil
}

// adminsUnknown marks a report whose administrators were never listed:
// the next scan records them rather than report them all as new.
const adminsUnknown = "the administrators couldn't be listed yet"

// noteAdmin adds an administrator made from the panel to the last scan's
// list, so the next scan doesn't report them as an intruder. Best effort:
// at worst they're reported once.
func (s *Service) noteAdmin(ctx context.Context, id string, a AdminAccount) {
	at, b, err := s.Store.Scan(ctx, id)
	if err != nil {
		return
	}
	var r ScanReport
	if json.Unmarshal(b, &r) != nil || r.Intrusion == nil || r.Intrusion.AdminsBaseline == adminsUnknown {
		return
	}
	if slices.ContainsFunc(r.Intrusion.Admins, func(p AdminAccount) bool { return p.ID == a.ID }) {
		return
	}
	r.Intrusion.Admins = append(r.Intrusion.Admins, a)
	if nb, err := json.Marshal(&r); err == nil {
		if err := s.Store.SaveScan(ctx, id, at, nb); err != nil {
			s.Log.Warn("recording an administrator made from the panel", "site", id, "err", err)
		}
	}
}
