// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! What the internet is, and what is denied always (ADR 0010, ADR 0012),
//! as the IANA registries of special-purpose addresses say.

use crate::prefix::Prefix;
use crate::ranges::{self, Range};
use std::net::Ipv4Addr;

/// Global unicast IPv6 addresses, the IANA IPv6 Address Space registry's
/// (https://www.iana.org/assignments/ipv6-address-space): all that IANA
/// hands out to the registries.
const IPV6_GLOBAL_UNICAST: &str = "2000::/3";

/// The IANA IPv6 Special-Purpose Address Registry
/// (https://www.iana.org/assignments/iana-ipv6-special-registry), as of
/// 2025-04: each block, and whether it is globally reachable. A more
/// specific block overrides the one it lies in, as the registry's note on
/// 2001::/23 says. Blocks marked N/A or not at all count as not globally
/// reachable.
const IPV6_SPECIAL: &[(&str, bool)] = &[
    ("::1/128", false),          // Loopback Address
    ("::/128", false),           // Unspecified Address
    ("::ffff:0:0/96", false),    // IPv4-mapped Address
    ("64:ff9b::/96", true),      // IPv4-IPv6 Translat.
    ("64:ff9b:1::/48", false),   // IPv4-IPv6 Translat.
    ("100::/64", false),         // Discard-Only Address Block
    ("100:0:0:1::/64", false),   // Dummy IPv6 Prefix
    ("2001::/23", false),        // IETF Protocol Assignments
    ("2001::/32", false),        // TEREDO: N/A
    ("2001:1::1/128", true),     // Port Control Protocol Anycast
    ("2001:1::2/128", true),     // Traversal Using Relays around NAT Anycast
    ("2001:1::3/128", true),     // DNS-SD Service Registration Protocol Anycast
    ("2001:2::/48", false),      // Benchmarking
    ("2001:3::/32", true),       // AMT
    ("2001:4:112::/48", true),   // AS112-v6
    ("2001:10::/28", false),     // Deprecated (previously ORCHID)
    ("2001:20::/28", true),      // ORCHIDv2
    ("2001:30::/28", true),      // Drone Remote ID Protocol Entity Tags (DETs) Prefix
    ("2001:db8::/32", false),    // Documentation
    ("2002::/16", false),        // 6to4: N/A
    ("2620:4f:8000::/48", true), // Direct Delegation AS112 Service
    ("3fff::/20", false),        // Documentation
    ("5f00::/16", false),        // Segment Routing (SRv6) SIDs
    ("fc00::/7", false),         // Unique-Local
    ("fe80::/10", false),        // Link-Local Unicast
];

/// The IANA IPv4 Special-Purpose Address Registry
/// (https://www.iana.org/assignments/iana-ipv4-special-registry), as of
/// 2025-04, read as `IPV6_SPECIAL` is; and multicast, which the IANA IPv4
/// Multicast Address Space registry holds (RFC 5771).
const IPV4_SPECIAL: &[(&str, bool)] = &[
    ("0.0.0.0/8", false),          // "This network"
    ("0.0.0.0/32", false),         // "This host on this network"
    ("10.0.0.0/8", false),         // Private-Use
    ("100.64.0.0/10", false),      // Shared Address Space
    ("127.0.0.0/8", false),        // Loopback
    ("169.254.0.0/16", false),     // Link Local
    ("172.16.0.0/12", false),      // Private-Use
    ("192.0.0.0/24", false),       // IETF Protocol Assignments
    ("192.0.0.0/29", false),       // IPv4 Service Continuity Prefix
    ("192.0.0.8/32", false),       // IPv4 dummy address
    ("192.0.0.9/32", true),        // Port Control Protocol Anycast
    ("192.0.0.10/32", true),       // Traversal Using Relays around NAT Anycast
    ("192.0.0.170/32", false),     // ipv4only.arpa (RFC 7050)
    ("192.0.0.171/32", false),     // ipv4only.arpa (RFC 7050)
    ("192.0.2.0/24", false),       // Documentation (TEST-NET-1)
    ("192.31.196.0/24", true),     // AS112-v4
    ("192.52.193.0/24", true),     // AMT
    ("192.88.99.0/24", false),     // Deprecated (6to4 Relay Anycast)
    ("192.88.99.2/32", false),     // 6a44-relay anycast address
    ("192.168.0.0/16", false),     // Private-Use
    ("192.175.48.0/24", true),     // Direct Delegation AS112 Service
    ("198.18.0.0/15", false),      // Benchmarking
    ("198.51.100.0/24", false),    // Documentation (TEST-NET-2)
    ("203.0.113.0/24", false),     // Documentation (TEST-NET-3)
    ("224.0.0.0/4", false),        // Multicast, RFC 5771: not in the registry
    ("240.0.0.0/4", false),        // Reserved
    ("255.255.255.255/32", false), // Limited Broadcast
];

/// What no pod reaches, whatever its groups say (ADR 0010): link-local,
/// loopback, multicast and unspecified addresses of both families. Of
/// IPv4, "this network", which holds the unspecified address and is no
/// packet's destination.
const ALWAYS_DENIED: &[&str] = &[
    "fe80::/10",
    "::1/128",
    "ff00::/8",
    "::/128",
    "169.254.0.0/16",
    "127.0.0.0/8",
    "224.0.0.0/4",
    "0.0.0.0/8",
];

/// The IPv6 internet of the default group: global unicast addresses;
/// normal.
pub(crate) fn internet6() -> Vec<Range> {
    IPV6_GLOBAL_UNICAST
        .parse::<Prefix>()
        .map(|scope| global(scope.range(), IPV6_SPECIAL))
        .unwrap_or_default()
}

/// The IPv4 internet of the default group: globally reachable
/// addresses; normal.
pub(crate) fn internet4() -> Vec<Range> {
    global(Prefix::V4(Ipv4Addr::UNSPECIFIED, 0).range(), IPV4_SPECIAL)
}

/// The prefixes that are denied always, of both families.
pub fn always_denied() -> impl Iterator<Item = Prefix> {
    ALWAYS_DENIED
        .iter()
        .filter_map(|prefix| prefix.parse::<Prefix>().ok())
}

/// The globally reachable addresses of `scope`, as `registry` has them: from
/// the shortest blocks to the longest, a block that is not globally
/// reachable is taken out, one that is is put back in. The registry is of
/// the scope's family.
fn global(scope: Range, registry: &[(&str, bool)]) -> Vec<Range> {
    let mut entries: Vec<(Prefix, bool)> = registry
        .iter()
        .filter_map(|&(prefix, global)| Some((prefix.parse::<Prefix>().ok()?, global)))
        .collect();
    entries.sort_by_key(|(prefix, _)| prefix.length());
    let mut reachable = vec![scope];
    for (prefix, global) in entries {
        let range = prefix.range();
        reachable = if global {
            match ranges::intersect(range, scope) {
                Some(range) => ranges::normalize([reachable, vec![range]].concat()),
                None => reachable,
            }
        } else {
            ranges::subtract(&reachable, &[range])
        };
    }
    reachable
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::net::IpAddr;

    /// Whether the IPv4 or the IPv6 ranges hold the address, as the number
    /// of its family.
    fn holds(ipv4: &[Range], ipv6: &[Range], address: &str) -> bool {
        let (ranges, address) = match address.parse::<IpAddr>().unwrap() {
            IpAddr::V4(address) => (ipv4, u128::from(u32::from(address))),
            IpAddr::V6(address) => (ipv6, u128::from(address)),
        };
        ranges
            .iter()
            .any(|&(start, end)| start <= address && address <= end)
    }

    #[test]
    fn the_registries_parse() {
        for &(prefix, _) in IPV6_SPECIAL {
            assert!(!prefix.parse::<Prefix>().unwrap().is_ipv4(), "{prefix}");
        }
        for &(prefix, _) in IPV4_SPECIAL {
            assert!(prefix.parse::<Prefix>().unwrap().is_ipv4(), "{prefix}");
        }
        for prefix in ALWAYS_DENIED.iter().chain([&IPV6_GLOBAL_UNICAST]) {
            assert!(prefix.parse::<Prefix>().is_ok(), "{prefix}");
        }
        assert_eq!(always_denied().count(), ALWAYS_DENIED.len());
    }

    #[test]
    fn the_internet_is_global_addresses() {
        let (ipv4, ipv6) = (internet4(), internet6());
        assert_eq!(ranges::normalize(ipv4.clone()), ipv4);
        assert_eq!(ranges::normalize(ipv6.clone()), ipv6);
        assert!(ipv4.iter().all(|&(_, end)| end <= u128::from(u32::MAX)));
        for address in [
            "8.8.8.8",
            "1.1.1.1",
            "2001:4860:4860::8888",
            "2606:4700:4700::1111",
            "2a00:1450:4001::1",
            "2001:1::1",
            "2001:3::1",
            "2001:4:112::1",
            "2001:20::1",
            "2620:4f:8000::1",
            "192.0.0.9",
            "192.0.0.10",
            "192.31.196.1",
            "223.255.255.255",
            "3ffe::1",
        ] {
            assert!(
                holds(&ipv4, &ipv6, address),
                "{address} is not in the internet"
            );
        }
        for address in [
            "10.1.2.3",
            "192.168.1.1",
            "172.16.0.1",
            "100.64.0.1",
            "127.0.0.1",
            "169.254.169.254",
            "169.254.16.1",
            "0.0.0.0",
            "192.0.0.1",
            "192.0.0.8",
            "192.0.0.170",
            "192.0.2.1",
            "192.88.99.1",
            "198.18.0.1",
            "198.51.100.1",
            "203.0.113.1",
            "224.0.0.1",
            "240.0.0.1",
            "255.255.255.255",
            "fd00::1",
            "fe80::1",
            "::1",
            "::",
            "ff02::1",
            "2001:db8::1",
            "2001::1",
            "2001:1::4",
            "2001:2::1",
            "2001:10::1",
            "2002::1",
            "3fff::1",
            "64:ff9b::808:808",
            "64:ff9b:1::1",
            "::ffff:8.8.8.8",
            "1fff:ffff:ffff:ffff:ffff:ffff:ffff:ffff",
            "4000::",
        ] {
            assert!(
                !holds(&ipv4, &ipv6, address),
                "{address} is in the internet"
            );
        }
    }

    #[test]
    fn link_local_loopback_multicast_and_unspecified_are_denied() {
        let (ipv4, ipv6): (Vec<Prefix>, Vec<Prefix>) = always_denied().partition(Prefix::is_ipv4);
        let ipv4: Vec<Range> = ipv4.iter().map(Prefix::range).collect();
        let ipv6: Vec<Range> = ipv6.iter().map(Prefix::range).collect();
        for address in [
            "fe80::1",
            "::1",
            "ff02::1",
            "::",
            "169.254.169.254",
            "169.254.0.53",
            "169.254.16.7",
            "127.0.0.1",
            "224.0.0.251",
            "0.0.0.0",
        ] {
            assert!(holds(&ipv4, &ipv6, address), "{address} is not denied");
        }
        for address in [
            "8.8.8.8",
            "2001:4860:4860::8888",
            "10.1.2.3",
            "fd00::1",
            "::2",
        ] {
            assert!(!holds(&ipv4, &ipv6, address), "{address} is denied");
        }
    }
}
