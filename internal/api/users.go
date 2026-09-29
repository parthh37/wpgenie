package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/parthh37/wpgenie/internal/auth"
	"github.com/parthh37/wpgenie/internal/store"
)

// ---- Your own account ----

// me is the signed-in user (the API token is not a user).
func (s *Server) me(r *http.Request) (*store.User, error) {
	p := principalFrom(r.Context())
	if p == nil || p.UserID == 0 {
		return nil, fmt.Errorf("%w: the API token has no account; sign in as a user", errBadRequest)
	}
	return s.Store.GetUser(r.Context(), p.UserID)
}

func (s *Server) account(w http.ResponseWriter, r *http.Request) error {
	u, err := s.me(r)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"user": u, "require_2fa": s.require2FA(r.Context())})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	u, err := s.me(r)
	if err != nil {
		return err
	}
	if !auth.CheckPassword(u.PasswordHash, in.Current) {
		return fmt.Errorf("%w: the current password is wrong", errBadRequest)
	}
	if err := auth.ValidatePassword(in.New); err != nil {
		return fmt.Errorf("%w: %v", errBadRequest, err)
	}
	hash, err := auth.HashPassword(in.New)
	if err != nil {
		return err
	}
	if err := s.Store.SetUserPassword(r.Context(), u.ID, hash); err != nil {
		return err
	}
	// Anyone who knew the old password is signed out, but not this browser.
	if err := s.Store.DeleteUserSessions(r.Context(), u.ID, principalFrom(r.Context()).SessionID); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// startTOTP begins enrolling an authenticator: a new secret is kept
// pending until a code from it is confirmed.
func (s *Server) startTOTP(w http.ResponseWriter, r *http.Request) error {
	u, err := s.me(r)
	if err != nil {
		return err
	}
	if u.TOTPEnabled {
		return fmt.Errorf("%w: two-factor authentication is already on; turn it off first to change authenticator", errConflict)
	}
	secret := auth.NewTOTPSecret()
	if err := s.Store.SetTOTPPending(r.Context(), u.ID, secret); err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]string{"secret": secret, "uri": auth.TOTPURI(totpIssuer, u.Username, secret)})
}

// totpQR renders the pending secret as a QR code image, fetched with an
// <img> so the panel's CSP never has to allow inline markup.
func (s *Server) totpQR(w http.ResponseWriter, r *http.Request) error {
	u, err := s.me(r)
	if err != nil {
		return err
	}
	if u.TOTPPending == "" {
		return store.ErrNotFound
	}
	svg, err := auth.QRSVG(auth.TOTPURI(totpIssuer, u.Username, u.TOTPPending))
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "no-store")
	_, err = w.Write([]byte(svg))
	return err
}

// confirmTOTP switches two-factor authentication on once the user proves
// their authenticator works, and returns recovery codes (shown once).
func (s *Server) confirmTOTP(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Code string `json:"code"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	u, err := s.me(r)
	if err != nil {
		return err
	}
	if u.TOTPPending == "" {
		return fmt.Errorf("%w: start enrolment first", errBadRequest)
	}
	step, err := auth.CheckTOTP(u.TOTPPending, in.Code, s.now(), 0)
	if err != nil {
		return fmt.Errorf("%w: that code doesn't match; check the time on your phone", errBadRequest)
	}
	codes, hashes := auth.NewRecoveryCodes(10)
	if err := s.Store.EnableTOTP(r.Context(), u.ID, u.TOTPPending, step, hashes); err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

func (s *Server) disableTOTP(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Password string `json:"password"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	u, err := s.me(r)
	if err != nil {
		return err
	}
	if !auth.CheckPassword(u.PasswordHash, in.Password) {
		return fmt.Errorf("%w: wrong password", errBadRequest)
	}
	if s.require2FA(r.Context()) {
		return fmt.Errorf("%w: this panel requires two-factor authentication", errForbidden)
	}
	if err := s.Store.DisableTOTP(r.Context(), u.ID); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) newRecoveryCodes(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Password string `json:"password"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	u, err := s.me(r)
	if err != nil {
		return err
	}
	if !auth.CheckPassword(u.PasswordHash, in.Password) {
		return fmt.Errorf("%w: wrong password", errBadRequest)
	}
	if !u.TOTPEnabled {
		return fmt.Errorf("%w: two-factor authentication is off", errBadRequest)
	}
	codes, hashes := auth.NewRecoveryCodes(10)
	if err := s.Store.EnableTOTP(r.Context(), u.ID, u.TOTPSecret, u.TOTPLastStep, hashes); err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

type sessionView struct {
	*store.Session
	Current bool `json:"current"`
}

func (s *Server) listSessions(userID int64, w http.ResponseWriter, r *http.Request) error {
	list, err := s.Store.Sessions(r.Context(), userID)
	if err != nil {
		return err
	}
	cur := principalFrom(r.Context()).SessionID
	out := make([]sessionView, 0, len(list))
	for _, sess := range list {
		out = append(out, sessionView{sess, sess.ID == cur})
	}
	return writeJSON(w, http.StatusOK, out)
}

func (s *Server) mySessions(w http.ResponseWriter, r *http.Request) error {
	u, err := s.me(r)
	if err != nil {
		return err
	}
	return s.listSessions(u.ID, w, r)
}

func (s *Server) deleteMySession(w http.ResponseWriter, r *http.Request) error {
	u, err := s.me(r)
	if err != nil {
		return err
	}
	if err := s.Store.DeleteSession(r.Context(), r.PathValue("id"), u.ID); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// ---- Administration ----

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) error {
	users, err := s.Store.ListUsers(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, users)
}

// userParam finds the user in {id}: a numeric ID or a username (the CLI
// uses names; usernames are never all digits).
func (s *Server) userParam(r *http.Request) (*store.User, error) {
	v := r.PathValue("id")
	if id, err := strconv.ParseInt(v, 10, 64); err == nil {
		return s.Store.GetUser(r.Context(), id)
	}
	return s.Store.UserByName(r.Context(), v)
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Username string `json:"username"`
		Role     string `json:"role"`
		Password string `json:"password"` // empty: generated and returned once
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if err := validUsername(in.Username); err != nil {
		return err
	}
	if !auth.ValidRole(in.Role) {
		return fmt.Errorf("%w: role must be admin, operator or viewer", errBadRequest)
	}
	generated := in.Password == ""
	if generated {
		in.Password = auth.GeneratePassword()
	}
	if err := auth.ValidatePassword(in.Password); err != nil {
		return fmt.Errorf("%w: %v", errBadRequest, err)
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return err
	}
	u, err := s.Store.CreateUser(r.Context(), in.Username, hash, in.Role)
	if errors.Is(err, store.ErrExists) {
		return fmt.Errorf("%w: user %s already exists", errConflict, in.Username)
	}
	if err != nil {
		return err
	}
	out := map[string]any{"user": u}
	if generated {
		out["password"] = in.Password
	}
	return writeJSON(w, http.StatusCreated, out)
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Role     *string `json:"role"`
		Disabled *bool   `json:"disabled"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	u, err := s.userParam(r)
	if err != nil {
		return err
	}
	role, disabled := u.Role, u.Disabled
	if in.Role != nil {
		if !auth.ValidRole(*in.Role) {
			return fmt.Errorf("%w: role must be admin, operator or viewer", errBadRequest)
		}
		role = *in.Role
	}
	if in.Disabled != nil {
		disabled = *in.Disabled
	}
	if (role != auth.RoleAdmin || disabled) && u.Role == auth.RoleAdmin && !u.Disabled {
		if n, err := s.Store.CountActiveAdmins(r.Context(), u.ID); err != nil {
			return err
		} else if n == 0 {
			return fmt.Errorf("%w", errLastAdmin)
		}
	}
	if err := s.Store.SetUserRole(r.Context(), u.ID, role, disabled); err != nil {
		return err
	}
	if disabled {
		s.Store.DeleteUserSessions(r.Context(), u.ID, "")
	}
	u, err = s.Store.GetUser(r.Context(), u.ID)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, u)
}

// resetPassword sets a new generated password (shown once) and signs the
// user out everywhere.
func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request) error {
	u, err := s.userParam(r)
	if err != nil {
		return err
	}
	pw := auth.GeneratePassword()
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	if err := s.Store.SetUserPassword(r.Context(), u.ID, hash); err != nil {
		return err
	}
	if err := s.Store.DeleteUserSessions(r.Context(), u.ID, ""); err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]string{"password": pw})
}

// resetTOTP switches a user's two-factor authentication off, for someone
// who lost their phone and recovery codes.
func (s *Server) resetTOTP(w http.ResponseWriter, r *http.Request) error {
	u, err := s.userParam(r)
	if err != nil {
		return err
	}
	if err := s.Store.DisableTOTP(r.Context(), u.ID); err != nil {
		return err
	}
	if err := s.Store.DeleteUserSessions(r.Context(), u.ID, ""); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) error {
	u, err := s.userParam(r)
	if err != nil {
		return err
	}
	if p := principalFrom(r.Context()); p.UserID == u.ID {
		return fmt.Errorf("%w: you can't delete your own account", errBadRequest)
	}
	if u.Role == auth.RoleAdmin && !u.Disabled {
		if n, err := s.Store.CountActiveAdmins(r.Context(), u.ID); err != nil {
			return err
		} else if n == 0 {
			return errLastAdmin
		}
	}
	if err := s.Store.DeleteUser(r.Context(), u.ID); err != nil { // sessions cascade
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) allSessions(w http.ResponseWriter, r *http.Request) error {
	return s.listSessions(0, w, r)
}

func (s *Server) deleteAnySession(w http.ResponseWriter, r *http.Request) error {
	if err := s.Store.DeleteSession(r.Context(), r.PathValue("id"), 0); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) auditLog(w http.ResponseWriter, r *http.Request) error {
	limit, err := limitParam(r, 100, 1000)
	if err != nil {
		return err
	}
	entries, err := s.Store.Audit(r.Context(), r.URL.Query().Get("actor"), limit)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, entries)
}

func (s *Server) authSettings(w http.ResponseWriter, r *http.Request) error {
	return writeJSON(w, http.StatusOK, map[string]bool{"require_2fa": s.require2FA(r.Context())})
}

// setAuthSettings: requiring 2FA takes effect at once for everyone's next
// request (accounts without it can only reach Account until they enrol).
func (s *Server) setAuthSettings(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Require2FA bool `json:"require_2fa"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	v := "0"
	if in.Require2FA {
		v = "1"
	}
	if err := s.Store.SetSetting(r.Context(), settingRequire2FA, v); err != nil {
		return err
	}
	return s.authSettings(w, r)
}
