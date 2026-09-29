// Package iprep is IP reputation for the shield: public blocklists of
// attacking and hijacked networks, and a country database for per-site
// country rules. Lookups run on every dynamic request, so data is kept in
// sorted, merged ranges searched in O(log n) with no locks (immutable
// snapshots swapped atomically).
package iprep

import (
	"bytes"
	"cmp"
	"net/netip"
	"slices"
)

// Set is an immutable set of address ranges.
type Set struct {
	v4 []range4
	v6 []range6
}

type range4 struct{ lo, hi uint32 }
type range6 struct{ lo, hi [16]byte }

// NewSet builds a set from prefixes (which may overlap).
func NewSet(prefixes []netip.Prefix) *Set {
	var v4 []range4
	var v6 []range6
	for _, p := range prefixes {
		p = p.Masked()
		if p.Addr().Is4() {
			lo := u32(p.Addr())
			hi := lo | (1<<(32-p.Bits()) - 1)
			if p.Bits() == 0 {
				hi = ^uint32(0)
			}
			v4 = append(v4, range4{lo, hi})
		} else {
			lo := p.Addr().As16()
			v6 = append(v6, range6{lo, lastAddr(lo, p.Bits())})
		}
	}
	return &Set{v4: merge4(v4), v6: merge6(v6)}
}

func u32(a netip.Addr) uint32 {
	b := a.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// lastAddr sets every host bit of a /bits prefix.
func lastAddr(lo [16]byte, bits int) [16]byte {
	for i := bits; i < 128; i++ {
		lo[i/8] |= 0x80 >> (i % 8)
	}
	return lo
}

func merge4(rs []range4) []range4 {
	slices.SortFunc(rs, func(a, b range4) int { return cmp.Compare(a.lo, b.lo) })
	out := rs[:0]
	for _, r := range rs {
		if n := len(out); n > 0 && (r.lo <= out[n-1].hi || out[n-1].hi != ^uint32(0) && r.lo == out[n-1].hi+1) {
			out[n-1].hi = max(out[n-1].hi, r.hi)
			continue
		}
		out = append(out, r)
	}
	return slices.Clip(out)
}

func merge6(rs []range6) []range6 {
	slices.SortFunc(rs, func(a, b range6) int { return bytes.Compare(a.lo[:], b.lo[:]) })
	out := rs[:0]
	for _, r := range rs {
		if n := len(out); n > 0 && bytes.Compare(r.lo[:], out[n-1].hi[:]) <= 0 {
			if bytes.Compare(r.hi[:], out[n-1].hi[:]) > 0 {
				out[n-1].hi = r.hi
			}
			continue
		}
		out = append(out, r)
	}
	return slices.Clip(out)
}

// Contains reports whether the set covers a (IPv4-mapped IPv6 addresses
// are looked up as IPv4).
func (s *Set) Contains(a netip.Addr) bool {
	if s == nil {
		return false
	}
	a = a.Unmap()
	if a.Is4() {
		x := u32(a)
		i, _ := slices.BinarySearchFunc(s.v4, x, func(r range4, x uint32) int { return cmp.Compare(r.lo, x) })
		// i is the first range starting after x (or at it): the candidate is
		// the one starting at x, or the one before.
		if i < len(s.v4) && s.v4[i].lo == x {
			return true
		}
		return i > 0 && x <= s.v4[i-1].hi
	}
	if !a.Is6() {
		return false
	}
	x := a.As16()
	i, _ := slices.BinarySearchFunc(s.v6, x, func(r range6, x [16]byte) int { return bytes.Compare(r.lo[:], x[:]) })
	if i < len(s.v6) && s.v6[i].lo == x {
		return true
	}
	return i > 0 && bytes.Compare(x[:], s.v6[i-1].hi[:]) <= 0
}

// Len is the number of merged ranges.
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	return len(s.v4) + len(s.v6)
}
