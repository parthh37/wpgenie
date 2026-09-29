// Package domain validates the domain names WPGenie puts into Caddy,
// mail server and DNS configuration.
package domain

import (
	"errors"
	"regexp"
	"strings"
)

var ErrInvalid = errors.New("invalid domain name")

var re = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// Normalize lower-cases a domain, strips a scheme, trailing slash and
// trailing dot, and rejects anything that isn't a plain hostname (no
// wildcards, IPs, ports, paths or config syntax).
func Normalize(d string) (string, error) {
	d = strings.ToLower(strings.TrimSpace(d))
	d = strings.TrimPrefix(strings.TrimPrefix(d, "https://"), "http://")
	d = strings.TrimSuffix(strings.TrimSuffix(d, "/"), ".")
	if len(d) > 253 || !re.MatchString(d) {
		return "", ErrInvalid
	}
	return d, nil
}
