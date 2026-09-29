package sftp

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestSHA512CryptVectors(t *testing.T) {
	// From the specification (default rounds).
	for _, v := range []struct{ salt, pw, want string }{
		{"saltstring", "Hello world!",
			"$6$saltstring$svn8UoSVapNtMuq1ukKS4tPQd8iKwSMHWjl/O817G3uBnIFNjnQJuesI68u4OTLiBFdcbYEdFCoEOfaS35inz1"},
		// Salts are cut at 16 characters.
		{"toolongsaltstringXXXXXXX", "This is just a test",
			"$6$toolongsaltstrin$lQ8jolhgVRVhY4b5pZKaysCLi0QBxGoNeKQzQ3glMhwllF7oGDZxUhx1yxdYcz/e1JSbq3y6JMxxl8audkUEm0"},
	} {
		if got := sha512Crypt([]byte(v.pw), []byte(v.salt)); got != v.want {
			t.Errorf("crypt(%q, %q) = %s\nwant %s", v.pw, v.salt, got, v.want)
		}
	}
}

// TestHashMatchesOpenSSL checks random salts and a long password against
// OpenSSL's implementation (in Docker).
func TestHashMatchesOpenSSL(t *testing.T) {
	if os.Getenv("WPGENIE_TEST_DOCKER") != "1" {
		t.Skip("set WPGENIE_TEST_DOCKER=1")
	}
	pw := strings.Repeat("correct horse battery staple ", 5) // > 64 bytes
	h := HashPassword(pw)
	salt := strings.Split(h, "$")[2]
	out, err := exec.Command("docker", "run", "--rm", "alpine:3.22", "sh", "-c",
		`apk add -q openssl >/dev/null 2>&1 && openssl passwd -6 -salt "$1" "$2"`, "sh", salt, pw).Output()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != h {
		t.Fatalf("openssl %s\nours    %s", got, h)
	}
}
