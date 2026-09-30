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

// WordPress's own administrators, from the panel: list them, sign in to
// wp-admin as one without their password (see internal/wplogin), and reset
// a forgotten password. Everything runs through WP-CLI with plugins and
// themes skipped, so a compromised plugin can't observe or steer it.

// WPUser is a WordPress administrator.
type WPUser struct {
	ID    int    `json:"id"`
	Login string `json:"login"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

// flexInt decodes a JSON number or numeric string (WP-CLI prints IDs
// either way depending on the command and version).
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	n, err := strconv.Atoi(strings.Trim(string(b), `"`))
	*f = flexInt(n)
	return err
}

// Administrators lists the site's WordPress administrators, oldest first.
func (s *Service) Administrators(ctx context.Context, id string) ([]WPUser, error) {
	if err := s.requireActive(ctx, id); err != nil {
		return nil, err
	}
	var rows []struct {
		ID          flexInt `json:"ID"`
		Login       string  `json:"user_login"`
		Email       string  `json:"user_email"`
		DisplayName string  `json:"display_name"`
	}
	if err := s.wpJSON(ctx, id, &rows, "user", "list", "--role=administrator",
		"--fields=ID,user_login,user_email,display_name", "--orderby=ID", "--order=ASC", "--format=json"); err != nil {
		return nil, err
	}
	out := make([]WPUser, 0, len(rows))
	for _, r := range rows {
		out = append(out, WPUser{ID: int(r.ID), Login: r.Login, Email: r.Email, Name: r.DisplayName})
	}
	return out, nil
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

// PasswordInput resets an administrator's password. An empty password
// makes a random one.
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

// ResetAdminPassword sets a new password for an administrator and ends
// all their sessions (a reset is what you do when an account may be
// compromised). WordPress sends no e-mail about it.
func (s *Service) ResetAdminPassword(ctx context.Context, id string, in PasswordInput) (*PasswordReset, error) {
	pw := in.Password
	if pw == "" {
		pw = randString(24, passAlphabet)
	} else if err := validWPPassword(pw); err != nil {
		return nil, err
	}
	admins, err := s.Administrators(ctx, id)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(admins, func(u WPUser) bool { return u.ID == in.UserID })
	if i < 0 {
		return nil, fmt.Errorf("%w: no administrator with ID %d on this site", ErrInvalidInput, in.UserID)
	}
	u := admins[i]
	uid := strconv.Itoa(u.ID)
	// The password goes on stdin, never argv (visible in the process list).
	if _, err := s.Runtime.WP(ctx, id, strings.NewReader(pw+"\n"), "user", "update", uid, "--prompt=user_pass", "--skip-email"); err != nil {
		return nil, err
	}
	if _, err := s.Runtime.WP(ctx, id, nil, "user", "session", "destroy", uid, "--all"); err != nil {
		s.Log.Warn("ending a WordPress user's sessions after a password reset", "site", id, "user", u.Login, "err", err)
	}
	s.event(id, "wp-admin", fmt.Sprintf("Password of WordPress administrator %s reset from the panel; their sessions ended", u.Login))
	return &PasswordReset{UserID: u.ID, User: u.Login, Password: pw}, nil
}
