// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! nftables over netlink: tables, chains, sets, verdict maps and the
//! expressions FeOS's rules need, sent as one batch, which the kernel
//! applies atomically or not at all. No `nft` binary and no libnftnl.
//!
//! A [`Table`] is replaced whole: the batch adds it (a no-op if it
//! exists), deletes it with everything in it, and adds it again with its
//! chains, sets and rules. [`update`] adds and deletes elements of its
//! sets and leaves everything else as it is. A timed set's elements carry
//! a timeout, after which the kernel removes them; adding one that is
//! there sets its timeout anew.

use std::collections::HashMap;
use std::io;
use std::net::{Ipv4Addr, Ipv6Addr};
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};
use std::time::Duration;

const NETLINK_NETFILTER: libc::c_int = 12;
const NETLINK_CAP_ACK: libc::c_int = 10;
const NFNL_SUBSYS_NFTABLES: u16 = 10;
const NFNL_MSG_BATCH_BEGIN: u16 = 0x10;
const NFNL_MSG_BATCH_END: u16 = 0x11;

const NFT_MSG_NEWTABLE: u16 = 0;
const NFT_MSG_DELTABLE: u16 = 2;
const NFT_MSG_NEWCHAIN: u16 = 3;
const NFT_MSG_GETCHAIN: u16 = 4;
const NFT_MSG_NEWRULE: u16 = 6;
const NFT_MSG_GETRULE: u16 = 7;
const NFT_MSG_NEWSET: u16 = 9;
const NFT_MSG_GETSET: u16 = 10;
const NFT_MSG_NEWSETELEM: u16 = 12;
const NFT_MSG_DELSETELEM: u16 = 14;

const NLM_F_REQUEST: u16 = 0x1;
const NLM_F_ACK: u16 = 0x4;
const NLM_F_DUMP: u16 = 0x300;
const NLM_F_CREATE: u16 = 0x400;
const NLM_F_APPEND: u16 = 0x800;
const NLMSG_ERROR: u16 = 0x2;
const NLMSG_DONE: u16 = 0x3;
const NLA_F_NESTED: u16 = 0x8000;

const NFTA_TABLE_NAME: u16 = 1;
const NFTA_CHAIN_TABLE: u16 = 1;
const NFTA_CHAIN_NAME: u16 = 3;
const NFTA_CHAIN_HOOK: u16 = 4;
const NFTA_CHAIN_POLICY: u16 = 5;
const NFTA_CHAIN_TYPE: u16 = 7;
const NFTA_HOOK_HOOKNUM: u16 = 1;
const NFTA_HOOK_PRIORITY: u16 = 2;
const NFTA_RULE_TABLE: u16 = 1;
const NFTA_RULE_CHAIN: u16 = 2;
const NFTA_RULE_EXPRESSIONS: u16 = 4;
const NFTA_SET_TABLE: u16 = 1;
const NFTA_SET_NAME: u16 = 2;
const NFTA_SET_FLAGS: u16 = 3;
const NFTA_SET_KEY_TYPE: u16 = 4;
const NFTA_SET_KEY_LEN: u16 = 5;
const NFTA_SET_DATA_TYPE: u16 = 6;
const NFTA_SET_ID: u16 = 10;
const NFTA_SET_USERDATA: u16 = 13;
const NFTA_SET_ELEM_LIST_TABLE: u16 = 1;
const NFTA_SET_ELEM_LIST_SET: u16 = 2;
const NFTA_SET_ELEM_LIST_ELEMENTS: u16 = 3;
const NFTA_SET_ELEM_LIST_SET_ID: u16 = 4;
const NFTA_SET_ELEM_KEY: u16 = 1;
const NFTA_SET_ELEM_DATA: u16 = 2;
const NFTA_SET_ELEM_FLAGS: u16 = 3;
const NFTA_SET_ELEM_TIMEOUT: u16 = 4;
const NFTA_SET_ELEM_EXPIRATION: u16 = 5;
const NFTA_SET_ELEM_USERDATA: u16 = 6;
const NFTA_LIST_ELEM: u16 = 1;
const NFTA_EXPR_NAME: u16 = 1;
const NFTA_EXPR_DATA: u16 = 2;
const NFTA_DATA_VALUE: u16 = 1;
const NFTA_DATA_VERDICT: u16 = 2;
const NFTA_VERDICT_CODE: u16 = 1;
const NFTA_VERDICT_CHAIN: u16 = 2;

const NFT_SET_INTERVAL: u32 = 0x4;
const NFT_SET_MAP: u32 = 0x8;
const NFT_SET_TIMEOUT: u32 = 0x10;
const NFT_SET_ELEM_INTERVAL_END: u32 = 0x1;
const NFT_DATA_VERDICT: u32 = 0xffff_ff00;
/// nft's user data, which `nft list` reads: the byte order of a set's
/// keys (NFTNL_UDATA_SET_KEYBYTEORDER), and the flags of an element
/// (NFTNL_UDATA_SET_ELEM_FLAGS) whose range runs to the last key
/// (NFTNL_SET_ELEM_F_INTERVAL_OPEN).
const UDATA_SET_KEYBYTEORDER: u8 = 0;
const UDATA_SET_ELEM_FLAGS: u8 = 1;
const SET_ELEM_F_INTERVAL_OPEN: u32 = 1;

const NFT_REG_VERDICT: u32 = 0;
const NFT_REG_1: u32 = 1;
const NF_DROP: u32 = 0;
const NF_ACCEPT: u32 = 1;
const NFT_JUMP: u32 = -3i32 as u32;
const NFT_GOTO: u32 = -4i32 as u32;
const NFT_RETURN: u32 = -5i32 as u32;

/// The bytes of elements per message: their attribute's length is 16
/// bits.
const ELEMENTS_PER_MESSAGE: usize = 60_000;

/// nftables families.
#[derive(Clone, Copy, Debug, PartialEq)]
pub enum Family {
    Inet = 1,
    Ip = 2,
    Bridge = 7,
}

/// The netfilter hooks of the families FeOS uses: the inet ones, which
/// the bridge family numbers the same.
#[derive(Clone, Copy, Debug, PartialEq)]
pub enum Hook {
    Prerouting = 0,
    Input = 1,
    Forward = 2,
    Output = 3,
    Postrouting = 4,
}

#[derive(Clone, Debug, PartialEq)]
pub struct Table {
    pub family: Family,
    pub name: String,
    pub sets: Vec<Set>,
    pub chains: Vec<Chain>,
}

/// A chain: a base chain on a hook, or a regular chain, which only jumps
/// and gotos reach.
#[derive(Clone, Debug, PartialEq)]
pub struct Chain {
    pub name: String,
    pub base: Option<BaseChain>,
    pub rules: Vec<Rule>,
}

/// Where a base chain hooks in, and what becomes of a packet that no
/// rule decided on.
#[derive(Clone, Debug, PartialEq)]
pub struct BaseChain {
    /// "filter" or "nat".
    pub kind: &'static str,
    pub hook: Hook,
    pub priority: i32,
    pub policy: Policy,
}

#[derive(Clone, Copy, Debug, PartialEq)]
pub enum Policy {
    Drop = 0,
    Accept = 1,
}

impl Chain {
    pub fn base(
        name: &str,
        kind: &'static str,
        hook: Hook,
        priority: i32,
        policy: Policy,
        rules: Vec<Rule>,
    ) -> Self {
        Self {
            name: name.to_string(),
            base: Some(BaseChain {
                kind,
                hook,
                priority,
                policy,
            }),
            rules,
        }
    }

    pub fn regular(name: &str, rules: Vec<Rule>) -> Self {
        Self {
            name: name.to_string(),
            base: None,
            rules,
        }
    }
}

/// A named set of keys or, with `interval`, of ranges of keys; with
/// `map`, a verdict map: each element has a verdict; with `timeout`, a
/// set of keys that each have a timeout.
#[derive(Clone, Debug, PartialEq)]
pub struct Set {
    pub name: String,
    pub key: KeyType,
    pub interval: bool,
    pub map: bool,
    pub timeout: bool,
    pub elements: Vec<Element>,
}

impl Set {
    pub fn keys(name: &str, key: KeyType, elements: Vec<Element>) -> Self {
        Self {
            name: name.to_string(),
            key,
            interval: false,
            map: false,
            timeout: false,
            elements,
        }
    }

    pub fn intervals(name: &str, key: KeyType, elements: Vec<Element>) -> Self {
        Self {
            interval: true,
            ..Self::keys(name, key, elements)
        }
    }

    pub fn verdict_map(name: &str, key: KeyType, elements: Vec<Element>) -> Self {
        Self {
            map: true,
            ..Self::keys(name, key, elements)
        }
    }

    /// A set of keys whose elements each have a timeout
    /// ([`Element::with_timeout`]).
    pub fn timed(name: &str, key: KeyType, elements: Vec<Element>) -> Self {
        Self {
            timeout: true,
            ..Self::keys(name, key, elements)
        }
    }
}

/// The type of a set's keys.
#[derive(Clone, Copy, Debug, PartialEq)]
pub enum KeyType {
    Ipv4Addr,
    Ipv6Addr,
    /// An interface name: IFNAMSIZ bytes, zero padded.
    IfName,
    /// A port, big endian.
    InetService,
}

impl KeyType {
    /// nft's number for the type, which `nft list` reads back.
    fn id(self) -> u32 {
        match self {
            Self::Ipv4Addr => 7,
            Self::Ipv6Addr => 8,
            Self::InetService => 13,
            Self::IfName => 41,
        }
    }

    /// nft's byte order of the type: host for names, big endian else.
    fn byteorder(self) -> u32 {
        match self {
            Self::IfName => 1,
            _ => 2,
        }
    }

    fn len(self) -> usize {
        match self {
            Self::Ipv4Addr => 4,
            Self::Ipv6Addr => 16,
            Self::IfName => libc::IFNAMSIZ,
            Self::InetService => 2,
        }
    }
}

/// An element of a set: a key, or in an interval set the keys from `key`
/// to `last`; in a verdict map, with its verdict; in a timed set, with
/// the time it stays.
#[derive(Clone, Debug, PartialEq)]
pub struct Element {
    pub key: Vec<u8>,
    pub last: Option<Vec<u8>>,
    pub verdict: Option<Verdict>,
    pub timeout: Option<Duration>,
}

impl Element {
    pub fn key(key: Vec<u8>) -> Self {
        Self {
            key,
            last: None,
            verdict: None,
            timeout: None,
        }
    }

    /// The keys from `first` to `last`, both included.
    pub fn range(first: Vec<u8>, last: Vec<u8>) -> Self {
        Self {
            last: (first != last).then_some(last),
            ..Self::key(first)
        }
    }

    pub fn ipv4(address: Ipv4Addr) -> Self {
        Self::key(address.octets().to_vec())
    }

    pub fn ipv4_prefix(address: Ipv4Addr, len: u8) -> Self {
        let mask = u32::MAX
            .checked_shl(32 - u32::from(len.min(32)))
            .unwrap_or(0);
        let first = u32::from(address) & mask;
        Self::range(
            first.to_be_bytes().to_vec(),
            (first | !mask).to_be_bytes().to_vec(),
        )
    }

    pub fn ipv4_range(first: Ipv4Addr, last: Ipv4Addr) -> Self {
        Self::range(first.octets().to_vec(), last.octets().to_vec())
    }

    pub fn ipv6(address: Ipv6Addr) -> Self {
        Self::key(address.octets().to_vec())
    }

    pub fn ipv6_prefix(address: Ipv6Addr, len: u8) -> Self {
        let mask = u128::MAX
            .checked_shl(128 - u32::from(len.min(128)))
            .unwrap_or(0);
        let first = u128::from(address) & mask;
        Self::range(
            first.to_be_bytes().to_vec(),
            (first | !mask).to_be_bytes().to_vec(),
        )
    }

    pub fn ipv6_range(first: Ipv6Addr, last: Ipv6Addr) -> Self {
        Self::range(first.octets().to_vec(), last.octets().to_vec())
    }

    pub fn ifname(name: &str) -> Self {
        Self::key(matches::ifname(name))
    }

    pub fn port(port: u16) -> Self {
        Self::key(port.to_be_bytes().to_vec())
    }

    /// The element with a verdict, for a verdict map.
    pub fn with_verdict(self, verdict: Verdict) -> Self {
        Self {
            verdict: Some(verdict),
            ..self
        }
    }

    /// The element for `timeout` from now, for a timed set: at least a
    /// millisecond, the kernel's unit.
    pub fn with_timeout(self, timeout: Duration) -> Self {
        Self {
            timeout: Some(timeout),
            ..self
        }
    }
}

#[derive(Clone, Debug, PartialEq)]
pub enum Verdict {
    Accept,
    Drop,
    /// Back to the rule after the jump.
    Return,
    /// To a regular chain, and back at its end.
    Jump(String),
    /// To a regular chain, for good.
    Goto(String),
}

/// Changes of a set's elements in the kernel.
#[derive(Clone, Debug, PartialEq)]
pub enum ElementOp {
    Add {
        set: String,
        elements: Vec<Element>,
    },
    /// The elements as they were added: a range, the same range.
    Delete {
        set: String,
        elements: Vec<Element>,
    },
    /// Every element of the set.
    Flush {
        set: String,
    },
}

/// Expressions, evaluated in order; a rule ends with a verdict or masq.
pub type Rule = Vec<Expr>;

/// Where a payload is read from.
#[derive(Clone, Copy, Debug, PartialEq)]
pub enum Base {
    LinkLayer = 0,
    Network = 1,
    /// The header after IPv6's extension headers, in the inet family.
    Transport = 2,
}

/// Comparisons of bytes, in order: big endian for the less and greater
/// ones.
#[derive(Clone, Copy, Debug, PartialEq)]
pub enum CmpOp {
    Eq = 0,
    Neq = 1,
    Lt = 2,
    Lte = 3,
    Gt = 4,
    Gte = 5,
}

/// Keys of the meta expression.
#[derive(Clone, Copy, Debug, PartialEq)]
pub enum MetaKey {
    Protocol = 1,
    Iif = 4,
    Oif = 5,
    IifName = 6,
    OifName = 7,
    NfProto = 15,
    L4Proto = 16,
}

/// Keys of the ct expression.
#[derive(Clone, Copy, Debug, PartialEq)]
pub enum CtKey {
    /// The bits of [`ct`], host byte order.
    State = 0,
    /// The packet's [`Direction`], a byte.
    Direction = 1,
    Status = 2,
    Mark = 3,
    /// Packets, a u64 in host byte order; with accounting, which the
    /// kernel turns on for this key.
    Packets = 14,
    Bytes = 15,
    /// A direction's destination address, IPv4: 4 bytes, network order.
    DstIp = 20,
    /// A direction's destination address, IPv6: 16 bytes, network order.
    DstIp6 = 22,
}

/// A connection's direction.
#[derive(Clone, Copy, Debug, PartialEq)]
pub enum Direction {
    Original = 0,
    Reply = 1,
}

/// The bits of `ct state`.
pub mod ct {
    pub const INVALID: u32 = 1;
    pub const ESTABLISHED: u32 = 2;
    pub const RELATED: u32 = 4;
    pub const NEW: u32 = 8;
    pub const UNTRACKED: u32 = 64;
}

/// The flags of fib: which address it looks up, and on which interface.
pub mod fib {
    pub const SADDR: u32 = 1;
    pub const DADDR: u32 = 2;
    pub const MARK: u32 = 4;
    pub const IIF: u32 = 8;
    pub const OIF: u32 = 16;
    /// Whether there is a route, rather than its interface.
    pub const PRESENT: u32 = 32;
}

/// What fib loads.
#[derive(Clone, Copy, Debug, PartialEq)]
pub enum FibResult {
    Oif = 1,
    OifName = 2,
    AddrType = 3,
}

/// The unit of a limit's rate.
#[derive(Clone, Copy, Debug, PartialEq)]
pub enum Per {
    Second,
    Minute,
    Hour,
    Day,
}

impl Per {
    fn seconds(self) -> u64 {
        match self {
            Self::Second => 1,
            Self::Minute => 60,
            Self::Hour => 3600,
            Self::Day => 86400,
        }
    }
}

#[derive(Clone, Debug, PartialEq)]
pub enum Expr {
    /// Loads a meta key into register 1.
    Meta(MetaKey),
    /// Loads `len` bytes at `offset` of a header into register 1.
    Payload {
        base: Base,
        offset: u32,
        len: u32,
    },
    /// Register 1 &= mask (^ 0).
    Bitwise {
        mask: Vec<u8>,
    },
    /// Compares register 1 with data.
    Cmp {
        op: CmpOp,
        data: Vec<u8>,
    },
    /// Loads a key of the packet's connection into register 1: of one
    /// direction, or of both.
    Ct {
        key: CtKey,
        direction: Option<Direction>,
    },
    /// Turns `len` bytes of register 1, `size` at a time, from host to
    /// network byte order.
    Hton {
        len: u32,
        size: u32,
    },
    /// Looks up a route as the [`fib`] flags say, into register 1.
    Fib {
        flags: u32,
        result: FibResult,
    },
    /// Whether register 1 is in a set, or with `invert` is not.
    Lookup {
        set: String,
        invert: bool,
    },
    /// The verdict of a verdict map for register 1; none if it has no
    /// element for it.
    Vmap {
        map: String,
    },
    /// Sends the packet to nflog group `group`, `snaplen` bytes of it, or
    /// as many as the listener asks for if 0.
    Log {
        group: u16,
        prefix: String,
        snaplen: u32,
    },
    /// Matches up to `rate` packets per unit, and bursts of `burst`.
    Limit {
        rate: u64,
        per: Per,
        burst: u32,
    },
    Counter,
    Masquerade,
    Verdict(Verdict),
}

/// Matches of the rules FeOS writes, as expressions.
pub mod matches {
    use super::*;

    pub const IPV4_SADDR: u32 = 12;
    pub const IPV4_DADDR: u32 = 16;
    pub const IPV6_SADDR: u32 = 8;
    pub const IPV6_DADDR: u32 = 24;
    pub const TCP: u8 = libc::IPPROTO_TCP as u8;
    pub const UDP: u8 = libc::IPPROTO_UDP as u8;
    pub const ICMP: u8 = libc::IPPROTO_ICMP as u8;
    pub const ICMPV6: u8 = libc::IPPROTO_ICMPV6 as u8;

    /// The interface name as nft compares it: IFNAMSIZ bytes, zero padded.
    pub fn ifname(name: &str) -> Vec<u8> {
        let mut bytes = name.as_bytes().to_vec();
        bytes.resize(libc::IFNAMSIZ, 0);
        bytes
    }

    pub fn iifname(op: CmpOp, name: &str) -> Vec<Expr> {
        vec![
            Expr::Meta(MetaKey::IifName),
            Expr::Cmp {
                op,
                data: ifname(name),
            },
        ]
    }

    pub fn oifname(op: CmpOp, name: &str) -> Vec<Expr> {
        vec![
            Expr::Meta(MetaKey::OifName),
            Expr::Cmp {
                op,
                data: ifname(name),
            },
        ]
    }

    /// An interface name that starts with `prefix`: nft's "pod-*".
    pub fn iifname_prefix(op: CmpOp, prefix: &str) -> Vec<Expr> {
        vec![
            Expr::Meta(MetaKey::IifName),
            Expr::Cmp {
                op,
                data: prefix.as_bytes().to_vec(),
            },
        ]
    }

    pub fn oifname_prefix(op: CmpOp, prefix: &str) -> Vec<Expr> {
        vec![
            Expr::Meta(MetaKey::OifName),
            Expr::Cmp {
                op,
                data: prefix.as_bytes().to_vec(),
            },
        ]
    }

    /// The verdict a map has for the input interface.
    pub fn iifname_vmap(map: &str) -> Vec<Expr> {
        vec![
            Expr::Meta(MetaKey::IifName),
            Expr::Vmap {
                map: map.to_string(),
            },
        ]
    }

    pub fn oifname_vmap(map: &str) -> Vec<Expr> {
        vec![
            Expr::Meta(MetaKey::OifName),
            Expr::Vmap {
                map: map.to_string(),
            },
        ]
    }

    /// An IPv4 packet, in the inet family. The IPv4 matches below start
    /// with it, as nft's do.
    pub fn ipv4() -> Vec<Expr> {
        vec![
            Expr::Meta(MetaKey::NfProto),
            Expr::Cmp {
                op: CmpOp::Eq,
                data: vec![libc::NFPROTO_IPV4 as u8],
            },
        ]
    }

    /// An address of the network header at `offset`, after `family`'s
    /// match, in, or out of, a prefix `len` bits long. As nft does, it
    /// compares the prefix's bytes if it ends on a byte, and the whole
    /// address, masked, if not.
    fn address_prefix(
        family: Vec<Expr>,
        offset: u32,
        op: CmpOp,
        address: &[u8],
        len: u8,
    ) -> Vec<Expr> {
        let len = usize::from(len).min(8 * address.len());
        let mask: Vec<u8> = (0..address.len())
            .map(|byte| {
                let bits = len.saturating_sub(8 * byte).min(8);
                (0xff00u16 >> bits) as u8
            })
            .collect();
        let whole_bytes = len > 0 && len.is_multiple_of(8);
        let bytes = if whole_bytes { len / 8 } else { address.len() };
        let mut exprs = family;
        exprs.push(Expr::Payload {
            base: Base::Network,
            offset,
            len: bytes as u32,
        });
        if !whole_bytes {
            exprs.push(Expr::Bitwise { mask: mask.clone() });
        }
        exprs.push(Expr::Cmp {
            op,
            data: address
                .iter()
                .zip(&mask)
                .take(bytes)
                .map(|(byte, mask)| byte & mask)
                .collect(),
        });
        exprs
    }

    /// The IPv4 source (offset 12) or destination (16) in, or out of, a
    /// prefix.
    pub fn ipv4_prefix(offset: u32, op: CmpOp, address: Ipv4Addr, len: u8) -> Vec<Expr> {
        address_prefix(ipv4(), offset, op, &address.octets(), len)
    }

    pub fn ipv4_saddr(op: CmpOp, address: Ipv4Addr) -> Vec<Expr> {
        ipv4_prefix(IPV4_SADDR, op, address, 32)
    }

    pub fn ipv4_daddr(op: CmpOp, address: Ipv4Addr) -> Vec<Expr> {
        ipv4_prefix(IPV4_DADDR, op, address, 32)
    }

    pub fn ipv4_daddr_prefix(address: Ipv4Addr, len: u8) -> Vec<Expr> {
        ipv4_prefix(IPV4_DADDR, CmpOp::Eq, address, len)
    }

    /// The IPv4 source or destination in a set of addresses, or with
    /// `invert` not.
    pub fn ipv4_lookup(offset: u32, set: &str, invert: bool) -> Vec<Expr> {
        let mut exprs = ipv4();
        exprs.push(Expr::Payload {
            base: Base::Network,
            offset,
            len: 4,
        });
        exprs.push(Expr::Lookup {
            set: set.to_string(),
            invert,
        });
        exprs
    }

    pub fn ipv4_saddr_in(set: &str) -> Vec<Expr> {
        ipv4_lookup(IPV4_SADDR, set, false)
    }

    pub fn ipv4_daddr_in(set: &str) -> Vec<Expr> {
        ipv4_lookup(IPV4_DADDR, set, false)
    }

    /// An IPv6 packet, in the inet family. The IPv6 matches below start
    /// with it, as nft's do.
    pub fn ipv6() -> Vec<Expr> {
        vec![
            Expr::Meta(MetaKey::NfProto),
            Expr::Cmp {
                op: CmpOp::Eq,
                data: vec![libc::NFPROTO_IPV6 as u8],
            },
        ]
    }

    /// The IPv6 source or destination in, or out of, a prefix.
    pub fn ipv6_prefix(offset: u32, op: CmpOp, address: Ipv6Addr, len: u8) -> Vec<Expr> {
        address_prefix(ipv6(), offset, op, &address.octets(), len)
    }

    pub fn ipv6_saddr(op: CmpOp, address: Ipv6Addr) -> Vec<Expr> {
        ipv6_prefix(IPV6_SADDR, op, address, 128)
    }

    pub fn ipv6_daddr(op: CmpOp, address: Ipv6Addr) -> Vec<Expr> {
        ipv6_prefix(IPV6_DADDR, op, address, 128)
    }

    pub fn ipv6_daddr_prefix(address: Ipv6Addr, len: u8) -> Vec<Expr> {
        ipv6_prefix(IPV6_DADDR, CmpOp::Eq, address, len)
    }

    /// The IPv6 source or destination in a set of addresses, or with
    /// `invert` not.
    pub fn ipv6_lookup(offset: u32, set: &str, invert: bool) -> Vec<Expr> {
        let mut exprs = ipv6();
        exprs.push(Expr::Payload {
            base: Base::Network,
            offset,
            len: 16,
        });
        exprs.push(Expr::Lookup {
            set: set.to_string(),
            invert,
        });
        exprs
    }

    pub fn ipv6_saddr_in(set: &str) -> Vec<Expr> {
        ipv6_lookup(IPV6_SADDR, set, false)
    }

    pub fn ipv6_daddr_in(set: &str) -> Vec<Expr> {
        ipv6_lookup(IPV6_DADDR, set, false)
    }

    /// The transport protocol, after IPv6's extension headers.
    pub fn l4proto(proto: u8) -> Vec<Expr> {
        vec![
            Expr::Meta(MetaKey::L4Proto),
            Expr::Cmp {
                op: CmpOp::Eq,
                data: vec![proto],
            },
        ]
    }

    /// A port of the transport header, source (offset 0) or destination
    /// (2): of TCP, UDP or SCTP, which l4proto says.
    pub fn th_port(offset: u32, op: CmpOp, port: u16) -> Vec<Expr> {
        vec![
            Expr::Payload {
                base: Base::Transport,
                offset,
                len: 2,
            },
            Expr::Cmp {
                op,
                data: port.to_be_bytes().to_vec(),
            },
        ]
    }

    pub fn th_dport(op: CmpOp, port: u16) -> Vec<Expr> {
        th_port(2, op, port)
    }

    /// A destination port from `first` to `last`.
    pub fn th_dport_range(first: u16, last: u16) -> Vec<Expr> {
        if first == last {
            return th_dport(CmpOp::Eq, first);
        }
        [th_dport(CmpOp::Gte, first), th_dport(CmpOp::Lte, last)].concat()
    }

    pub fn icmpv6_type(icmp_type: u8) -> Vec<Expr> {
        let mut exprs = [ipv6(), l4proto(ICMPV6)].concat();
        exprs.push(Expr::Payload {
            base: Base::Transport,
            offset: 0,
            len: 1,
        });
        exprs.push(Expr::Cmp {
            op: CmpOp::Eq,
            data: vec![icmp_type],
        });
        exprs
    }

    /// An ICMP message of IPv4, of this type.
    pub fn icmp_type(icmp_type: u8) -> Vec<Expr> {
        let mut exprs = [ipv4(), l4proto(ICMP)].concat();
        exprs.push(Expr::Payload {
            base: Base::Transport,
            offset: 0,
            len: 1,
        });
        exprs.push(Expr::Cmp {
            op: CmpOp::Eq,
            data: vec![icmp_type],
        });
        exprs
    }

    /// The connection is in one of the states of `bits` ([`ct`]).
    pub fn ct_state(bits: u32) -> Vec<Expr> {
        vec![
            Expr::Ct {
                key: CtKey::State,
                direction: None,
            },
            Expr::Bitwise {
                mask: bits.to_ne_bytes().to_vec(),
            },
            Expr::Cmp {
                op: CmpOp::Neq,
                data: vec![0; 4],
            },
        ]
    }

    /// The packet goes in this direction of its connection.
    pub fn ct_direction(direction: Direction) -> Vec<Expr> {
        vec![
            Expr::Ct {
                key: CtKey::Direction,
                direction: None,
            },
            Expr::Cmp {
                op: CmpOp::Eq,
                data: vec![direction as u8],
            },
        ]
    }

    /// Compares the packets that the connection's initiator sent, this one
    /// included.
    pub fn ct_original_packets(op: CmpOp, packets: u64) -> Vec<Expr> {
        vec![
            Expr::Ct {
                key: CtKey::Packets,
                direction: Some(Direction::Original),
            },
            Expr::Hton { len: 8, size: 8 },
            Expr::Cmp {
                op,
                data: packets.to_be_bytes().to_vec(),
            },
        ]
    }

    /// The destination the connection's initiator chose, in a set of
    /// IPv4 addresses: before a DNAT on the way, such as kube-proxy's to a
    /// service's endpoints, which the packet's own header no longer shows.
    /// A packet without a connection does not match.
    pub fn ct_original_ipv4_daddr_in(set: &str) -> Vec<Expr> {
        let mut exprs = ipv4();
        exprs.push(Expr::Ct {
            key: CtKey::DstIp,
            direction: Some(Direction::Original),
        });
        exprs.push(Expr::Lookup {
            set: set.to_string(),
            invert: false,
        });
        exprs
    }

    /// [`ct_original_ipv4_daddr_in`] for IPv6.
    pub fn ct_original_ipv6_daddr_in(set: &str) -> Vec<Expr> {
        let mut exprs = ipv6();
        exprs.push(Expr::Ct {
            key: CtKey::DstIp6,
            direction: Some(Direction::Original),
        });
        exprs.push(Expr::Lookup {
            set: set.to_string(),
            invert: false,
        });
        exprs
    }

    /// No route back to the source address leaves through the interface
    /// the packet came in on: the reverse path check.
    pub fn fib_saddr_iif_oif_missing() -> Vec<Expr> {
        vec![
            Expr::Fib {
                flags: fib::SADDR | fib::IIF | fib::PRESENT,
                result: FibResult::Oif,
            },
            Expr::Cmp {
                op: CmpOp::Eq,
                data: vec![0; 4],
            },
        ]
    }

    pub fn log_group(group: u16, prefix: &str, snaplen: u32) -> Vec<Expr> {
        vec![Expr::Log {
            group,
            prefix: prefix.to_string(),
            snaplen,
        }]
    }

    pub fn limit(rate: u64, per: Per, burst: u32) -> Vec<Expr> {
        vec![Expr::Limit { rate, per, burst }]
    }

    /// The EtherType of a frame, in the bridge family.
    pub fn ether_type(op: CmpOp, ether_type: u16) -> Vec<Expr> {
        vec![
            Expr::Payload {
                base: Base::LinkLayer,
                offset: 12,
                len: 2,
            },
            Expr::Cmp {
                op,
                data: ether_type.to_be_bytes().to_vec(),
            },
        ]
    }

    pub fn ether_saddr(op: CmpOp, mac: [u8; 6]) -> Vec<Expr> {
        vec![
            Expr::Payload {
                base: Base::LinkLayer,
                offset: 6,
                len: 6,
            },
            Expr::Cmp {
                op,
                data: mac.to_vec(),
            },
        ]
    }

    /// ARP's sender hardware (offset 8) and protocol (14) address.
    pub fn arp_sha(op: CmpOp, mac: [u8; 6]) -> Vec<Expr> {
        vec![
            Expr::Payload {
                base: Base::Network,
                offset: 8,
                len: 6,
            },
            Expr::Cmp {
                op,
                data: mac.to_vec(),
            },
        ]
    }

    pub fn arp_spa(op: CmpOp, address: Ipv4Addr) -> Vec<Expr> {
        vec![
            Expr::Payload {
                base: Base::Network,
                offset: 14,
                len: 4,
            },
            Expr::Cmp {
                op,
                data: address.octets().to_vec(),
            },
        ]
    }
}

/// Replaces `table` atomically: whatever was in it goes.
pub fn replace(table: &Table) -> io::Result<()> {
    send(&replace_batch(table, 1)?)
}

/// Adds and deletes elements of the sets of `table`, in one batch: all of
/// them or none. `table` is the one in the kernel; its sets say what
/// their elements are, and their elements here do not matter. Adding an
/// element that is there is a no-op; deleting one that is not fails.
pub fn update(table: &Table, ops: &[ElementOp]) -> io::Result<()> {
    let batch = update_batch(table, ops, 1)?;
    if batch.what.is_empty() {
        return Ok(());
    }
    send(&batch)
}

/// Deletes `table`, if it exists.
pub fn delete(family: Family, name: &str) -> io::Result<()> {
    let mut batch = Batch::new(1);
    batch.message(
        format!("table {name}"),
        NFT_MSG_NEWTABLE,
        NLM_F_CREATE,
        family,
        |m| m.str(NFTA_TABLE_NAME, name),
    );
    batch.message(
        format!("delete table {name}"),
        NFT_MSG_DELTABLE,
        0,
        family,
        |m| m.str(NFTA_TABLE_NAME, name),
    );
    batch.finish();
    send(&batch)
}

/// The names of the chains of a table in the kernel; none if there is no
/// such table. For tests and diagnostics: it reads, and changes nothing.
pub fn chains(family: Family, table: &str) -> io::Result<Vec<String>> {
    dump(
        family,
        table,
        NFT_MSG_GETCHAIN,
        NFT_MSG_NEWCHAIN,
        NFTA_CHAIN_NAME,
    )
}

/// The names of the sets and maps of a table in the kernel, as
/// [`chains`].
pub fn sets(family: Family, table: &str) -> io::Result<Vec<String>> {
    dump(family, table, NFT_MSG_GETSET, NFT_MSG_NEWSET, NFTA_SET_NAME)
}

/// The chain of every rule of a table in the kernel, in order: a chain's
/// name as often as it has rules. As [`chains`].
pub fn rules(family: Family, table: &str) -> io::Result<Vec<String>> {
    dump(
        family,
        table,
        NFT_MSG_GETRULE,
        NFT_MSG_NEWRULE,
        NFTA_RULE_CHAIN,
    )
}

/// Dumps what of a table `get` asks for, and reads the attribute `name`
/// of each answer `new` that is of the table: a chain, set or rule, whose
/// first attribute is its table.
fn dump(family: Family, table: &str, get: u16, new: u16, name: u16) -> io::Result<Vec<String>> {
    const NFTA_TABLE: u16 = 1;
    let mut request = Batch::new(1);
    request.buf.clear();
    request.header(
        (NFNL_SUBSYS_NFTABLES << 8) | get,
        NLM_F_REQUEST | NLM_F_DUMP,
        family as u8,
        0,
        |m| m.str(NFTA_TABLE, table),
    );
    // SAFETY: plain socket(2); the fd is owned from here on.
    let fd = unsafe {
        libc::socket(
            libc::AF_NETLINK,
            libc::SOCK_RAW | libc::SOCK_CLOEXEC,
            NETLINK_NETFILTER,
        )
    };
    if fd < 0 {
        return Err(io::Error::last_os_error());
    }
    // SAFETY: fd is a socket this function just opened.
    let socket = unsafe { OwnedFd::from_raw_fd(fd) };
    // SAFETY: sockaddr_nl is plain data; all zeroes addresses the kernel.
    let mut kernel: libc::sockaddr_nl = unsafe { std::mem::zeroed() };
    kernel.nl_family = libc::AF_NETLINK as libc::sa_family_t;
    // SAFETY: the buffer and the address outlive the call.
    let sent = unsafe {
        libc::sendto(
            socket.as_raw_fd(),
            request.buf.as_ptr().cast(),
            request.buf.len(),
            0,
            (&kernel as *const libc::sockaddr_nl).cast(),
            size_of::<libc::sockaddr_nl>() as libc::socklen_t,
        )
    };
    if sent < 0 {
        return Err(io::Error::last_os_error());
    }
    let mut names = Vec::new();
    let mut buf = vec![0u8; 65536];
    loop {
        // SAFETY: the buffer outlives the call.
        let n = unsafe { libc::recv(socket.as_raw_fd(), buf.as_mut_ptr().cast(), buf.len(), 0) };
        if n < 0 {
            return Err(io::Error::last_os_error());
        }
        if n == 0 {
            return Ok(names);
        }
        let mut rest = &buf[..n as usize];
        while rest.len() >= 16 {
            let len = u32::from_ne_bytes(rest[0..4].try_into().unwrap_or_default()) as usize;
            let ty = u16::from_ne_bytes(rest[4..6].try_into().unwrap_or_default());
            if len < 16 || len > rest.len() {
                break;
            }
            match ty {
                NLMSG_DONE => return Ok(names),
                NLMSG_ERROR if len >= 20 => {
                    let code = i32::from_ne_bytes(rest[16..20].try_into().unwrap_or_default());
                    return match -code {
                        0 => Ok(names),
                        libc::ENOENT => Ok(Vec::new()),
                        code => Err(io::Error::from_raw_os_error(code)),
                    };
                }
                _ if ty == (NFNL_SUBSYS_NFTABLES << 8) | new && len > 20 => {
                    // After nlmsghdr and nfgenmsg: the attributes. Only
                    // what is of the table counts, whatever the kernel
                    // filters.
                    let mut attrs = &rest[20..len];
                    let (mut of_table, mut found) = (None, None);
                    while attrs.len() >= 4 {
                        let alen = u16::from_ne_bytes([attrs[0], attrs[1]]) as usize;
                        let aty = u16::from_ne_bytes([attrs[2], attrs[3]]) & 0x3fff;
                        if alen < 4 || alen > attrs.len() {
                            break;
                        }
                        let value = &attrs[4..alen];
                        let value = value.split(|b| *b == 0).next().unwrap_or_default();
                        if aty == NFTA_TABLE {
                            of_table = Some(value == table.as_bytes());
                        } else if aty == name {
                            found = Some(String::from_utf8_lossy(value).into_owned());
                        }
                        attrs = &attrs[((alen + 3) & !3).min(attrs.len())..];
                    }
                    if let (Some(true), Some(found)) = (of_table, found) {
                        names.push(found);
                    }
                }
                _ => {}
            }
            rest = &rest[((len + 3) & !3).min(rest.len())..];
        }
    }
}

fn replace_batch(table: &Table, seq: u32) -> io::Result<Batch> {
    let mut batch = Batch::new(seq);
    let name = table.name.as_str();
    let family = table.family;
    // Add, delete and add again: the first add makes the delete find a
    // table on a node that has none yet.
    batch.message(
        format!("table {name}"),
        NFT_MSG_NEWTABLE,
        NLM_F_CREATE,
        family,
        |m| m.str(NFTA_TABLE_NAME, name),
    );
    batch.message(
        format!("delete table {name}"),
        NFT_MSG_DELTABLE,
        0,
        family,
        |m| m.str(NFTA_TABLE_NAME, name),
    );
    batch.message(
        format!("table {name}"),
        NFT_MSG_NEWTABLE,
        NLM_F_CREATE,
        family,
        |m| m.str(NFTA_TABLE_NAME, name),
    );
    // Chains before sets and rules, whose verdicts name them.
    for chain in &table.chains {
        batch.message(
            format!("chain {}", chain.name),
            NFT_MSG_NEWCHAIN,
            NLM_F_CREATE,
            family,
            |m| {
                m.str(NFTA_CHAIN_TABLE, name);
                m.str(NFTA_CHAIN_NAME, &chain.name);
                if let Some(base) = &chain.base {
                    m.nested(NFTA_CHAIN_HOOK, |m| {
                        m.u32(NFTA_HOOK_HOOKNUM, base.hook as u32);
                        m.u32(NFTA_HOOK_PRIORITY, base.priority as u32);
                    });
                    m.u32(NFTA_CHAIN_POLICY, base.policy as u32);
                    m.str(NFTA_CHAIN_TYPE, base.kind);
                }
            },
        );
    }
    // Rules find a set of this batch by name, and by its ID as nft sends
    // it.
    let mut ids = HashMap::new();
    for (index, set) in table.sets.iter().enumerate() {
        let id = index as u32 + 1;
        ids.insert(set.name.as_str(), id);
        let elements = set_elements(set, &set.elements, Elements::Whole)?;
        batch.message(
            format!("set {}", set.name),
            NFT_MSG_NEWSET,
            NLM_F_CREATE,
            family,
            |m| {
                m.str(NFTA_SET_TABLE, name);
                m.str(NFTA_SET_NAME, &set.name);
                let mut flags = 0;
                if set.interval {
                    flags |= NFT_SET_INTERVAL;
                }
                if set.map {
                    flags |= NFT_SET_MAP;
                }
                if set.timeout {
                    flags |= NFT_SET_TIMEOUT;
                }
                m.u32(NFTA_SET_FLAGS, flags);
                m.u32(NFTA_SET_KEY_TYPE, set.key.id());
                m.u32(NFTA_SET_KEY_LEN, set.key.len() as u32);
                if set.map {
                    m.u32(NFTA_SET_DATA_TYPE, NFT_DATA_VERDICT);
                }
                m.u32(NFTA_SET_ID, id);
                m.bytes(
                    NFTA_SET_USERDATA,
                    &udata(UDATA_SET_KEYBYTEORDER, set.key.byteorder()),
                );
            },
        );
        element_messages(
            &mut batch,
            family,
            name,
            &set.name,
            Some(id),
            &elements,
            false,
        );
    }
    for chain in &table.chains {
        for (index, rule) in chain.rules.iter().enumerate() {
            batch.message(
                format!("rule {} of chain {}", index + 1, chain.name),
                NFT_MSG_NEWRULE,
                NLM_F_CREATE | NLM_F_APPEND,
                family,
                |m| {
                    m.str(NFTA_RULE_TABLE, name);
                    m.str(NFTA_RULE_CHAIN, &chain.name);
                    m.nested(NFTA_RULE_EXPRESSIONS, |m| {
                        for expr in rule {
                            m.nested(NFTA_LIST_ELEM, |m| expression(m, expr, &ids));
                        }
                    });
                },
            );
        }
    }
    batch.finish();
    Ok(batch)
}

fn update_batch(table: &Table, ops: &[ElementOp], seq: u32) -> io::Result<Batch> {
    let mut batch = Batch::new(seq);
    for op in ops {
        let (set, elements, delete) = match op {
            ElementOp::Add { set, elements } => (set, elements, false),
            ElementOp::Delete { set, elements } => (set, elements, true),
            ElementOp::Flush { set } => (set, &Vec::new(), true),
        };
        let set = table.sets.iter().find(|s| s.name == *set).ok_or_else(|| {
            io::Error::new(
                io::ErrorKind::InvalidInput,
                format!("table {} has no set {set}", table.name),
            )
        })?;
        if let ElementOp::Flush { .. } = op {
            // A delete without elements flushes the set.
            batch.message(
                format!("flush set {}", set.name),
                NFT_MSG_DELSETELEM,
                0,
                table.family,
                |m| {
                    m.str(NFTA_SET_ELEM_LIST_TABLE, &table.name);
                    m.str(NFTA_SET_ELEM_LIST_SET, &set.name);
                },
            );
            continue;
        }
        let mode = if delete {
            Elements::Delete
        } else {
            Elements::Add
        };
        let elements = set_elements(set, elements, mode)?;
        element_messages(
            &mut batch,
            table.family,
            &table.name,
            &set.name,
            None,
            &elements,
            delete,
        );
    }
    batch.finish();
    Ok(batch)
}

/// What a list of elements is for.
#[derive(Clone, Copy, PartialEq)]
enum Elements {
    /// All of a new set.
    Whole,
    Add,
    /// Keys only: the kernel finds elements by them.
    Delete,
}

/// The elements as the kernel takes them, each an attribute of its own.
/// In an interval set, a range is a start and an end, the key after its
/// last, sorted, as nft sends them; a range that runs to the last key has
/// no end. A whole set starts with an end at key zero, as nft's do.
fn set_elements(set: &Set, elements: &[Element], mode: Elements) -> io::Result<Vec<Vec<u8>>> {
    let invalid = |what: &str| {
        io::Error::new(
            io::ErrorKind::InvalidInput,
            format!("set {}: {what}", set.name),
        )
    };
    let len = set.key.len();
    for element in elements {
        if element.key.len() != len || element.last.as_ref().is_some_and(|l| l.len() != len) {
            return Err(invalid(&format!("a key is not {len} bytes")));
        }
        if element.last.is_some() && !set.interval {
            return Err(invalid("a range, but no intervals"));
        }
        if mode != Elements::Delete && element.verdict.is_some() != set.map {
            return Err(invalid(if set.map {
                "an element without a verdict"
            } else {
                "a verdict, but no map"
            }));
        }
        if element.timeout.is_some() && !set.timeout {
            return Err(invalid("a timeout, but no timeouts"));
        }
    }
    if set.timeout && set.interval {
        return Err(invalid("timeouts of intervals"));
    }
    let verdict = |element: &Element| match mode {
        Elements::Delete => None,
        _ => element.verdict.clone(),
    };
    if !set.interval {
        return Ok(elements
            .iter()
            .map(|e| {
                let timeout = e.timeout.filter(|_| mode != Elements::Delete);
                timed_element(&e.key, verdict(e).as_ref(), timeout)
            })
            .collect());
    }
    let mut ranges: Vec<_> = elements
        .iter()
        .map(|e| (&e.key, e.last.as_ref().unwrap_or(&e.key), verdict(e)))
        .collect();
    ranges.sort_by(|a, b| (a.0, a.1).cmp(&(b.0, b.1)));
    ranges.dedup();
    let mut encoded = Vec::new();
    if mode == Elements::Whole && ranges.first().is_some_and(|r| r.0.iter().any(|b| *b != 0)) {
        encoded.push(element(&vec![0; len], true, None, false));
    }
    let mut previous: Option<&Vec<u8>> = None;
    for (first, last, verdict) in &ranges {
        let (first, last) = (*first, *last);
        if first > last {
            return Err(invalid("a range ends before it starts"));
        }
        if previous.is_some_and(|previous| previous >= first) {
            return Err(invalid("ranges overlap"));
        }
        let end = next(last);
        encoded.push(element(first, false, verdict.as_ref(), end.is_none()));
        if let Some(end) = end {
            encoded.push(element(&end, true, None, false));
        }
        previous = Some(last);
    }
    Ok(encoded)
}

/// The key after `key`, big endian; none after the last.
fn next(key: &[u8]) -> Option<Vec<u8>> {
    let mut next = key.to_vec();
    for byte in next.iter_mut().rev() {
        if *byte == u8::MAX {
            *byte = 0;
        } else {
            *byte += 1;
            return Some(next);
        }
    }
    None
}

/// An element: an interval's end, or a key with its verdict; `open` if
/// its range runs to the last key.
fn element(key: &[u8], end: bool, verdict: Option<&Verdict>, open: bool) -> Vec<u8> {
    encode_element(key, end, verdict, open, None)
}

/// A key of a set that is no interval set, with its verdict and timeout.
fn timed_element(key: &[u8], verdict: Option<&Verdict>, timeout: Option<Duration>) -> Vec<u8> {
    encode_element(key, false, verdict, false, timeout)
}

fn encode_element(
    key: &[u8],
    end: bool,
    verdict: Option<&Verdict>,
    open: bool,
    timeout: Option<Duration>,
) -> Vec<u8> {
    let mut buf = Vec::new();
    Attrs { buf: &mut buf }.nested(NFTA_LIST_ELEM, |m| {
        m.nested(NFTA_SET_ELEM_KEY, |m| m.bytes(NFTA_DATA_VALUE, key));
        if end {
            m.u32(NFTA_SET_ELEM_FLAGS, NFT_SET_ELEM_INTERVAL_END);
        }
        if let Some(verdict) = verdict {
            m.nested(NFTA_SET_ELEM_DATA, |m| verdict_data(m, verdict));
        }
        if let Some(timeout) = timeout {
            // The expiration too: an element that is there and has this
            // timeout already keeps its old expiry without one (Linux
            // 6.12, nft_add_set_elem).
            let millis = u64::try_from(timeout.as_millis())
                .unwrap_or(u64::MAX)
                .max(1);
            m.u64(NFTA_SET_ELEM_TIMEOUT, millis);
            m.u64(NFTA_SET_ELEM_EXPIRATION, millis);
        }
        if open {
            let flags = udata(UDATA_SET_ELEM_FLAGS, SET_ELEM_F_INTERVAL_OPEN);
            m.bytes(NFTA_SET_ELEM_USERDATA, &flags);
        }
    });
    buf
}

/// One item of nft's user data: type, length, value in host byte order.
fn udata(ty: u8, value: u32) -> Vec<u8> {
    [&[ty, 4][..], &value.to_ne_bytes()].concat()
}

/// Messages that add, or delete, elements of a set: as many as their
/// attribute's 16 bit length takes.
fn element_messages(
    batch: &mut Batch,
    family: Family,
    table: &str,
    set: &str,
    id: Option<u32>,
    elements: &[Vec<u8>],
    delete: bool,
) {
    let (ty, flags, what) = if delete {
        (NFT_MSG_DELSETELEM, 0, "delete elements of set")
    } else {
        (NFT_MSG_NEWSETELEM, NLM_F_CREATE, "elements of set")
    };
    let mut rest = elements;
    while !rest.is_empty() {
        let mut size = 0;
        let count = rest
            .iter()
            .take_while(|element| {
                size += element.len();
                size <= ELEMENTS_PER_MESSAGE
            })
            .count()
            .max(1);
        let (chunk, tail) = rest.split_at(count);
        batch.message(format!("{what} {set}"), ty, flags, family, |m| {
            m.str(NFTA_SET_ELEM_LIST_TABLE, table);
            m.str(NFTA_SET_ELEM_LIST_SET, set);
            if let Some(id) = id {
                m.u32(NFTA_SET_ELEM_LIST_SET_ID, id);
            }
            m.nested(NFTA_SET_ELEM_LIST_ELEMENTS, |m| {
                for element in chunk {
                    m.raw(element);
                }
            });
        });
        rest = tail;
    }
}

fn verdict_data(m: &mut Attrs, verdict: &Verdict) {
    let (code, chain) = match verdict {
        Verdict::Accept => (NF_ACCEPT, None),
        Verdict::Drop => (NF_DROP, None),
        Verdict::Return => (NFT_RETURN, None),
        Verdict::Jump(chain) => (NFT_JUMP, Some(chain)),
        Verdict::Goto(chain) => (NFT_GOTO, Some(chain)),
    };
    m.nested(NFTA_DATA_VERDICT, |m| {
        m.u32(NFTA_VERDICT_CODE, code);
        if let Some(chain) = chain {
            m.str(NFTA_VERDICT_CHAIN, chain);
        }
    });
}

/// An expression; `sets` are the IDs of the sets of the batch.
fn expression(m: &mut Attrs, expr: &Expr, sets: &HashMap<&str, u32>) {
    let data_value = |m: &mut Attrs, ty: u16, value: &[u8]| {
        m.nested(ty, |m| m.bytes(NFTA_DATA_VALUE, value));
    };
    match expr {
        Expr::Meta(key) => {
            m.str(NFTA_EXPR_NAME, "meta");
            m.nested(NFTA_EXPR_DATA, |m| {
                m.u32(1, NFT_REG_1); // NFTA_META_DREG
                m.u32(2, *key as u32); // NFTA_META_KEY
            });
        }
        Expr::Payload { base, offset, len } => {
            m.str(NFTA_EXPR_NAME, "payload");
            m.nested(NFTA_EXPR_DATA, |m| {
                m.u32(1, NFT_REG_1); // NFTA_PAYLOAD_DREG
                m.u32(2, *base as u32); // NFTA_PAYLOAD_BASE
                m.u32(3, *offset); // NFTA_PAYLOAD_OFFSET
                m.u32(4, *len); // NFTA_PAYLOAD_LEN
            });
        }
        Expr::Bitwise { mask } => {
            m.str(NFTA_EXPR_NAME, "bitwise");
            m.nested(NFTA_EXPR_DATA, |m| {
                m.u32(1, NFT_REG_1); // NFTA_BITWISE_SREG
                m.u32(2, NFT_REG_1); // NFTA_BITWISE_DREG
                m.u32(3, mask.len() as u32); // NFTA_BITWISE_LEN
                data_value(m, 4, mask); // NFTA_BITWISE_MASK
                data_value(m, 5, &vec![0; mask.len()]); // NFTA_BITWISE_XOR
            });
        }
        Expr::Cmp { op, data } => {
            m.str(NFTA_EXPR_NAME, "cmp");
            m.nested(NFTA_EXPR_DATA, |m| {
                m.u32(1, NFT_REG_1); // NFTA_CMP_SREG
                m.u32(2, *op as u32); // NFTA_CMP_OP
                data_value(m, 3, data); // NFTA_CMP_DATA
            });
        }
        Expr::Ct { key, direction } => {
            m.str(NFTA_EXPR_NAME, "ct");
            m.nested(NFTA_EXPR_DATA, |m| {
                m.u32(1, NFT_REG_1); // NFTA_CT_DREG
                m.u32(2, *key as u32); // NFTA_CT_KEY
                if let Some(direction) = direction {
                    m.u8(3, *direction as u8); // NFTA_CT_DIRECTION
                }
            });
        }
        Expr::Hton { len, size } => {
            m.str(NFTA_EXPR_NAME, "byteorder");
            m.nested(NFTA_EXPR_DATA, |m| {
                m.u32(1, NFT_REG_1); // NFTA_BYTEORDER_SREG
                m.u32(2, NFT_REG_1); // NFTA_BYTEORDER_DREG
                m.u32(3, 1); // NFTA_BYTEORDER_OP: NFT_BYTEORDER_HTON
                m.u32(4, *len); // NFTA_BYTEORDER_LEN
                m.u32(5, *size); // NFTA_BYTEORDER_SIZE
            });
        }
        Expr::Fib { flags, result } => {
            m.str(NFTA_EXPR_NAME, "fib");
            m.nested(NFTA_EXPR_DATA, |m| {
                m.u32(1, NFT_REG_1); // NFTA_FIB_DREG
                m.u32(2, *result as u32); // NFTA_FIB_RESULT
                m.u32(3, *flags); // NFTA_FIB_FLAGS
            });
        }
        Expr::Lookup { set, invert } => {
            m.str(NFTA_EXPR_NAME, "lookup");
            m.nested(NFTA_EXPR_DATA, |m| {
                m.str(1, set); // NFTA_LOOKUP_SET
                m.u32(2, NFT_REG_1); // NFTA_LOOKUP_SREG
                if let Some(id) = sets.get(set.as_str()) {
                    m.u32(4, *id); // NFTA_LOOKUP_SET_ID
                }
                if *invert {
                    m.u32(5, 1); // NFTA_LOOKUP_FLAGS: NFT_LOOKUP_F_INV
                }
            });
        }
        Expr::Vmap { map } => {
            m.str(NFTA_EXPR_NAME, "lookup");
            m.nested(NFTA_EXPR_DATA, |m| {
                m.str(1, map); // NFTA_LOOKUP_SET
                m.u32(2, NFT_REG_1); // NFTA_LOOKUP_SREG
                m.u32(3, NFT_REG_VERDICT); // NFTA_LOOKUP_DREG
                if let Some(id) = sets.get(map.as_str()) {
                    m.u32(4, *id); // NFTA_LOOKUP_SET_ID
                }
            });
        }
        Expr::Log {
            group,
            prefix,
            snaplen,
        } => {
            m.str(NFTA_EXPR_NAME, "log");
            m.nested(NFTA_EXPR_DATA, |m| {
                m.u16(1, *group); // NFTA_LOG_GROUP
                if !prefix.is_empty() {
                    m.str(2, prefix); // NFTA_LOG_PREFIX
                }
                if *snaplen != 0 {
                    m.u32(3, *snaplen); // NFTA_LOG_SNAPLEN
                }
            });
        }
        Expr::Limit { rate, per, burst } => {
            m.str(NFTA_EXPR_NAME, "limit");
            m.nested(NFTA_EXPR_DATA, |m| {
                m.u64(1, *rate); // NFTA_LIMIT_RATE
                m.u64(2, per.seconds()); // NFTA_LIMIT_UNIT
                m.u32(3, *burst); // NFTA_LIMIT_BURST
                m.u32(4, 0); // NFTA_LIMIT_TYPE: NFT_LIMIT_PKTS
                m.u32(5, 0); // NFTA_LIMIT_FLAGS
            });
        }
        Expr::Counter => {
            m.str(NFTA_EXPR_NAME, "counter");
            m.nested(NFTA_EXPR_DATA, |_| {});
        }
        Expr::Masquerade => m.str(NFTA_EXPR_NAME, "masq"),
        Expr::Verdict(verdict) => {
            m.str(NFTA_EXPR_NAME, "immediate");
            m.nested(NFTA_EXPR_DATA, |m| {
                m.u32(1, NFT_REG_VERDICT); // NFTA_IMMEDIATE_DREG
                m.nested(2, |m| verdict_data(m, verdict)); // NFTA_IMMEDIATE_DATA
            });
        }
    }
}

/// Netlink attributes, appended to a message.
struct Attrs<'a> {
    buf: &'a mut Vec<u8>,
}

impl Attrs<'_> {
    fn attr(&mut self, ty: u16, payload: &[u8]) {
        let len = 4 + payload.len();
        self.buf.extend_from_slice(&(len as u16).to_ne_bytes());
        self.buf.extend_from_slice(&ty.to_ne_bytes());
        self.buf.extend_from_slice(payload);
        pad(self.buf);
    }

    fn str(&mut self, ty: u16, value: &str) {
        let mut bytes = value.as_bytes().to_vec();
        bytes.push(0);
        self.attr(ty, &bytes);
    }

    fn u8(&mut self, ty: u16, value: u8) {
        self.attr(ty, &[value]);
    }

    /// nftables' integers are big endian.
    fn u16(&mut self, ty: u16, value: u16) {
        self.attr(ty, &value.to_be_bytes());
    }

    fn u32(&mut self, ty: u16, value: u32) {
        self.attr(ty, &value.to_be_bytes());
    }

    fn u64(&mut self, ty: u16, value: u64) {
        self.attr(ty, &value.to_be_bytes());
    }

    fn bytes(&mut self, ty: u16, value: &[u8]) {
        self.attr(ty, value);
    }

    /// Attributes encoded already.
    fn raw(&mut self, attrs: &[u8]) {
        self.buf.extend_from_slice(attrs);
    }

    fn nested(&mut self, ty: u16, fill: impl FnOnce(&mut Attrs)) {
        let start = self.buf.len();
        self.buf.extend_from_slice(&[0; 4]);
        fill(&mut Attrs { buf: self.buf });
        let len = (self.buf.len() - start) as u16;
        self.buf[start..start + 2].copy_from_slice(&len.to_ne_bytes());
        self.buf[start + 2..start + 4].copy_from_slice(&(ty | NLA_F_NESTED).to_ne_bytes());
    }
}

fn pad(buf: &mut Vec<u8>) {
    while !buf.len().is_multiple_of(4) {
        buf.push(0);
    }
}

/// Netlink messages of nfnetlink, between a batch's begin and end.
struct Batch {
    buf: Vec<u8>,
    /// The sequence number of the batch's begin; its messages follow.
    first: u32,
    seq: u32,
    /// What each message does, for errors.
    what: Vec<String>,
    /// Where the last message starts.
    last: usize,
}

impl Batch {
    fn new(seq: u32) -> Self {
        let mut batch = Self {
            buf: Vec::new(),
            first: seq,
            seq,
            what: Vec::new(),
            last: 0,
        };
        batch.header(
            NFNL_MSG_BATCH_BEGIN,
            NLM_F_REQUEST,
            0,
            NFNL_SUBSYS_NFTABLES,
            |_| {},
        );
        batch
    }

    fn message(
        &mut self,
        what: String,
        ty: u16,
        flags: u16,
        family: Family,
        fill: impl FnOnce(&mut Attrs),
    ) {
        self.last = self.buf.len();
        self.what.push(what);
        self.header(
            (NFNL_SUBSYS_NFTABLES << 8) | ty,
            NLM_F_REQUEST | flags,
            family as u8,
            0,
            fill,
        );
    }

    /// Ends the batch. Only its last message asks for an acknowledgement,
    /// which the kernel sends once it has applied the batch, or not: one
    /// per message would overflow the socket's buffer.
    fn finish(&mut self) {
        if !self.what.is_empty() {
            let at = self.last + 6;
            if let Some(flags) = self.buf.get_mut(at..at + 2) {
                let value = u16::from_ne_bytes([flags[0], flags[1]]) | NLM_F_ACK;
                flags.copy_from_slice(&value.to_ne_bytes());
            }
        }
        self.header(
            NFNL_MSG_BATCH_END,
            NLM_F_REQUEST,
            0,
            NFNL_SUBSYS_NFTABLES,
            |_| {},
        );
    }

    /// A netlink header, nfgenmsg (family, version 0, resource ID) and the
    /// attributes.
    fn header(
        &mut self,
        ty: u16,
        flags: u16,
        family: u8,
        res_id: u16,
        fill: impl FnOnce(&mut Attrs),
    ) {
        let start = self.buf.len();
        self.buf.extend_from_slice(&[0; 4]); // nlmsg_len
        self.buf.extend_from_slice(&ty.to_ne_bytes());
        self.buf.extend_from_slice(&flags.to_ne_bytes());
        self.buf.extend_from_slice(&self.seq.to_ne_bytes());
        self.buf.extend_from_slice(&0u32.to_ne_bytes()); // nlmsg_pid
        self.buf.push(family);
        self.buf.push(0);
        self.buf.extend_from_slice(&res_id.to_be_bytes());
        fill(&mut Attrs { buf: &mut self.buf });
        let len = (self.buf.len() - start) as u32;
        self.buf[start..start + 4].copy_from_slice(&len.to_ne_bytes());
        self.seq += 1;
    }

    /// The error the kernel sent for the message with `seq`.
    fn error(&self, seq: u32, code: i32) -> io::Error {
        let os = io::Error::from_raw_os_error(code);
        let what = seq
            .checked_sub(self.first + 1)
            .and_then(|index| self.what.get(index as usize))
            .map_or("the batch", String::as_str);
        io::Error::new(os.kind(), format!("nftables: {what}: {os}"))
    }
}

/// Sends a batch and reads the kernel's answer: errors, and the last
/// message's acknowledgement.
fn send(batch: &Batch) -> io::Result<()> {
    // SAFETY: plain socket(2); the fd is owned from here on.
    let fd = unsafe {
        libc::socket(
            libc::AF_NETLINK,
            libc::SOCK_RAW | libc::SOCK_CLOEXEC,
            NETLINK_NETFILTER,
        )
    };
    if fd < 0 {
        return Err(io::Error::last_os_error());
    }
    // SAFETY: fd is a socket this function just opened.
    let socket = unsafe { OwnedFd::from_raw_fd(fd) };
    // The batch is one datagram, which has to fit the send buffer: as nft
    // does, it is raised to the batch's size, past net.core.wmem_max. If
    // that fails, sendto says so.
    let size = libc::c_int::try_from(batch.buf.len()).unwrap_or(libc::c_int::MAX);
    setsockopt(&socket, libc::SOL_SOCKET, libc::SO_SNDBUFFORCE, size);
    // Errors carry the failed message's header, not the whole message.
    setsockopt(&socket, libc::SOL_NETLINK, NETLINK_CAP_ACK, 1);
    // SAFETY: sockaddr_nl is plain data; all zeroes addresses the kernel.
    let mut kernel: libc::sockaddr_nl = unsafe { std::mem::zeroed() };
    kernel.nl_family = libc::AF_NETLINK as libc::sa_family_t;
    // SAFETY: the buffer and the address outlive the call.
    let sent = unsafe {
        libc::sendto(
            socket.as_raw_fd(),
            batch.buf.as_ptr().cast(),
            batch.buf.len(),
            0,
            (&kernel as *const libc::sockaddr_nl).cast(),
            size_of::<libc::sockaddr_nl>() as libc::socklen_t,
        )
    };
    if sent < 0 {
        return Err(io::Error::last_os_error());
    }
    // The kernel handles the batch within sendto: its answer is queued by
    // now.
    let last = batch.first + batch.what.len() as u32;
    let mut acknowledged = false;
    let mut buf = vec![0u8; 65536];
    loop {
        // SAFETY: the buffer outlives the call.
        let n = unsafe {
            libc::recv(
                socket.as_raw_fd(),
                buf.as_mut_ptr().cast(),
                buf.len(),
                libc::MSG_DONTWAIT,
            )
        };
        if n < 0 {
            let e = io::Error::last_os_error();
            if e.kind() == io::ErrorKind::WouldBlock {
                break;
            }
            return Err(e);
        }
        let mut rest = &buf[..n as usize];
        while rest.len() >= 16 {
            let len = u32::from_ne_bytes(rest[0..4].try_into().unwrap_or_default()) as usize;
            let ty = u16::from_ne_bytes(rest[4..6].try_into().unwrap_or_default());
            if len < 16 || len > rest.len() {
                break;
            }
            // nlmsgerr: the error, and the header of the message it answers.
            if ty == NLMSG_ERROR && len >= 36 {
                let code = i32::from_ne_bytes(rest[16..20].try_into().unwrap_or_default());
                let seq = u32::from_ne_bytes(rest[28..32].try_into().unwrap_or_default());
                if code != 0 {
                    return Err(batch.error(seq, -code));
                }
                acknowledged |= seq == last;
            }
            rest = &rest[((len + 3) & !3).min(rest.len())..];
        }
    }
    if acknowledged {
        Ok(())
    } else {
        Err(io::Error::other(
            "nftables: the kernel did not acknowledge the batch",
        ))
    }
}

fn setsockopt(socket: &OwnedFd, level: libc::c_int, name: libc::c_int, value: libc::c_int) {
    // SAFETY: the value outlives the call; a failure leaves the socket as
    // it was.
    unsafe {
        libc::setsockopt(
            socket.as_raw_fd(),
            level,
            name,
            (&value as *const libc::c_int).cast(),
            size_of::<libc::c_int>() as libc::socklen_t,
        );
    }
}

#[cfg(test)]
mod tests {
    use super::matches::*;
    use super::*;

    fn table() -> Table {
        Table {
            family: Family::Inet,
            name: "feos-test".to_string(),
            sets: Vec::new(),
            chains: vec![Chain::base(
                "input",
                "filter",
                Hook::Input,
                0,
                Policy::Accept,
                vec![[
                    iifname(CmpOp::Eq, "feos-pods"),
                    vec![Expr::Verdict(Verdict::Drop)],
                ]
                .concat()],
            )],
        }
    }

    /// The messages of a batch: type, flags, and the attributes after
    /// nfgenmsg.
    fn messages(batch: &[u8]) -> Vec<(u16, u16, &[u8])> {
        let mut messages = Vec::new();
        let mut rest = batch;
        while rest.len() >= 20 {
            let len = u32::from_ne_bytes(rest[0..4].try_into().unwrap()) as usize;
            let ty = u16::from_ne_bytes(rest[4..6].try_into().unwrap());
            let flags = u16::from_ne_bytes(rest[6..8].try_into().unwrap());
            messages.push((ty & 0xff, flags, &rest[20..len]));
            rest = &rest[(len + 3) & !3..];
        }
        messages
    }

    /// Attributes: type without the nested flag, and payload.
    fn attrs(mut buf: &[u8]) -> Vec<(u16, &[u8])> {
        let mut attrs = Vec::new();
        while buf.len() >= 4 {
            let len = u16::from_ne_bytes([buf[0], buf[1]]) as usize;
            let ty = u16::from_ne_bytes([buf[2], buf[3]]) & !NLA_F_NESTED;
            attrs.push((ty, &buf[4..len]));
            buf = &buf[((len + 3) & !3).min(buf.len())..];
        }
        attrs
    }

    fn attr(buf: &[u8], ty: u16) -> Option<&[u8]> {
        attrs(buf).into_iter().find(|a| a.0 == ty).map(|a| a.1)
    }

    /// Elements: key, and whether it ends an interval.
    fn decode(list: &[u8]) -> Vec<(Vec<u8>, bool)> {
        attrs(list)
            .into_iter()
            .map(|(_, element)| {
                let key = attr(attr(element, NFTA_SET_ELEM_KEY).unwrap(), 1).unwrap();
                let end = attr(element, NFTA_SET_ELEM_FLAGS) == Some(&[0, 0, 0, 1]);
                (key.to_vec(), end)
            })
            .collect()
    }

    /// The elements of a set elements message.
    fn keys(message: &[u8]) -> Vec<(Vec<u8>, bool)> {
        decode(attr(message, NFTA_SET_ELEM_LIST_ELEMENTS).unwrap())
    }

    fn encoded(set: &Set) -> Vec<(Vec<u8>, bool)> {
        decode(
            &set_elements(set, &set.elements, Elements::Whole)
                .unwrap()
                .concat(),
        )
    }

    fn v6(address: &str) -> Vec<u8> {
        address.parse::<Ipv6Addr>().unwrap().octets().to_vec()
    }

    #[test]
    fn a_batch_has_begin_messages_and_end() {
        let batch = replace_batch(&table(), 7).unwrap();
        // Begin, add, delete, add, a chain, a rule, end.
        let messages = messages(&batch.buf);
        assert_eq!(messages.len(), 7);
        assert_eq!(messages[0].0, NFNL_MSG_BATCH_BEGIN);
        assert_eq!(messages[6].0, NFNL_MSG_BATCH_END);
        assert!(batch.buf.len().is_multiple_of(4));
        // Only the last message asks for an acknowledgement.
        let acks: Vec<_> = messages.iter().map(|m| m.1 & NLM_F_ACK != 0).collect();
        assert_eq!(acks, [false, false, false, false, false, true, false]);
        assert_eq!(batch.what[4], "rule 1 of chain input");
        let error = batch.error(7 + 5, libc::ENOENT);
        assert_eq!(error.kind(), io::ErrorKind::NotFound);
        assert!(error
            .to_string()
            .starts_with("nftables: rule 1 of chain input: "));
        assert!(batch
            .error(7, libc::EINVAL)
            .to_string()
            .contains("the batch"));
    }

    #[test]
    fn interface_names_are_padded() {
        let name = ifname("eth0");
        assert_eq!(name.len(), 16);
        assert_eq!(&name[..5], b"eth0\0");
    }

    /// As nft 1.1.3 sends a /20, a /16 and a /0 of IPv4, from `nft
    /// --debug=netlink`.
    #[test]
    fn ipv4_prefixes_compare_as_nft_compares_them() {
        let address = Ipv4Addr::new(169, 254, 31, 3);
        let exprs = ipv4_daddr_prefix(address, 20);
        assert_eq!(exprs[..2], ipv4()[..]);
        assert_eq!(
            exprs[2..],
            [
                Expr::Payload {
                    base: Base::Network,
                    offset: 16,
                    len: 4
                },
                Expr::Bitwise {
                    mask: vec![255, 255, 0xf0, 0]
                },
                Expr::Cmp {
                    op: CmpOp::Eq,
                    data: vec![169, 254, 16, 0]
                },
            ]
        );
        // A prefix of whole bytes: those bytes, no mask.
        let exprs = ipv4_prefix(IPV4_SADDR, CmpOp::Eq, Ipv4Addr::new(10, 77, 2, 3), 16);
        assert_eq!(
            exprs[2..],
            [
                Expr::Payload {
                    base: Base::Network,
                    offset: 12,
                    len: 2
                },
                Expr::Cmp {
                    op: CmpOp::Eq,
                    data: vec![10, 77]
                },
            ]
        );
        let exprs = ipv4_saddr(CmpOp::Neq, address);
        assert_eq!(
            exprs[2..],
            [
                Expr::Payload {
                    base: Base::Network,
                    offset: 12,
                    len: 4
                },
                Expr::Cmp {
                    op: CmpOp::Neq,
                    data: vec![169, 254, 31, 3]
                },
            ]
        );
        let any = ipv4_prefix(IPV4_DADDR, CmpOp::Eq, address, 0);
        assert_eq!(any[3], Expr::Bitwise { mask: vec![0; 4] });
        assert_eq!(
            any[4],
            Expr::Cmp {
                op: CmpOp::Eq,
                data: vec![0; 4]
            }
        );
        // Longer than an address: the address.
        assert_eq!(
            ipv4_prefix(IPV4_DADDR, CmpOp::Eq, address, 200),
            ipv4_daddr(CmpOp::Eq, address)
        );
        // Lookups and ICMP start with the family, too.
        assert_eq!(ipv4_daddr_in("deny4")[..2], ipv4()[..]);
        assert_eq!(
            ipv4_saddr_in("deny4")[2],
            Expr::Payload {
                base: Base::Network,
                offset: 12,
                len: 4
            }
        );
        let redirect = icmp_type(5);
        assert_eq!(redirect[..4], [ipv4(), l4proto(1)].concat()[..]);
        assert_eq!(
            redirect[5],
            Expr::Cmp {
                op: CmpOp::Eq,
                data: vec![5]
            }
        );
    }

    /// As nft 1.1.3 sends `ip6 daddr fe80::/10` and `ip6 daddr
    /// fd00:fe05::/48`, from `nft --debug=netlink`.
    #[test]
    fn ipv6_prefixes_compare_as_nft_compares_them() {
        let address = "fe80::1".parse().unwrap();
        let exprs = ipv6_daddr_prefix(address, 10);
        assert_eq!(exprs[..2], ipv6()[..]);
        let mut mask = vec![0; 16];
        mask[..2].copy_from_slice(&[0xff, 0xc0]);
        assert_eq!(
            exprs[2..],
            [
                Expr::Payload {
                    base: Base::Network,
                    offset: 24,
                    len: 16
                },
                Expr::Bitwise { mask },
                Expr::Cmp {
                    op: CmpOp::Eq,
                    data: v6("fe80::")
                },
            ]
        );
        let exprs = ipv6_daddr_prefix("fd00:fe05::1".parse().unwrap(), 48);
        assert_eq!(
            exprs[2..],
            [
                Expr::Payload {
                    base: Base::Network,
                    offset: 24,
                    len: 6
                },
                Expr::Cmp {
                    op: CmpOp::Eq,
                    data: v6("fd00:fe05::")[..6].to_vec()
                },
            ]
        );
        let exprs = ipv6_saddr(CmpOp::Neq, address);
        assert_eq!(exprs.len(), 4);
        assert_eq!(
            exprs[3],
            Expr::Cmp {
                op: CmpOp::Neq,
                data: v6("fe80::1")
            }
        );
        let any = ipv6_prefix(IPV6_DADDR, CmpOp::Eq, address, 0);
        assert_eq!(any[3], Expr::Bitwise { mask: vec![0; 16] });
        assert_eq!(
            any[4],
            Expr::Cmp {
                op: CmpOp::Eq,
                data: vec![0; 16]
            }
        );
    }

    /// The elements nft 1.1.3 sends for `elements = { fe80::/10, ::1,
    /// 2001:db8::/32 }`, from `nft --debug=netlink`.
    #[test]
    fn intervals_are_sent_as_nft_sends_them() {
        let deny = Set::intervals(
            "deny",
            KeyType::Ipv6Addr,
            vec![
                Element::ipv6_prefix("fe80::".parse().unwrap(), 10),
                Element::ipv6("::1".parse().unwrap()),
                Element::ipv6_prefix("2001:db8::".parse().unwrap(), 32),
            ],
        );
        assert_eq!(
            encoded(&deny),
            [
                (v6("::"), true),
                (v6("::1"), false),
                (v6("::2"), true),
                (v6("2001:db8::"), false),
                (v6("2001:db9::"), true),
                (v6("fe80::"), false),
                (v6("fec0::"), true),
            ]
        );
    }

    #[test]
    fn a_range_to_the_last_key_has_no_end() {
        let internet = Set::intervals(
            "internet",
            KeyType::Ipv6Addr,
            vec![Element::ipv6_prefix(Ipv6Addr::UNSPECIFIED, 0)],
        );
        assert_eq!(encoded(&internet), [(v6("::"), false)]);
        let elements = set_elements(&internet, &internet.elements, Elements::Whole).unwrap();
        // nft's note that the range is open, which `nft list` reads.
        let element = attrs(&elements[0])[0].1;
        assert_eq!(
            attr(element, NFTA_SET_ELEM_USERDATA),
            Some(&[1, 4, 1, 0, 0, 0][..])
        );
        let top = Set::intervals(
            "top",
            KeyType::Ipv6Addr,
            vec![Element::ipv6_range(
                "8000::".parse().unwrap(),
                Ipv6Addr::from(u128::MAX),
            )],
        );
        assert_eq!(encoded(&top), [(v6("::"), true), (v6("8000::"), false)]);
        let v4 = Set::intervals(
            "v4",
            KeyType::Ipv4Addr,
            vec![
                Element::ipv4(Ipv4Addr::new(192, 168, 1, 1)),
                Element::ipv4_prefix(Ipv4Addr::new(10, 1, 2, 3), 8),
            ],
        );
        assert_eq!(
            encoded(&v4),
            [
                (vec![0, 0, 0, 0], true),
                (vec![10, 0, 0, 0], false),
                (vec![11, 0, 0, 0], true),
                (vec![192, 168, 1, 1], false),
                (vec![192, 168, 1, 2], true),
            ]
        );
    }

    #[test]
    fn sets_refuse_what_they_cannot_hold() {
        let set = |elements| Set::intervals("s", KeyType::Ipv6Addr, elements);
        let overlap = set(vec![
            Element::ipv6_prefix("2001:db8::".parse().unwrap(), 32),
            Element::ipv6("2001:db8::1".parse().unwrap()),
        ]);
        let error = set_elements(&overlap, &overlap.elements, Elements::Whole).unwrap_err();
        assert_eq!(error.kind(), io::ErrorKind::InvalidInput);
        assert!(error.to_string().contains("overlap"));
        // The same element twice is once.
        let twice = set(vec![Element::ipv6("::5".parse().unwrap()); 2]);
        assert_eq!(encoded(&twice).len(), 3);
        // Neighbours stay apart, so that each can go on its own.
        let neighbours = set(vec![
            Element::ipv6("::5".parse().unwrap()),
            Element::ipv6("::6".parse().unwrap()),
        ]);
        assert_eq!(encoded(&neighbours).len(), 5);
        let short = set(vec![Element::ipv4(Ipv4Addr::LOCALHOST)]);
        assert!(set_elements(&short, &short.elements, Elements::Whole).is_err());
        let keys = Set::keys(
            "k",
            KeyType::Ipv6Addr,
            vec![Element::ipv6_prefix(Ipv6Addr::UNSPECIFIED, 64)],
        );
        assert!(set_elements(&keys, &keys.elements, Elements::Whole).is_err());
        let map = Set::verdict_map("m", KeyType::IfName, vec![Element::ifname("pod-1")]);
        assert!(set_elements(&map, &map.elements, Elements::Whole).is_err());
        assert!(set_elements(&map, &map.elements, Elements::Delete).is_ok());
    }

    /// A verdict map's element as nft 1.1.3 sends it, from `nft
    /// --debug=mnl`.
    #[test]
    fn map_elements_are_sent_as_nft_sends_them() {
        let element = Element::ifname("pod-1234abcd")
            .with_verdict(Verdict::Goto("out-pod-1234abcd".to_string()));
        let map = Set::verdict_map("pod-out", KeyType::IfName, vec![element]);
        let elements = set_elements(&map, &map.elements, Elements::Whole).unwrap();
        let mut expected = Vec::new();
        let words: [&[u8]; 17] = [
            &[68, 0, 1, 0x80],
            &[24, 0, 1, 0x80],
            &[20, 0, 1, 0],
            b"pod-",
            b"1234",
            b"abcd",
            &[0; 4],
            &[40, 0, 2, 0x80],
            &[36, 0, 2, 0x80],
            &[8, 0, 1, 0],
            &[0xff, 0xff, 0xff, 0xfc],
            &[21, 0, 2, 0],
            b"out-",
            b"pod-",
            b"1234",
            b"abcd",
            &[0; 4],
        ];
        for word in words {
            expected.extend_from_slice(word);
        }
        assert_eq!(elements, [expected]);
        // A delete names the key only.
        let delete = set_elements(&map, &map.elements, Elements::Delete).unwrap();
        assert_eq!(delete[0].len(), 28);
    }

    #[test]
    fn deltas_change_elements_only() {
        let mut table = table();
        table
            .sets
            .push(Set::intervals("g-web", KeyType::Ipv6Addr, Vec::new()));
        let ops = [
            ElementOp::Add {
                set: "g-web".to_string(),
                elements: vec![Element::ipv6("fd00::6".parse().unwrap())],
            },
            ElementOp::Delete {
                set: "g-web".to_string(),
                elements: vec![Element::ipv6("fd00::5".parse().unwrap())],
            },
        ];
        let batch = update_batch(&table, &ops, 1).unwrap();
        let messages = messages(&batch.buf);
        assert_eq!(messages.len(), 4);
        assert_eq!(messages[1].0, NFT_MSG_NEWSETELEM);
        assert_eq!(messages[2].0, NFT_MSG_DELSETELEM);
        // No end at zero: the set has its own.
        assert_eq!(
            keys(messages[1].2),
            [(v6("fd00::6"), false), (v6("fd00::7"), true)]
        );
        assert_eq!(
            keys(messages[2].2),
            [(v6("fd00::5"), false), (v6("fd00::6"), true)]
        );
        let unknown = [ElementOp::Add {
            set: "g-db".to_string(),
            elements: Vec::new(),
        }];
        assert!(update_batch(&table, &unknown, 1).is_err());
        let none = [ElementOp::Add {
            set: "g-web".to_string(),
            elements: Vec::new(),
        }];
        assert!(update_batch(&table, &none, 1).unwrap().what.is_empty());
    }

    #[test]
    fn timed_sets_carry_timeouts_and_flush() {
        let address = || Element::ipv4(Ipv4Addr::new(192, 0, 2, 7));
        let mut table = table();
        table.sets.push(Set::timed(
            "named4",
            KeyType::Ipv4Addr,
            vec![address().with_timeout(Duration::from_secs(300))],
        ));
        // The set has the timeout flag, and its element its timeout and
        // expiration, in milliseconds, big endian.
        let batch = replace_batch(&table, 1).unwrap();
        let sent = messages(&batch.buf);
        let set = sent.iter().find(|m| m.0 == NFT_MSG_NEWSET).expect("a set");
        assert_eq!(
            attr(set.2, NFTA_SET_FLAGS),
            Some(&NFT_SET_TIMEOUT.to_be_bytes()[..])
        );
        let elements = sent
            .iter()
            .find(|m| m.0 == NFT_MSG_NEWSETELEM)
            .expect("its elements");
        let list = attr(elements.2, NFTA_SET_ELEM_LIST_ELEMENTS).unwrap();
        let element = attrs(list)[0].1;
        let millis = 300_000u64.to_be_bytes();
        assert_eq!(attr(element, NFTA_SET_ELEM_TIMEOUT), Some(&millis[..]));
        assert_eq!(attr(element, NFTA_SET_ELEM_EXPIRATION), Some(&millis[..]));
        assert_eq!(keys(elements.2), [(vec![192, 0, 2, 7], false)]);

        // Added again, with a timeout less than a millisecond: one.
        let ops = [
            ElementOp::Add {
                set: "named4".to_string(),
                elements: vec![address().with_timeout(Duration::from_micros(10))],
            },
            ElementOp::Flush {
                set: "named4".to_string(),
            },
        ];
        let batch = update_batch(&table, &ops, 1).unwrap();
        let sent = messages(&batch.buf);
        assert_eq!(sent.len(), 4);
        let list = attr(sent[1].2, NFTA_SET_ELEM_LIST_ELEMENTS).unwrap();
        assert_eq!(
            attr(attrs(list)[0].1, NFTA_SET_ELEM_TIMEOUT),
            Some(&1u64.to_be_bytes()[..])
        );
        // A flush is a delete without elements.
        assert_eq!(sent[2].0, NFT_MSG_DELSETELEM);
        assert_eq!(attr(sent[2].2, NFTA_SET_ELEM_LIST_ELEMENTS), None);
        assert_eq!(batch.what[1], "flush set named4");

        // A timeout where there are none, and timed intervals, are refused.
        let plain = Set::keys(
            "plain",
            KeyType::Ipv4Addr,
            vec![address().with_timeout(Duration::from_secs(1))],
        );
        assert!(set_elements(&plain, &plain.elements, Elements::Whole).is_err());
        let intervals = Set {
            interval: true,
            ..Set::timed("intervals", KeyType::Ipv4Addr, vec![address()])
        };
        assert!(set_elements(&intervals, &intervals.elements, Elements::Whole).is_err());
    }

    #[test]
    fn large_sets_take_several_messages() {
        let elements = (0..5000u128)
            .map(|i| Element::ipv6(Ipv6Addr::from((0xfd00 << 112) | (i * 2))))
            .collect();
        let mut table = table();
        table
            .sets
            .push(Set::intervals("big", KeyType::Ipv6Addr, elements));
        let batch = replace_batch(&table, 1).unwrap();
        let messages = messages(&batch.buf);
        let lists: Vec<_> = messages
            .iter()
            .filter(|m| m.0 == NFT_MSG_NEWSETELEM)
            .map(|m| keys(m.2))
            .collect();
        assert!(lists.len() > 1);
        assert_eq!(lists.iter().map(Vec::len).sum::<usize>(), 1 + 2 * 5000);
    }

    #[test]
    fn expressions_are_encoded_as_nft_encodes_them() {
        let encode = |expr: &Expr| {
            let mut buf = Vec::new();
            expression(
                &mut Attrs { buf: &mut buf },
                expr,
                &HashMap::from([("deny", 1)]),
            );
            buf
        };
        let data = |buf: &[u8]| attr(buf, NFTA_EXPR_DATA).unwrap().to_vec();
        // ct state established,related: the bits in host byte order.
        let state = ct_state(ct::ESTABLISHED | ct::RELATED);
        let mask = attr(&data(&encode(&state[1])), 4).map(|m| attr(m, 1).unwrap().to_vec());
        assert_eq!(mask, Some(6u32.to_ne_bytes().to_vec()));
        // ct original packets < 5: a u64 turned big endian.
        let packets = ct_original_packets(CmpOp::Lt, 5);
        assert_eq!(attr(&data(&encode(&packets[0])), 3), Some(&[0][..]));
        assert_eq!(
            attr(&data(&encode(&packets[2])), 2),
            Some(&2u32.to_be_bytes()[..])
        );
        // ct direction original: a byte.
        let direction = ct_direction(Direction::Original);
        assert_eq!(
            attr(&data(&encode(&direction[0])), 2),
            Some(&1u32.to_be_bytes()[..])
        );
        assert_eq!(
            attr(attr(&data(&encode(&direction[1])), 3).unwrap(), 1),
            Some(&[0][..])
        );
        // fib saddr . iif oif missing.
        let fib = fib_saddr_iif_oif_missing();
        assert_eq!(attr(&data(&encode(&fib[0])), 3), Some(&[0, 0, 0, 0x29][..]));
        // Lookups name the set, and its ID in the batch.
        let lookup = data(&encode(&ipv6_daddr_in("deny")[3]));
        assert_eq!(attr(&lookup, 1), Some(&b"deny\0"[..]));
        assert_eq!(attr(&lookup, 4), Some(&[0, 0, 0, 1][..]));
        assert_eq!(attr(&lookup, 3), None);
        let vmap = data(&encode(&iifname_vmap("pod-out")[1]));
        assert_eq!(attr(&vmap, 3), Some(&[0, 0, 0, 0][..]));
        assert_eq!(attr(&vmap, 4), None);
        // log group 3 snaplen 2048 prefix "copy"; the group is 16 bits.
        let log = data(&encode(&log_group(3, "copy", 2048)[0]));
        assert_eq!(attr(&log, 1), Some(&[0, 3][..]));
        assert_eq!(attr(&log, 3), Some(&[0, 0, 8, 0][..]));
        // limit rate 10/second burst 5 packets.
        let rate = data(&encode(&limit(10, Per::Second, 5)[0]));
        assert_eq!(attr(&rate, 1), Some(&10u64.to_be_bytes()[..]));
        assert_eq!(attr(&rate, 2), Some(&1u64.to_be_bytes()[..]));
        // jump.
        let jump = data(&encode(&Expr::Verdict(Verdict::Jump(
            "in-pod-1".to_string(),
        ))));
        let verdict = attr(attr(&jump, 2).unwrap(), NFTA_DATA_VERDICT).unwrap();
        assert_eq!(attr(verdict, 1), Some(&[0xff, 0xff, 0xff, 0xfd][..]));
        assert_eq!(attr(verdict, 2), Some(&b"in-pod-1\0"[..]));
    }

    fn nft(args: &[&str], input: Option<&str>) -> String {
        use std::io::Write;
        use std::process::{Command, Stdio};
        let program = std::env::var("NFT").unwrap_or_else(|_| "nft".to_string());
        let mut child = Command::new(program)
            .args(args)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::inherit())
            .spawn()
            .expect("nft, or $NFT");
        if let Some(input) = input {
            child
                .stdin
                .take()
                .unwrap()
                .write_all(input.as_bytes())
                .unwrap();
        }
        drop(child.stdin.take());
        let output = child.wait_with_output().unwrap();
        assert!(output.status.success(), "nft {args:?}");
        String::from_utf8(output.stdout).unwrap()
    }

    /// The pod network of ADR 0010 and 0012, roughly: what feosd writes.
    fn pod_network(name: &str) -> Table {
        let accept = || vec![Expr::Verdict(Verdict::Accept)];
        let drop = || vec![Expr::Verdict(Verdict::Drop)];
        let pod = "pod-1234abcd";
        let out = vec![
            [
                ipv6_daddr_in("deny"),
                limit(10, Per::Second, 5),
                log_group(2, "deny", 0),
            ]
            .concat(),
            [ipv6_daddr_in("deny"), drop()].concat(),
            [
                ct_original_packets(CmpOp::Lt, 5),
                l4proto(TCP),
                log_group(3, "copy", 2048),
            ]
            .concat(),
            [
                l4proto(TCP),
                th_dport(CmpOp::Eq, 443),
                ipv6_daddr_in("g-web"),
                accept(),
            ]
            .concat(),
            [
                l4proto(TCP),
                th_dport_range(8000, 8100),
                ipv6_lookup(IPV6_DADDR, "b-top", true),
                accept(),
            ]
            .concat(),
            [ipv6_daddr_in("internet"), vec![Expr::Counter], accept()].concat(),
            [ipv4_daddr_in("v4"), drop()].concat(),
            [l4proto(ICMP), ipv4_lookup(IPV4_DADDR, "v4", true), accept()].concat(),
            [limit(10, Per::Minute, 20), log_group(2, "no-rule", 0)].concat(),
            vec![Expr::Verdict(Verdict::Return)],
            drop(),
        ];
        let input = vec![
            [
                iifname_prefix(CmpOp::Eq, "pod-"),
                icmpv6_type(135),
                accept(),
            ]
            .concat(),
            [
                iifname_prefix(CmpOp::Eq, "pod-"),
                ipv6_daddr(CmpOp::Eq, "2001:db8:53::53".parse().unwrap()),
                l4proto(UDP),
                th_dport(CmpOp::Eq, 53),
                accept(),
            ]
            .concat(),
            [
                iifname_prefix(CmpOp::Eq, "pod-"),
                ipv6_daddr_prefix("2001:db8:53::".parse().unwrap(), 48),
                drop(),
            ]
            .concat(),
            [
                iifname_prefix(CmpOp::Eq, "pod-"),
                ipv4_daddr(CmpOp::Eq, Ipv4Addr::new(169, 254, 0, 53)),
                l4proto(TCP),
                th_dport(CmpOp::Eq, 53),
                accept(),
            ]
            .concat(),
            [
                iifname_prefix(CmpOp::Eq, "pod-"),
                ipv6_prefix(IPV6_SADDR, CmpOp::Neq, "fe80::".parse().unwrap(), 10),
                drop(),
            ]
            .concat(),
            [iifname_prefix(CmpOp::Eq, "pod-"), drop()].concat(),
        ];
        Table {
            family: Family::Inet,
            name: name.to_string(),
            sets: vec![
                Set::intervals(
                    "deny",
                    KeyType::Ipv6Addr,
                    vec![
                        Element::ipv6_prefix("fe80::".parse().unwrap(), 10),
                        Element::ipv6("::1".parse().unwrap()),
                        Element::ipv6_prefix("2001:db8::".parse().unwrap(), 32),
                    ],
                ),
                Set::intervals(
                    "g-web",
                    KeyType::Ipv6Addr,
                    vec![
                        Element::ipv6("fd00:fe05::5".parse().unwrap()),
                        Element::ipv6("fd00:fe05::7".parse().unwrap()),
                    ],
                ),
                Set::intervals(
                    "internet",
                    KeyType::Ipv6Addr,
                    vec![Element::ipv6_prefix(Ipv6Addr::UNSPECIFIED, 0)],
                ),
                Set::intervals(
                    "b-top",
                    KeyType::Ipv6Addr,
                    vec![Element::ipv6_range(
                        "8000::".parse().unwrap(),
                        Ipv6Addr::from(u128::MAX),
                    )],
                ),
                Set::intervals(
                    "v4",
                    KeyType::Ipv4Addr,
                    vec![
                        Element::ipv4_prefix(Ipv4Addr::new(10, 0, 0, 0), 8),
                        Element::ipv4(Ipv4Addr::new(192, 168, 1, 1)),
                    ],
                ),
                Set::keys("ports", KeyType::InetService, vec![Element::port(22)]),
                Set::verdict_map(
                    "pod-in",
                    KeyType::IfName,
                    vec![Element::ifname(pod).with_verdict(Verdict::Jump(format!("in-{pod}")))],
                ),
                Set::verdict_map(
                    "pod-out",
                    KeyType::IfName,
                    vec![Element::ifname(pod).with_verdict(Verdict::Goto(format!("out-{pod}")))],
                ),
            ],
            chains: vec![
                Chain::regular(
                    &format!("in-{pod}"),
                    vec![
                        [ipv4_saddr_in("v4"), drop()].concat(),
                        [ipv6_saddr_in("g-web"), accept()].concat(),
                        drop(),
                    ],
                ),
                Chain::regular(&format!("out-{pod}"), out),
                Chain::base(
                    "prerouting",
                    "filter",
                    Hook::Prerouting,
                    -150,
                    Policy::Accept,
                    vec![
                        [
                            iifname_prefix(CmpOp::Eq, "pod-"),
                            fib_saddr_iif_oif_missing(),
                            log_group(2, "spoofed", 0),
                            drop(),
                        ]
                        .concat(),
                        [iifname_prefix(CmpOp::Eq, "pod-"), icmp_type(5), drop()].concat(),
                    ],
                ),
                Chain::base("input", "filter", Hook::Input, 0, Policy::Accept, input),
                Chain::base(
                    "forward",
                    "filter",
                    Hook::Forward,
                    0,
                    Policy::Drop,
                    vec![
                        [ct_state(ct::ESTABLISHED | ct::RELATED), accept()].concat(),
                        iifname_vmap("pod-out"),
                        oifname_vmap("pod-in"),
                    ],
                ),
                Chain::base(
                    "postrouting",
                    "nat",
                    Hook::Postrouting,
                    100,
                    Policy::Accept,
                    vec![
                        [
                            ipv4_prefix(IPV4_SADDR, CmpOp::Eq, Ipv4Addr::new(10, 77, 0, 0), 16),
                            oifname(CmpOp::Eq, "eth0"),
                            vec![Expr::Masquerade],
                        ]
                        .concat(),
                        [
                            ipv4_prefix(IPV4_SADDR, CmpOp::Eq, Ipv4Addr::new(169, 254, 16, 0), 20),
                            oifname_prefix(CmpOp::Neq, "pod-"),
                            vec![Expr::Masquerade],
                        ]
                        .concat(),
                    ],
                ),
            ],
        }
    }

    /// The same table, as nft source.
    const POD_NETWORK: &str = r#"
table inet NAME {
  set deny { type ipv6_addr; flags interval; elements = { fe80::/10, ::1, 2001:db8::/32 }; }
  set g-web { type ipv6_addr; flags interval; elements = { fd00:fe05::5, fd00:fe05::7 }; }
  set internet { type ipv6_addr; flags interval; elements = { ::/0 }; }
  set b-top { type ipv6_addr; flags interval; elements = { 8000::-ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff }; }
  set v4 { type ipv4_addr; flags interval; elements = { 10.0.0.0/8, 192.168.1.1 }; }
  set ports { type inet_service; elements = { 22 }; }
  map pod-in { type ifname : verdict; elements = { "pod-1234abcd" : jump in-pod-1234abcd }; }
  map pod-out { type ifname : verdict; elements = { "pod-1234abcd" : goto out-pod-1234abcd }; }
  chain in-pod-1234abcd {
    ip saddr @v4 drop
    ip6 saddr @g-web accept
    drop
  }
  chain out-pod-1234abcd {
    ip6 daddr @deny limit rate 10/second burst 5 packets log prefix "deny" group 2
    ip6 daddr @deny drop
    ct original packets < 5 meta l4proto tcp log prefix "copy" group 3 snaplen 2048
    meta l4proto tcp th dport 443 ip6 daddr @g-web accept
    meta l4proto tcp th dport >= 8000 th dport <= 8100 ip6 daddr != @b-top accept
    ip6 daddr @internet counter accept
    ip daddr @v4 drop
    meta l4proto icmp ip daddr != @v4 accept
    limit rate 10/minute burst 20 packets log prefix "no-rule" group 2
    return
    drop
  }
  chain prerouting {
    type filter hook prerouting priority -150; policy accept;
    iifname "pod-*" fib saddr . iif oif missing log prefix "spoofed" group 2 drop
    iifname "pod-*" icmp type redirect drop
  }
  chain input {
    type filter hook input priority 0; policy accept;
    iifname "pod-*" icmpv6 type nd-neighbor-solicit accept
    iifname "pod-*" ip6 daddr 2001:db8:53::53 udp dport 53 accept
    iifname "pod-*" ip6 daddr 2001:db8:53::/48 drop
    iifname "pod-*" ip daddr 169.254.0.53 tcp dport 53 accept
    iifname "pod-*" ip6 saddr != fe80::/10 drop
    iifname "pod-*" drop
  }
  chain forward {
    type filter hook forward priority 0; policy drop;
    ct state established,related accept
    iifname vmap @pod-out
    oifname vmap @pod-in
  }
  chain postrouting {
    type nat hook postrouting priority 100; policy accept;
    ip saddr 10.77.0.0/16 oifname "eth0" masquerade
    ip saddr 169.254.16.0/20 oifname != "pod-*" masquerade
  }
}
"#;

    /// `nft list` of a table, under the name NAME.
    fn listing(name: &str) -> String {
        nft(&["list", "table", "inet", name], None).replace(name, "NAME")
    }

    /// Writes and deletes a table in the kernel; needs CAP_NET_ADMIN.
    #[test]
    #[ignore]
    fn the_kernel_takes_a_table() {
        replace(&table()).unwrap();
        replace(&table()).unwrap();
        assert_eq!(chains(Family::Inet, "feos-test").unwrap(), ["input"]);
        assert_eq!(rules(Family::Inet, "feos-test").unwrap(), ["input"]);
        assert!(sets(Family::Inet, "feos-test").unwrap().is_empty());
        // What is read is of the table alone.
        let other = pod_network("feos-test-other");
        replace(&other).unwrap();
        assert_eq!(chains(Family::Inet, "feos-test").unwrap(), ["input"]);
        let names: Vec<String> = other.sets.iter().map(|set| set.name.clone()).collect();
        let mut listed = sets(Family::Inet, "feos-test-other").unwrap();
        listed.sort();
        let mut expected = names.clone();
        expected.sort();
        assert_eq!(listed, expected);
        let counted = rules(Family::Inet, "feos-test-other").unwrap();
        for chain in &other.chains {
            let n = counted.iter().filter(|name| **name == chain.name).count();
            assert_eq!(n, chain.rules.len(), "{}", chain.name);
        }
        delete(Family::Inet, "feos-test-other").unwrap();
        delete(Family::Inet, "feos-test").unwrap();
        delete(Family::Inet, "feos-test").unwrap();
        assert!(chains(Family::Inet, "feos-test").unwrap().is_empty());
        assert!(sets(Family::Inet, "feos-test").unwrap().is_empty());
        assert!(rules(Family::Inet, "feos-test").unwrap().is_empty());
    }

    /// A timed set in the kernel: its elements go after their timeout,
    /// one added again stays for its timeout from then, even the same
    /// timeout, and a flush empties the set. Deleting an element that is
    /// not there fails, which tells whether it is. Needs CAP_NET_ADMIN.
    #[test]
    #[ignore]
    fn the_kernel_times_elements_out() {
        use std::thread::sleep;
        let name = "feos-test-timed";
        let address = || Element::ipv4(Ipv4Addr::new(192, 0, 2, 7));
        let timed = Set::timed("t4", KeyType::Ipv4Addr, Vec::new());
        let mut table = table();
        table.name = name.to_string();
        table.sets.push(timed);
        table.chains.push(Chain::regular(
            "out",
            vec![[
                ct_original_ipv4_daddr_in("t4"),
                vec![Expr::Verdict(Verdict::Return)],
            ]
            .concat()],
        ));
        replace(&table).unwrap();
        let add = |timeout: Duration| {
            update(
                &table,
                &[ElementOp::Add {
                    set: "t4".to_string(),
                    elements: vec![address().with_timeout(timeout)],
                }],
            )
        };
        let delete_it = || {
            update(
                &table,
                &[ElementOp::Delete {
                    set: "t4".to_string(),
                    elements: vec![address()],
                }],
            )
        };
        let two = Duration::from_secs(2);
        add(two).unwrap();
        sleep(Duration::from_millis(1200));
        add(two).unwrap();
        sleep(Duration::from_millis(1200));
        delete_it().expect("added again, it stays its timeout from then");
        add(Duration::from_millis(500)).unwrap();
        sleep(Duration::from_millis(1000));
        let gone = delete_it().unwrap_err();
        assert_eq!(gone.kind(), io::ErrorKind::NotFound, "{gone}");
        add(two).unwrap();
        update(
            &table,
            &[ElementOp::Flush {
                set: "t4".to_string(),
            }],
        )
        .unwrap();
        assert!(delete_it().is_err(), "flushed");
        // A whole table takes elements with their timeouts.
        let mut whole = table.clone();
        whole.sets[0].elements = vec![address().with_timeout(two)];
        replace(&whole).unwrap();
        delete_it().unwrap();
        delete(Family::Inet, name).unwrap();
    }

    /// Writes the pod network and changes its sets, as feosd and as nft
    /// do, and compares what nft reads back. Needs CAP_NET_ADMIN, in a
    /// network namespace of its own, and nft ($NFT).
    #[test]
    #[ignore]
    fn the_kernel_takes_the_pod_network_as_from_nft() {
        let mut table = pod_network("feos-test");
        replace(&table).unwrap();
        replace(&table).unwrap();
        delete(Family::Inet, "feos-test-nft").unwrap();
        nft(
            &["-f", "-"],
            Some(&POD_NETWORK.replace("NAME", "feos-test-nft")),
        );
        assert_eq!(listing("feos-test"), listing("feos-test-nft"));

        // A delta: a member comes, one goes, the deny list changes, a pod
        // goes and another comes.
        let ops = [
            ElementOp::Add {
                set: "g-web".to_string(),
                elements: vec![
                    Element::ipv6("fd00:fe05::6".parse().unwrap()),
                    Element::ipv6_prefix("2001:db8:1::".parse().unwrap(), 48),
                ],
            },
            ElementOp::Delete {
                set: "g-web".to_string(),
                elements: vec![Element::ipv6("fd00:fe05::5".parse().unwrap())],
            },
            ElementOp::Delete {
                set: "deny".to_string(),
                elements: vec![Element::ipv6_prefix("2001:db8::".parse().unwrap(), 32)],
            },
            ElementOp::Delete {
                set: "b-top".to_string(),
                elements: vec![Element::ipv6_range(
                    "8000::".parse().unwrap(),
                    Ipv6Addr::from(u128::MAX),
                )],
            },
            ElementOp::Delete {
                set: "pod-out".to_string(),
                elements: vec![Element::ifname("pod-1234abcd")],
            },
            ElementOp::Add {
                set: "pod-out".to_string(),
                elements: vec![Element::ifname("pod-5678abcd")
                    .with_verdict(Verdict::Goto("out-pod-1234abcd".to_string()))],
            },
            ElementOp::Delete {
                set: "v4".to_string(),
                elements: vec![Element::ipv4(Ipv4Addr::new(192, 168, 1, 1))],
            },
            ElementOp::Add {
                set: "v4".to_string(),
                elements: vec![Element::ipv4_range(
                    Ipv4Addr::new(169, 254, 0, 0),
                    Ipv4Addr::new(169, 255, 255, 255),
                )],
            },
        ];
        update(&table, &ops).unwrap();
        // Adding what is there changes nothing.
        update(&table, &ops[..1]).unwrap();
        nft(
            &["-f", "-"],
            Some(
                r#"
add element inet feos-test-nft g-web { fd00:fe05::6, 2001:db8:1::/48 }
delete element inet feos-test-nft g-web { fd00:fe05::5 }
delete element inet feos-test-nft deny { 2001:db8::/32 }
delete element inet feos-test-nft b-top { 8000::-ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff }
delete element inet feos-test-nft pod-out { "pod-1234abcd" }
add element inet feos-test-nft pod-out { "pod-5678abcd" : goto out-pod-1234abcd }
delete element inet feos-test-nft v4 { 192.168.1.1 }
add element inet feos-test-nft v4 { 169.254.0.0-169.255.255.255 }
"#,
            ),
        );
        let listed = listing("feos-test");
        assert_eq!(listed, listing("feos-test-nft"));
        let words = listed.split_whitespace().collect::<Vec<_>>().join(" ");
        assert!(words.contains("elements = { 2001:db8:1::/48, fd00:fe05::6, fd00:fe05::7 }"));
        assert!(words.contains("elements = { 10.0.0.0/8, 169.254.0.0/15 }"));

        // The kernel finds addresses in the intervals.
        let get = |set: &str, key: &str| {
            nft(
                &[
                    "get",
                    "element",
                    "inet",
                    "feos-test",
                    set,
                    &format!("{{ {key} }}"),
                ],
                None,
            )
        };
        assert!(get("internet", "2a00::1").contains("::/0"));
        assert!(get("g-web", "fd00:fe05::6").contains("fd00:fe05::6"));
        assert!(get("deny", "fe80::1").contains("fe80::/10"));
        assert!(get("v4", "169.254.169.254").contains("169.254.0.0/15"));

        // A delete of what is not there fails, and the batch with it.
        let delete_one = |address: &str| ElementOp::Delete {
            set: "g-web".to_string(),
            elements: vec![Element::ipv6(address.parse().unwrap())],
        };
        let error = update(
            &table,
            &[delete_one("fd00:fe05::6"), delete_one("fd00:fe05::5")],
        )
        .unwrap_err();
        assert_eq!(error.kind(), io::ErrorKind::NotFound);
        assert!(error.to_string().contains("delete elements of set g-web"));
        assert_eq!(listing("feos-test"), listed);

        // Replacing the table brings its sets back as they were.
        replace(&table).unwrap();
        delete(Family::Inet, "feos-test-nft").unwrap();
        nft(
            &["-f", "-"],
            Some(&POD_NETWORK.replace("NAME", "feos-test-nft")),
        );
        assert_eq!(listing("feos-test"), listing("feos-test-nft"));

        // An error names the message.
        table.chains[0]
            .rules
            .push(vec![Expr::Verdict(Verdict::Jump("nowhere".to_string()))]);
        let error = replace(&table).unwrap_err();
        assert!(
            error
                .to_string()
                .contains("rule 4 of chain in-pod-1234abcd"),
            "{error}"
        );

        delete(Family::Inet, "feos-test").unwrap();
        delete(Family::Inet, "feos-test-nft").unwrap();
    }

    /// A node full of pods: a batch of megabytes, sets of thousands.
    #[test]
    #[ignore]
    fn the_kernel_takes_large_batches() {
        let mut table = pod_network("feos-test");
        let template = table.chains[1].rules.clone();
        for i in 0..1000 {
            table.chains.push(Chain::regular(
                &format!("out-pod-{i:08x}"),
                template.clone(),
            ));
        }
        table.sets[1].elements = (1..=20000u128)
            .map(|i| Element::ipv6(Ipv6Addr::from((0xfd00 << 112) | (i * 2))))
            .collect();
        let batch = replace_batch(&table, 1).unwrap();
        assert!(batch.buf.len() > 2 << 20, "{}", batch.buf.len());
        replace(&table).unwrap();
        let listed = nft(&["list", "set", "inet", "feos-test", "g-web"], None);
        assert_eq!(listed.matches("fd00::").count(), 20000);
        let ops: Vec<_> = (1..=20000u128)
            .step_by(2)
            .map(|i| ElementOp::Delete {
                set: "g-web".to_string(),
                elements: vec![Element::ipv6(Ipv6Addr::from((0xfd00 << 112) | (i * 2)))],
            })
            .collect();
        update(&table, &ops).unwrap();
        let listed = nft(&["list", "set", "inet", "feos-test", "g-web"], None);
        assert_eq!(listed.matches("fd00::").count(), 10000);
        delete(Family::Inet, "feos-test").unwrap();
    }
}
