// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! feosd's resolver for its pods (ADR 0010): it answers the names of a
//! pod's name tables itself, and forwards the other queries to the
//! upstream resolvers that a pod names, or the node's.
//!
//! It answers the pods it knows, told apart by the source address of a
//! query, which the pod's TAP pins, and refuses everyone else. A name of
//! one label, in any case, that the pod's name tables hold (ADR 0018) is
//! answered from them: its IPv6 addresses to AAAA, its IPv4 addresses to
//! A, nothing to the other types, with a TTL of a few seconds, and never
//! asked upstream. A name whose addresses include the pod's own is the
//! pod itself (an attached service in its own sandbox, ADR 0035): it is
//! answered with the loopback addresses, `127.0.0.1` to A and `::1` to
//! AAAA, so that a process that listens on loopback alone is reached by
//! its name inside its pod. It forwards any other query to the pod's upstreams in
//! order, to the next when one does not answer, and answers SERVFAIL when
//! none does. Answers pass as the upstream gave them, but HTTPS and SVCB
//! records lose `ech`, so that clients send the server name in the clear
//! (ADR 0013). Every query of a pod that is answered becomes a traffic
//! record (ADR 0013).
//!
//! Before its name tables, a pod's HTTP bindings (ADR 0028): the names
//! of its bindings, a label each in the name mode and the upstream's host
//! name in the intercept mode, are answered with the node's address for
//! them (`Config::http_address`), A alone, and an empty answer for every
//! other type, with a short TTL. Every other pod resolves them as before.
//!
//! A pod whose egress is restricted by name (ADR 0034, revised;
//! [`Egress::Names`]) is answered upstream names only when they are in its
//! egress: an exact name, or one strictly below a `*.` suffix. Every other
//! name it asks upstream is answered REFUSED and recorded so. The
//! addresses of an allowed answer, A and AAAA, are handed to the
//! resolver's [`Admit`] before the pod has the answer, each for its TTL,
//! at least [`MIN_ADMIT`] and at most [`MAX_ADMIT`]: what fills the pod's
//! egress set. The names
//! of its name tables and of its HTTP bindings are answered as before.
//!
//! feosd binds the sockets and hands them over, so that they can come from
//! socket activation and outlive feosd.

mod svcb;
#[cfg(test)]
mod tests;
mod upstream;

use feos_proto::network_service::{traffic_record, DnsQuery, TrafficRecord};
use hickory_proto::op::{Edns, Message, MessageType, OpCode, Query, ResponseCode};
use hickory_proto::rr::rdata::{A, AAAA};
use hickory_proto::rr::{DNSClass, Name, RData, Record, RecordType};
use log::{debug, warn};
use std::collections::{HashMap, HashSet};
use std::io;
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr};
use std::sync::{Arc, PoisonError, RwLock};
use std::time::{Duration, SystemTime};
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream, UdpSocket};
use tokio::sync::{broadcast, Mutex, OwnedSemaphorePermit, Semaphore};
use tokio::task::JoinSet;
use tokio::time;

/// The resolver's address on every node (ADR 0010).
pub const ADDRESS: Ipv4Addr = Ipv4Addr::new(169, 254, 0, 53);
/// How long an upstream has to answer over UDP, by default.
pub const UPSTREAM_TIMEOUT: Duration = Duration::from_secs(2);

/// The largest UDP message the resolver sends, and asks upstreams for
/// (DNS flag day 2020).
const UDP_PAYLOAD: u16 = 1232;
const HEADER_LENGTH: usize = 12;
/// The largest query read from UDP.
const QUERY_BUFFER: usize = 4096;
/// Queries in flight, of all pods and of one.
const MAX_QUERIES: usize = 512;
const MAX_CLIENT_QUERIES: usize = 64;
/// TCP connections, of all pods and of one.
const MAX_CONNECTIONS: usize = 128;
const MAX_CLIENT_CONNECTIONS: usize = 8;
/// Queries in flight on one TCP connection.
const MAX_CONNECTION_QUERIES: usize = 16;
/// How long a TCP connection may take to send a query, and to take an
/// answer.
const TCP_TIMEOUT: Duration = Duration::from_secs(10);
/// Records kept for subscribers that fall behind.
const RECORDS: usize = 1024;
/// How long the answers from name tables may be kept: their addresses
/// change as replicas come and go.
pub const NAME_TTL: u32 = 5;
/// How long the answers for HTTP bindings' names may be kept.
pub const HTTP_TTL: u32 = 5;
/// How long an address of an allowed name is admitted at least: programs
/// keep addresses longer than their TTL (ADR 0034, revised).
pub const MIN_ADMIT: Duration = Duration::from_secs(300);
/// How long it is admitted at most, whatever TTL an upstream gives: a
/// day.
pub const MAX_ADMIT: Duration = Duration::from_secs(86_400);

/// The addresses of a name, one label in any case, in the first of the
/// name tables of a pod that holds it; None when none does.
pub type Names = Arc<dyn Fn(&[String], &str) -> Option<Vec<IpAddr>> + Send + Sync>;

/// Takes the addresses an allowed name of a pod, by its ID, was answered
/// with, each with how long it is admitted. It runs on a blocking thread,
/// and the pod has its answer once it returns.
pub type Admit = Arc<dyn Fn(&str, &[(IpAddr, Duration)]) + Send + Sync>;

/// Which upstream names a pod is answered (ADR 0034, revised).
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub enum Egress {
    /// Every name, as before.
    #[default]
    Any,
    /// These alone: exact names, or `*.example.com` for every name below
    /// `example.com`, not the name itself. In any case, with or without
    /// the final dot. None: no upstream name at all.
    Names(Vec<String>),
}

/// A pod's egress names, as they are looked up.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
enum Allowed {
    #[default]
    Any,
    Names {
        exact: HashSet<String>,
        /// The suffixes of wildcards, with their leading dot.
        below: Vec<String>,
    },
}

impl Allowed {
    fn new(egress: &Egress) -> Self {
        let Egress::Names(names) = egress else {
            return Self::Any;
        };
        let (mut exact, mut below) = (HashSet::new(), Vec::new());
        for name in names {
            let name = name.trim_end_matches('.').to_ascii_lowercase();
            match name.strip_prefix('*') {
                Some(suffix) if suffix.starts_with('.') && suffix.len() > 1 => {
                    below.push(suffix.to_string());
                }
                Some(_) => {}
                None if !name.is_empty() => {
                    exact.insert(name);
                }
                None => {}
            }
        }
        Self::Names { exact, below }
    }

    fn restricted(&self) -> bool {
        matches!(self, Self::Names { .. })
    }

    /// Whether `name`, as records have it, may be asked upstream.
    fn allows(&self, name: &str) -> bool {
        match self {
            Self::Any => true,
            Self::Names { exact, below } => {
                exact.contains(name)
                    || below
                        .iter()
                        .any(|suffix| name.len() > suffix.len() && name.ends_with(suffix.as_str()))
            }
        }
    }
}

/// Checks a name of a pod's egress: a DNS name, or one after `*.`; in any
/// case, with or without the final dot.
pub fn check_egress_name(name: &str) -> Result<(), String> {
    let bare = name.strip_suffix('.').unwrap_or(name);
    let bare = bare.strip_prefix("*.").unwrap_or(bare);
    let label = |label: &str| {
        (1..=63).contains(&label.len())
            && !label.starts_with('-')
            && !label.ends_with('-')
            && label
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
    };
    if bare.is_empty() || bare.len() > 253 || !bare.split('.').all(label) {
        return Err(format!(
            "egress name {name:?} is not a DNS name, or one after \"*.\""
        ));
    }
    Ok(())
}

/// What the resolver does for all pods.
#[derive(Clone, Debug)]
pub struct Config {
    /// The upstreams of pods that name none.
    pub default_upstreams: Vec<SocketAddr>,
    /// How long an upstream has to answer over UDP before the next is
    /// asked; over TCP, twice that.
    pub upstream_timeout: Duration,
    /// Where the pods reach their HTTP bindings (ADR 0028): the node's
    /// address their names are answered with; none answers them.
    pub http_address: Option<Ipv4Addr>,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            default_upstreams: Vec::new(),
            upstream_timeout: UPSTREAM_TIMEOUT,
            http_address: None,
        }
    }
}

/// A pod's HTTP bindings' names, as the resolver answers them (ADR 0028):
/// the name mode's labels and the intercept mode's host names.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct HttpNames {
    pub names: Vec<String>,
    pub hosts: Vec<String>,
}

impl HttpNames {
    /// The names as they are looked up: in lower case, without the final
    /// dot.
    fn lookup(&self) -> HashSet<String> {
        self.names
            .iter()
            .chain(&self.hosts)
            .map(|name| name.trim_end_matches('.').to_ascii_lowercase())
            .filter(|name| !name.is_empty())
            .collect()
    }
}

/// The resolver. It serves the sockets it is given until the future of
/// `serve` is dropped.
pub struct Resolver {
    config: Config,
    clients: RwLock<HashMap<IpAddr, Arc<Client>>>,
    /// Where the names of the pods' tables are looked up.
    names: RwLock<Option<Names>>,
    /// What takes the addresses of the allowed names of restricted pods.
    admit: RwLock<Option<Admit>>,
    records: broadcast::Sender<TrafficRecord>,
    queries: Arc<Semaphore>,
    connections: Arc<Semaphore>,
}

/// A pod, as the resolver knows it.
struct Client {
    pod_id: String,
    /// Where its upstreams are asked; empty: the default upstreams.
    upstreams: Vec<SocketAddr>,
    /// The name tables it names, in order.
    tables: Vec<String>,
    /// Its own addresses: a name that holds one is answered with loopback.
    own: Vec<IpAddr>,
    /// Its HTTP bindings' names, in lower case without the final dot.
    http: HashSet<String>,
    /// The upstream names it is answered.
    egress: Allowed,
    queries: Arc<Semaphore>,
    connections: Arc<Semaphore>,
}

#[derive(Clone, Copy, Debug, PartialEq)]
enum Transport {
    Udp,
    Tcp,
}

/// What a query of a pod holds while it is in flight.
type Permits = (OwnedSemaphorePermit, Option<OwnedSemaphorePermit>);

impl Resolver {
    pub fn new(config: Config) -> Arc<Self> {
        let (records, _) = broadcast::channel(RECORDS);
        Arc::new(Self {
            config,
            clients: RwLock::default(),
            names: RwLock::default(),
            admit: RwLock::default(),
            records,
            queries: Arc::new(Semaphore::new(MAX_QUERIES)),
            connections: Arc::new(Semaphore::new(MAX_CONNECTIONS)),
        })
    }

    /// Serves the pod `pod_id` at `address`, which its queries come from:
    /// its IPv4 address. Answers the names of its name tables `tables`,
    /// and asks `upstreams`, or the default upstreams when there are none,
    /// the rest, as far as `egress` allows. `ipv6` is the pod's IPv6
    /// address, if it has one: with `address`, what its names answer with
    /// loopback. Replaces what the resolver knew of `address`.
    pub fn set_client(
        &self,
        address: IpAddr,
        ipv6: Option<Ipv6Addr>,
        pod_id: &str,
        upstreams: Vec<SocketAddr>,
        tables: Vec<String>,
        egress: &Egress,
    ) {
        let address = address.to_canonical();
        let own = std::iter::once(address)
            .chain(ipv6.map(IpAddr::V6))
            .collect();
        let mut clients = self.clients.write().unwrap_or_else(PoisonError::into_inner);
        // A pod that is set again keeps its limits.
        let (queries, connections) = match clients.get(&address) {
            Some(client) => (client.queries.clone(), client.connections.clone()),
            None => (
                Arc::new(Semaphore::new(MAX_CLIENT_QUERIES)),
                Arc::new(Semaphore::new(MAX_CLIENT_CONNECTIONS)),
            ),
        };
        // And its HTTP bindings' names, which `set_http` sets.
        let http = clients
            .get(&address)
            .filter(|client| client.pod_id == pod_id)
            .map(|client| client.http.clone())
            .unwrap_or_default();
        let client = Client {
            pod_id: pod_id.to_string(),
            upstreams,
            tables,
            own,
            http,
            egress: Allowed::new(egress),
            queries,
            connections,
        };
        clients.insert(address, Arc::new(client));
    }

    /// Answers the names of the HTTP bindings `http` to the pod at
    /// `address` from now on, in place of those it had; a pod the
    /// resolver does not serve yet gets them when it is set.
    pub fn set_http(&self, address: IpAddr, pod_id: &str, http: &HttpNames) {
        let address = address.to_canonical();
        let mut clients = self.clients.write().unwrap_or_else(PoisonError::into_inner);
        let Some(client) = clients.get(&address).filter(|c| c.pod_id == pod_id) else {
            return;
        };
        let client = Client {
            pod_id: client.pod_id.clone(),
            upstreams: client.upstreams.clone(),
            tables: client.tables.clone(),
            own: client.own.clone(),
            http: http.lookup(),
            egress: client.egress.clone(),
            queries: client.queries.clone(),
            connections: client.connections.clone(),
        };
        clients.insert(address, Arc::new(client));
    }

    /// Looks the names of the pods' tables up in `names` from now on;
    /// until then, no pod has any.
    pub fn set_names(
        &self,
        names: impl Fn(&[String], &str) -> Option<Vec<IpAddr>> + Send + Sync + 'static,
    ) {
        *self.names.write().unwrap_or_else(PoisonError::into_inner) = Some(Arc::new(names));
    }

    /// Hands the addresses of the allowed names of restricted pods to
    /// `admit` from now on; until then, they go nowhere.
    pub fn set_admit(&self, admit: impl Fn(&str, &[(IpAddr, Duration)]) + Send + Sync + 'static) {
        *self.admit.write().unwrap_or_else(PoisonError::into_inner) = Some(Arc::new(admit));
    }

    /// Stops serving the pod at `address`: its queries are refused from
    /// now on.
    pub fn remove_client(&self, address: IpAddr) {
        self.clients
            .write()
            .unwrap_or_else(PoisonError::into_inner)
            .remove(&address.to_canonical());
    }

    /// The pods' queries from now on, one record for each that is
    /// answered. A receiver that falls behind misses records, and learns
    /// how many.
    pub fn subscribe(&self) -> broadcast::Receiver<TrafficRecord> {
        self.records.subscribe()
    }

    /// Serves queries on `udp` and `tcp`, both bound already, until the
    /// future is dropped. Returns only on an error the sockets do not
    /// recover from.
    pub async fn serve(self: Arc<Self>, udp: UdpSocket, tcp: TcpListener) -> io::Result<()> {
        tokio::try_join!(self.clone().serve_udp(udp), self.serve_tcp(tcp))?;
        Ok(())
    }

    async fn serve_udp(self: Arc<Self>, socket: UdpSocket) -> io::Result<()> {
        let socket = Arc::new(socket);
        let mut queries = JoinSet::new();
        let mut buffer = vec![0; QUERY_BUFFER];
        loop {
            while queries.try_join_next().is_some() {}
            let (length, peer) = match socket.recv_from(&mut buffer).await {
                Ok(received) => received,
                Err(error) => {
                    pause(error).await?;
                    continue;
                }
            };
            let client = self.client(source_address(peer));
            // Over its limits, a query is dropped; the client asks again.
            let Some(permits) = self.try_permits(client.as_deref()) else {
                debug!("dns: too many queries in flight, dropped one from {peer}");
                continue;
            };
            let query = buffer[..length].to_vec();
            let (resolver, socket) = (self.clone(), socket.clone());
            queries.spawn(async move {
                let _permits = permits;
                let answer = resolver
                    .answer(&query, client.as_deref(), Transport::Udp)
                    .await;
                if let Some(answer) = answer
                    && let Err(error) = socket.send_to(&answer, peer).await
                {
                    debug!("dns: answering {peer}: {error}");
                }
            });
        }
    }

    async fn serve_tcp(self: Arc<Self>, listener: TcpListener) -> io::Result<()> {
        let mut connections = JoinSet::new();
        loop {
            while connections.try_join_next().is_some() {}
            let (stream, peer) = match listener.accept().await {
                Ok(accepted) => accepted,
                Err(error) => {
                    pause(error).await?;
                    continue;
                }
            };
            let source = source_address(peer);
            let Some(permits) = self.try_connection(source) else {
                debug!("dns: too many connections, closed one from {peer}");
                continue;
            };
            connections.spawn(self.clone().serve_connection(stream, source, permits));
        }
    }

    /// Answers the queries on a TCP connection, several at a time and in
    /// the order their answers come (RFC 7766), until the client closes it
    /// or stays idle.
    async fn serve_connection(
        self: Arc<Self>,
        stream: TcpStream,
        source: IpAddr,
        _permits: Permits,
    ) {
        let (mut reader, writer) = stream.into_split();
        // None once an answer could not be written: the others are not.
        let writer = Arc::new(Mutex::new(Some(writer)));
        let mut queries = JoinSet::new();
        loop {
            while queries.try_join_next().is_some() {}
            while queries.len() >= MAX_CONNECTION_QUERIES {
                queries.join_next().await;
            }
            let query = match time::timeout(TCP_TIMEOUT, read_message(&mut reader)).await {
                Ok(Ok(Some(query))) => query,
                // Closed, broken, or idle.
                _ => break,
            };
            // Asked for each query: the pod may have gone meanwhile.
            let client = self.client(source);
            let Some(permits) = self.permits(client.as_deref()).await else {
                break;
            };
            let (resolver, writer) = (self.clone(), writer.clone());
            queries.spawn(async move {
                let _permits = permits;
                let Some(answer) = resolver
                    .answer(&query, client.as_deref(), Transport::Tcp)
                    .await
                else {
                    return;
                };
                let mut writer = writer.lock().await;
                let Some(stream) = writer.as_mut() else {
                    return;
                };
                let error = match time::timeout(TCP_TIMEOUT, write_message(stream, &answer)).await {
                    Ok(Ok(())) => return,
                    Ok(Err(error)) => error,
                    Err(_) => io::Error::from(io::ErrorKind::TimedOut),
                };
                debug!("dns: answering {source}: {error}");
                *writer = None;
            });
        }
        while queries.join_next().await.is_some() {}
    }

    fn client(&self, address: IpAddr) -> Option<Arc<Client>> {
        self.clients
            .read()
            .unwrap_or_else(PoisonError::into_inner)
            .get(&address)
            .cloned()
    }

    /// Permits for a UDP query, None when the client or all are at their
    /// limit.
    fn try_permits(&self, client: Option<&Client>) -> Option<Permits> {
        let own = match client {
            Some(client) => Some(client.queries.clone().try_acquire_owned().ok()?),
            None => None,
        };
        Some((self.queries.clone().try_acquire_owned().ok()?, own))
    }

    /// Permits for a query over TCP, which waits for them.
    async fn permits(&self, client: Option<&Client>) -> Option<Permits> {
        let own = match client {
            Some(client) => Some(client.queries.clone().acquire_owned().await.ok()?),
            None => None,
        };
        Some((self.queries.clone().acquire_owned().await.ok()?, own))
    }

    /// Permits for a TCP connection, None when the client or all are at
    /// their limit.
    fn try_connection(&self, source: IpAddr) -> Option<Permits> {
        let own = match self.client(source) {
            Some(client) => Some(client.connections.clone().try_acquire_owned().ok()?),
            None => None,
        };
        Some((self.connections.clone().try_acquire_owned().ok()?, own))
    }

    /// The answer to the query `bytes` from `client`, unknown if None, on
    /// the wire. None for what is no query.
    async fn answer(
        &self,
        bytes: &[u8],
        client: Option<&Client>,
        transport: Transport,
    ) -> Option<Vec<u8>> {
        // Runts and responses get nothing.
        if bytes.len() < HEADER_LENGTH || bytes[2] & 0x80 != 0 {
            return None;
        }
        let Ok(request) = Message::from_vec(bytes) else {
            let code = match client {
                Some(_) => ResponseCode::FormErr,
                None => ResponseCode::Refused,
            };
            return Some(header_only(bytes, code));
        };
        let limit = match transport {
            Transport::Udp => usize::from(request.max_payload().clamp(512, UDP_PAYLOAD)),
            Transport::Tcp => usize::from(u16::MAX),
        };
        let Some(client) = client else {
            return encode(&respond(&request, Err(ResponseCode::Refused)), limit);
        };
        let response = match check(&request) {
            Some(code) => respond(&request, Err(code)),
            None if let Some(answer) = self.http(&request, client) => {
                let mut response = respond(&request, Ok(answer));
                response.metadata.authoritative = true;
                response
            }
            None if let Some(answer) = self.local(&request, client) => {
                let mut response = respond(&request, Ok(answer));
                response.metadata.authoritative = true;
                response
            }
            None if !request
                .queries
                .first()
                .is_some_and(|question| client.egress.allows(&record_name(question.name()))) =>
            {
                respond(&request, Err(ResponseCode::Refused))
            }
            None => match self.ask(&request, client, transport).await {
                Some(mut response) => {
                    let answers = svcb::strip(&mut response.answers);
                    let additionals = svcb::strip(&mut response.additionals);
                    // What was changed is no longer what was signed.
                    if answers || additionals {
                        response.metadata.authentic_data = false;
                    }
                    self.admit(client, &response).await;
                    respond(&request, Ok(response))
                }
                None => respond(&request, Err(ResponseCode::ServFail)),
            },
        };
        if let [question] = request.queries.as_slice() {
            self.record(client, question, &response);
        }
        encode(&response, limit)
    }

    /// The answer for a name of the client's HTTP bindings (ADR 0028), if
    /// the question asks for one: the node's address for them to A,
    /// nothing to every other type.
    fn http(&self, request: &Message, client: &Client) -> Option<Message> {
        if client.http.is_empty() {
            return None;
        }
        let address = self.config.http_address?;
        let question = request.queries.first()?;
        if question.query_class() != DNSClass::IN {
            return None;
        }
        let owner = question.name();
        if !client.http.contains(&record_name(owner)) {
            return None;
        }
        let mut answer = Message::new(0, MessageType::Query, OpCode::Query);
        if question.query_type() == RecordType::A {
            answer.add_answer(Record::from_rdata(
                owner.clone(),
                HTTP_TTL,
                RData::A(A(address)),
            ));
        }
        Some(answer)
    }

    /// The answer from the client's name tables to the question of
    /// `request`, if they hold its name, a name of one label: the
    /// addresses of the asked type, none for another type.
    fn local(&self, request: &Message, client: &Client) -> Option<Message> {
        if client.tables.is_empty() {
            return None;
        }
        let question = request.queries.first()?;
        let owner = question.name();
        if owner.num_labels() != 1 || question.query_class() != DNSClass::IN {
            return None;
        }
        let label = std::str::from_utf8(owner.iter().next()?).ok()?;
        let names = self
            .names
            .read()
            .unwrap_or_else(PoisonError::into_inner)
            .clone()?;
        let mut addresses = names(&client.tables, label)?;
        if addresses
            .iter()
            .any(|a| client.own.contains(&a.to_canonical()))
        {
            addresses = vec![Ipv4Addr::LOCALHOST.into(), Ipv6Addr::LOCALHOST.into()];
        }
        let mut answer = Message::new(0, MessageType::Query, OpCode::Query);
        for address in addresses {
            let data = match (address, question.query_type()) {
                (IpAddr::V6(address), RecordType::AAAA) => RData::AAAA(AAAA(address)),
                (IpAddr::V4(address), RecordType::A) => RData::A(A(address)),
                _ => continue,
            };
            answer.add_answer(Record::from_rdata(owner.clone(), NAME_TTL, data));
        }
        Some(answer)
    }

    /// What the client's upstreams answer to the question of `request`.
    /// None when none answers.
    async fn ask(
        &self,
        request: &Message,
        client: &Client,
        transport: Transport,
    ) -> Option<Message> {
        let question = request.queries.first()?.clone();
        let upstreams = match client.upstreams.is_empty() {
            true => &self.config.default_upstreams,
            false => &client.upstreams,
        };
        let query = upstream::query(request, question);
        let tcp = transport == Transport::Tcp;
        upstream::ask(upstreams, &query, tcp, self.config.upstream_timeout).await
    }

    /// Hands the addresses of an upstream's answer to a restricted client
    /// to the resolver's [`Admit`], and waits for it.
    async fn admit(&self, client: &Client, response: &Message) {
        if !client.egress.restricted() {
            return;
        }
        let Some(admit) = self
            .admit
            .read()
            .unwrap_or_else(PoisonError::into_inner)
            .clone()
        else {
            return;
        };
        let addresses = admitted(response);
        if addresses.is_empty() {
            return;
        }
        let pod_id = client.pod_id.clone();
        let admitting = tokio::task::spawn_blocking(move || admit(&pod_id, &addresses));
        if let Err(e) = admitting.await {
            warn!("dns: admitting the addresses of pod {}: {e}", client.pod_id);
        }
    }

    /// Publishes the record of an answered query.
    fn record(&self, client: &Client, question: &Query, response: &Message) {
        let addresses = response
            .answers
            .iter()
            .filter_map(|record| match &record.data {
                RData::A(A(address)) => Some(address.to_string()),
                RData::AAAA(AAAA(address)) => Some(address.to_string()),
                _ => None,
            })
            .collect();
        let record = TrafficRecord {
            time: Some(SystemTime::now().into()),
            pod_id: client.pod_id.clone(),
            missed: 0,
            record: Some(traffic_record::Record::Dns(DnsQuery {
                name: record_name(question.name()),
                r#type: u16::from(question.query_type()).into(),
                rcode: u16::from(response.metadata.response_code).into(),
                addresses,
            })),
        };
        // Nobody may be listening.
        let _ = self.records.send(record);
    }
}

/// The addresses of an answer's A and AAAA records, an IPv4-mapped one as
/// IPv4, each once, for its longest TTL, at least [`MIN_ADMIT`] and at
/// most [`MAX_ADMIT`].
fn admitted(response: &Message) -> Vec<(IpAddr, Duration)> {
    let mut addresses: Vec<(IpAddr, Duration)> = Vec::new();
    for record in &response.answers {
        let address = match &record.data {
            RData::A(A(address)) => IpAddr::V4(*address),
            RData::AAAA(AAAA(address)) => IpAddr::V6(*address).to_canonical(),
            _ => continue,
        };
        let timeout = Duration::from_secs(record.ttl.into()).clamp(MIN_ADMIT, MAX_ADMIT);
        match addresses.iter_mut().find(|(a, _)| *a == address) {
            Some((_, longest)) => *longest = (*longest).max(timeout),
            None => addresses.push((address, timeout)),
        }
    }
    addresses
}

/// The error a request gets before it is resolved, if any.
fn check(request: &Message) -> Option<ResponseCode> {
    if request.metadata.op_code != OpCode::Query {
        return Some(ResponseCode::NotImp);
    }
    let [question] = request.queries.as_slice() else {
        return Some(ResponseCode::FormErr);
    };
    if request
        .edns
        .as_ref()
        .is_some_and(|edns| edns.version() != 0)
    {
        return Some(ResponseCode::BADVERS);
    }
    // Zone transfers are not for pods.
    if matches!(question.query_type(), RecordType::AXFR | RecordType::IXFR) {
        return Some(ResponseCode::Refused);
    }
    None
}

/// The response to `request`: what the upstreams answered, or an error.
fn respond(request: &Message, upstream: Result<Message, ResponseCode>) -> Message {
    let mut response = Message::response(request.metadata.id, request.metadata.op_code);
    response.metadata.recursion_desired = request.metadata.recursion_desired;
    response.metadata.recursion_available = true;
    response.metadata.checking_disabled = request.metadata.checking_disabled;
    response.add_queries(request.queries.iter().cloned());
    match upstream {
        Ok(upstream) => {
            response.metadata.response_code = upstream.metadata.response_code;
            response.metadata.authentic_data = upstream.metadata.authentic_data;
            response.answers = upstream.answers;
            response.authorities = upstream.authorities;
            response.additionals = upstream.additionals;
        }
        Err(code) => {
            response.metadata.response_code = code;
        }
    }
    // EDNS of our own, if the client spoke it; the upstream's options were
    // for us.
    match &request.edns {
        Some(edns) => {
            let mut ours = Edns::new();
            ours.set_max_payload(UDP_PAYLOAD)
                .set_dnssec_ok(edns.flags().dnssec_ok);
            response.set_edns(ours);
        }
        // A code that needs EDNS cannot be told.
        None if response.metadata.response_code.high() != 0 => {
            response.metadata.response_code = ResponseCode::ServFail;
        }
        None => {}
    }
    response
}

/// `response` on the wire: whole if it fits in `limit` bytes, else
/// truncated, for the client to ask again over TCP.
fn encode(response: &Message, limit: usize) -> Option<Vec<u8>> {
    match response.to_vec() {
        Ok(bytes) if bytes.len() <= limit => Some(bytes),
        Ok(_) => response.truncate().to_vec().ok(),
        Err(error) => {
            debug!("dns: encoding an answer: {error}");
            let mut failure = response.truncate();
            failure.metadata.truncation = false;
            failure.metadata.response_code = ResponseCode::ServFail;
            failure.to_vec().ok()
        }
    }
}

/// A response of a header alone, to a query that cannot be read.
fn header_only(query: &[u8], code: ResponseCode) -> Vec<u8> {
    let mut header = vec![0; HEADER_LENGTH];
    header[..2].copy_from_slice(&query[..2]);
    // QR, and the query's opcode and RD.
    header[2] = 0x80 | (query[2] & 0x79);
    // RA, and the code.
    header[3] = 0x80 | code.low();
    header
}

/// Whether the client of `request` wants DNSSEC records (RFC 3225).
fn dnssec_ok(request: &Message) -> bool {
    request
        .edns
        .as_ref()
        .is_some_and(|edns| edns.flags().dnssec_ok)
}

/// A name as records have it: in lower case, without the final dot.
fn record_name(name: &Name) -> String {
    let name = name.to_lowercase().to_ascii();
    match name.strip_suffix('.') {
        Some(relative) if !relative.is_empty() => relative.to_string(),
        _ => name,
    }
}

/// The address a peer is known by; an IPv4 peer of an IPv6 socket by its
/// IPv4 address.
fn source_address(peer: SocketAddr) -> IpAddr {
    peer.ip().to_canonical()
}

/// Waits a moment after a socket error that may pass, such as running out
/// of file descriptors, or skips one of a single connection. Returns the
/// errors of a socket that is broken.
async fn pause(error: io::Error) -> io::Result<()> {
    match error.raw_os_error() {
        Some(libc::EBADF | libc::ENOTSOCK | libc::EINVAL | libc::EOPNOTSUPP) => Err(error),
        Some(libc::ECONNABORTED | libc::ECONNRESET | libc::EPROTO | libc::EPERM) => {
            debug!("dns: {error}");
            Ok(())
        }
        _ => {
            warn!("dns: {error}");
            time::sleep(Duration::from_millis(100)).await;
            Ok(())
        }
    }
}

/// Reads a message of DNS over TCP, after its length (RFC 1035 4.2.2).
/// None when the stream ends before one.
async fn read_message<R: AsyncRead + Unpin>(reader: &mut R) -> io::Result<Option<Vec<u8>>> {
    let mut length = [0; 2];
    match reader.read_exact(&mut length).await {
        Ok(_) => {}
        Err(error) if error.kind() == io::ErrorKind::UnexpectedEof => return Ok(None),
        Err(error) => return Err(error),
    }
    let length = usize::from(u16::from_be_bytes(length));
    if length == 0 {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "an empty message",
        ));
    }
    let mut message = vec![0; length];
    reader.read_exact(&mut message).await?;
    Ok(Some(message))
}

/// Writes a message of DNS over TCP, after its length.
async fn write_message<W: AsyncWrite + Unpin>(writer: &mut W, message: &[u8]) -> io::Result<()> {
    let length = u16::try_from(message.len())
        .map_err(|_| io::Error::new(io::ErrorKind::InvalidData, "a message over 64 KiB"))?;
    let mut framed = Vec::with_capacity(2 + message.len());
    framed.extend_from_slice(&length.to_be_bytes());
    framed.extend_from_slice(message);
    writer.write_all(&framed).await
}
