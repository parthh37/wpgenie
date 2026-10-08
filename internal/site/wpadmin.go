package site

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/parthh37/wpgenie/internal/runtime"
	"github.com/parthh37/wpgenie/internal/store"
)

// WordPress's own users, from the panel: list the administrators and
// editors, add and delete them, sign in to wp-admin as an administrator
// without their password (see internal/wplogin), and reset a forgotten
// password. Everything runs through WP-CLI with plugins and themes
// skipped, so a compromised plugin can't observe or steer it.
//
// The site's first user (the lowest ID: the account `wp core install`
// made, or the original owner of an imported site) is its owner: it can't
// be deleted from the panel, and deleted users' content moves to it.

// WPUser is a WordPress administrator or editor.
type WPUser struct {
	ID    int    `json:"id"`
	Login string `json:"login"`
	Email string `json:"email"`
	Name  string `json:"name"`
	Role  string `json:"role"`            // RoleAdministrator or RoleEditor
	Owner bool   `json:"owner,omitempty"` // the site's first user
}

// The roles the panel manages.
const (
	RoleAdministrator = "administrator"
	RoleEditor        = "editor"
)

// flexInt decodes a JSON number or numeric string (WP-CLI prints IDs
// either way depending on the command and version).
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	n, err := strconv.Atoi(strings.Trim(string(b), `"`))
	*f = flexInt(n)
	return err
}

// listUsersPHP prints the administrators and editors, oldest first, and
// the ID of the site's first user whatever its role. Someone with both
// roles counts as an administrator.
const listUsersPHP = `
$first = get_users( array( 'orderby' => 'ID', 'order' => 'ASC', 'number' => 1, 'fields' => 'ID' ) );
$users = array();
foreach ( get_users( array( 'role__in' => array( 'administrator', 'editor' ), 'orderby' => 'ID', 'order' => 'ASC' ) ) as $u ) {
	$users[] = array(
		'id' => $u->ID, 'login' => $u->user_login, 'email' => $u->user_email, 'name' => $u->display_name,
		'role' => in_array( 'administrator', (array) $u->roles, true ) ? 'administrator' : 'editor',
	);
}
echo wp_json_encode( array( 'first' => $first ? (int) $first[0] : 0, 'users' => $users ) );
`

// Users lists the site's WordPress administrators and editors, oldest
// first, the site's owner marked.
func (s *Service) Users(ctx context.Context, id string) ([]WPUser, error) {
	users, _, err := s.wpUsers(ctx, id)
	return users, err
}

// wpUsers is Users and the ID of the site's first user (0: no users),
// who may be neither an administrator nor an editor.
func (s *Service) wpUsers(ctx context.Context, id string) ([]WPUser, int, error) {
	if err := s.requireActive(ctx, id); err != nil {
		return nil, 0, err
	}
	var r struct {
		First flexInt `json:"first"`
		Users []struct {
			ID    flexInt `json:"id"`
			Login string  `json:"login"`
			Email string  `json:"email"`
			Name  string  `json:"name"`
			Role  string  `json:"role"`
		} `json:"users"`
	}
	if err := s.wpJSON(ctx, id, &r, "eval", listUsersPHP); err != nil {
		return nil, 0, err
	}
	out := make([]WPUser, 0, len(r.Users))
	for _, u := range r.Users {
		role := RoleEditor
		if u.Role == RoleAdministrator {
			role = RoleAdministrator
		}
		out = append(out, WPUser{ID: int(u.ID), Login: u.Login, Email: u.Email, Name: u.Name,
			Role: role, Owner: int(u.ID) == int(r.First)})
	}
	return out, int(r.First), nil
}

func (s *Service) requireActive(ctx context.Context, id string) error {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return err
	}
	if st.Status != store.StatusActive {
		return fmt.Errorf("%w: site is %s", ErrInvalidInput, st.Status)
	}
	return nil
}

// AuthCookie is one of WordPress's authentication cookies.
type AuthCookie struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Path  string `json:"path"`
}

// AdminSession is a WordPress session made for someone signing in from
// the panel: the cookies wp_set_auth_cookie() would have set, and where.
type AdminSession struct {
	UserID int    `json:"user_id"`
	User   string `json:"user"`
	// Host is the domain WordPress lives on (its siteurl), which the
	// cookies belong to; AdminPath is wp-admin's path there.
	Host      string       `json:"host"`
	AdminPath string       `json:"admin_path"`
	Domain    string       `json:"domain,omitempty"` // COOKIE_DOMAIN, if the site sets one
	Cookies   []AuthCookie `json:"cookies"`
	Expires   time.Time    `json:"expires"`
}

const noAdminMarker = "wpgenie: no such administrator"

// adminLoginPHP mints a session the way wp_signon() does, minus the
// password: a session token (listed in the user's sessions, ended by
// "log out everywhere" and password changes), then the secure_auth and
// logged_in cookies for it, on the paths wp_set_auth_cookie() uses. Not
// "remember me": WordPress's two days. The client address and browser are
// the requester's, so the session list shows who it is.
const adminLoginPHP = `
$uid = %d;
if ( $uid <= 0 ) {
	$ids = get_users( array( 'role' => 'administrator', 'orderby' => 'ID', 'order' => 'ASC', 'number' => 1, 'fields' => 'ID' ) );
	$uid = $ids ? (int) $ids[0] : 0;
}
$user = $uid > 0 ? get_userdata( $uid ) : false;
if ( ! $user || ! in_array( 'administrator', (array) $user->roles, true ) ) {
	fwrite( STDERR, "` + noAdminMarker + `\n" );
	exit( 3 );
}
$_SERVER['REMOTE_ADDR']     = base64_decode( '%s' );
$_SERVER['HTTP_USER_AGENT'] = base64_decode( '%s' );
$expiration = time() + (int) apply_filters( 'auth_cookie_expiration', 2 * DAY_IN_SECONDS, $user->ID, false );
$token  = WP_Session_Tokens::get_instance( $user->ID )->create( $expiration );
$secure = wp_generate_auth_cookie( $user->ID, $expiration, 'secure_auth', $token );
$logged = wp_generate_auth_cookie( $user->ID, $expiration, 'logged_in', $token );
$cookies = array(
	array( 'name' => SECURE_AUTH_COOKIE, 'value' => $secure, 'path' => PLUGINS_COOKIE_PATH ),
	array( 'name' => SECURE_AUTH_COOKIE, 'value' => $secure, 'path' => ADMIN_COOKIE_PATH ),
	array( 'name' => LOGGED_IN_COOKIE, 'value' => $logged, 'path' => COOKIEPATH ),
);
if ( COOKIEPATH !== SITECOOKIEPATH ) {
	$cookies[] = array( 'name' => LOGGED_IN_COOKIE, 'value' => $logged, 'path' => SITECOOKIEPATH );
}
echo wp_json_encode( array(
	'user_id' => $user->ID, 'user' => $user->user_login, 'expires' => $expiration,
	'site_url' => site_url( '/' ), 'admin_url' => admin_url( '/' ), 'domain' => (string) COOKIE_DOMAIN,
	'cookies' => $cookies,
) );
`

// AdminLogin makes a WordPress session for an administrator (userID 0:
// the oldest one) on behalf of the client at ip with browser ua.
func (s *Service) AdminLogin(ctx context.Context, id string, userID int, ip, ua string) (*AdminSession, error) {
	st, err := s.Store.GetSite(ctx, id)
	if err != nil {
		return nil, err
	}
	if st.Status != store.StatusActive {
		return nil, fmt.Errorf("%w: site is %s", ErrInvalidInput, st.Status)
	}
	if userID < 0 {
		return nil, fmt.Errorf("%w: user ID", ErrInvalidInput)
	}
	if net.ParseIP(ip) == nil {
		ip = ""
	}
	ua = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, ua)
	if len(ua) > 400 {
		ua = ua[:400]
	}
	enc := base64.StdEncoding.EncodeToString
	code := fmt.Sprintf(adminLoginPHP, userID, enc([]byte(ip)), enc([]byte(ua)))
	var out bytes.Buffer
	if err := s.Runtime.Exec(ctx, id, nil, &out, runtime.WPArgs("eval", code)...); err != nil {
		if strings.Contains(err.Error(), noAdminMarker) {
			return nil, fmt.Errorf("%w: no such administrator on this site", ErrInvalidInput)
		}
		return nil, fmt.Errorf("making a WordPress session: %w", err)
	}
	var r struct {
		UserID   flexInt      `json:"user_id"`
		User     string       `json:"user"`
		Expires  int64        `json:"expires"`
		SiteURL  string       `json:"site_url"`
		AdminURL string       `json:"admin_url"`
		Domain   string       `json:"domain"`
		Cookies  []AuthCookie `json:"cookies"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &r); err != nil {
		return nil, fmt.Errorf("making a WordPress session: unexpected output: %w", err)
	}
	sess := &AdminSession{UserID: int(r.UserID), User: r.User, Domain: r.Domain, Cookies: r.Cookies,
		Expires: time.Unix(r.Expires, 0).UTC(), Host: st.PrimaryDomain, AdminPath: "/wp-admin/"}
	// The cookies belong to WordPress's own host: that's where the sign-in
	// link must go, as long as it is one of the site's domains.
	if u, err := url.Parse(r.SiteURL); err == nil {
		if h := strings.ToLower(u.Hostname()); slices.Contains(st.Domains, h) {
			sess.Host = h
		}
	}
	if u, err := url.Parse(r.AdminURL); err == nil && strings.HasPrefix(u.Path, "/") && !strings.HasPrefix(u.Path, "//") {
		sess.AdminPath = u.Path
	}
	if len(sess.Cookies) == 0 {
		return nil, fmt.Errorf("making a WordPress session: no cookies")
	}
	for _, c := range sess.Cookies {
		if !validCookie(c) {
			return nil, fmt.Errorf("making a WordPress session: unexpected cookie %q", c.Name)
		}
	}
	return sess, nil
}

// validCookie: what WordPress printed goes into Set-Cookie headers, so
// it must be a plain cookie (site code could have redefined the cookie
// constants in wp-config.php's editable part).
func validCookie(c AuthCookie) bool {
	ok := func(v string, extra string) bool {
		return v != "" && !strings.ContainsFunc(v, func(r rune) bool {
			return r <= ' ' || r >= 0x7f || r == ';' || r == ',' || r == '"' || r == '\\' || strings.ContainsRune(extra, r)
		})
	}
	return ok(c.Name, "=()<>@:/[]?{}") && ok(c.Value, "") && strings.HasPrefix(c.Path, "/") && ok(c.Path, "")
}

// PasswordInput resets an administrator's or editor's password. An empty
// password makes a random one.
type PasswordInput struct {
	UserID   int    `json:"user_id"`
	Password string `json:"password"`
}

// PasswordReset is the result: the new password is shown once.
type PasswordReset struct {
	UserID   int    `json:"user_id"`
	User     string `json:"user"`
	Password string `json:"password"`
}

func validWPPassword(pw string) error {
	if len(pw) < 12 || len(pw) > 200 {
		return fmt.Errorf("%w: the password must be 12 to 200 characters", ErrInvalidInput)
	}
	if strings.ContainsFunc(pw, unicode.IsControl) {
		return fmt.Errorf("%w: the password has control characters", ErrInvalidInput)
	}
	return nil
}

// ResetAdminPassword sets a new password for an administrator or editor
// and ends all their sessions (a reset is what you do when an account may be
// compromised). WordPress sends no e-mail about it.
func (s *Service) ResetAdminPassword(ctx context.Context, id string, in PasswordInput) (*PasswordReset, error) {
	pw := in.Password
	if pw == "" {
		pw = randString(24, passAlphabet)
	} else if err := validWPPassword(pw); err != nil {
		return nil, err
	}
	users, err := s.Users(ctx, id)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(users, func(u WPUser) bool { return u.ID == in.UserID })
	if i < 0 {
		return nil, fmt.Errorf("%w: no administrator or editor with ID %d on this site", ErrInvalidInput, in.UserID)
	}
	u := users[i]
	uid := strconv.Itoa(u.ID)
	// The password goes on stdin, never argv (visible in the process list).
	if _, err := s.Runtime.WP(ctx, id, strings.NewReader(pw+"\n"), "user", "update", uid, "--prompt=user_pass", "--skip-email"); err != nil {
		return nil, err
	}
	if _, err := s.Runtime.WP(ctx, id, nil, "user", "session", "destroy", uid, "--all"); err != nil {
		s.Log.Warn("ending a WordPress user's sessions after a password reset", "site", id, "user", u.Login, "err", err)
	}
	s.event(id, "wp-admin", fmt.Sprintf("Password of WordPress %s %s reset from the panel; their sessions ended", u.Role, u.Login))
	return &PasswordReset{UserID: u.ID, User: u.Login, Password: pw}, nil
}

// NewUserInput adds a WordPress user. An empty password makes a random
// one; an empty name leaves WordPress's default (the username).
type NewUserInput struct {
	Login    string `json:"login"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	Role     string `json:"role"` // RoleAdministrator or RoleEditor
	Password string `json:"password"`
}

// NewUser is the user made and their password, shown once.
type NewUser struct {
	User     WPUser `json:"user"`
	Password string `json:"password"`
}

// validNewUser checks a new user's fields. Login and e-mail go to WP-CLI
// as positional arguments, so neither may start with "-" (it would be
// read as an option: a login of "--role=administrator").
func validNewUser(in *NewUserInput) error {
	in.Login = strings.TrimSpace(in.Login)
	in.Email = strings.TrimSpace(in.Email)
	in.Name = strings.TrimSpace(in.Name)
	if !userRe.MatchString(in.Login) || strings.HasPrefix(in.Login, "-") {
		return fmt.Errorf("%w: the username must be 3 to 60 letters, digits, dots, dashes or underscores, not starting with a dash", ErrInvalidInput)
	}
	if len(in.Email) > 100 || !emailRe.MatchString(in.Email) || strings.HasPrefix(in.Email, "-") {
		return fmt.Errorf("%w: e-mail address", ErrInvalidInput)
	}
	if len(in.Name) > 250 || strings.ContainsFunc(in.Name, unicode.IsControl) {
		return fmt.Errorf("%w: the name must be at most 250 characters, without control characters", ErrInvalidInput)
	}
	if in.Role != RoleAdministrator && in.Role != RoleEditor {
		return fmt.Errorf("%w: the role must be %s or %s", ErrInvalidInput, RoleAdministrator, RoleEditor)
	}
	if in.Password != "" {
		return validWPPassword(in.Password)
	}
	return nil
}

// CreateUser adds a WordPress administrator or editor. WordPress sends no
// e-mail about it: the panel shows the password once.
func (s *Service) CreateUser(ctx context.Context, id string, in NewUserInput) (*NewUser, error) {
	if err := validNewUser(&in); err != nil {
		return nil, err
	}
	if err := s.requireActive(ctx, id); err != nil {
		return nil, err
	}
	pw := in.Password
	if pw == "" {
		pw = randString(24, passAlphabet)
	}
	args := []string{"user", "create", in.Login, in.Email, "--role=" + in.Role}
	if in.Name != "" {
		args = append(args, "--display_name="+in.Name)
	}
	// The password goes on stdin, never argv (visible in the process list).
	// WP-CLI echoes it back to stdout, so the output isn't used: the new
	// user is looked up afterwards.
	if _, err := s.Runtime.WP(ctx, id, strings.NewReader(pw+"\n"), append(args, "--prompt=user_pass")...); err != nil {
		if msg := err.Error(); strings.Contains(msg, "already exists") || strings.Contains(msg, "already used") ||
			strings.Contains(msg, "already registered") {
			return nil, fmt.Errorf("%w: a user with that username or e-mail address already exists on this site", ErrInvalidInput)
		}
		return nil, err
	}
	s.event(id, "wp-admin", fmt.Sprintf("WordPress %s %s added from the panel", in.Role, in.Login))
	res := &NewUser{User: WPUser{Login: in.Login, Email: in.Email, Name: in.Name, Role: in.Role}, Password: pw}
	if users, err := s.Users(ctx, id); err == nil {
		if i := slices.IndexFunc(users, func(u WPUser) bool { return strings.EqualFold(u.Login, in.Login) }); i >= 0 {
			res.User = users[i]
		}
	}
	// Made here: not an intruder for the next scan.
	if in.Role == RoleAdministrator && res.User.ID > 0 {
		s.noteAdmin(ctx, id, AdminAccount{ID: res.User.ID, Login: res.User.Login, Role: RoleAdministrator})
	}
	return res, nil
}

// DeleteUser deletes a WordPress administrator or editor; their posts and
// pages move to the site's first user. Neither that first user nor the
// last administrator can be deleted.
func (s *Service) DeleteUser(ctx context.Context, id string, userID int) (*WPUser, error) {
	users, first, err := s.wpUsers(ctx, id)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(users, func(u WPUser) bool { return u.ID == userID })
	if i < 0 {
		return nil, fmt.Errorf("%w: no administrator or editor with ID %d on this site", ErrInvalidInput, userID)
	}
	u := users[i]
	if u.ID == first {
		return nil, fmt.Errorf("%w: %s is the site's first user, which can't be deleted", ErrInvalidInput, u.Login)
	}
	admins := 0
	for _, o := range users {
		if o.Role == RoleAdministrator {
			admins++
		}
	}
	if u.Role == RoleAdministrator && admins <= 1 {
		return nil, fmt.Errorf("%w: %s is the site's only administrator", ErrInvalidInput, u.Login)
	}
	if _, err := s.Runtime.WP(ctx, id, nil, "user", "delete", strconv.Itoa(u.ID), "--reassign="+strconv.Itoa(first), "--yes"); err != nil {
		return nil, err
	}
	s.event(id, "wp-admin", fmt.Sprintf("WordPress %s %s deleted from the panel; their content moved to the site's first user", u.Role, u.Login))
	return &u, nil
}
