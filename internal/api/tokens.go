package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/parthh37/wpgenie/internal/auth"
	"github.com/parthh37/wpgenie/internal/store"
)

// Per-user API tokens: named credentials for scripts and billing systems
// (the WHMCS module, reseller automation). A token acts as its user, with
// the user's current role and account scope, never more: demoting,
// disabling or terminating the user's account takes effect on the next
// request. Only the SHA-256 of a token is stored; it is shown once.
//
// Tokens are made from a signed-in session (or by an administrator), never
// with another token: a leaked token can't mint itself successors that
// outlive its revocation. A token needs no code per request (scripts can't
// type them), but while the panel requires two-factor authentication only
// users who have it can make or use one: otherwise a password alone would
// buy a credential past the requirement. Resetting a user's password or
// two-factor authentication (an administrator's answer to a compromised
// account) revokes their tokens.

const (
	apiTokenPrefix   = "wpg_"
	maxTokensPerUser = 20
	maxTokenDays     = 3650
)

// tokenPrincipal authenticates a user's API token.
func (s *Server) tokenPrincipal(r *http.Request, tok string) (*Principal, error) {
	ctx := r.Context()
	if len(tok) > 100 {
		return nil, errUnauthorized
	}
	t, err := s.Store.APITokenByHash(ctx, auth.HashToken(tok))
	if err != nil {
		return nil, errUnauthorized
	}
	now := s.now()
	if !t.ExpiresAt.IsZero() && !now.Before(t.ExpiresAt) {
		return nil, errUnauthorized
	}
	u, err := s.Store.GetUser(ctx, t.UserID)
	if err != nil || u.Disabled {
		return nil, errUnauthorized
	}
	p, err := s.principalFor(ctx, u)
	if err != nil {
		return nil, err
	}
	p.TokenID = t.ID
	if now.Sub(t.LastUsedAt) > time.Minute { // don't write on every request
		s.Store.TouchAPIToken(context.WithoutCancel(ctx), t.ID, now, clientIP(r))
	}
	return p, nil
}

func validTokenName(n string) error {
	if n == "" || utf8.RuneCountInString(n) > 64 || strings.IndexFunc(n, unicode.IsControl) >= 0 {
		return fmt.Errorf("%w: name must be 1-64 characters on one line", errBadRequest)
	}
	return nil
}

// newToken creates a token for a user and returns it with the secret,
// which is never shown again.
func (s *Server) newToken(w http.ResponseWriter, r *http.Request, u *store.User) error {
	p := principalFrom(r.Context())
	if p.SessionID == "" && p.UserID != 0 {
		return fmt.Errorf("%w: API tokens are created from a signed-in session, not with another token", errForbidden)
	}
	// On a panel requiring two-factor authentication, a token of a user
	// without it would be refused anyway (see route).
	if !u.TOTPEnabled && s.require2FA(r.Context()) {
		return fmt.Errorf("%w: this panel requires two-factor authentication: %s must enable it first", errForbidden, u.Username)
	}
	var in struct {
		Name        string `json:"name"`
		ExpiresDays int    `json:"expires_days"` // 0: never
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	in.Name = strings.TrimSpace(in.Name)
	if err := validTokenName(in.Name); err != nil {
		return err
	}
	if in.ExpiresDays < 0 || in.ExpiresDays > maxTokenDays {
		return fmt.Errorf("%w: expires_days must be 0 (never) to %d", errBadRequest, maxTokenDays)
	}
	have, err := s.Store.APITokens(r.Context(), u.ID)
	if err != nil {
		return err
	}
	if len(have) >= maxTokensPerUser {
		return fmt.Errorf("%w: %d tokens at most; revoke one first", errConflict, maxTokensPerUser)
	}
	secret := apiTokenPrefix + auth.RandomToken(32)
	t := &store.APIToken{UserID: u.ID, Name: in.Name, Hint: secret[:len(apiTokenPrefix)+6] + "…"}
	if in.ExpiresDays > 0 {
		t.ExpiresAt = s.now().Add(time.Duration(in.ExpiresDays) * 24 * time.Hour)
	}
	t, err = s.Store.CreateAPIToken(r.Context(), t, auth.HashToken(secret))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, map[string]any{"token": secret, "api_token": t})
}

func (s *Server) myTokens(w http.ResponseWriter, r *http.Request) error {
	u, err := s.me(r)
	if err != nil {
		return err
	}
	list, err := s.Store.APITokens(r.Context(), u.ID)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, list)
}

func (s *Server) createMyToken(w http.ResponseWriter, r *http.Request) error {
	u, err := s.me(r)
	if err != nil {
		return err
	}
	return s.newToken(w, r, u)
}

func tokenID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return 0, errBadRequest
	}
	return id, nil
}

func (s *Server) deleteMyToken(w http.ResponseWriter, r *http.Request) error {
	u, err := s.me(r)
	if err != nil {
		return err
	}
	id, err := tokenID(r)
	if err != nil {
		return err
	}
	if err := s.Store.DeleteAPIToken(r.Context(), id, u.ID); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// allTokens lists every user's tokens (administrators).
func (s *Server) allTokens(w http.ResponseWriter, r *http.Request) error {
	list, err := s.Store.APITokens(r.Context(), 0)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, list)
}

func (s *Server) deleteAnyToken(w http.ResponseWriter, r *http.Request) error {
	id, err := tokenID(r)
	if err != nil {
		return err
	}
	if err := s.Store.DeleteAPIToken(r.Context(), id, 0); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// createUserToken makes a token for any user (administrators; the CLI
// uses it to set up a billing system's token).
func (s *Server) createUserToken(w http.ResponseWriter, r *http.Request) error {
	u, err := s.userParam(r)
	if err != nil {
		return err
	}
	if u.Disabled {
		return fmt.Errorf("%w: the user is disabled", errBadRequest)
	}
	return s.newToken(w, r, u)
}
