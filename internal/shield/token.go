package shield

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/bits"
	"net"
	"strings"
	"time"
)

// Tokens are stateless: base64url(json payload) + "." + base64url(HMAC).
// No server-side session store means the shield scales horizontally — any
// node holding the secret can verify any token.

var errBadToken = errors.New("invalid token")

type challengePayload struct {
	Site       string `json:"s"`
	Net        string `json:"n"` // client network bucket, see ipBucket
	Seed       string `json:"r"`
	Difficulty int    `json:"d"` // required leading zero bits
	Expires    int64  `json:"x"`
}

type passPayload struct {
	Site    string `json:"s"`
	Net     string `json:"n"`
	UA      string `json:"u"` // truncated UA hash: stops cookie reuse across tools
	Expires int64  `json:"x"`
}

func sign(secret []byte, payload any) (string, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(b)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func open(secret []byte, token string, into any) error {
	body, sig, ok := strings.Cut(token, ".")
	if !ok {
		return errBadToken
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return errBadToken
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(body))
	if !hmac.Equal(got, mac.Sum(nil)) {
		return errBadToken
	}
	b, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return errBadToken
	}
	return json.Unmarshal(b, into)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b)
}

// ipBucket groups an address into its /24 (IPv4) or /64 (IPv6) so a pass
// survives mobile clients hopping between addresses in the same network.
func ipBucket(ip string) string {
	p := net.ParseIP(ip)
	if p == nil {
		return ""
	}
	if v4 := p.To4(); v4 != nil {
		return v4.Mask(net.CIDRMask(24, 32)).String()
	}
	return p.Mask(net.CIDRMask(64, 128)).String()
}

func uaHash(ua string) string {
	h := sha256.Sum256([]byte(ua))
	return hex.EncodeToString(h[:6])
}

// leadingZeroBits counts leading zero bits of sha256(token + ":" + nonce).
// The browser must find a nonce reaching the difficulty; verifying costs one
// hash. That asymmetry is what makes proof-of-work a CAPTCHA without a
// third party or any user interaction.
func leadingZeroBits(token, nonce string) int {
	sum := sha256.Sum256([]byte(token + ":" + nonce))
	n := 0
	for _, b := range sum {
		if b == 0 {
			n += 8
			continue
		}
		n += bits.LeadingZeros8(b)
		break
	}
	return n
}

func expired(unix int64, now time.Time) bool { return now.Unix() > unix }
