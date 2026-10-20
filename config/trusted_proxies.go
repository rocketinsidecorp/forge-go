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
// Empty entries, malformed entries, prefixes of length 0 (they would trust
// every address) and IPv4-mapped IPv6 prefixes (peers are compared in their
// IPv4 form, so they would never match) are errors naming the entry's
// position, never its text. Prefixes are returned masked.
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
		if err := checkTrustedProxy(prefix); err != nil {
			return nil, fmt.Errorf("invalid FORGE_HTTP_TRUSTED_PROXIES: entry %d %s", i+1, err)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

// checkTrustedProxy holds the rules web.NewTrustedProxies enforces too.
func checkTrustedProxy(prefix netip.Prefix) error {
	switch {
	case !prefix.IsValid():
		return errors.New("is not a valid CIDR prefix")
	case prefix.Addr().Is4In6():
		return errors.New("is an IPv4-mapped IPv6 prefix; use the IPv4 form")
	case prefix.Bits() == 0:
		return errors.New("trusts every address; list the proxy networks instead")
	}
	return nil
}
