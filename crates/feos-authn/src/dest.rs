// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Where a binding may connect (ADR 0014, 0028 §3): the upstream's name is
//! resolved, every address it resolves to is checked against the node's
//! deny prefixes (`feosd`'s `--deny-prefix` and its own networks, from its
//! `hello`), and the checked address is dialed from the pod's second
//! address. One denied address refuses the name: a name that answers with
//! both can be made to answer with the denied one only.
//!
//! Names are resolved with the system's resolver (getaddrinfo), which
//! reads `/etc/resolv.conf` in the process's mount namespace.

use std::fmt;
use std::future::Future;
use std::io;
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr};
use std::os::fd::AsRawFd;
use std::pin::Pin;
use std::str::FromStr;
use std::sync::{Arc, RwLock};
use std::time::Duration;

use tokio::net::{TcpSocket, TcpStream};

const RESOLVE_TIMEOUT: Duration = Duration::from_secs(5);
const CONNECT_TIMEOUT: Duration = Duration::from_secs(10);

/// An IP prefix, "10.0.0.0/8" or "fd00::/8"; an address alone is a host
/// prefix.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Prefix {
    addr: IpAddr,
    len: u8,
}

impl FromStr for Prefix {
    type Err = String;

    fn from_str(s: &str) -> Result<Prefix, String> {
        let (a, len) = match s.split_once('/') {
            Some((a, l)) => (a, Some(l)),
            None => (s, None),
        };
        let addr: IpAddr = a.parse().map_err(|_| format!("{s:?} is not a prefix"))?;
        let max = if addr.is_ipv4() { 32 } else { 128 };
        let len = match len {
            Some(l) => l
                .parse::<u8>()
                .ok()
                .filter(|&l| l <= max)
                .ok_or_else(|| format!("{s:?} is not a prefix"))?,
            None => max,
        };
        Ok(Prefix { addr, len })
    }
}

impl fmt::Display for Prefix {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}/{}", self.addr, self.len)
    }
}

impl Prefix {
    pub fn contains(&self, a: IpAddr) -> bool {
        match (self.addr, a) {
            (IpAddr::V4(p), IpAddr::V4(a)) => {
                let mask = u32::MAX.checked_shl(32 - u32::from(self.len)).unwrap_or(0);
                u32::from(p) & mask == u32::from(a) & mask
            }
            (IpAddr::V6(p), IpAddr::V6(a)) => {
                let mask = u128::MAX
                    .checked_shl(128 - u32::from(self.len))
                    .unwrap_or(0);
                u128::from(p) & mask == u128::from(a) & mask
            }
            _ => false,
        }
    }
}

/// An IPv4 address mapped into IPv6 is that IPv4 address.
pub fn canonical(a: IpAddr) -> IpAddr {
    match a {
        IpAddr::V6(v6) => v6.to_ipv4_mapped().map(IpAddr::V4).unwrap_or(a),
        a => a,
    }
}

/// Judges one address: Ok, or why not. Besides the deny prefixes, an
/// address that is no unicast destination at all (unspecified, "this
/// network", broadcast, multicast) is never dialed, nor one of the node's
/// own (loopback, link-local: the node's services at 169.254.169.1 and
/// its resolver among them), IPv4-mapped ones too: feos-authn runs in the
/// node's network namespace.
pub fn check(a: IpAddr, deny: &[Prefix]) -> Result<(), String> {
    check_address(a, deny, false)
}

/// [`check`], with the loopback address allowed for `loopback_ok`.
fn check_address(a: IpAddr, deny: &[Prefix], loopback_ok: bool) -> Result<(), String> {
    let a = canonical(a);
    let never = match a {
        IpAddr::V4(v4) => v4.octets()[0] == 0 || v4.is_broadcast() || v4.is_multicast(),
        IpAddr::V6(v6) => v6.is_unspecified() || v6.is_multicast(),
    };
    if never {
        return Err(format!("{a} is not a unicast address"));
    }
    let node = match a {
        IpAddr::V4(v4) => (v4.is_loopback() && !loopback_ok) || v4.is_link_local(),
        IpAddr::V6(v6) => (v6.is_loopback() && !loopback_ok) || v6.is_unicast_link_local(),
    };
    if node {
        return Err(format!("{a} is a loopback or link-local address"));
    }
    match deny.iter().find(|p| p.contains(a)) {
        Some(p) => Err(format!("{a} is an internal address ({p})")),
        None => Ok(()),
    }
}

/// What `feosd` says about the node's network, in its `hello`.
#[derive(Clone, Debug, Default)]
pub struct NetConfig {
    pub deny: Vec<Prefix>,
    /// The pod's second addresses, which a binding's connections leave
    /// from; none: the connection is not bound.
    pub source_v4: Option<Ipv4Addr>,
    pub source_v6: Option<Ipv6Addr>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ConnectErrorKind {
    /// The destination rule refused the address (403 denied_destination).
    Denied,
    /// No address, or none that answered (502 unreachable).
    Unreachable,
    /// The TLS handshake or the server's certificate failed (502 tls).
    Tls,
}

#[derive(Clone, Debug)]
pub struct ConnectError {
    pub kind: ConnectErrorKind,
    pub message: String,
}

impl ConnectError {
    pub fn new(kind: ConnectErrorKind, message: impl Into<String>) -> ConnectError {
        ConnectError {
            kind,
            message: message.into(),
        }
    }
}

impl fmt::Display for ConnectError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.message)
    }
}

impl std::error::Error for ConnectError {}

pub type ResolveFuture = Pin<Box<dyn Future<Output = io::Result<Vec<SocketAddr>>> + Send>>;

/// Resolves a host and port to the addresses to try.
pub trait Resolve: Send + Sync + 'static {
    fn resolve(&self, host: &str, port: u16) -> ResolveFuture;
}

/// getaddrinfo, on tokio's blocking pool.
pub struct SystemResolver;

impl Resolve for SystemResolver {
    fn resolve(&self, host: &str, port: u16) -> ResolveFuture {
        let host = host.to_string();
        Box::pin(async move {
            Ok(tokio::net::lookup_host((host.as_str(), port))
                .await?
                .collect())
        })
    }
}

/// The network side of every binding: `feosd`'s configuration, which a new
/// `hello` replaces, and the resolver. Until the first `hello` nothing is
/// dialed.
pub struct Net {
    config: RwLock<Option<Arc<NetConfig>>>,
    resolver: Arc<dyn Resolve>,
    /// Upstreams on the loopback address are dialed: for tests alone.
    loopback_ok: bool,
}

impl Net {
    pub fn new(resolver: Arc<dyn Resolve>) -> Net {
        Net {
            config: RwLock::new(None),
            resolver,
            loopback_ok: false,
        }
    }

    /// A network whose upstreams may be on the loopback address, as a
    /// test's are; never in a pod's process.
    pub fn allowing_loopback(resolver: Arc<dyn Resolve>) -> Net {
        Net {
            loopback_ok: true,
            ..Net::new(resolver)
        }
    }

    pub fn set(&self, c: Option<NetConfig>) {
        *self.config.write().unwrap_or_else(|e| e.into_inner()) = c.map(Arc::new);
    }

    pub fn config(&self) -> Option<Arc<NetConfig>> {
        self.config
            .read()
            .unwrap_or_else(|e| e.into_inner())
            .clone()
    }

    /// Resolves `host`, checks every address and connects to the first
    /// checked one that answers.
    pub async fn dial(
        &self,
        host: &str,
        port: u16,
    ) -> Result<(TcpStream, SocketAddr), ConnectError> {
        use ConnectErrorKind::*;
        let cfg = self.config().ok_or_else(|| {
            ConnectError::new(Unreachable, "the node has not configured the network yet")
        })?;
        let addrs = match host.parse::<IpAddr>() {
            Ok(ip) => vec![SocketAddr::new(ip, port)],
            Err(_) => tokio::time::timeout(RESOLVE_TIMEOUT, self.resolver.resolve(host, port))
                .await
                .map_err(|_| ConnectError::new(Unreachable, format!("resolve {host}: timed out")))?
                .map_err(|e| ConnectError::new(Unreachable, format!("resolve {host}: {e}")))?,
        };
        if addrs.is_empty() {
            return Err(ConnectError::new(
                Unreachable,
                format!("{host} has no address"),
            ));
        }
        for a in &addrs {
            check_address(a.ip(), &cfg.deny, self.loopback_ok)
                .map_err(|why| ConnectError::new(Denied, format!("{host}: {why}")))?;
        }
        let mut last = String::new();
        for a in addrs {
            let a = SocketAddr::new(canonical(a.ip()), a.port());
            match tokio::time::timeout(CONNECT_TIMEOUT, connect_from(a, &cfg)).await {
                Ok(Ok(s)) => return Ok((s, a)),
                Ok(Err(e)) => last = format!("{a}: {e}"),
                Err(_) => last = format!("{a}: timed out"),
            }
        }
        Err(ConnectError::new(
            Unreachable,
            format!("connect to {host}: {last}"),
        ))
    }
}

/// Connects from the pod's address of the destination's family, when
/// `feosd` named one. The port is the kernel's choice at connect
/// (IP_BIND_ADDRESS_NO_PORT), so binding does not use up ports.
async fn connect_from(a: SocketAddr, cfg: &NetConfig) -> io::Result<TcpStream> {
    let (socket, source) = match a {
        SocketAddr::V4(_) => (TcpSocket::new_v4()?, cfg.source_v4.map(IpAddr::V4)),
        SocketAddr::V6(_) => (TcpSocket::new_v6()?, cfg.source_v6.map(IpAddr::V6)),
    };
    if let Some(src) = source {
        let one: libc::c_int = 1;
        // SAFETY: setsockopt on a socket this function owns, with a
        // pointer to an int and its size.
        let r = unsafe {
            libc::setsockopt(
                socket.as_raw_fd(),
                libc::IPPROTO_IP,
                libc::IP_BIND_ADDRESS_NO_PORT,
                &one as *const libc::c_int as *const libc::c_void,
                std::mem::size_of::<libc::c_int>() as libc::socklen_t,
            )
        };
        if r != 0 {
            return Err(io::Error::last_os_error());
        }
        socket.bind(SocketAddr::new(src, 0))?;
    }
    let s = socket.connect(a).await?;
    s.set_nodelay(true)?;
    Ok(s)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn deny(list: &[&str]) -> Vec<Prefix> {
        list.iter().map(|p| p.parse().unwrap()).collect()
    }

    #[test]
    fn prefixes() {
        let p: Prefix = "10.0.0.0/8".parse().unwrap();
        assert!(p.contains("10.255.0.1".parse().unwrap()));
        assert!(!p.contains("11.0.0.1".parse().unwrap()));
        assert!(!p.contains("::ffff:10.0.0.1".parse().unwrap()));
        let p: Prefix = "fd00::/8".parse().unwrap();
        assert!(p.contains("fd12::1".parse().unwrap()));
        assert!(!p.contains("fe80::1".parse().unwrap()));
        let all: Prefix = "0.0.0.0/0".parse().unwrap();
        assert!(all.contains("192.0.2.1".parse().unwrap()));
        let host: Prefix = "192.0.2.7".parse().unwrap();
        assert!(host.contains("192.0.2.7".parse().unwrap()));
        assert!(!host.contains("192.0.2.8".parse().unwrap()));
        for bad in ["10.0.0.0/33", "fd00::/129", "x/8", "10.0.0.0/"] {
            assert!(bad.parse::<Prefix>().is_err(), "{bad}");
        }
    }

    #[test]
    fn check_denies_prefixes_and_mapped_addresses() {
        let d = deny(&["10.0.0.0/8", "169.254.0.0/16", "fd00::/8"]);
        assert!(check("192.0.2.1".parse().unwrap(), &d).is_ok());
        assert!(check("2001:db8::1".parse().unwrap(), &d).is_ok());
        assert!(check("10.1.2.3".parse().unwrap(), &d).is_err());
        assert!(check("::ffff:10.1.2.3".parse().unwrap(), &d).is_err());
        assert!(check("169.254.169.254".parse().unwrap(), &d).is_err());
        assert!(check("fd5e::1".parse().unwrap(), &d).is_err());
        for never in [
            "0.0.0.0",
            "0.1.2.3",
            "255.255.255.255",
            "224.0.0.1",
            "::",
            "ff02::1",
        ] {
            assert!(check(never.parse().unwrap(), &[]).is_err(), "{never}");
        }
    }

    #[test]
    fn the_nodes_own_addresses_are_never_dialed() {
        for node in [
            "127.0.0.1",
            "127.53.0.1",
            "::1",
            "169.254.169.1",
            "169.254.169.254",
            "169.254.0.53",
            "fe80::1",
            "febf::1",
            "::ffff:127.0.0.1",
            "::ffff:169.254.169.1",
        ] {
            let a: IpAddr = node.parse().unwrap();
            assert!(check(a, &[]).is_err(), "{node}");
            // A test's loopback upstream: link-local stays refused.
            let loopback = canonical(a).is_loopback();
            assert_eq!(check_address(a, &[], true).is_ok(), loopback, "{node}");
        }
        assert!(check("fec0::1".parse().unwrap(), &[]).is_ok());
    }

    struct Fixed(Vec<SocketAddr>);

    impl Resolve for Fixed {
        fn resolve(&self, _host: &str, _port: u16) -> ResolveFuture {
            let v = self.0.clone();
            Box::pin(async move { Ok(v) })
        }
    }

    #[tokio::test]
    async fn one_denied_address_refuses_the_name() {
        let l = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let ok = l.local_addr().unwrap();
        // Strict: the loopback address is the node's.
        let net = Net::new(Arc::new(Fixed(vec![ok])));
        net.set(Some(NetConfig::default()));
        let e = net.dial("api.example", ok.port()).await.unwrap_err();
        assert_eq!(e.kind, ConnectErrorKind::Denied);
        let net =
            Net::allowing_loopback(Arc::new(Fixed(vec![ok, "10.0.0.1:443".parse().unwrap()])));
        // Nothing is dialed before feosd's hello.
        let e = net.dial("api.example", 443).await.unwrap_err();
        assert_eq!(e.kind, ConnectErrorKind::Unreachable);
        net.set(Some(NetConfig {
            deny: deny(&["10.0.0.0/8"]),
            ..Default::default()
        }));
        let e = net.dial("api.example", 443).await.unwrap_err();
        assert_eq!(e.kind, ConnectErrorKind::Denied);
        // An address literal is checked too.
        let e = net.dial("10.0.0.1", 443).await.unwrap_err();
        assert_eq!(e.kind, ConnectErrorKind::Denied);
        // The checked address is dialed, from the given source.
        let net = Net::allowing_loopback(Arc::new(Fixed(vec![ok])));
        net.set(Some(NetConfig {
            deny: deny(&["10.0.0.0/8"]),
            source_v4: Some("127.0.0.1".parse().unwrap()),
            source_v6: None,
        }));
        let (s, a) = net.dial("api.example", ok.port()).await.unwrap();
        assert_eq!(a, ok);
        assert_eq!(
            s.local_addr().unwrap().ip(),
            "127.0.0.1".parse::<IpAddr>().unwrap()
        );
    }

    #[tokio::test]
    async fn unreachable_when_nothing_answers() {
        // A port that was just free.
        let l = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = l.local_addr().unwrap();
        drop(l);
        let net = Net::allowing_loopback(Arc::new(Fixed(vec![addr])));
        net.set(Some(NetConfig::default()));
        let e = net.dial("api.example", addr.port()).await.unwrap_err();
        assert_eq!(e.kind, ConnectErrorKind::Unreachable);
        let net = Net::new(Arc::new(Fixed(vec![])));
        net.set(Some(NetConfig::default()));
        let e = net.dial("api.example", 443).await.unwrap_err();
        assert_eq!(e.kind, ConnectErrorKind::Unreachable);
    }
}
