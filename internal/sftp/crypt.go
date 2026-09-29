package sftp

import (
	"crypto/rand"
	"crypto/sha512"
	"strings"
)

// SHA-512 crypt ("$6$", Ulrich Drepper's specification): the password hash
// format the SFTP container's sshd understands (musl's crypt, no PAM).
// bcrypt, which Go has, is not accepted there.

const cryptAlphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// cryptRounds is the specification's default, which needs no "rounds="
// field in the hash.
const cryptRounds = 5000

// HashPassword returns a SHA-512 crypt hash of password with a random salt.
func HashPassword(password string) string {
	b := make([]byte, 16)
	rand.Read(b)
	salt := make([]byte, 16)
	for i, v := range b {
		salt[i] = cryptAlphabet[int(v)%len(cryptAlphabet)]
	}
	return sha512Crypt([]byte(password), salt)
}

func sha512Crypt(pw, salt []byte) string {
	if len(salt) > 16 {
		salt = salt[:16]
	}
	repeat := func(h interface{ Write([]byte) (int, error) }, src []byte, n int) {
		for ; n > 64; n -= 64 {
			h.Write(src)
		}
		h.Write(src[:n])
	}

	b := sha512.New()
	b.Write(pw)
	b.Write(salt)
	b.Write(pw)
	sumB := b.Sum(nil)

	a := sha512.New()
	a.Write(pw)
	a.Write(salt)
	repeat(a, sumB, len(pw))
	for n := len(pw); n > 0; n >>= 1 {
		if n&1 != 0 {
			a.Write(sumB)
		} else {
			a.Write(pw)
		}
	}
	sumA := a.Sum(nil)

	dp := sha512.New()
	for range pw {
		dp.Write(pw)
	}
	sumDP := dp.Sum(nil)
	p := make([]byte, 0, len(pw))
	for n := len(pw); n > 0; n -= min(n, 64) {
		p = append(p, sumDP[:min(n, 64)]...)
	}

	ds := sha512.New()
	for range 16 + int(sumA[0]) {
		ds.Write(salt)
	}
	sumDS := ds.Sum(nil)
	s := sumDS[:len(salt)]

	c := sumA
	for i := range cryptRounds {
		h := sha512.New()
		if i&1 != 0 {
			h.Write(p)
		} else {
			h.Write(c)
		}
		if i%3 != 0 {
			h.Write(s)
		}
		if i%7 != 0 {
			h.Write(p)
		}
		if i&1 != 0 {
			h.Write(c)
		} else {
			h.Write(p)
		}
		c = h.Sum(nil)
	}

	var out strings.Builder
	out.WriteString("$6$")
	out.Write(salt)
	out.WriteByte('$')
	enc := func(b2, b1, b0 byte, n int) {
		w := uint(b2)<<16 | uint(b1)<<8 | uint(b0)
		for range n {
			out.WriteByte(cryptAlphabet[w&0x3f])
			w >>= 6
		}
	}
	for _, t := range [][3]int{{0, 21, 42}, {22, 43, 1}, {44, 2, 23}, {3, 24, 45}, {25, 46, 4}, {47, 5, 26},
		{6, 27, 48}, {28, 49, 7}, {50, 8, 29}, {9, 30, 51}, {31, 52, 10}, {53, 11, 32}, {12, 33, 54},
		{34, 55, 13}, {56, 14, 35}, {15, 36, 57}, {37, 58, 16}, {59, 17, 38}, {18, 39, 60}, {40, 61, 19},
		{62, 20, 41}} {
		enc(c[t[0]], c[t[1]], c[t[2]], 4)
	}
	enc(0, 0, c[63], 2)
	return out.String()
}
