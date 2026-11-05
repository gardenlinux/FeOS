// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! feosd's nftables table, `inet feos`, from the compiled security groups
//! (ADR 0010, 0012, 0013):
//!
//! - `prerouting` drops what a pod sends from an address that is not
//!   routed back out its TAP, of either family, and router advertisements
//!   and redirects; on a mesh (ADR 0017), whatever comes from an address of
//!   the cluster's ULA through another device than the one it is routed
//!   to, the mesh's or a pod's TAP.
//! - `input` takes neighbor discovery, DNS to the resolver and OTLP to its
//!   own address (docs/architecture/traces.md) from pods, nothing else of
//!   theirs.
//! - `forward` copies the first packets of the TCP connections pods open
//!   to nflog, passes what belongs to a connection, and sends what is new
//!   from or to a pod's TAP through `from-pods` and `to-pods`. Their
//!   verdict maps `pod-out` and `pod-in` go from a TAP to the pod's chains
//!   `out-<TAP>` and `in-<TAP>`; a TAP the maps do not have is dropped.
//!   A pod's chains drop denied prefixes, return what a rule of the pod's
//!   groups allows, and drop the rest: out of both families, in of IPv6
//!   alone, since nothing new comes in over IPv4. They end in `return`
//!   rather than `accept`, which would end the base chain: a packet
//!   between two pods passes both. The maps go to them, so their `return`
//!   leads back to `forward`.
//! - `postrouting` masquerades the pods' IPv4 where it leaves the node.
//!
//! Base chains accept by default: the node's other traffic is not
//! feosd's. Drops are logged to nflog, limited per pod.

use feos_utils::nft::matches::*;
use feos_utils::nft::{
    ct, Chain, CmpOp, Direction, Element, ElementOp, Expr, Family, Hook, KeyType, Per,
    Policy as ChainPolicy, Rule, Set, Table, Verdict,
};
use network_service::{
    AddressRange, Allow, Family as AddressFamily, Policy, Protocol, SetChange, DENY4, DENY6,
};
use std::collections::BTreeMap;
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr};
use traffic_service::{prefix, COPY_GROUP, COPY_SNAPLEN, DROP_GROUP};

/// feosd's table, in the inet family.
pub const TABLE: &str = "feos";
/// The verdict maps from a pod's TAP to its chains.
pub const POD_IN: &str = "pod-in";
pub const POD_OUT: &str = "pod-out";
/// The chains that look a pod's TAP up in the maps.
pub const FROM_PODS: &str = "from-pods";
pub const TO_PODS: &str = "to-pods";
/// What the names of pods' TAP devices start with.
const PODS: &str = "pod-";
/// Drops are logged at most this often, per pod and reason.
const LOG_RATE: u64 = 10;
const LOG_BURST: u32 = 20;
/// The packets of a connection a pod opens that are copied to nflog: the
/// handshake and the ClientHello, even over two segments.
const COPY_PACKETS: u64 = 5;

const ICMP_REDIRECT: u8 = 5;
const ND_ROUTER_ADVERT: u8 = 134;
const ND_NEIGHBOR_SOLICIT: u8 = 135;
const ND_NEIGHBOR_ADVERT: u8 = 136;
const ND_REDIRECT: u8 = 137;
const DNS_PORT: u16 = 53;
/// OTLP's ports, gRPC and HTTP.
pub const OTLP_PORTS: (u16, u16) = (4317, 4318);
/// The HTTP bindings' ports at OTLP's address (ADR 0028): the name mode's
/// plain HTTP and the intercept mode's TLS.
pub const HTTP_PORTS: (u16, u16) = (80, 443);

/// What the table needs besides the policy.
#[derive(Clone, Debug, PartialEq)]
pub struct Options {
    /// The resolver's address (`--dns-address`, ADR 0010), which pods
    /// reach at port 53 over UDP and TCP: an IPv4 address.
    pub dns: IpAddr,
    /// OTLP's address (`--otlp-address`), which pods reach at
    /// [`OTLP_PORTS`] over TCP, and their HTTP bindings at [`HTTP_PORTS`].
    pub otlp: IpAddr,
    /// The pods' IPv4 addresses (`--pod-ipv4-range`), which are
    /// masqueraded where they leave the node.
    pub ipv4_range: (Ipv4Addr, u8),
    /// The cluster's ULA, on a mesh.
    pub mesh: Option<(Ipv6Addr, u8)>,
}

/// The pod's chains.
pub fn in_chain(tap: &str) -> String {
    format!("in-{tap}")
}

pub fn out_chain(tap: &str) -> String {
    format!("out-{tap}")
}

pub fn table(policy: &Policy, options: &Options) -> Table {
    let mut sets: Vec<Set> = policy
        .sets
        .iter()
        .map(|(name, set)| {
            let key = match set.family {
                AddressFamily::V4 => KeyType::Ipv4Addr,
                AddressFamily::V6 => KeyType::Ipv6Addr,
            };
            Set::intervals(name, key, set.ranges.iter().map(element).collect())
        })
        .collect();
    let map = |name: &str, chain: fn(&str) -> String| {
        let elements = policy
            .pods
            .iter()
            .map(|pod| Element::ifname(&pod.tap).with_verdict(Verdict::Goto(chain(&pod.tap))))
            .collect();
        Set::verdict_map(name, KeyType::IfName, elements)
    };
    sets.push(map(POD_IN, in_chain));
    sets.push(map(POD_OUT, out_chain));

    let mut chains = Vec::new();
    for pod in &policy.pods {
        chains.push(pod_chain(
            &in_chain(&pod.tap),
            &pod.ingress,
            End::Source,
            &[AddressFamily::V6],
        ));
        chains.push(pod_chain(
            &out_chain(&pod.tap),
            &pod.egress,
            End::Destination,
            &[AddressFamily::V6, AddressFamily::V4],
        ));
    }
    chains.push(lookup_chain(FROM_PODS, iifname_vmap(POD_OUT)));
    chains.push(lookup_chain(TO_PODS, oifname_vmap(POD_IN)));
    chains.push(Chain::base(
        "prerouting",
        "filter",
        Hook::Prerouting,
        -150,
        ChainPolicy::Accept,
        prerouting(options),
    ));
    chains.push(Chain::base(
        "input",
        "filter",
        Hook::Input,
        0,
        ChainPolicy::Accept,
        input(options),
    ));
    chains.push(Chain::base(
        "forward",
        "filter",
        Hook::Forward,
        0,
        ChainPolicy::Accept,
        forward(),
    ));
    chains.push(Chain::base(
        "postrouting",
        "nat",
        Hook::Postrouting,
        100,
        ChainPolicy::Accept,
        postrouting(options),
    ));
    Table {
        family: Family::Inet,
        name: TABLE.to_string(),
        sets,
        chains,
    }
}

/// The changes of a delta, in one batch: removals before additions.
pub fn element_ops(changes: &BTreeMap<String, SetChange>) -> Vec<ElementOp> {
    let removals = changes
        .iter()
        .filter(|(_, change)| !change.remove.is_empty())
        .map(|(set, change)| ElementOp::Delete {
            set: set.clone(),
            elements: change.remove.iter().map(element).collect(),
        });
    let additions = changes
        .iter()
        .filter(|(_, change)| !change.add.is_empty())
        .map(|(set, change)| ElementOp::Add {
            set: set.clone(),
            elements: change.add.iter().map(element).collect(),
        });
    removals.chain(additions).collect()
}

fn element(range: &AddressRange) -> Element {
    match range {
        AddressRange::V4(range) => Element::ipv4_range(*range.start(), *range.end()),
        AddressRange::V6(range) => Element::ipv6_range(*range.start(), *range.end()),
    }
}

fn verdict(verdict: Verdict) -> Vec<Expr> {
    vec![Expr::Verdict(verdict)]
}

/// Logs a dropped packet to the drop group, limited.
fn log(reason: &str) -> Vec<Expr> {
    [
        limit(LOG_RATE, Per::Second, LOG_BURST),
        log_group(DROP_GROUP, reason, 0),
    ]
    .concat()
}

fn from_pods() -> Vec<Expr> {
    iifname_prefix(CmpOp::Eq, PODS)
}

fn to_pods() -> Vec<Expr> {
    oifname_prefix(CmpOp::Eq, PODS)
}

/// The end of a packet that a pod's chain finds its peers at: the source
/// of what the pod receives, the destination of what it sends.
#[derive(Clone, Copy)]
enum End {
    Source,
    Destination,
}

/// The address at `end` in a set of `family`. A destination is the one the
/// pod chose: the connection's original one. A DNAT before the forward
/// hook, such as kube-proxy's of a LoadBalancer's address to its
/// endpoints on a node of a Kubernetes cluster, would otherwise turn a
/// public address the pod may reach into a cluster address it may not.
/// Only new connections come here (forward accepts established ones), and
/// every packet of a pod has one (forward drops the untracked).
fn peer(family: AddressFamily, end: End, set: &str) -> Vec<Expr> {
    match (family, end) {
        (AddressFamily::V4, End::Source) => ipv4_saddr_in(set),
        (AddressFamily::V4, End::Destination) => ct_original_ipv4_daddr_in(set),
        (AddressFamily::V6, End::Source) => ipv6_saddr_in(set),
        (AddressFamily::V6, End::Destination) => ct_original_ipv6_daddr_in(set),
    }
}

fn deny_set(family: AddressFamily) -> &'static str {
    match family {
        AddressFamily::V4 => DENY4,
        AddressFamily::V6 => DENY6,
    }
}

/// A pod's chain for one direction: its peers are at `end`, and only
/// packets of `families` may pass.
fn pod_chain(name: &str, allows: &[Allow], end: End, families: &[AddressFamily]) -> Chain {
    let mut rules: Vec<Rule> = Vec::new();
    for &family in families {
        let denied = peer(family, end, deny_set(family));
        rules.push([denied.clone(), log(prefix::DENY)].concat());
        rules.push([denied, verdict(Verdict::Drop)].concat());
    }
    for allow in allows
        .iter()
        .filter(|allow| families.contains(&allow.family))
    {
        let protocol = allow.protocol.number().map(l4proto).unwrap_or_default();
        let peers = peer(allow.family, end, &allow.peer);
        let ports = match allow.protocol {
            Protocol::Tcp | Protocol::Udp => allow.ports.as_slice(),
            _ => &[],
        };
        if ports.is_empty() {
            rules.push([protocol, peers, verdict(Verdict::Return)].concat());
            continue;
        }
        for range in ports {
            rules.push(
                [
                    protocol.clone(),
                    th_dport_range(*range.start(), *range.end()),
                    peers.clone(),
                    verdict(Verdict::Return),
                ]
                .concat(),
            );
        }
    }
    rules.push(log(prefix::NO_RULE));
    rules.push(verdict(Verdict::Drop));
    Chain::regular(name, rules)
}

/// Goes to the chain of the pod whose TAP the map has, and drops what
/// comes from or goes to a TAP of a pod it does not have, such as one
/// whose rules failed to be written.
fn lookup_chain(name: &str, vmap: Rule) -> Chain {
    Chain::regular(
        name,
        vec![vmap, log(prefix::NO_RULE), verdict(Verdict::Drop)],
    )
}

fn prerouting(options: &Options) -> Vec<Rule> {
    let spoofed = fib_saddr_iif_oif_missing;
    let mut rules = vec![
        // A pod sends from its addresses alone, of either family: those
        // routed to its TAP.
        [from_pods(), spoofed(), log(prefix::SPOOFED)].concat(),
        [from_pods(), spoofed(), verdict(Verdict::Drop)].concat(),
        [
            from_pods(),
            icmpv6_type(ND_ROUTER_ADVERT),
            verdict(Verdict::Drop),
        ]
        .concat(),
        [
            from_pods(),
            icmpv6_type(ND_REDIRECT),
            verdict(Verdict::Drop),
        ]
        .concat(),
        [
            from_pods(),
            icmp_type(ICMP_REDIRECT),
            verdict(Verdict::Drop),
        ]
        .concat(),
    ];
    // An address of the cluster's comes from its node: through the mesh,
    // which takes a peer's packets from its /64 alone, or from the pod's
    // TAP; not through the node's other devices.
    if let Some((network, len)) = options.mesh {
        let cluster = ipv6_prefix(IPV6_SADDR, CmpOp::Eq, network, len);
        rules.push([cluster.clone(), spoofed(), log(prefix::SPOOFED)].concat());
        rules.push([cluster, spoofed(), verdict(Verdict::Drop)].concat());
    }
    rules
}

fn input(options: &Options) -> Vec<Rule> {
    let daddr = |address| match address {
        IpAddr::V4(address) => ipv4_daddr(CmpOp::Eq, address),
        IpAddr::V6(address) => ipv6_daddr(CmpOp::Eq, address),
    };
    let resolver = daddr(options.dns);
    let dns = |protocol| {
        [
            from_pods(),
            resolver.clone(),
            l4proto(protocol),
            th_dport(CmpOp::Eq, DNS_PORT),
            verdict(Verdict::Accept),
        ]
        .concat()
    };
    // The HTTP bindings at OTLP's address (ADR 0028).
    let http = |port| {
        [
            from_pods(),
            daddr(options.otlp),
            l4proto(TCP),
            th_dport(CmpOp::Eq, port),
            verdict(Verdict::Accept),
        ]
        .concat()
    };
    vec![
        [
            from_pods(),
            icmpv6_type(ND_NEIGHBOR_SOLICIT),
            verdict(Verdict::Accept),
        ]
        .concat(),
        [
            from_pods(),
            icmpv6_type(ND_NEIGHBOR_ADVERT),
            verdict(Verdict::Accept),
        ]
        .concat(),
        dns(UDP),
        dns(TCP),
        [
            from_pods(),
            daddr(options.otlp),
            l4proto(TCP),
            th_dport_range(OTLP_PORTS.0, OTLP_PORTS.1),
            verdict(Verdict::Accept),
        ]
        .concat(),
        http(HTTP_PORTS.0),
        http(HTTP_PORTS.1),
        [from_pods(), verdict(Verdict::Drop)].concat(),
    ]
}

fn forward() -> Vec<Rule> {
    vec![
        // Before anything passes: copies of the first packets of the
        // connections pods open, of either family, for their server names
        // (ADR 0013).
        [
            from_pods(),
            l4proto(TCP),
            ct_direction(Direction::Original),
            ct_original_packets(CmpOp::Lt, COPY_PACKETS),
            log_group(COPY_GROUP, prefix::COPY, COPY_SNAPLEN),
        ]
        .concat(),
        [
            ct_state(ct::ESTABLISHED | ct::RELATED),
            verdict(Verdict::Accept),
        ]
        .concat(),
        [from_pods(), ct_state(ct::INVALID), verdict(Verdict::Drop)].concat(),
        [to_pods(), ct_state(ct::INVALID), verdict(Verdict::Drop)].concat(),
        // The pods' chains check destinations by their connections; a
        // packet without one, which another table's notrack would make,
        // has nothing for them to check.
        [from_pods(), ct_state(ct::UNTRACKED), verdict(Verdict::Drop)].concat(),
        [from_pods(), verdict(Verdict::Jump(FROM_PODS.to_string()))].concat(),
        [to_pods(), verdict(Verdict::Jump(TO_PODS.to_string()))].concat(),
    ]
}

/// The pods' IPv4 leaves as the node's, except into a pod's TAP, where
/// nothing new over IPv4 goes anyway.
fn postrouting(options: &Options) -> Vec<Rule> {
    let (range, len) = options.ipv4_range;
    vec![[
        ipv4_prefix(IPV4_SADDR, CmpOp::Eq, range, len),
        oifname_prefix(CmpOp::Neq, PODS),
        vec![Expr::Masquerade],
    ]
    .concat()]
}

#[cfg(test)]
mod tests {
    use super::*;
    use feos_utils::nft::{self, MetaKey};
    use network_service::{AddressSet, Family as F, PodPolicy, INTERNET4, INTERNET6};
    use std::net::Ipv6Addr;

    fn a(s: &str) -> Ipv6Addr {
        s.parse().unwrap()
    }

    fn v4(s: &str) -> Ipv4Addr {
        s.parse().unwrap()
    }

    fn range6(first: &str, last: &str) -> AddressRange {
        AddressRange::V6(a(first)..=a(last))
    }

    fn range4(first: &str, last: &str) -> AddressRange {
        AddressRange::V4(v4(first)..=v4(last))
    }

    fn options() -> Options {
        Options {
            dns: IpAddr::V4(v4("169.254.0.53")),
            otlp: IpAddr::V4(v4("169.254.169.1")),
            ipv4_range: (v4("169.254.16.0"), 20),
            mesh: None,
        }
    }

    fn pod(tap: &str, ingress: Vec<Allow>, egress: Vec<Allow>) -> PodPolicy {
        PodPolicy {
            pod_id: format!("{tap}-id"),
            tap: tap.to_string(),
            address: a("fd00:fe05:1::2"),
            ingress,
            egress,
            missing: Vec::new(),
        }
    }

    fn allow(protocol: Protocol, ports: &[(u16, u16)], peer: &str, family: F) -> Allow {
        Allow {
            protocol,
            ports: ports.iter().map(|&(first, last)| first..=last).collect(),
            peer: peer.to_string(),
            family,
        }
    }

    fn set(family: F, ranges: Vec<AddressRange>) -> AddressSet {
        AddressSet { family, ranges }
    }

    fn policy() -> Policy {
        Policy {
            sets: BTreeMap::from([
                (
                    "b-v4".to_string(),
                    set(F::V4, vec![range4("192.0.2.0", "192.0.2.255")]),
                ),
                (
                    DENY4.to_string(),
                    set(F::V4, vec![range4("169.254.0.0", "169.254.255.255")]),
                ),
                (
                    DENY6.to_string(),
                    set(F::V6, vec![range6("fe80::", "febf::")]),
                ),
                (
                    INTERNET4.to_string(),
                    set(F::V4, vec![range4("8.0.0.0", "8.255.255.255")]),
                ),
                (
                    INTERNET6.to_string(),
                    set(F::V6, vec![range6("2000::", "3fff::")]),
                ),
                ("g-web".to_string(), set(F::V6, vec![])),
            ]),
            pods: vec![
                pod(
                    "pod-11111111",
                    vec![
                        allow(Protocol::Tcp, &[(80, 80), (8000, 8100)], "g-web", F::V6),
                        allow(Protocol::Icmpv6, &[], "g-web", F::V6),
                        // As no compiled policy has it.
                        allow(Protocol::Any, &[], "b-v4", F::V4),
                    ],
                    vec![
                        allow(Protocol::Any, &[], INTERNET4, F::V4),
                        allow(Protocol::Any, &[], INTERNET6, F::V6),
                        allow(Protocol::Udp, &[(53, 53)], "b-v4", F::V4),
                        allow(Protocol::Icmp, &[], "b-v4", F::V4),
                    ],
                ),
                pod("pod-22222222", vec![], vec![]),
            ],
        }
    }

    fn chain<'a>(table: &'a Table, name: &str) -> &'a Chain {
        table
            .chains
            .iter()
            .find(|chain| chain.name == name)
            .unwrap_or_else(|| panic!("no chain {name}"))
    }

    fn verdict_of(rule: &Rule) -> Option<&Verdict> {
        match rule.last() {
            Some(Expr::Verdict(verdict)) => Some(verdict),
            _ => None,
        }
    }

    fn logs(rule: &Rule, group: u16, reason: &str) -> bool {
        rule.iter().any(|expr| {
            matches!(expr, Expr::Log { group: g, prefix, .. } if *g == group && prefix == reason)
        })
    }

    /// Whether the rule has these expressions, one after the other.
    fn has(rule: &Rule, exprs: &[Expr]) -> bool {
        rule.windows(exprs.len()).any(|w| w == exprs)
    }

    #[test]
    fn every_pod_has_its_chains_and_map_entries() {
        let table = table(&policy(), &options());
        assert_eq!((table.family, table.name.as_str()), (Family::Inet, "feos"));
        let names: Vec<&str> = table.sets.iter().map(|set| set.name.as_str()).collect();
        assert_eq!(
            names,
            ["b-v4", DENY4, DENY6, "g-web", INTERNET4, INTERNET6, POD_IN, POD_OUT]
        );
        // Every set has its family's keys.
        let keys: Vec<KeyType> = table.sets[..6].iter().map(|set| set.key).collect();
        assert_eq!(
            keys,
            [
                KeyType::Ipv4Addr,
                KeyType::Ipv4Addr,
                KeyType::Ipv6Addr,
                KeyType::Ipv6Addr,
                KeyType::Ipv4Addr,
                KeyType::Ipv6Addr
            ]
        );
        assert_eq!(
            table.sets[1].elements,
            [Element::ipv4_prefix(v4("169.254.0.0"), 16)]
        );
        // An empty set is a set all the same.
        assert!(table.sets[3].elements.is_empty() && table.sets[3].interval);
        let pod_in = &table.sets[6];
        assert!(pod_in.map);
        assert_eq!(
            pod_in.elements,
            [
                Element::ifname("pod-11111111")
                    .with_verdict(Verdict::Goto("in-pod-11111111".to_string())),
                Element::ifname("pod-22222222")
                    .with_verdict(Verdict::Goto("in-pod-22222222".to_string())),
            ]
        );
        let names: Vec<&str> = table.chains.iter().map(|c| c.name.as_str()).collect();
        assert_eq!(
            names,
            [
                "in-pod-11111111",
                "out-pod-11111111",
                "in-pod-22222222",
                "out-pod-22222222",
                "from-pods",
                "to-pods",
                "prerouting",
                "input",
                "forward",
                "postrouting"
            ]
        );
        for chain in &table.chains[..6] {
            assert!(chain.base.is_none(), "{} is a regular chain", chain.name);
        }
    }

    #[test]
    fn a_pod_sends_both_families_and_receives_ipv6() {
        let table = table(&policy(), &options());
        let egress = &chain(&table, "out-pod-11111111").rules;
        // Denied prefixes first, of both families, logged, then dropped.
        for (rules, denied) in [
            (&egress[0..2], ct_original_ipv6_daddr_in(DENY6)),
            (&egress[2..4], ct_original_ipv4_daddr_in(DENY4)),
        ] {
            assert!(logs(&rules[0], DROP_GROUP, "deny"));
            assert!(has(&rules[0], &denied) && has(&rules[1], &denied));
            assert_eq!(verdict_of(&rules[1]), Some(&Verdict::Drop));
        }
        // The rules of both families, which return: an accept would end
        // the base chain before the other pod's chain.
        assert_eq!(
            egress[4..8],
            [
                [
                    ct_original_ipv4_daddr_in(INTERNET4),
                    verdict(Verdict::Return)
                ]
                .concat(),
                [
                    ct_original_ipv6_daddr_in(INTERNET6),
                    verdict(Verdict::Return)
                ]
                .concat(),
                [
                    l4proto(UDP),
                    th_dport(CmpOp::Eq, 53),
                    ct_original_ipv4_daddr_in("b-v4"),
                    verdict(Verdict::Return)
                ]
                .concat(),
                [
                    l4proto(ICMP),
                    ct_original_ipv4_daddr_in("b-v4"),
                    verdict(Verdict::Return)
                ]
                .concat(),
            ]
        );
        assert!(logs(&egress[8], DROP_GROUP, "no-rule"));
        assert_eq!(egress[9], verdict(Verdict::Drop));
        assert_eq!(egress.len(), 10);

        let ingress = &chain(&table, "in-pod-11111111").rules;
        // IPv6 alone: denied sources, a rule per port range, ICMPv6.
        assert!(logs(&ingress[0], DROP_GROUP, "deny"));
        assert!(has(&ingress[1], &ipv6_saddr_in(DENY6)));
        assert_eq!(verdict_of(&ingress[1]), Some(&Verdict::Drop));
        let allowed = &ingress[2..5];
        for rule in allowed {
            assert_eq!(verdict_of(rule), Some(&Verdict::Return));
            assert!(has(rule, &ipv6_saddr_in("g-web")), "the source is the peer");
        }
        assert_eq!(&allowed[0][..2], &l4proto(TCP)[..]);
        assert!(has(&allowed[1], &th_dport_range(8000, 8100)));
        assert_eq!(&allowed[2][..2], &l4proto(ICMPV6)[..]);
        assert!(logs(&ingress[5], DROP_GROUP, "no-rule"));
        assert_eq!(verdict_of(&ingress[6]), Some(&Verdict::Drop));
        assert_eq!(ingress.len(), 7, "nothing new comes in over IPv4");
        assert!(!ingress.iter().any(|rule| has(rule, &ipv4())));

        // Allowed nothing: no return at all.
        let nothing = &chain(&table, "out-pod-22222222").rules;
        assert!(nothing
            .iter()
            .all(|rule| verdict_of(rule) != Some(&Verdict::Return)));
        assert_eq!(verdict_of(nothing.last().unwrap()), Some(&Verdict::Drop));
    }

    #[test]
    fn forward_copies_first_and_goes_through_the_maps() {
        let table = table(&policy(), &options());
        let forward = chain(&table, "forward");
        let base = forward.base.as_ref().unwrap();
        assert_eq!(
            (base.hook, base.policy),
            (Hook::Forward, ChainPolicy::Accept)
        );
        let rules = &forward.rules;
        // The copy is not terminal, of both families, and comes before
        // established packets pass.
        assert!(logs(&rules[0], COPY_GROUP, "copy"));
        assert_eq!(verdict_of(&rules[0]), None);
        assert!(!has(&rules[0], &[Expr::Meta(MetaKey::NfProto)]));
        assert!(has(&rules[0], &ct_direction(Direction::Original)));
        assert!(rules[0].contains(&Expr::Log {
            group: COPY_GROUP,
            prefix: "copy".to_string(),
            snaplen: COPY_SNAPLEN
        }));
        assert_eq!(
            rules[1],
            [
                ct_state(ct::ESTABLISHED | ct::RELATED),
                verdict(Verdict::Accept)
            ]
            .concat()
        );
        assert_eq!(rules.len(), 7);
        // The pods' chains check destinations by the connection: what has
        // none from a pod is dropped before them.
        assert_eq!(
            rules[4],
            [from_pods(), ct_state(ct::UNTRACKED), verdict(Verdict::Drop)].concat()
        );
        assert_eq!(
            rules[5],
            [from_pods(), verdict(Verdict::Jump(FROM_PODS.to_string()))].concat()
        );
        assert_eq!(
            rules[6],
            [to_pods(), verdict(Verdict::Jump(TO_PODS.to_string()))].concat()
        );
        // A TAP the maps do not have is dropped, not passed.
        for (name, vmap) in [
            (FROM_PODS, iifname_vmap(POD_OUT)),
            (TO_PODS, oifname_vmap(POD_IN)),
        ] {
            let lookup = &chain(&table, name).rules;
            assert_eq!(lookup.len(), 3);
            assert_eq!(lookup[0], vmap);
            assert!(logs(&lookup[1], DROP_GROUP, "no-rule"));
            assert_eq!(lookup[2], verdict(Verdict::Drop));
        }
    }

    #[test]
    fn prerouting_pins_pods_to_their_addresses() {
        let table = table(&policy(), &options());
        let prerouting = chain(&table, "prerouting");
        assert_eq!(prerouting.base.as_ref().unwrap().priority, -150);
        let rules = &prerouting.rules;
        // Of either family: fib looks the source up in its own.
        assert!(logs(&rules[0], DROP_GROUP, "spoofed"));
        for rule in &rules[..2] {
            assert!(has(rule, &fib_saddr_iif_oif_missing()));
            assert!(!has(rule, &[Expr::Meta(MetaKey::NfProto)]));
        }
        assert_eq!(verdict_of(&rules[1]), Some(&Verdict::Drop));
        // Router advertisements and redirects of both families.
        for (rule, icmp) in rules[2..].iter().zip([
            icmpv6_type(ND_ROUTER_ADVERT),
            icmpv6_type(ND_REDIRECT),
            icmp_type(ICMP_REDIRECT),
        ]) {
            assert_eq!(rule, &[from_pods(), icmp, verdict(Verdict::Drop)].concat());
        }
        assert_eq!(rules.len(), 5, "IPv4 of pods passes");

        // On a mesh, the cluster's addresses come through the device they
        // are routed to, whichever it is.
        let mesh = Options {
            mesh: Some((a("fd5e:1b2c:93a0::"), 48)),
            ..options()
        };
        let table = super::table(&policy(), &mesh);
        let rules = &chain(&table, "prerouting").rules;
        assert_eq!(rules.len(), 7);
        let cluster = ipv6_prefix(IPV6_SADDR, CmpOp::Eq, a("fd5e:1b2c:93a0::"), 48);
        assert!(logs(&rules[5], DROP_GROUP, "spoofed"));
        assert_eq!(
            rules[6],
            [cluster, fib_saddr_iif_oif_missing(), verdict(Verdict::Drop)].concat()
        );
    }

    #[test]
    fn input_takes_neighbor_discovery_dns_otlp_and_http_bindings_from_pods() {
        let table = table(&policy(), &options());
        let rules = &chain(&table, "input").rules;
        assert_eq!(
            rules[..2],
            [
                [
                    from_pods(),
                    icmpv6_type(ND_NEIGHBOR_SOLICIT),
                    verdict(Verdict::Accept)
                ]
                .concat(),
                [
                    from_pods(),
                    icmpv6_type(ND_NEIGHBOR_ADVERT),
                    verdict(Verdict::Accept)
                ]
                .concat(),
            ]
        );
        let resolver = ipv4_daddr(CmpOp::Eq, v4("169.254.0.53"));
        for (rule, protocol) in rules[2..4].iter().zip([UDP, TCP]) {
            assert_eq!(
                rule,
                &[
                    from_pods(),
                    resolver.clone(),
                    l4proto(protocol),
                    th_dport(CmpOp::Eq, 53),
                    verdict(Verdict::Accept)
                ]
                .concat()
            );
        }
        // OTLP, gRPC and HTTP, over TCP alone.
        assert_eq!(
            rules[4],
            [
                from_pods(),
                ipv4_daddr(CmpOp::Eq, v4("169.254.169.1")),
                l4proto(TCP),
                th_dport(CmpOp::Gte, 4317),
                th_dport(CmpOp::Lte, 4318),
                verdict(Verdict::Accept)
            ]
            .concat()
        );
        // The HTTP bindings, 80 and 443, over TCP alone.
        for (rule, port) in rules[5..7].iter().zip([80, 443]) {
            assert_eq!(
                rule,
                &[
                    from_pods(),
                    ipv4_daddr(CmpOp::Eq, v4("169.254.169.1")),
                    l4proto(TCP),
                    th_dport(CmpOp::Eq, port),
                    verdict(Verdict::Accept)
                ]
                .concat()
            );
        }
        assert_eq!(rules[7], [from_pods(), verdict(Verdict::Drop)].concat());
        assert_eq!(rules.len(), 8);
        // An IPv6 resolver is matched as such.
        let ipv6 = Options {
            dns: IpAddr::V6(a("fd00::53")),
            ..options()
        };
        let table = super::table(&policy(), &ipv6);
        assert!(has(
            &chain(&table, "input").rules[2],
            &ipv6_daddr(CmpOp::Eq, a("fd00::53"))
        ));
    }

    #[test]
    fn the_pods_ipv4_is_masqueraded() {
        let table = table(&policy(), &options());
        let postrouting = chain(&table, "postrouting");
        let base = postrouting.base.as_ref().unwrap();
        assert_eq!(
            (base.kind, base.hook, base.priority),
            ("nat", Hook::Postrouting, 100)
        );
        assert_eq!(
            postrouting.rules,
            [[
                ipv4_prefix(IPV4_SADDR, CmpOp::Eq, v4("169.254.16.0"), 20),
                oifname_prefix(CmpOp::Neq, "pod-"),
                vec![Expr::Masquerade],
            ]
            .concat()]
        );
        // Another range is another prefix.
        let other = Options {
            ipv4_range: (v4("10.77.0.0"), 16),
            ..options()
        };
        let table = super::table(&policy(), &other);
        assert!(has(
            &chain(&table, "postrouting").rules[0],
            &ipv4_prefix(IPV4_SADDR, CmpOp::Eq, v4("10.77.0.0"), 16)
        ));
    }

    #[test]
    fn deltas_remove_before_they_add() {
        let changes = BTreeMap::from([
            (
                "g-a".to_string(),
                SetChange {
                    remove: vec![],
                    add: vec![range6("fd00::1", "fd00::1")],
                },
            ),
            (
                "g-b".to_string(),
                SetChange {
                    remove: vec![range6("fd00::2", "fd00::3")],
                    add: vec![range6("fd00::2", "fd00::4")],
                },
            ),
            (
                DENY4.to_string(),
                SetChange {
                    remove: vec![range4("192.0.2.7", "192.0.2.7")],
                    add: vec![range4("192.0.2.8", "192.0.2.9")],
                },
            ),
        ]);
        let ops = element_ops(&changes);
        assert_eq!(
            ops,
            [
                ElementOp::Delete {
                    set: DENY4.to_string(),
                    elements: vec![Element::ipv4(v4("192.0.2.7"))]
                },
                ElementOp::Delete {
                    set: "g-b".to_string(),
                    elements: vec![Element::ipv6_range(a("fd00::2"), a("fd00::3"))]
                },
                ElementOp::Add {
                    set: DENY4.to_string(),
                    elements: vec![Element::ipv4_range(v4("192.0.2.8"), v4("192.0.2.9"))]
                },
                ElementOp::Add {
                    set: "g-a".to_string(),
                    elements: vec![Element::ipv6(a("fd00::1"))]
                },
                ElementOp::Add {
                    set: "g-b".to_string(),
                    elements: vec![Element::ipv6_range(a("fd00::2"), a("fd00::4"))]
                },
            ]
        );
    }

    /// Whether this process has a network namespace of its own: no device
    /// but loopback.
    fn alone() -> bool {
        std::fs::read_to_string("/proc/self/net/dev").is_ok_and(|dev| {
            dev.lines()
                .skip(2)
                .all(|line| line.trim_start().starts_with("lo:"))
        })
    }

    /// nft's answer to `args`, if nft is in `$NFT`.
    fn nft_says(args: &[&str]) -> Option<String> {
        let program = std::env::var("NFT").ok()?;
        let output = std::process::Command::new(program)
            .args(args)
            .output()
            .expect("nft in $NFT");
        assert!(output.status.success(), "nft {args:?}: {output:?}");
        Some(String::from_utf8_lossy(&output.stdout).into_owned())
    }

    /// The table in the kernel holds what was written: its chains, sets,
    /// and every chain's rules.
    fn assert_written(table: &Table) {
        let sorted = |mut names: Vec<String>| {
            names.sort();
            names
        };
        let chains = nft::chains(Family::Inet, &table.name).unwrap();
        let written: Vec<String> = table.chains.iter().map(|c| c.name.clone()).collect();
        assert_eq!(sorted(chains), sorted(written));
        let sets = nft::sets(Family::Inet, &table.name).unwrap();
        let written: Vec<String> = table.sets.iter().map(|s| s.name.clone()).collect();
        assert_eq!(sorted(sets), sorted(written));
        let rules = nft::rules(Family::Inet, &table.name).unwrap();
        for chain in &table.chains {
            let n = rules.iter().filter(|name| **name == chain.name).count();
            assert_eq!(n, chain.rules.len(), "the rules of {}", chain.name);
        }
    }

    /// Writes the table of two pods, changes elements of sets of both
    /// families, and replaces it when a pod goes, as feosd does. Needs
    /// CAP_NET_ADMIN, in a network namespace of its own: `sudo unshare -n`
    /// or a container. With nft in `$NFT`, it checks nft's listing too,
    /// which `--nocapture` prints.
    #[test]
    #[ignore]
    fn the_kernel_takes_the_table() {
        use feos_proto::network_service::{self as proto, rule, AddressBlock, PortRange};
        use network_service::{compile, delta, Delta, Pod, Prefix};

        assert!(alone(), "run this in a network namespace of its own");
        let name = "feos-test-rules";
        let block = |cidr: &str, except: &[&str]| {
            Some(rule::Peer::Block(AddressBlock {
                cidr: cidr.to_string(),
                except: except.iter().map(|e| e.to_string()).collect(),
            }))
        };
        let group = |id: &str| Some(rule::Peer::Group(id.to_string()));
        let rule = |direction: proto::Direction, protocol: proto::Protocol, ports: &[u32], peer| {
            proto::Rule {
                direction: direction as i32,
                protocol: protocol as i32,
                ports: ports
                    .iter()
                    .map(|&first| PortRange { first, last: 0 })
                    .collect(),
                peer,
            }
        };
        let (into, out) = (proto::Direction::Ingress, proto::Direction::Egress);
        let groups = |lb: &[&str]| {
            [
                proto::SecurityGroup {
                    id: "web".to_string(),
                    version: 1,
                    rules: vec![
                        rule(into, proto::Protocol::Tcp, &[80, 443], group("lb")),
                        rule(into, proto::Protocol::Icmpv6, &[], block("::/0", &[])),
                        rule(
                            out,
                            proto::Protocol::Tcp,
                            &[443],
                            block("0.0.0.0/0", &["10.0.0.0/8"]),
                        ),
                        rule(out, proto::Protocol::Icmp, &[], block("0.0.0.0/0", &[])),
                        rule(
                            out,
                            proto::Protocol::Udp,
                            &[53],
                            block("2001:4860:4860::8888/128", &[]),
                        ),
                        rule(out, proto::Protocol::Any, &[], group("db")),
                    ],
                    members: vec![],
                },
                proto::SecurityGroup {
                    id: "lb".to_string(),
                    version: 1,
                    rules: vec![],
                    members: lb.iter().map(|m| m.to_string()).collect(),
                },
            ]
            .into_iter()
            .map(|group| (group.id.clone(), group))
            .collect::<BTreeMap<_, _>>()
        };
        let pods = [
            Pod {
                pod_id: "11111111-0000-0000-0000-000000000000".to_string(),
                tap: "pod-11111111".to_string(),
                address: a("fd00:fe05:1::2"),
                groups: vec!["web".to_string()],
            },
            Pod {
                pod_id: "22222222-0000-0000-0000-000000000000".to_string(),
                tap: "pod-22222222".to_string(),
                address: a("fd00:fe05:1::3"),
                groups: vec![],
            },
        ];
        let denied = |node: &str| -> Vec<Prefix> {
            ["169.254.169.254/32", node, "2001:db8::7/128"]
                .iter()
                .map(|p| p.parse().unwrap())
                .collect()
        };
        let lb = ["fd00:fe05:2::5", "fd00:fe05:2::6"];

        // Written twice: the second replaces the first.
        let nat = compile(&groups(&lb), &pods, &denied("192.0.2.7/32"));
        let mut table = table(&nat, &options());
        table.name = name.to_string();
        nft::replace(&table).unwrap();
        nft::replace(&table).unwrap();
        assert_written(&table);
        if let Some(listed) = nft_says(&["list", "table", "inet", name]) {
            println!("{listed}");
            let words = listed.split_whitespace().collect::<Vec<_>>().join(" ");
            for line in [
                "ip6 daddr @deny6 drop",
                "ip daddr @deny4 drop",
                "ip daddr @internet4 return",
                "ip6 daddr @internet6 return",
                "ip6 saddr @deny6 drop",
                "tcp dport 443 ip6 saddr @g-lb return",
                "meta l4proto 1 ip daddr @b-",
                "meta l4proto 58 ip6 saddr @b-",
                "iifname \"pod-*\" fib saddr . iif oif missing drop",
                "iifname \"pod-*\" icmp type redirect drop",
                "iifname \"pod-*\" ip daddr 169.254.0.53 udp dport 53 accept",
                "iifname \"pod-*\" ip daddr 169.254.0.53 tcp dport 53 accept",
                "iifname \"pod-*\" ip daddr 169.254.169.1 tcp dport 4317-4318 accept",
                "iifname \"pod-*\" ip daddr 169.254.169.1 tcp dport 80 accept",
                "iifname \"pod-*\" ip daddr 169.254.169.1 tcp dport 443 accept",
                "ip saddr 169.254.16.0/20 oifname != \"pod-*\" masquerade",
                "elements = { 0.0.0.0/8, 127.0.0.0/8, 169.254.0.0/16, 192.0.2.7, 224.0.0.0/4 }",
            ] {
                assert!(words.contains(line), "no {line:?}");
            }
        }

        // A member joins its neighbours and the node's address changes:
        // elements of sets of both families, and back.
        let next = compile(
            &groups(&[lb[0], lb[1], "fd00:fe05:2::7"]),
            &pods,
            &denied("192.0.2.9/32"),
        );
        let Delta::Elements(changes) = delta(&nat, &next) else {
            panic!("the table was replaced");
        };
        assert_eq!(changes.keys().collect::<Vec<_>>(), [DENY4, "g-lb"]);
        nft::update(&table, &element_ops(&changes)).unwrap();
        if let Some(found) =
            nft_says(&["get", "element", "inet", name, "g-lb", "{ fd00:fe05:2::7 }"])
        {
            assert!(found.contains("fd00:fe05:2::5-fd00:fe05:2::7"), "{found}");
            nft_says(&["get", "element", "inet", name, DENY4, "{ 192.0.2.9 }"]);
        }
        let Delta::Elements(back) = delta(&next, &nat) else {
            panic!("the table was replaced");
        };
        nft::update(&table, &element_ops(&back)).unwrap();
        nft_says(&["get", "element", "inet", name, DENY4, "{ 192.0.2.7 }"]);

        // A pod goes: the table is replaced, and its chains go with it.
        let one = compile(&groups(&lb), &pods[1..], &denied("192.0.2.7/32"));
        assert_eq!(delta(&nat, &one), Delta::Replace);
        let mut table = super::table(&one, &options());
        table.name = name.to_string();
        nft::replace(&table).unwrap();
        assert_written(&table);
        let chains = nft::chains(Family::Inet, name).unwrap();
        assert!(!chains.contains(&"out-pod-11111111".to_string()));

        nft::delete(Family::Inet, name).unwrap();
        assert!(nft::chains(Family::Inet, name).unwrap().is_empty());
    }
}
