// Package auth holds the panel's account primitives: password hashing,
// TOTP two-factor codes (RFC 6238), recovery codes, session tokens and
// roles. Storage and HTTP handling live in the store and api packages.
package auth

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Roles, from least to most privileged.
const (
	RoleViewer   = "viewer"   // read-only
	RoleOperator = "operator" // runs sites: shield, scaling, caches, updates, scans, mailboxes
	RoleAdmin    = "admin"    // everything: users, site creation/deletion, server settings, self-update
)

// Level orders roles; unknown roles have none.
func Level(role string) int {
	switch role {
	case RoleViewer:
		return 1
	case RoleOperator:
		return 2
	case RoleAdmin:
		return 3
	}
	return 0
}

func ValidRole(role string) bool { return Level(role) > 0 }

// Tenant roles belong to users of a customer or reseller account (see
// internal/billing). They have no staff level at all, so every route
// that only checks Level refuses them: a tenant reaches only what the API
// explicitly opens to tenants, for the sites and accounts they own.
const (
	RoleCustomer = "customer" // users of a customer account: its sites
	RoleReseller = "reseller" // users of a reseller account: its sites, customer accounts and their sites
)

// IsTenant reports whether role is a tenant role.
func IsTenant(role string) bool { return role == RoleCustomer || role == RoleReseller }

// TenantRole is the role of the users of an account of kind ("customer" or
// "reseller"), or "" for an unknown kind. A tenant user's role always
// follows their account, never the users table.
func TenantRole(kind string) string {
	switch kind {
	case "customer":
		return RoleCustomer
	case "reseller":
		return RoleReseller
	}
	return ""
}

// Password hashing: PBKDF2-HMAC-SHA256 from the standard library, at
// OWASP's 2023 iteration count. Stored as
// "pbkdf2-sha256$<iterations>$<salt>$<hash>" (base64, unpadded), so the
// cost can be raised later without invalidating existing hashes.
const (
	pbkdf2Iterations = 600_000
	saltLen          = 16
	keyLen           = 32
	MinPasswordLen   = 12
	MaxPasswordLen   = 256 // bounds the hashing work an attacker can ask for
)

var b64 = base64.RawStdEncoding

// ValidatePassword enforces the password policy: length only, as current
// NIST guidance recommends (composition rules make passwords worse).
func ValidatePassword(pw string) error {
	n := utf8.RuneCountInString(pw)
	if n < MinPasswordLen {
		return fmt.Errorf("password must be at least %d characters", MinPasswordLen)
	}
	if len(pw) > MaxPasswordLen {
		return fmt.Errorf("password must be at most %d bytes", MaxPasswordLen)
	}
	return nil
}

func HashPassword(pw string) (string, error) {
	salt := make([]byte, saltLen)
	rand.Read(salt)
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iterations, keyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iterations, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// CheckPassword reports whether pw matches hash, in constant time.
func CheckPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" || len(pw) > MaxPasswordLen {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1000 || iter > 10_000_000 {
		return false
	}
	salt, err1 := b64.DecodeString(parts[2])
	want, err2 := b64.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash is checked against when a user name doesn't exist, so a login
// takes as long for an unknown user as for a wrong password. Computed on
// first use: every CLI command would otherwise pay for it at start.
var dummyHash = sync.OnceValue(func() string {
	h, _ := HashPassword("wpgenie timing equaliser")
	return h
})

// CheckNoUser burns the time of a password check.
func CheckNoUser(pw string) { CheckPassword(dummyHash(), pw) }

// RandomToken returns n random bytes, URL-safe base64 encoded.
func RandomToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashToken is how session tokens and recovery codes are stored: a
// database copy can't be used to sign in.
func HashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// GeneratePassword makes a random password (128 bits, base32) for
// accounts created or reset by an administrator; it is shown once.
func GeneratePassword() string { return rand.Text() }

// TOTP (RFC 6238): HMAC-SHA1, 30-second steps, 6 digits, which is what
// every authenticator app implements.
const (
	totpStep   = 30
	totpDigits = 6
	// totpSkew accepts the previous and next code too: phone clocks drift.
	totpSkew = 1
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a random 160-bit secret, base32 encoded.
func NewTOTPSecret() string {
	b := make([]byte, 20)
	rand.Read(b)
	return b32.EncodeToString(b)
}

// TOTPURI is the otpauth:// URI authenticator apps import (as a QR code).
func TOTPURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	q := url.Values{"secret": {secret}, "issuer": {issuer}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	return "otpauth://totp/" + label + "?" + q.Encode()
}

func totpCode(key []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, v%1_000_000)
}

// TOTPCode is the code for a time (for tests and enrolment checks).
func TOTPCode(secret string, t time.Time) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(secret))
	if err != nil {
		return "", err
	}
	return totpCode(key, t.Unix()/totpStep), nil
}

var ErrBadCode = errors.New("invalid code")

// CheckTOTP verifies a code at now, accepting only time steps after
// lastStep (a code works once, even within its validity window). It
// returns the step to record.
func CheckTOTP(secret, code string, now time.Time, lastStep int64) (int64, error) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != totpDigits {
		return 0, ErrBadCode
	}
	key, err := b32.DecodeString(strings.ToUpper(secret))
	if err != nil {
		return 0, err
	}
	cur := now.Unix() / totpStep
	for d := int64(-totpSkew); d <= totpSkew; d++ {
		step := cur + d
		if step <= lastStep {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(totpCode(key, step)), []byte(code)) == 1 {
			return step, nil
		}
	}
	return 0, ErrBadCode
}

// NewRecoveryCodes returns n one-time codes ("abcd-efgh-ijkl", 60 random
// bits each) and their hashes (what is stored).
func NewRecoveryCodes(n int) (codes, hashes []string) {
	for range n {
		b := strings.ToLower(rand.Text()[:12])
		c := b[:4] + "-" + b[4:8] + "-" + b[8:]
		codes = append(codes, c)
		hashes = append(hashes, HashRecoveryCode(c))
	}
	return codes, hashes
}

// HashRecoveryCode normalises what a user typed (case, dashes, spaces)
// before hashing.
func HashRecoveryCode(code string) string {
	c := strings.ToLower(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(code)))
	return HashToken("recovery:" + c)
}
