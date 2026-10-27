// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The resolver on loopback, against fake upstreams with canned answers.
//! They need no root.

use super::*;
use hickory_proto::rr::rdata::svcb::{
    Alpn, EchConfigList, IpHint, SvcParamKey, SvcParamValue, SVCB,
};
use hickory_proto::rr::rdata::{CNAME, HTTPS, PTR, SOA};
use hickory_proto::rr::Record;
use std::net::Ipv6Addr;
use tokio::net::TcpSocket;

const POD: &str = "4f9a1c2e-0000-4000-8000-000000000001";
const OTHER_POD: &str = "4f9a1c2e-0000-4000-8000-000000000002";
/// The resolver's address and its pod's: IPv4, as on a node.
const LOOPBACK: Ipv4Addr = Ipv4Addr::LOCALHOST;
/// Short, for the tests of upstreams that do not answer.
const TIMEOUT: Duration = Duration::from_millis(200);

/// A query a fake upstream got: its name, type, and whether over TCP.
type Asked = (String, RecordType, bool);

/// A fake upstream on loopback that answers from a zone, over UDP and TCP
/// on the same port.
struct Upstream {
    address: SocketAddr,
    asked: Arc<std::sync::Mutex<Vec<Asked>>>,
    _tasks: JoinSet<()>,
}

/// Answers of a zone: None for silence.
type Zone = fn(&Query, bool) -> Option<Message>;

/// A UDP and a TCP socket on one port of `ip`. Another program may hold
/// the UDP port's number in TCP, as tests running beside these do: then
/// another port.
async fn sockets(ip: IpAddr) -> (UdpSocket, TcpListener) {
    loop {
        let udp = UdpSocket::bind((ip, 0)).await.unwrap();
        match TcpListener::bind(udp.local_addr().unwrap()).await {
            Ok(tcp) => return (udp, tcp),
            Err(e) if e.kind() == io::ErrorKind::AddrInUse => continue,
            Err(e) => panic!("TCP on the UDP socket's port: {e}"),
        }
    }
}

impl Upstream {
    async fn start(ip: IpAddr, zone: Zone) -> Self {
        let (udp, tcp) = sockets(ip).await;
        let address = udp.local_addr().unwrap();
        let asked = Arc::new(std::sync::Mutex::new(Vec::new()));
        let mut tasks = JoinSet::new();
        let log = asked.clone();
        tasks.spawn(async move {
            let mut buffer = vec![0; 65535];
            loop {
                let (length, peer) = udp.recv_from(&mut buffer).await.unwrap();
                if let Some(answer) = reply(&buffer[..length], zone, false, &log) {
                    udp.send_to(&answer, peer).await.unwrap();
                }
            }
        });
        let log = asked.clone();
        tasks.spawn(async move {
            loop {
                let (mut stream, _) = tcp.accept().await.unwrap();
                let log = log.clone();
                tokio::spawn(async move {
                    while let Ok(Some(query)) = read_message(&mut stream).await {
                        if let Some(answer) = reply(&query, zone, true, &log) {
                            write_message(&mut stream, &answer).await.unwrap();
                        }
                    }
                });
            }
        });
        Self {
            address,
            asked,
            _tasks: tasks,
        }
    }

    fn asked(&self) -> Vec<Asked> {
        self.asked.lock().unwrap().clone()
    }
}

fn reply(
    query: &[u8],
    zone: Zone,
    tcp: bool,
    log: &std::sync::Mutex<Vec<Asked>>,
) -> Option<Vec<u8>> {
    let query = Message::from_vec(query).unwrap();
    let question = query.queries[0].clone();
    let name = question.name().to_lowercase().to_ascii();
    log.lock().unwrap().push((name, question.query_type(), tcp));
    let mut response = zone(&question, tcp)?;
    response.metadata.id = query.metadata.id;
    response.metadata.message_type = MessageType::Response;
    response.metadata.recursion_desired = query.metadata.recursion_desired;
    response.metadata.recursion_available = true;
    response.add_query(question);
    Some(response.to_vec().unwrap())
}

fn name(name: &str) -> Name {
    Name::from_ascii(name).unwrap()
}

fn soa() -> Record {
    let soa = SOA::new(
        name("ns.test."),
        name("hostmaster.test."),
        1,
        3600,
        600,
        86400,
        60,
    );
    Record::from_rdata(name("test."), 3600, RData::SOA(soa))
}

/// The zone of the tests' names.
fn zone(query: &Query, tcp: bool) -> Option<Message> {
    let owner = query.name().clone();
    let record = |data| Record::from_rdata(owner.clone(), 300, data);
    let a = |octets: [u8; 4]| record(RData::A(A(octets.into())));
    let aaaa = |address: &str| record(RData::AAAA(AAAA(address.parse().unwrap())));
    let mut response = Message::new(0, MessageType::Query, OpCode::Query);
    fn nodata(response: &mut Message) {
        response.add_authority(soa());
    }
    match (
        query.name().to_lowercase().to_ascii().as_str(),
        query.query_type(),
    ) {
        ("v6.test.", RecordType::AAAA) => {
            response.add_answer(aaaa("2001:db8::6"));
        }
        ("v6.test.", RecordType::A) => {
            response.add_answer(a([1, 2, 3, 6]));
        }
        ("v4.test.", RecordType::A) => {
            response.add_answers([a([1, 2, 3, 4]), a([5, 6, 7, 8])]);
        }
        ("alias.test.", _) => {
            response.add_answer(record(RData::CNAME(CNAME(name("v4.test.")))));
            if query.query_type() == RecordType::A {
                let target = |octets: [u8; 4]| {
                    Record::from_rdata(name("v4.test."), 300, RData::A(A(octets.into())))
                };
                response.add_answers([target([1, 2, 3, 4]), target([5, 6, 7, 8])]);
            } else {
                nodata(&mut response);
            }
        }
        ("mapped.test.", RecordType::AAAA) => {
            response.add_answer(aaaa("::ffff:1.2.3.4"));
        }
        ("mapped.test.", RecordType::A) => {
            response.add_answer(a([1, 2, 3, 4]));
        }
        ("big.test.", RecordType::A) => {
            if !tcp {
                response.metadata.truncation = true;
            } else {
                response.add_answer(a([1, 2, 3, 10]));
            }
        }
        ("many.test.", RecordType::A) => {
            response.add_answers((1..=60).map(|last| a([1, 2, 3, last])));
        }
        ("svc.test.", RecordType::HTTPS) => {
            let svcb = SVCB::new(
                1,
                Name::root(),
                vec![
                    (
                        SvcParamKey::Alpn,
                        SvcParamValue::Alpn(Alpn(vec!["h2".into()])),
                    ),
                    (
                        SvcParamKey::Ipv4Hint,
                        SvcParamValue::Ipv4Hint(IpHint(vec![A([1, 2, 3, 4].into())])),
                    ),
                    (
                        SvcParamKey::EchConfigList,
                        SvcParamValue::EchConfigList(EchConfigList(vec![0, 2, 0xfe, 0x0d])),
                    ),
                    (
                        SvcParamKey::Ipv6Hint,
                        SvcParamValue::Ipv6Hint(IpHint(vec![AAAA("2001:db8::1".parse().unwrap())])),
                    ),
                ],
            );
            response.add_answer(record(RData::HTTPS(HTTPS(svcb))));
            response.metadata.authentic_data = true;
        }
        ("plain.test.", RecordType::HTTPS) => {
            let alpn = SvcParamValue::Alpn(Alpn(vec!["h2".into()]));
            let svcb = SVCB::new(1, Name::root(), vec![(SvcParamKey::Alpn, alpn)]);
            response.add_answer(record(RData::HTTPS(HTTPS(svcb))));
            response.metadata.authentic_data = true;
        }
        ("ipv4only.arpa.", RecordType::A) => {
            response.add_answers([a([192, 0, 0, 170]), a([192, 0, 0, 171])]);
        }
        ("ipv4only.arpa.", _) => nodata(&mut response),
        ("4.3.2.1.in-addr.arpa.", RecordType::PTR) => {
            response.add_answer(record(RData::PTR(PTR(name("v4.test.")))));
        }
        ("silent.test.", _) => return None,
        ("missing.test.", _) => {
            response.metadata.response_code = ResponseCode::NXDomain;
            nodata(&mut response);
        }
        (other, _) if other.ends_with(".test.") => nodata(&mut response),
        _ => {
            response.metadata.response_code = ResponseCode::NXDomain;
        }
    }
    Some(response)
}

/// A zone that never answers.
fn silence(_: &Query, _: bool) -> Option<Message> {
    None
}

/// A zone that fails everything.
fn failure(_: &Query, _: bool) -> Option<Message> {
    let mut response = Message::new(0, MessageType::Query, OpCode::Query);
    response.metadata.response_code = ResponseCode::ServFail;
    Some(response)
}

/// A resolver on loopback, with `127.0.0.1` as a client of `upstreams`.
async fn resolver(config: Config, upstreams: Vec<SocketAddr>) -> (Arc<Resolver>, SocketAddr) {
    resolver_at(LOOPBACK.into(), config, upstreams).await
}

/// A resolver at `address`, which is its own client of `upstreams`.
async fn resolver_at(
    address: IpAddr,
    config: Config,
    upstreams: Vec<SocketAddr>,
) -> (Arc<Resolver>, SocketAddr) {
    let (udp, tcp) = sockets(address).await;
    let address = udp.local_addr().unwrap();
    let config = Config {
        upstream_timeout: TIMEOUT,
        ..config
    };
    let resolver = Resolver::new(config);
    resolver.set_client(address.ip(), POD, upstreams, Vec::new());
    tokio::spawn(resolver.clone().serve(udp, tcp));
    (resolver, address)
}

/// A fake upstream on `::1`.
async fn upstream() -> Upstream {
    Upstream::start(IpAddr::V6(Ipv6Addr::LOCALHOST), zone).await
}

fn query(owner: &str, record_type: RecordType) -> Message {
    let mut query = Message::new(rand::random(), MessageType::Query, OpCode::Query);
    query.metadata.recursion_desired = true;
    query.add_query(Query::query(name(owner), record_type));
    let mut edns = Edns::new();
    edns.set_max_payload(1232);
    query.set_edns(edns);
    query
}

/// Sends `bytes` from `source` and waits for an answer as long as `wait`.
async fn ask_udp_bytes(
    source: IpAddr,
    server: SocketAddr,
    bytes: &[u8],
    wait: Duration,
) -> Option<Vec<u8>> {
    let socket = UdpSocket::bind((source, 0)).await.unwrap();
    socket.connect(server).await.unwrap();
    socket.send(bytes).await.unwrap();
    let mut buffer = vec![0; 65535];
    let length = time::timeout(wait, socket.recv(&mut buffer))
        .await
        .ok()?
        .unwrap();
    Some(buffer[..length].to_vec())
}

async fn ask_udp(server: SocketAddr, query: &Message) -> Message {
    ask_udp_from(LOOPBACK.into(), server, query).await
}

async fn ask_udp_from(source: IpAddr, server: SocketAddr, query: &Message) -> Message {
    let bytes = query.to_vec().unwrap();
    let answer = ask_udp_bytes(source, server, &bytes, Duration::from_secs(3))
        .await
        .expect("an answer");
    let answer = Message::from_vec(&answer).unwrap();
    assert_eq!(answer.metadata.id, query.metadata.id);
    assert_eq!(answer.metadata.message_type, MessageType::Response);
    assert_eq!(answer.queries, query.queries);
    answer
}

async fn ask_tcp(server: SocketAddr, query: &Message) -> Message {
    ask_tcp_from(LOOPBACK.into(), server, query).await
}

async fn ask_tcp_from(source: IpAddr, server: SocketAddr, query: &Message) -> Message {
    let socket = match source {
        IpAddr::V4(_) => TcpSocket::new_v4(),
        IpAddr::V6(_) => TcpSocket::new_v6(),
    }
    .unwrap();
    socket.bind((source, 0).into()).unwrap();
    let mut stream = socket.connect(server).await.unwrap();
    write_message(&mut stream, &query.to_vec().unwrap())
        .await
        .unwrap();
    let answer = time::timeout(Duration::from_secs(3), read_message(&mut stream))
        .await
        .unwrap()
        .unwrap()
        .unwrap();
    let answer = Message::from_vec(&answer).unwrap();
    assert_eq!(answer.metadata.id, query.metadata.id);
    assert_eq!(answer.queries, query.queries);
    answer
}

fn addresses(message: &Message) -> Vec<IpAddr> {
    message
        .answers
        .iter()
        .filter_map(|record| match &record.data {
            RData::A(A(address)) => Some(IpAddr::V4(*address)),
            RData::AAAA(AAAA(address)) => Some(IpAddr::V6(*address)),
            _ => None,
        })
        .collect()
}

fn ips(addresses: &[&str]) -> Vec<IpAddr> {
    addresses.iter().map(|a| a.parse().unwrap()).collect()
}

fn asked(name: &str, record_type: RecordType, tcp: bool) -> Asked {
    (name.to_string(), record_type, tcp)
}

#[tokio::test]
async fn passes_aaaa_records() {
    let upstream = upstream().await;
    let (_, server) = resolver(Config::default(), vec![upstream.address]).await;
    let answer = ask_udp(server, &query("v6.test.", RecordType::AAAA)).await;
    assert_eq!(answer.metadata.response_code, ResponseCode::NoError);
    assert!(answer.metadata.recursion_available);
    assert_eq!(addresses(&answer), ips(&["2001:db8::6"]));
    assert_eq!(
        upstream.asked(),
        [asked("v6.test.", RecordType::AAAA, false)]
    );
}

#[tokio::test]
async fn synthesizes_nothing() {
    let upstream = upstream().await;
    let (_, server) = resolver(Config::default(), vec![upstream.address]).await;
    // A name with A records alone has no AAAA records.
    let answer = ask_udp(server, &query("v4.test.", RecordType::AAAA)).await;
    assert_eq!(answer.metadata.response_code, ResponseCode::NoError);
    assert!(answer.answers.is_empty());
    // The negative answer, with its SOA record.
    assert_eq!(answer.authorities.len(), 1);
    assert_eq!(
        upstream.asked(),
        [asked("v4.test.", RecordType::AAAA, false)]
    );
    // A records pass as they are.
    let answer = ask_udp(server, &query("v4.test.", RecordType::A)).await;
    assert_eq!(addresses(&answer), ips(&["1.2.3.4", "5.6.7.8"]));
}

#[tokio::test]
async fn passes_ipv4_mapped_aaaa_records() {
    let upstream = upstream().await;
    let (_, server) = resolver(Config::default(), vec![upstream.address]).await;
    let answer = ask_udp(server, &query("mapped.test.", RecordType::AAAA)).await;
    assert_eq!(addresses(&answer), ips(&["::ffff:1.2.3.4"]));
}

#[tokio::test]
async fn keeps_the_cname_chain() {
    let upstream = upstream().await;
    let (_, server) = resolver(Config::default(), vec![upstream.address]).await;
    let answer = ask_udp(server, &query("alias.test.", RecordType::A)).await;
    let answers = &answer.answers;
    assert_eq!(answers.len(), 3);
    assert_eq!(&answers[0].name, &name("alias.test."));
    assert_eq!(&answers[0].data, &RData::CNAME(CNAME(name("v4.test."))));
    assert!(answers[1..]
        .iter()
        .all(|record| record.name == name("v4.test.")));
    assert_eq!(addresses(&answer), ips(&["1.2.3.4", "5.6.7.8"]));
    // Without AAAA records at its end, the chain alone.
    let answer = ask_udp(server, &query("alias.test.", RecordType::AAAA)).await;
    assert_eq!(answer.metadata.response_code, ResponseCode::NoError);
    assert_eq!(answer.answers.len(), 1);
    assert_eq!(
        &answer.answers[0].data,
        &RData::CNAME(CNAME(name("v4.test.")))
    );
}

#[tokio::test]
async fn passes_nxdomain() {
    let upstream = upstream().await;
    let (_, server) = resolver(Config::default(), vec![upstream.address]).await;
    let answer = ask_udp(server, &query("missing.test.", RecordType::AAAA)).await;
    assert_eq!(answer.metadata.response_code, ResponseCode::NXDomain);
    assert_eq!(answer.authorities.len(), 1);
    assert_eq!(
        upstream.asked(),
        [asked("missing.test.", RecordType::AAAA, false)]
    );
}

#[tokio::test]
async fn keeps_the_flags_of_a_validating_client() {
    let upstream = upstream().await;
    let (_, server) = resolver(Config::default(), vec![upstream.address]).await;
    let mut request = query("v6.test.", RecordType::AAAA);
    request.metadata.checking_disabled = true;
    request.edns.as_mut().unwrap().set_dnssec_ok(true);
    let answer = ask_udp(server, &request).await;
    assert_eq!(answer.metadata.response_code, ResponseCode::NoError);
    assert_eq!(addresses(&answer), ips(&["2001:db8::6"]));
    assert!(answer.metadata.checking_disabled);
    assert!(answer.edns.as_ref().unwrap().flags().dnssec_ok);
    assert_eq!(
        upstream.asked(),
        [asked("v6.test.", RecordType::AAAA, false)]
    );
}

#[tokio::test]
async fn asks_the_upstreams_for_ipv4only_arpa() {
    let upstream = upstream().await;
    let (_, server) = resolver(Config::default(), vec![upstream.address]).await;
    let answer = ask_udp(server, &query("ipv4only.arpa.", RecordType::AAAA)).await;
    assert_eq!(answer.metadata.response_code, ResponseCode::NoError);
    assert!(answer.answers.is_empty());
    let answer = ask_udp(server, &query("ipv4only.arpa.", RecordType::A)).await;
    assert_eq!(addresses(&answer), ips(&["192.0.0.170", "192.0.0.171"]));
    assert_eq!(
        upstream.asked(),
        [
            asked("ipv4only.arpa.", RecordType::AAAA, false),
            asked("ipv4only.arpa.", RecordType::A, false)
        ]
    );
}

#[tokio::test]
async fn passes_reverse_queries() {
    let upstream = upstream().await;
    let (_, server) = resolver(Config::default(), vec![upstream.address]).await;
    let answer = ask_udp(server, &query("4.3.2.1.in-addr.arpa.", RecordType::PTR)).await;
    assert_eq!(answer.metadata.response_code, ResponseCode::NoError);
    assert_eq!(answer.answers.len(), 1);
    assert_eq!(&answer.answers[0].data, &RData::PTR(PTR(name("v4.test."))));
    // An IPv6 reverse name is asked as it is.
    let address: Ipv6Addr = "2001:db8::102:304".parse().unwrap();
    let reverse = Name::from(address);
    let answer = ask_udp(server, &query(&reverse.to_ascii(), RecordType::PTR)).await;
    assert_eq!(answer.metadata.response_code, ResponseCode::NXDomain);
    assert_eq!(
        upstream.asked(),
        [
            asked("4.3.2.1.in-addr.arpa.", RecordType::PTR, false),
            asked(&reverse.to_lowercase().to_ascii(), RecordType::PTR, false)
        ]
    );
}

#[tokio::test]
async fn strips_ech() {
    let upstream = upstream().await;
    let (_, server) = resolver(Config::default(), vec![upstream.address]).await;
    let request = query("svc.test.", RecordType::HTTPS);
    for answer in [
        ask_udp(server, &request).await,
        ask_tcp(server, &request).await,
    ] {
        let RData::HTTPS(HTTPS(svcb)) = &answer.answers[0].data else {
            panic!("not an HTTPS record");
        };
        let keys: Vec<_> = svcb.svc_params.iter().map(|(key, _)| *key).collect();
        assert_eq!(
            keys,
            [
                SvcParamKey::Alpn,
                SvcParamKey::Ipv4Hint,
                SvcParamKey::Ipv6Hint
            ]
        );
        assert!(!answer.metadata.authentic_data);
    }

    // A record without ech passes as it is.
    let answer = ask_udp(server, &query("plain.test.", RecordType::HTTPS)).await;
    let RData::HTTPS(HTTPS(svcb)) = &answer.answers[0].data else {
        panic!("not an HTTPS record");
    };
    assert_eq!(svcb.svc_params.len(), 1);
    assert!(answer.metadata.authentic_data);
}

#[tokio::test]
async fn fails_over_and_fails() {
    let silent = Upstream::start(IpAddr::V6(Ipv6Addr::LOCALHOST), silence).await;
    let failing = Upstream::start(IpAddr::V4(Ipv4Addr::LOCALHOST), failure).await;
    let upstream = Upstream::start(IpAddr::V4(Ipv4Addr::LOCALHOST), zone).await;
    let (resolver, server) = resolver(
        Config::default(),
        vec![silent.address, failing.address, upstream.address],
    )
    .await;
    let answer = ask_udp(server, &query("v6.test.", RecordType::A)).await;
    assert_eq!(addresses(&answer), ips(&["1.2.3.6"]));
    assert_eq!(silent.asked(), [asked("v6.test.", RecordType::A, false)]);
    assert_eq!(failing.asked(), [asked("v6.test.", RecordType::A, false)]);

    // No upstream that answers: SERVFAIL.
    resolver.set_client(LOOPBACK.into(), POD, vec![silent.address], Vec::new());
    let answer = ask_udp(server, &query("v6.test.", RecordType::A)).await;
    assert_eq!(answer.metadata.response_code, ResponseCode::ServFail);
    // Only failures: the last stands.
    resolver.set_client(
        LOOPBACK.into(),
        POD,
        vec![silent.address, failing.address],
        Vec::new(),
    );
    let answer = ask_udp(server, &query("v6.test.", RecordType::A)).await;
    assert_eq!(answer.metadata.response_code, ResponseCode::ServFail);
    // No upstream at all.
    resolver.set_client(LOOPBACK.into(), POD, Vec::new(), Vec::new());
    let answer = ask_udp(server, &query("v6.test.", RecordType::A)).await;
    assert_eq!(answer.metadata.response_code, ResponseCode::ServFail);
}

#[tokio::test]
async fn asks_the_default_upstreams() {
    let upstream = upstream().await;
    let config = Config {
        default_upstreams: vec![upstream.address],
        ..Config::default()
    };
    let (_, server) = resolver(config, Vec::new()).await;
    let answer = ask_udp(server, &query("v6.test.", RecordType::A)).await;
    assert_eq!(addresses(&answer), ips(&["1.2.3.6"]));
}

#[tokio::test]
async fn retries_truncated_answers_over_tcp() {
    let upstream = upstream().await;
    let (_, server) = resolver(Config::default(), vec![upstream.address]).await;
    let answer = ask_udp(server, &query("big.test.", RecordType::A)).await;
    assert!(!answer.metadata.truncation);
    assert_eq!(addresses(&answer), ips(&["1.2.3.10"]));
    assert_eq!(
        upstream.asked(),
        [
            asked("big.test.", RecordType::A, false),
            asked("big.test.", RecordType::A, true)
        ]
    );
}

#[tokio::test]
async fn truncates_what_does_not_fit() {
    let upstream = upstream().await;
    let (_, server) = resolver(Config::default(), vec![upstream.address]).await;
    // Without EDNS, a client takes 512 bytes over UDP.
    let mut request = query("many.test.", RecordType::A);
    request.edns = None;
    let answer = ask_udp(server, &request).await;
    assert!(answer.metadata.truncation);
    assert!(answer.answers.is_empty());
    let answer = ask_tcp(server, &request).await;
    assert!(!answer.metadata.truncation);
    assert_eq!(answer.answers.len(), 60);
    assert!(answer.edns.is_none());
}

#[tokio::test]
async fn serves_tcp() {
    let upstream = upstream().await;
    let (_, server) = resolver(Config::default(), vec![upstream.address]).await;
    let answer = ask_tcp(server, &query("v4.test.", RecordType::A)).await;
    assert_eq!(addresses(&answer), ips(&["1.2.3.4", "5.6.7.8"]));
    // A query over TCP goes upstream over TCP.
    assert_eq!(upstream.asked(), [asked("v4.test.", RecordType::A, true)]);

    // Several queries on one connection, answered as they come.
    let mut stream = TcpStream::connect(server).await.unwrap();
    let queries = [
        query("v4.test.", RecordType::A),
        query("v6.test.", RecordType::AAAA),
        query("missing.test.", RecordType::A),
    ];
    for query in &queries {
        write_message(&mut stream, &query.to_vec().unwrap())
            .await
            .unwrap();
    }
    let mut ids = Vec::new();
    for _ in &queries {
        let answer = read_message(&mut stream).await.unwrap().unwrap();
        ids.push(Message::from_vec(&answer).unwrap().metadata.id);
    }
    ids.sort();
    let mut expected: Vec<_> = queries.iter().map(|query| query.metadata.id).collect();
    expected.sort();
    assert_eq!(ids, expected);
}

#[tokio::test]
async fn refuses_unknown_clients() {
    let upstream = upstream().await;
    let (resolver, server) = resolver(Config::default(), vec![upstream.address]).await;
    resolver.remove_client(LOOPBACK.into());
    let answer = ask_udp(server, &query("v6.test.", RecordType::AAAA)).await;
    assert_eq!(answer.metadata.response_code, ResponseCode::Refused);
    let answer = ask_tcp(server, &query("v6.test.", RecordType::AAAA)).await;
    assert_eq!(answer.metadata.response_code, ResponseCode::Refused);
    assert!(upstream.asked().is_empty());
}

#[tokio::test]
async fn records_answered_queries() {
    let upstream = upstream().await;
    let (resolver, server) = resolver(Config::default(), vec![upstream.address]).await;
    let mut records = resolver.subscribe();
    ask_udp(server, &query("V4.Test.", RecordType::A)).await;
    ask_udp(server, &query("missing.test.", RecordType::AAAA)).await;

    let record = records.recv().await.unwrap();
    assert_eq!(record.pod_id, POD);
    assert!(record.time.is_some());
    let Some(traffic_record::Record::Dns(dns)) = record.record else {
        panic!("not a DNS record");
    };
    assert_eq!(
        dns,
        DnsQuery {
            name: "v4.test".into(),
            r#type: 1,
            rcode: 0,
            addresses: vec!["1.2.3.4".into(), "5.6.7.8".into()],
        }
    );
    let record = records.recv().await.unwrap();
    let Some(traffic_record::Record::Dns(dns)) = record.record else {
        panic!("not a DNS record");
    };
    assert_eq!(
        dns,
        DnsQuery {
            name: "missing.test".into(),
            r#type: 28,
            rcode: 3,
            addresses: Vec::new(),
        }
    );

    // Unknown clients leave no record.
    resolver.remove_client(LOOPBACK.into());
    ask_udp(server, &query("v6.test.", RecordType::A)).await;
    assert!(records.try_recv().is_err());
}

#[tokio::test]
async fn survives_garbage() {
    let upstream = upstream().await;
    let (_, server) = resolver(Config::default(), vec![upstream.address]).await;
    // Runts and responses get no answer.
    let wait = Duration::from_millis(300);
    let source = IpAddr::V4(LOOPBACK);
    assert!(ask_udp_bytes(source, server, &[1, 2, 3], wait)
        .await
        .is_none());
    let mut response = query("v6.test.", RecordType::A);
    response.metadata.message_type = MessageType::Response;
    let bytes = response.to_vec().unwrap();
    assert!(ask_udp_bytes(source, server, &bytes, wait).await.is_none());
    // A header with a broken body: FORMERR.
    let mut broken = query("v6.test.", RecordType::A).to_vec().unwrap();
    broken.truncate(16);
    let answer = ask_udp_bytes(source, server, &broken, Duration::from_secs(3))
        .await
        .unwrap();
    let answer = Message::from_vec(&answer).unwrap();
    assert_eq!(answer.metadata.response_code, ResponseCode::FormErr);
    assert_eq!(
        answer.metadata.id,
        u16::from_be_bytes([broken[0], broken[1]])
    );
    // Other opcodes are not implemented.
    let mut notify = query("v6.test.", RecordType::SOA);
    notify.metadata.op_code = OpCode::Notify;
    let answer = ask_udp(server, &notify).await;
    assert_eq!(answer.metadata.response_code, ResponseCode::NotImp);
    // Garbage over TCP closes the connection.
    let mut stream = TcpStream::connect(server).await.unwrap();
    stream.write_all(&[0, 0]).await.unwrap();
    assert!(read_message(&mut stream).await.unwrap().is_none());
    // And the resolver still answers.
    let answer = ask_udp(server, &query("v6.test.", RecordType::A)).await;
    assert_eq!(addresses(&answer), ips(&["1.2.3.6"]));
}

#[tokio::test]
async fn tells_pods_apart_by_their_ipv4_address() {
    let upstream = upstream().await;
    let failing = Upstream::start(IpAddr::V4(LOOPBACK), failure).await;
    let (resolver, server) = resolver(Config::default(), vec![upstream.address]).await;
    let other = IpAddr::V4(Ipv4Addr::new(127, 0, 0, 2));
    resolver.set_client(other, OTHER_POD, vec![failing.address], Vec::new());
    let mut records = resolver.subscribe();

    // Each pod's queries go to its own upstreams, over UDP and TCP.
    let request = query("v6.test.", RecordType::A);
    let answer = ask_udp(server, &request).await;
    assert_eq!(addresses(&answer), ips(&["1.2.3.6"]));
    let answer = ask_udp_from(other, server, &request).await;
    assert_eq!(answer.metadata.response_code, ResponseCode::ServFail);
    let answer = ask_tcp_from(other, server, &request).await;
    assert_eq!(answer.metadata.response_code, ResponseCode::ServFail);
    assert_eq!(upstream.asked(), [asked("v6.test.", RecordType::A, false)]);
    assert_eq!(
        failing.asked(),
        [
            asked("v6.test.", RecordType::A, false),
            asked("v6.test.", RecordType::A, true)
        ]
    );
    // And their records name them.
    let pods: Vec<_> = (0..3).map(|_| records.try_recv().unwrap().pod_id).collect();
    assert_eq!(pods, [POD, OTHER_POD, OTHER_POD]);

    // An address the resolver does not know is refused.
    let unknown = IpAddr::V4(Ipv4Addr::new(127, 0, 0, 3));
    let answer = ask_udp_from(unknown, server, &request).await;
    assert_eq!(answer.metadata.response_code, ResponseCode::Refused);
    let answer = ask_tcp_from(unknown, server, &request).await;
    assert_eq!(answer.metadata.response_code, ResponseCode::Refused);
    assert!(records.try_recv().is_err());
    // And so is a pod that is gone.
    resolver.remove_client(other);
    let answer = ask_udp_from(other, server, &request).await;
    assert_eq!(answer.metadata.response_code, ResponseCode::Refused);
}

#[tokio::test]
async fn knows_ipv4_clients_by_their_ipv4_address() {
    // An IPv6 socket sees an IPv4 peer at its IPv4-mapped address.
    let peer: SocketAddr = "[::ffff:169.254.16.1]:40000".parse().unwrap();
    assert_eq!(source_address(peer), IpAddr::from([169, 254, 16, 1]));
    let peer: SocketAddr = "[fd00:fe05:1::2]:40000".parse().unwrap();
    assert_eq!(source_address(peer), peer.ip());

    // A client set at its IPv4-mapped address is its IPv4 address.
    let upstream = upstream().await;
    let (resolver, server) = resolver(Config::default(), vec![upstream.address]).await;
    resolver.remove_client(LOOPBACK.into());
    let mapped = IpAddr::V6(LOOPBACK.to_ipv6_mapped());
    resolver.set_client(mapped, POD, vec![upstream.address], Vec::new());
    let answer = ask_udp(server, &query("v6.test.", RecordType::A)).await;
    assert_eq!(addresses(&answer), ips(&["1.2.3.6"]));
    resolver.remove_client(mapped);
    let answer = ask_udp(server, &query("v6.test.", RecordType::A)).await;
    assert_eq!(answer.metadata.response_code, ResponseCode::Refused);
}

#[tokio::test]
async fn serves_ipv6_clients() {
    let upstream = upstream().await;
    let (_, server) = resolver_at(
        Ipv6Addr::LOCALHOST.into(),
        Config::default(),
        vec![upstream.address],
    )
    .await;
    let source = IpAddr::V6(Ipv6Addr::LOCALHOST);
    let answer = ask_udp_from(source, server, &query("v6.test.", RecordType::AAAA)).await;
    assert_eq!(addresses(&answer), ips(&["2001:db8::6"]));
    let answer = ask_tcp_from(source, server, &query("v4.test.", RecordType::A)).await;
    assert_eq!(addresses(&answer), ips(&["1.2.3.4", "5.6.7.8"]));
}

/// The tables of the tests' names: env-1 with web (two replicas) and db
/// (IPv6 alone), sbx-1 with vite on loopback.
fn names(tables: &[String], name: &str) -> Option<Vec<IpAddr>> {
    let mut out = None;
    for table in tables {
        out = match (table.as_str(), name.to_ascii_lowercase().as_str()) {
            ("env-1", "web") => Some(ips(&["fd00::a", "fd00::b"])),
            ("env-1", "db") => Some(ips(&["fd00::d"])),
            ("sbx-1", "vite") => Some(ips(&["127.0.0.1", "::1"])),
            _ => continue,
        };
        break;
    }
    out
}

#[tokio::test]
async fn answers_the_names_of_the_pods_tables() {
    let upstream = upstream().await;
    let (resolver, server) = resolver(Config::default(), vec![upstream.address]).await;
    resolver.set_names(names);
    let tables = vec!["sbx-1".to_string(), "env-1".to_string()];
    resolver.set_client(LOOPBACK.into(), POD, vec![upstream.address], tables);
    let mut records = resolver.subscribe();

    // Every address of a name, in any case, over UDP and TCP, with a short
    // TTL; never asked upstream.
    let answer = ask_udp(server, &query("WeB.", RecordType::AAAA)).await;
    assert_eq!(answer.metadata.response_code, ResponseCode::NoError);
    assert!(answer.metadata.authoritative && answer.metadata.recursion_available);
    assert_eq!(addresses(&answer), ips(&["fd00::a", "fd00::b"]));
    assert!(answer.answers.iter().all(|record| record.ttl == NAME_TTL));
    assert_eq!(&answer.answers[0].name, &name("WeB."), "the query's case");
    let answer = ask_tcp(server, &query("vite.", RecordType::A)).await;
    assert_eq!(addresses(&answer), ips(&["127.0.0.1"]));
    let answer = ask_udp(server, &query("vite.", RecordType::AAAA)).await;
    assert_eq!(addresses(&answer), ips(&["::1"]));
    // A name of the tables has nothing of another type: NODATA.
    for record_type in [RecordType::A, RecordType::HTTPS, RecordType::MX] {
        let answer = ask_udp(server, &query("db.", record_type)).await;
        assert_eq!(
            answer.metadata.response_code,
            ResponseCode::NoError,
            "{record_type}"
        );
        assert!(answer.answers.is_empty(), "{record_type}");
    }
    assert!(upstream.asked().is_empty());
    // Each answer is recorded.
    let record = records.try_recv().unwrap();
    let Some(traffic_record::Record::Dns(dns)) = record.record else {
        panic!("not a DNS record: {record:?}");
    };
    assert_eq!((dns.name.as_str(), dns.r#type), ("web", 28));
    assert_eq!(dns.addresses, ["fd00::a", "fd00::b"]);

    // Other names go upstream as before: one not in the tables, one of
    // more labels, and one in another table than the pod's.
    let answer = ask_udp(server, &query("v6.test.", RecordType::AAAA)).await;
    assert_eq!(addresses(&answer), ips(&["2001:db8::6"]));
    let answer = ask_udp(server, &query("web.test.", RecordType::AAAA)).await;
    assert!(answer.answers.is_empty());
    let answer = ask_udp(server, &query("other.", RecordType::AAAA)).await;
    assert_eq!(answer.metadata.response_code, ResponseCode::NXDomain);
    resolver.set_client(
        LOOPBACK.into(),
        POD,
        vec![upstream.address],
        vec!["env-1".to_string()],
    );
    let answer = ask_udp(server, &query("vite.", RecordType::A)).await;
    assert_eq!(answer.metadata.response_code, ResponseCode::NXDomain);
    assert_eq!(
        upstream.asked(),
        [
            asked("v6.test.", RecordType::AAAA, false),
            asked("web.test.", RecordType::AAAA, false),
            asked("other.", RecordType::AAAA, false),
            asked("vite.", RecordType::A, false),
        ]
    );
    // A pod without tables asks upstream for every name.
    resolver.set_client(LOOPBACK.into(), POD, vec![upstream.address], Vec::new());
    let answer = ask_udp(server, &query("web.", RecordType::AAAA)).await;
    assert_eq!(answer.metadata.response_code, ResponseCode::NXDomain);
}

#[tokio::test]
async fn without_a_lookup_no_pod_has_names() {
    let upstream = upstream().await;
    let (resolver, server) = resolver(Config::default(), vec![upstream.address]).await;
    resolver.set_client(
        LOOPBACK.into(),
        POD,
        vec![upstream.address],
        vec!["env-1".to_string()],
    );
    let answer = ask_udp(server, &query("web.", RecordType::AAAA)).await;
    assert_eq!(answer.metadata.response_code, ResponseCode::NXDomain);
    assert_eq!(upstream.asked(), [asked("web.", RecordType::AAAA, false)]);
}
