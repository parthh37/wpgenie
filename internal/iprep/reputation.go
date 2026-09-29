package iprep

import "net/netip"

// Reputation combines the blocklists and the country database into what
// the shield asks about each client (shield.Reputation).
type Reputation struct {
	Lists     *Lists
	Countries *Countries
}

func (r *Reputation) Listed(a netip.Addr) (string, bool) {
	if r.Lists == nil {
		return "", false
	}
	return r.Lists.Listed(a)
}

func (r *Reputation) Country(a netip.Addr) (string, bool) {
	if r.Countries == nil {
		return "", false
	}
	return r.Countries.Country(a)
}
