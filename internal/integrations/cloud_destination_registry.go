package integrations

import "net/netip"

// IANA special-purpose registries, 2025-10-09. Exact CSV snapshots and
// SHA256 are retained with the T1 qualification artifact, never fetched at runtime.
const conditionalDestinationRegistryIdentity = "iana-special/2025-10-09/cbc4b7653c8bc640ba1e8e5d4ebbf11e7c7a1d867e7f1593aad001d3f8632aac/bf56772aef97fbf9b4a27c98b8511f2bb1ba96da4b9ccc12ab203a2496faba66"

type conditionalSpecialPrefix struct {
	prefix netip.Prefix
	public bool
}

var conditionalSpecialPrefixes = [...]conditionalSpecialPrefix{
	// ipv4 CSV SHA256 cbc4b7653c8bc640ba1e8e5d4ebbf11e7c7a1d867e7f1593aad001d3f8632aac
	{netip.MustParsePrefix("0.0.0.0/8"), false},
	{netip.MustParsePrefix("0.0.0.0/32"), false},
	{netip.MustParsePrefix("10.0.0.0/8"), false},
	{netip.MustParsePrefix("100.64.0.0/10"), false},
	{netip.MustParsePrefix("127.0.0.0/8"), false},
	{netip.MustParsePrefix("169.254.0.0/16"), false},
	{netip.MustParsePrefix("172.16.0.0/12"), false},
	{netip.MustParsePrefix("192.0.0.0/24"), false},
	{netip.MustParsePrefix("192.0.0.0/29"), false},
	{netip.MustParsePrefix("192.0.0.8/32"), false},
	{netip.MustParsePrefix("192.0.0.9/32"), true},
	{netip.MustParsePrefix("192.0.0.10/32"), true},
	{netip.MustParsePrefix("192.0.0.170/32"), false},
	{netip.MustParsePrefix("192.0.0.171/32"), false},
	{netip.MustParsePrefix("192.0.2.0/24"), false},
	{netip.MustParsePrefix("192.31.196.0/24"), true},
	{netip.MustParsePrefix("192.52.193.0/24"), true},
	{netip.MustParsePrefix("192.88.99.0/24"), false},
	{netip.MustParsePrefix("192.88.99.2/32"), false},
	{netip.MustParsePrefix("192.168.0.0/16"), false},
	{netip.MustParsePrefix("192.175.48.0/24"), true},
	{netip.MustParsePrefix("198.18.0.0/15"), false},
	{netip.MustParsePrefix("198.51.100.0/24"), false},
	{netip.MustParsePrefix("203.0.113.0/24"), false},
	{netip.MustParsePrefix("240.0.0.0/4"), false},
	{netip.MustParsePrefix("255.255.255.255/32"), false},
	// ipv6 CSV SHA256 bf56772aef97fbf9b4a27c98b8511f2bb1ba96da4b9ccc12ab203a2496faba66
	{netip.MustParsePrefix("::1/128"), false},
	{netip.MustParsePrefix("::/128"), false},
	{netip.MustParsePrefix("::ffff:0:0/96"), false},
	{netip.MustParsePrefix("64:ff9b::/96"), true},
	{netip.MustParsePrefix("64:ff9b:1::/48"), false},
	{netip.MustParsePrefix("100::/64"), false},
	{netip.MustParsePrefix("100:0:0:1::/64"), false},
	{netip.MustParsePrefix("2001::/23"), false},
	{netip.MustParsePrefix("2001::/32"), false},
	{netip.MustParsePrefix("2001:1::1/128"), true},
	{netip.MustParsePrefix("2001:1::2/128"), true},
	{netip.MustParsePrefix("2001:1::3/128"), true},
	{netip.MustParsePrefix("2001:2::/48"), false},
	{netip.MustParsePrefix("2001:3::/32"), true},
	{netip.MustParsePrefix("2001:4:112::/48"), true},
	{netip.MustParsePrefix("2001:10::/28"), false},
	{netip.MustParsePrefix("2001:20::/28"), true},
	{netip.MustParsePrefix("2001:30::/28"), true},
	{netip.MustParsePrefix("2001:db8::/32"), false},
	{netip.MustParsePrefix("2002::/16"), false},
	{netip.MustParsePrefix("2620:4f:8000::/48"), true},
	{netip.MustParsePrefix("3fff::/20"), false},
	{netip.MustParsePrefix("5f00::/16"), false},
	{netip.MustParsePrefix("fc00::/7"), false},
	{netip.MustParsePrefix("fe80::/10"), false},
}

func conditionalPublicAddress(address netip.Addr) bool {
	if !address.IsValid() || address.Zone() != "" {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() {
		return false
	}
	// Public unassigned/obsolete IPv6 classes are not inferred from unicast.
	// Only currently allocated global-unicast space or a positive special
	// exception is eligible. This rejects compatible IPv4 and fec0::/10.
	admitted := address.Is4() || netip.MustParsePrefix("2000::/3").Contains(address)
	best := -1
	for _, entry := range conditionalSpecialPrefixes {
		if entry.prefix.Contains(address) && entry.prefix.Bits() > best {
			best = entry.prefix.Bits()
			admitted = entry.public
		}
	}
	if !admitted {
		return false
	}
	if netip.MustParsePrefix("64:ff9b::/96").Contains(address) {
		raw := address.As16()
		return conditionalPublicAddress(netip.AddrFrom4([4]byte{raw[12], raw[13], raw[14], raw[15]}))
	}
	return true
}
