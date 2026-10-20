package config

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// ParseTrustedProxies parses FORGE_HTTP_TRUSTED_PROXIES: a comma-separated
// list of CIDR prefixes or bare addresses (a bare address is a /32 or /128),
// such as "172.18.0.0/16, fd00:1::/64, 10.0.0.7". A blank list trusts nobody.
// Empty entries, malformed entries, overly broad prefixes (IPv4 shorter than
// /8, IPv6 shorter than /16, which includes /0) and IPv4-mapped IPv6
// prefixes (peers are compared in their IPv4 form, so they would never
// match) are errors naming the entry's position, never its text (see
// CheckTrustedProxy). Prefixes are returned masked.
func ParseTrustedProxies(list string) ([]netip.Prefix, error) {
	if strings.TrimSpace(list) == "" {
		return nil, nil
	}
	var prefixes []netip.Prefix
	for i, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		var prefix netip.Prefix
		var err error
		if strings.Contains(item, "/") {
			prefix, err = netip.ParsePrefix(item)
		} else {
			var addr netip.Addr
			if addr, err = netip.ParseAddr(item); err == nil && addr.Zone() != "" {
				err = errors.New("zoned address")
			} else if err == nil {
				prefix = netip.PrefixFrom(addr, addr.BitLen())
			}
		}
		if err != nil {
			return nil, fmt.Errorf("invalid FORGE_HTTP_TRUSTED_PROXIES: entry %d is not an IP address or CIDR prefix such as 172.18.0.0/16", i+1)
		}
		if err := CheckTrustedProxy(prefix); err != nil {
			return nil, fmt.Errorf("invalid FORGE_HTTP_TRUSTED_PROXIES: entry %d %s", i+1, err)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

// Narrowest prefix lengths a trusted proxy network may have. Shorter prefixes
// trust a large part of the address space (two /1 prefixes cover the whole
// IPv4 internet), letting almost any client choose its address.
const (
	minTrustedPrefixBits4 = 8
	minTrustedPrefixBits6 = 16
)

// CheckTrustedProxy reports why prefix is not a safe trusted proxy network,
// or nil. It is the single definition of the rules: ParseTrustedProxies,
// Config.Validate and web.NewTrustedProxies all apply it. The error text
// completes the sentence "entry N ...", names no value, and never echoes the
// prefix.
//
// It refuses invalid prefixes, IPv4-mapped IPv6 prefixes (peers are compared
// in their IPv4 form, so they would never match), IPv4 prefixes shorter than
// /8 and IPv6 prefixes shorter than /16 (they would trust far more than a
// proxy network; this includes /0, which trusts every address). A bare
// address is a /32 or /128 and is unaffected.
func CheckTrustedProxy(prefix netip.Prefix) error {
	switch {
	case !prefix.IsValid():
		return errors.New("is not a valid CIDR prefix")
	case prefix.Addr().Is4In6():
		return errors.New("is an IPv4-mapped IPv6 prefix; use the IPv4 form")
	case prefix.Addr().Is4() && prefix.Bits() < minTrustedPrefixBits4:
		return errors.New("is too broad: IPv4 prefixes shorter than /8 trust too many addresses; list the proxy networks instead")
	case prefix.Addr().Is6() && prefix.Bits() < minTrustedPrefixBits6:
		return errors.New("is too broad: IPv6 prefixes shorter than /16 trust too many addresses; list the proxy networks instead")
	}
	return nil
}
