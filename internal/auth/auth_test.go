package auth

import (
	"strings"
	"testing"
	"time"
)

func TestPasswordHashing(t *testing.T) {
	h, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "pbkdf2-sha256$600000$") {
		t.Errorf("hash format %q", h)
	}
	if !CheckPassword(h, "correct horse battery staple") {
		t.Error("right password rejected")
	}
	for _, bad := range []string{"", "correct horse battery stapl", "Correct horse battery staple"} {
		if CheckPassword(h, bad) {
			t.Errorf("wrong password %q accepted", bad)
		}
	}
	h2, _ := HashPassword("correct horse battery staple")
	if h == h2 {
		t.Error("hashes are not salted")
	}
	for _, bad := range []string{"", "plain", "pbkdf2-sha256$1$AA$AA", "md5$1$x$y", h + "$"} {
		if CheckPassword(bad, "x") {
			t.Errorf("malformed hash %q accepted", bad)
		}
	}
	if ValidatePassword("short") == nil || ValidatePassword(strings.Repeat("a", 300)) == nil || ValidatePassword("twelve chars") != nil {
		t.Error("password policy")
	}
	if len(GeneratePassword()) < 20 || GeneratePassword() == GeneratePassword() {
		t.Error("generated passwords")
	}
}

// RFC 6238 appendix B test vector (SHA1, secret "12345678901234567890"),
// truncated to 6 digits.
func TestTOTPRFCVector(t *testing.T) {
	secret := b32.EncodeToString([]byte("12345678901234567890"))
	for unix, want := range map[int64]string{59: "287082", 1111111109: "081804", 1234567890: "005924", 2000000000: "279037"} {
		got, err := TOTPCode(secret, time.Unix(unix, 0))
		if err != nil || got != want {
			t.Errorf("TOTP at %d = %s, want %s", unix, got, want)
		}
	}
}

func TestTOTPWindowAndReplay(t *testing.T) {
	secret := NewTOTPSecret()
	now := time.Unix(1_800_000_000, 0)
	code, _ := TOTPCode(secret, now)
	step, err := CheckTOTP(secret, code, now, 0)
	if err != nil || step != now.Unix()/30 {
		t.Fatalf("current code: %v %d", err, step)
	}
	if _, err := CheckTOTP(secret, code, now, step); err == nil {
		t.Error("a code was accepted twice")
	}
	prev, _ := TOTPCode(secret, now.Add(-30*time.Second))
	if _, err := CheckTOTP(secret, prev, now, 0); err != nil {
		t.Error("previous step rejected: clocks drift")
	}
	old, _ := TOTPCode(secret, now.Add(-90*time.Second))
	if _, err := CheckTOTP(secret, old, now, 0); err == nil {
		t.Error("a code from 90s ago was accepted")
	}
	if _, err := CheckTOTP(secret, code[:3]+" "+code[3:], now, 0); err != nil {
		t.Error("spaces in a typed code should be ignored")
	}
	for _, bad := range []string{"", "12345", "abcdef", "1234567"} {
		if _, err := CheckTOTP(secret, bad, now, 0); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	uri := TOTPURI("WPGenie", "ann@example.com", secret)
	if !strings.HasPrefix(uri, "otpauth://totp/WPGenie:ann@example.com?") || !strings.Contains(uri, "secret="+secret) {
		t.Errorf("uri %s", uri)
	}
}

func TestRecoveryCodes(t *testing.T) {
	codes, hashes := NewRecoveryCodes(10)
	if len(codes) != 10 || len(hashes) != 10 {
		t.Fatal("count")
	}
	seen := map[string]bool{}
	for i, c := range codes {
		if len(c) != 14 || c[4] != '-' || seen[c] {
			t.Errorf("code %q", c)
		}
		seen[c] = true
		if HashRecoveryCode(strings.ToUpper(strings.ReplaceAll(c, "-", " "))) != hashes[i] {
			t.Error("typed variants of a code must hash the same")
		}
	}
}

func TestQRSVG(t *testing.T) {
	svg, err := QRSVG(TOTPURI("WPGenie", "ann", NewTOTPSecret()))
	if err != nil || !strings.HasPrefix(svg, "<svg") || !strings.Contains(svg, "h1v1h-1z") {
		t.Fatalf("svg %v %.80s", err, svg)
	}
}

func TestRoles(t *testing.T) {
	if !(Level(RoleAdmin) > Level(RoleOperator) && Level(RoleOperator) > Level(RoleViewer) && Level(RoleViewer) > Level("root")) {
		t.Error("role order")
	}
}
