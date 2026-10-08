package site

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/parthh37/wpgenie/internal/jobs"
	"github.com/parthh37/wpgenie/internal/runtime"
	"github.com/parthh37/wpgenie/internal/store"
)

// The site's Tools: search & replace in the database, WordPress's
// scheduled tasks, and a few WordPress settings, all through WP-CLI with
// plugins and themes skipped (see runtime.WPArgs). Maintenance mode, debug
// mode and themes have files of their own.

// ---- Search & replace ----

const (
	maxSearchReplace     = 1000 // bytes, each of search and replace
	minSearch            = 3    // characters: "a" → "b" would wreck a database
	searchReplaceTimeout = 45 * time.Minute
	searchReplaceOutput  = 4 << 20
)

// SearchReplaceInput is a search & replace across the site's tables.
type SearchReplaceInput struct {
	Search  string `json:"search"`
	Replace string `json:"replace"`
	// DryRun only counts what would change.
	DryRun bool `json:"dry_run"`
	// HaveBackup: the person confirms they have a recent backup. Needed
	// for a real run only when WPGenie can't take one first (no backups
	// configured on this server).
	HaveBackup bool `json:"have_backup"`
}

// SearchReplaceTable is one table's count.
type SearchReplaceTable struct {
	Table        string `json:"table"`
	Replacements int    `json:"replacements"`
}

// SearchReplaceResult is a search & replace's outcome (the job's result).
type SearchReplaceResult struct {
	DryRun  bool                 `json:"dry_run"`
	Search  string               `json:"search"`
	Replace string               `json:"replace"`
	Total   int                  `json:"total"`
	Tables  []SearchReplaceTable `json:"tables"`
	// Backup is the backup taken first (short ID), if any. BackupFirst:
	// a real run takes one (backups are configured on this server).
	Backup      string `json:"backup,omitempty"`
	BackupFirst bool   `json:"backup_first"`
}

// validSearchReplace checks a search & replace. Both strings go to WP-CLI
// as arguments of their own (never through a shell), but WP-CLI reads
// anything starting with "--" as an option of its own, so neither may
// start with a dash.
func validSearchReplace(in *SearchReplaceInput) error {
	check := func(what, v string) error {
		switch {
		case v == "" || strings.TrimSpace(v) == "":
			return fmt.Errorf("%w: enter the text to %s", ErrInvalidInput, what)
		case len(v) > maxSearchReplace:
			return fmt.Errorf("%w: the text to %s is longer than %d characters", ErrInvalidInput, what, maxSearchReplace)
		case !utf8.ValidString(v):
			return fmt.Errorf("%w: the text to %s isn't valid text", ErrInvalidInput, what)
		case strings.ContainsFunc(v, unicode.IsControl):
			return fmt.Errorf("%w: the text to %s has line breaks or control characters", ErrInvalidInput, what)
		case strings.HasPrefix(v, "-"):
			return fmt.Errorf("%w: the text to %s can't start with a dash", ErrInvalidInput, what)
		}
		return nil
	}
	if err := check("search for", in.Search); err != nil {
		return err
	}
	if err := check("replace it with", in.Replace); err != nil {
		return err
	}
	if utf8.RuneCountInString(in.Search) < minSearch {
		return fmt.Errorf("%w: search for at least %d characters", ErrInvalidInput, minSearch)
	}
	if in.Search == in.Replace {
		return fmt.Errorf("%w: the search and the replacement are the same", ErrInvalidInput)
	}
	return nil
}

// replaceTextArgs is the WP-CLI command line: every table with the
// site's prefix, serialized data handled in PHP (--precise), and never the
// posts' guid (feed readers use it as a permanent ID).
func replaceTextArgs(in SearchReplaceInput) []string {
	args := []string{"search-replace", in.Search, in.Replace, "--precise", "--all-tables-with-prefix",
		"--skip-columns=guid", "--report-changed-only"}
	if in.DryRun {
		args = append(args, "--dry-run")
	}
	return args
}

var srTotalRe = regexp.MustCompile(`(?m)^Success: (?:Made )?(\d+) replacements?\b`)

// parseSearchReplace reads WP-CLI's report: a table of table, column,
// replacements and type, tab-separated when its output isn't a terminal
// (as here), drawn with "|" when it is; then a "Success: …" line.
func parseSearchReplace(out string) (total int, tables []SearchReplaceTable, err error) {
	byTable := map[string]int{}
	var order []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		var f []string
		switch {
		case strings.Count(line, "\t") == 3:
			f = strings.Split(line, "\t")
		case strings.HasPrefix(line, "|") && strings.HasSuffix(line, "|"):
			f = strings.Split(strings.Trim(line, "|"), "|")
		}
		if len(f) != 4 {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(f[2]))
		if err != nil || n < 0 {
			continue // the header
		}
		t := strings.TrimSpace(f[0])
		if t == "" {
			continue
		}
		if _, seen := byTable[t]; !seen {
			order = append(order, t)
		}
		byTable[t] += n
	}
	sum := 0
	tables = []SearchReplaceTable{}
	for _, t := range order {
		if byTable[t] > 0 {
			tables = append(tables, SearchReplaceTable{Table: t, Replacements: byTable[t]})
			sum += byTable[t]
		}
	}
	m := srTotalRe.FindStringSubmatch(out)
	switch {
	case m != nil:
		total, _ = strconv.Atoi(m[1])
	case len(order) > 0:
		total = sum
	default:
		return 0, nil, errors.New("WP-CLI reported no result")
	}
	slices.SortStableFunc(tables, func(a, b SearchReplaceTable) int { return cmp.Compare(b.Replacements, a.Replacements) })
	return total, tables, nil
}

// StartSearchReplace runs a search & replace as a job: a dry run counts,
// a real run backs the site up first (when backups are available here)
// and empties the caches afterwards.
func (s *Service) StartSearchReplace(ctx context.Context, id string, in SearchReplaceInput) (int64, error) {
	if err := validSearchReplace(&in); err != nil {
		return 0, err
	}
	if err := s.requireActive(ctx, id); err != nil {
		return 0, err
	}
	if !in.DryRun && s.Backups == nil && !in.HaveBackup {
		return 0, fmt.Errorf("%w: WPGenie can't back the site up first here: confirm you have a recent backup", ErrInvalidInput)
	}
	kind := "search-replace"
	if in.DryRun {
		kind = "search-replace-preview"
	}
	spec := s.siteJob(id, kind, !in.DryRun)
	spec.Timeout = searchReplaceTimeout
	return s.Jobs.Submit(ctx, spec, func(ctx context.Context, t *jobs.Task) error {
		st, err := s.Store.GetSite(ctx, id)
		if err != nil {
			return err
		}
		if err := activeSite(st); err != nil {
			return err
		}
		res := &SearchReplaceResult{DryRun: in.DryRun, Search: in.Search, Replace: in.Replace, Tables: []SearchReplaceTable{},
			BackupFirst: s.Backups != nil}
		from := 5
		if !in.DryRun && s.Backups != nil {
			repo, err := s.siteRepo(ctx, id)
			if err != nil {
				return err
			}
			b, err := s.backupLocked(ctx, st, repo, BackupSafety, func(pct int, step string) {
				t.Progress(pct*50/100, "Backing up first: "+strings.ToLower(step))
			})
			if err != nil {
				return fmt.Errorf("backing up first failed, nothing was replaced: %w", err)
			}
			res.Backup, from = b.ShortID, 55
		}
		step := "Searching the database"
		if !in.DryRun {
			step = "Replacing in the database"
		}
		t.Progress(from, step)
		var out bytes.Buffer
		err = s.Runtime.Exec(ctx, id, nil, &limitWriter{w: &out, n: searchReplaceOutput}, runtime.WPArgs(replaceTextArgs(in)...)...)
		if err != nil {
			s.searchReplaceEvent(id, res, err)
			return fmt.Errorf("search & replace: %w", err)
		}
		if res.Total, res.Tables, err = parseSearchReplace(out.String()); err != nil {
			return fmt.Errorf("search & replace: %w", err)
		}
		t.SetResult(res)
		if !in.DryRun {
			t.Progress(95, "Emptying the caches")
			if err := s.Purge(ctx, id); err != nil {
				s.Log.Warn("purging the cache after a search & replace", "site", id, "err", err)
			}
			s.searchReplaceEvent(id, res, nil)
		}
		return nil
	})
}

func (s *Service) searchReplaceEvent(id string, r *SearchReplaceResult, err error) {
	if r.DryRun {
		return
	}
	what := fmt.Sprintf("%q → %q", truncate(r.Search, 80), truncate(r.Replace, 80))
	backup := ""
	if r.Backup != "" {
		backup = " (backup " + r.Backup + " was taken first)"
	}
	if err != nil {
		s.event(id, "tools", fmt.Sprintf("Search & replace %s FAILED: %s%s", what, truncate(err.Error(), 200), backup))
		return
	}
	s.event(id, "tools", fmt.Sprintf("Search & replace %s: %d replacement%s in %d table%s%s", what,
		r.Total, plural(r.Total, "", "s"), len(r.Tables), plural(len(r.Tables), "", "s"), backup))
}

// ---- Scheduled tasks (WP-Cron) ----

const (
	maxCronEvents = 500
	// cronOverdue: an event this late means cron isn't running (WPGenie
	// runs it every minute).
	cronOverdue = 10 * time.Minute
	// cronStale: no run of the every-minute loop seen for this long.
	cronStale = 5 * time.Minute
)

// CronEvent is one scheduled WordPress event.
type CronEvent struct {
	Hook    string    `json:"hook"`
	NextRun time.Time `json:"next_run"`
	// Recurrence is how often it repeats, in words ("" = once).
	Recurrence string `json:"recurrence"`
	Schedule   string `json:"schedule,omitempty"`
	Interval   int    `json:"interval,omitempty"` // seconds
	// Sig identifies the event among the hook's (by its arguments); Args
	// is how many arguments it gets (their values aren't shown).
	Sig     string `json:"sig"`
	Args    int    `json:"args"`
	Overdue bool   `json:"overdue"`
}

// CronRunner is whether WPGenie's every-minute cron runs for the site.
type CronRunner struct {
	// State: ok, failing, waiting (no run seen since the daemon started),
	// staging (staging sites get no cron) or old_image.
	State   string    `json:"state"`
	LastRun time.Time `json:"last_run,omitzero"`
	Error   string    `json:"error,omitempty"`
}

// CronInfo is a site's scheduled tasks and its cron runner.
type CronInfo struct {
	Events  []CronEvent `json:"events"`
	Overdue int         `json:"overdue"`
	Runner  CronRunner  `json:"runner"`
}

// looseInt decodes a JSON number or numeric string; anything else
// (false, null) is 0. WP-CLI prints "interval" as false for one-off events.
type looseInt int

func (l *looseInt) UnmarshalJSON(b []byte) error {
	n, _ := strconv.Atoi(strings.Trim(string(b), `"`))
	*l = looseInt(n)
	return nil
}

// looseString decodes a JSON string; anything else is "" (WP-CLI prints
// "schedule" as false for one-off events).
type looseString string

func (l *looseString) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*l = looseString(s)
	} else {
		*l = ""
	}
	return nil
}

var sigRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// parseCronEvents reads `wp cron event list --format=json` with the fields
// cronFields, soonest first.
func parseCronEvents(b []byte, now time.Time) ([]CronEvent, error) {
	var raw []struct {
		Hook       string          `json:"hook"`
		Time       looseInt        `json:"time"`
		Sig        string          `json:"sig"`
		Args       json.RawMessage `json:"args"`
		Schedule   looseString     `json:"schedule"`
		Interval   looseInt        `json:"interval"`
		Recurrence looseString     `json:"recurrence"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("cron events: unexpected output: %w", err)
	}
	out := make([]CronEvent, 0, min(len(raw), maxCronEvents))
	for _, r := range raw {
		if r.Hook == "" || r.Time <= 0 {
			continue
		}
		e := CronEvent{Hook: truncate(strings.ToValidUTF8(r.Hook, "?"), 200), NextRun: time.Unix(int64(r.Time), 0).UTC(),
			Schedule: string(r.Schedule), Interval: int(r.Interval)}
		if sigRe.MatchString(r.Sig) {
			e.Sig = r.Sig
		} else {
			e.Sig = ""
		}
		if rec := string(r.Recurrence); rec != "" && !strings.EqualFold(rec, "Non-repeating") {
			e.Recurrence = rec
		}
		var args []json.RawMessage
		if json.Unmarshal(r.Args, &args) == nil {
			e.Args = len(args)
		} else {
			var obj map[string]json.RawMessage
			if json.Unmarshal(r.Args, &obj) == nil {
				e.Args = len(obj)
			}
		}
		e.Overdue = now.Sub(e.NextRun) > cronOverdue
		out = append(out, e)
	}
	slices.SortStableFunc(out, func(a, b CronEvent) int { return a.NextRun.Compare(b.NextRun) })
	if len(out) > maxCronEvents {
		out = out[:maxCronEvents]
	}
	return out, nil
}

const cronFields = "--fields=hook,time,sig,args,schedule,interval,recurrence"

// CronEvents lists the site's scheduled events and whether WPGenie's
// every-minute cron is running them.
func (s *Service) CronEvents(ctx context.Context, id string) (*CronInfo, error) {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := activeSite(st); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := s.Runtime.Exec(ctx, id, nil, &limitWriter{w: &out, n: 4 << 20},
		runtime.WPArgs("cron", "event", "list", "--format=json", cronFields)...); err != nil {
		return nil, fmt.Errorf("listing scheduled tasks: %w", err)
	}
	now := time.Now()
	events, err := parseCronEvents(bytes.TrimSpace(out.Bytes()), now)
	if err != nil {
		return nil, err
	}
	info := &CronInfo{Events: events, Runner: s.cronRunner(st, now)}
	for _, e := range events {
		if e.Overdue {
			info.Overdue++
		}
	}
	return info, nil
}

// cronRunner says how the every-minute loop is doing for a site.
func (s *Service) cronRunner(st *store.Site, now time.Time) CronRunner {
	if st.ParentID != "" {
		return CronRunner{State: "staging"}
	}
	v, ok := s.cronRuns.Load(st.ID)
	if !ok {
		return CronRunner{State: "waiting"}
	}
	r := v.(cronRun)
	out := CronRunner{LastRun: r.At, Error: r.Err}
	switch {
	case r.NoJail:
		out.State = "old_image"
	case r.Err != "":
		out.State = "failing"
	case now.Sub(r.At) > cronStale:
		out.State, out.Error = "failing", "no run in the last few minutes"
	default:
		out.State = "ok"
	}
	return out
}

// CronRunInput picks one scheduled event (as listed).
type CronRunInput struct {
	Hook string `json:"hook"`
	Time int64  `json:"time"`
	Sig  string `json:"sig"`
}

const noEventMarker = "wpgenie: no such event"

// cronDuePHP moves one event to "due now" in WordPress's cron array, the
// way wp_schedule_single_event() stores events; WordPress's own cron run
// then runs it (and schedules its next run, if it repeats).
const cronDuePHP = `
$hook = base64_decode( '%s' );
$ts   = %d;
$sig  = '%s';
$crons = _get_cron_array();
if ( ! is_array( $crons ) || ! isset( $crons[ $ts ][ $hook ][ $sig ] ) ) {
	fwrite( STDERR, "` + noEventMarker + `\n" );
	exit( 3 );
}
$event = $crons[ $ts ][ $hook ][ $sig ];
unset( $crons[ $ts ][ $hook ][ $sig ] );
if ( empty( $crons[ $ts ][ $hook ] ) ) {
	unset( $crons[ $ts ][ $hook ] );
}
if ( empty( $crons[ $ts ] ) ) {
	unset( $crons[ $ts ] );
}
$crons[ time() - 1 ][ $hook ][ $sig ] = $event;
uksort( $crons, 'strnatcasecmp' );
_set_cron_array( $crons );
echo 'ok';
`

func validCronRun(in CronRunInput) error {
	if in.Hook == "" || len(in.Hook) > 200 || !utf8.ValidString(in.Hook) || strings.ContainsFunc(in.Hook, unicode.IsControl) {
		return fmt.Errorf("%w: hook", ErrInvalidInput)
	}
	if !sigRe.MatchString(in.Sig) {
		return fmt.Errorf("%w: sig", ErrInvalidInput)
	}
	if in.Time <= 0 {
		return fmt.Errorf("%w: time", ErrInvalidInput)
	}
	return nil
}

// RunCronEvent runs one scheduled event now: it is made due, then
// WordPress's cron runs in the background, as the every-minute loop runs
// it (with the site's plugins, in the web jail). Staging sites run no
// cron: running it would also run every other task due on the copy (a
// shop's renewals, its e-mails).
func (s *Service) RunCronEvent(ctx context.Context, id string, in CronRunInput) error {
	if err := validCronRun(in); err != nil {
		return err
	}
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return err
	}
	if err := activeSite(st); err != nil {
		return err
	}
	if st.ParentID != "" {
		return fmt.Errorf("%w: staging sites don't run scheduled tasks (a copy must not send e-mail or charge renewals)", ErrInvalidInput)
	}
	code := fmt.Sprintf(cronDuePHP, base64.StdEncoding.EncodeToString([]byte(in.Hook)), in.Time, in.Sig)
	if err := s.Runtime.Exec(ctx, id, nil, nil, runtime.WPArgs("eval", code)...); err != nil {
		if strings.Contains(err.Error(), noEventMarker) {
			return fmt.Errorf("%w: that task isn't scheduled any more (it may just have run); refresh the list", ErrInvalidInput)
		}
		return fmt.Errorf("scheduling the task: %w", err)
	}
	s.event(id, "tools", "Scheduled task "+truncate(in.Hook, 120)+" run from the panel")
	spec := runtime.SiteSpec{ID: st.ID, Dir: s.Cfg.SiteDir(st.ID), Docroot: s.Cfg.SiteRoot(st.ID), Domain: st.PrimaryDomain}
	go func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 6*time.Minute)
		defer cancel()
		out, err := s.Runtime.RunCron(c, spec)
		if err != nil && !errors.Is(err, runtime.ErrNoJail) {
			s.Log.Warn("running cron for a task run from the panel", "site", id, "err", err, "out", truncate(string(out), 500))
		}
	}()
	return nil
}

// ---- WordPress settings ----

// WPSettings are the WordPress settings the panel changes.
type WPSettings struct {
	Title   string `json:"title"`
	Tagline string `json:"tagline"`
	// Timezone is an IANA name (or "UTC"); "" when WordPress uses a fixed
	// UTC offset (UTCOffset, hours) instead.
	Timezone  string  `json:"timezone"`
	UTCOffset float64 `json:"utc_offset"`
	// DiscourageSearch asks search engines not to index the site
	// (blog_public 0).
	DiscourageSearch bool `json:"discourage_search"`
	// CommentsOpen: new posts accept comments (default_comment_status).
	CommentsOpen bool `json:"comments_open"`
}

// WPSettingsInput changes some of them (nil: unchanged).
type WPSettingsInput struct {
	Title            *string `json:"title"`
	Tagline          *string `json:"tagline"`
	Timezone         *string `json:"timezone"`
	DiscourageSearch *bool   `json:"discourage_search"`
	CommentsOpen     *bool   `json:"comments_open"`
}

// wpSettingOptions are the only options the panel reads or writes.
var wpSettingOptions = []string{"blogname", "blogdescription", "timezone_string", "gmt_offset", "blog_public", "default_comment_status"}

var timezoneRe = regexp.MustCompile(`^(UTC|[A-Z][A-Za-z_-]+(/[A-Za-z0-9_+-]+){1,2})$`)

func validSettingText(what, v string, maxRunes int) error {
	switch {
	case !utf8.ValidString(v):
		return fmt.Errorf("%w: the %s isn't valid text", ErrInvalidInput, what)
	case utf8.RuneCountInString(v) > maxRunes:
		return fmt.Errorf("%w: the %s is longer than %d characters", ErrInvalidInput, what, maxRunes)
	case strings.ContainsFunc(v, unicode.IsControl):
		return fmt.Errorf("%w: the %s has line breaks or control characters", ErrInvalidInput, what)
	}
	return nil
}

// wpSettingUpdates validates a change and returns the options to write
// (name, value), in a fixed order, skipping what is already so.
func wpSettingUpdates(cur *WPSettings, in WPSettingsInput) ([][2]string, error) {
	var out [][2]string
	if in.Title != nil {
		v := strings.TrimSpace(*in.Title)
		if err := validSettingText("site title", v, 200); err != nil {
			return nil, err
		}
		if v != cur.Title {
			out = append(out, [2]string{"blogname", v})
		}
	}
	if in.Tagline != nil {
		v := strings.TrimSpace(*in.Tagline)
		if err := validSettingText("tagline", v, 300); err != nil {
			return nil, err
		}
		if v != cur.Tagline {
			out = append(out, [2]string{"blogdescription", v})
		}
	}
	if in.Timezone != nil {
		v := strings.TrimSpace(*in.Timezone)
		if len(v) > 64 || !timezoneRe.MatchString(v) || strings.Contains(v, "..") {
			return nil, fmt.Errorf("%w: choose a time zone from the list", ErrInvalidInput)
		}
		if v != cur.Timezone {
			out = append(out, [2]string{"timezone_string", v})
		}
	}
	if in.DiscourageSearch != nil && *in.DiscourageSearch != cur.DiscourageSearch {
		out = append(out, [2]string{"blog_public", map[bool]string{true: "0", false: "1"}[*in.DiscourageSearch]})
	}
	if in.CommentsOpen != nil && *in.CommentsOpen != cur.CommentsOpen {
		out = append(out, [2]string{"default_comment_status", map[bool]string{true: "open", false: "closed"}[*in.CommentsOpen]})
	}
	return out, nil
}

// settingsFrom turns the options' JSON values (`wp option get --format=json`)
// into WPSettings. WordPress stores the title and tagline HTML-escaped.
func settingsFrom(opts map[string]json.RawMessage) *WPSettings {
	str := func(name string) string {
		raw := opts[name]
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		var n json.Number
		if json.Unmarshal(raw, &n) == nil {
			return n.String()
		}
		return ""
	}
	out := &WPSettings{
		Title:            html.UnescapeString(str("blogname")),
		Tagline:          html.UnescapeString(str("blogdescription")),
		Timezone:         str("timezone_string"),
		DiscourageSearch: str("blog_public") == "0",
		CommentsOpen:     str("default_comment_status") == "open",
	}
	if out.Timezone == "" {
		out.UTCOffset, _ = strconv.ParseFloat(str("gmt_offset"), 64)
	}
	return out
}

// WordPressSettings reads the settings, one `wp option get` each.
func (s *Service) WordPressSettings(ctx context.Context, id string) (*WPSettings, error) {
	if err := s.requireActive(ctx, id); err != nil {
		return nil, err
	}
	opts := map[string]json.RawMessage{}
	for _, name := range wpSettingOptions {
		var v json.RawMessage
		if err := s.wpJSON(ctx, id, &v, "option", "get", name, "--format=json"); err != nil {
			if strings.Contains(err.Error(), "Does it exist") {
				continue // never set: WordPress's default
			}
			return nil, fmt.Errorf("reading %s: %w", name, err)
		}
		opts[name] = v
	}
	return settingsFrom(opts), nil
}

// SetWordPressSettings changes the settings given; values go to
// `wp option update` on stdin, never as arguments.
func (s *Service) SetWordPressSettings(ctx context.Context, id string, in WPSettingsInput) (*WPSettings, error) {
	// Checked before anything runs in the site.
	if _, err := wpSettingUpdates(&WPSettings{}, in); err != nil {
		return nil, err
	}
	cur, err := s.WordPressSettings(ctx, id)
	if err != nil {
		return nil, err
	}
	updates, err := wpSettingUpdates(cur, in)
	if err != nil {
		return nil, err
	}
	if len(updates) == 0 {
		return cur, nil
	}
	var changed []string
	for _, u := range updates {
		if _, err := s.Runtime.WP(ctx, id, strings.NewReader(u[1]), "option", "update", u[0]); err != nil {
			return nil, fmt.Errorf("saving %s: %w", u[0], err)
		}
		changed = append(changed, settingLabel[u[0]])
	}
	after, err := s.WordPressSettings(ctx, id)
	if err != nil {
		return nil, err
	}
	// WordPress refuses time zones it doesn't know, silently.
	if in.Timezone != nil && after.Timezone != strings.TrimSpace(*in.Timezone) {
		return nil, fmt.Errorf("%w: WordPress doesn't know the time zone %q", ErrInvalidInput, *in.Timezone)
	}
	// The title and the search engine setting are on every page.
	if err := s.Purge(ctx, id); err != nil {
		s.Log.Warn("purging the cache after changing WordPress settings", "site", id, "err", err)
	}
	s.event(id, "tools", "WordPress settings changed: "+strings.Join(changed, ", "))
	return after, nil
}

var settingLabel = map[string]string{
	"blogname": "site title", "blogdescription": "tagline", "timezone_string": "time zone",
	"blog_public": "search engine visibility", "default_comment_status": "comments on new posts",
}
