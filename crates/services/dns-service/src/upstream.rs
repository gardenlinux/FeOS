// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Asking the upstream resolvers: over UDP from a socket of its own, so from
//! a port of the kernel's choosing, with an ID of ours; over TCP when the
//! query came over TCP or the answer does not fit. A response counts only
//! from the upstream asked, with the ID and the question of the query.

use crate::{dnssec_ok, read_message, write_message, UDP_PAYLOAD};
use hickory_proto::op::{Edns, Message, MessageType, OpCode, Query, ResponseCode};
use log::debug;
use std::io;
use std::net::{Ipv4Addr, Ipv6Addr, SocketAddr};
use std::time::Duration;
use tokio::net::{TcpStream, UdpSocket};
use tokio::time;

/// The largest UDP answer read; a larger one is asked for again over TCP.
const RESPONSE_BUFFER: usize = 4096;

/// The query for `query` that goes upstream on behalf of `request`: the
/// request's flags, and EDNS of our own.
pub fn query(request: &Message, query: Query) -> Message {
    let mut message = Message::new(0, MessageType::Query, OpCode::Query);
    message.metadata.recursion_desired = request.metadata.recursion_desired;
    message.metadata.authentic_data = request.metadata.authentic_data;
    message.metadata.checking_disabled = request.metadata.checking_disabled;
    message.add_query(query);
    let mut edns = Edns::new();
    edns.set_max_payload(UDP_PAYLOAD)
        .set_dnssec_ok(dnssec_ok(request));
    message.set_edns(edns);
    message
}

/// Asks `upstreams` in order until one answers. One that fails, refuses or
/// does not answer in time is skipped; when all are, the last refusal or
/// failure stands, or None when none answered.
pub async fn ask(
    upstreams: &[SocketAddr],
    query: &Message,
    tcp: bool,
    timeout: Duration,
) -> Option<Message> {
    let mut last = None;
    for &upstream in upstreams {
        match exchange(upstream, query, tcp, timeout).await {
            Ok(response)
                if matches!(
                    response.metadata.response_code,
                    ResponseCode::ServFail | ResponseCode::Refused
                ) =>
            {
                debug!(
                    "dns: upstream {upstream}: {}",
                    response.metadata.response_code
                );
                last = Some(response);
            }
            Ok(response) => return Some(response),
            Err(error) => debug!("dns: upstream {upstream}: {error}"),
        }
    }
    last
}

/// Asks one upstream: over UDP unless `tcp`, then over TCP if the answer
/// did not fit.
async fn exchange(
    upstream: SocketAddr,
    query: &Message,
    tcp: bool,
    timeout: Duration,
) -> io::Result<Message> {
    if !tcp {
        let answer = time::timeout(timeout, udp(upstream, query, rand::random()))
            .await
            .map_err(|_| io::Error::from(io::ErrorKind::TimedOut))??;
        if let Some(response) = answer {
            return Ok(response);
        }
    }
    // A connection takes a round trip more.
    time::timeout(timeout * 2, self::tcp(upstream, query, rand::random()))
        .await
        .map_err(|_| io::Error::from(io::ErrorKind::TimedOut))?
}

/// Asks over UDP. None when the answer is truncated or cannot be read, for
/// TCP to take over.
async fn udp(upstream: SocketAddr, query: &Message, id: u16) -> io::Result<Option<Message>> {
    let bytes = encode(query, id)?;
    let local: SocketAddr = match upstream {
        SocketAddr::V4(_) => (Ipv4Addr::UNSPECIFIED, 0).into(),
        SocketAddr::V6(_) => (Ipv6Addr::UNSPECIFIED, 0).into(),
    };
    // Connected, the socket takes datagrams from the upstream only.
    let socket = UdpSocket::bind(local).await?;
    socket.connect(upstream).await?;
    socket.send(&bytes).await?;
    let mut buffer = vec![0; RESPONSE_BUFFER];
    loop {
        let length = socket.recv(&mut buffer).await?;
        let datagram = &buffer[..length];
        // Another ID: a stray or a forgery.
        if datagram.len() < 2 || u16::from_be_bytes([datagram[0], datagram[1]]) != id {
            continue;
        }
        let Ok(response) = Message::from_vec(datagram) else {
            return Ok(None);
        };
        if !answers(query, &response) {
            continue;
        }
        if response.metadata.truncation {
            return Ok(None);
        }
        return Ok(Some(response));
    }
}

/// Asks over a TCP connection of its own.
async fn tcp(upstream: SocketAddr, query: &Message, id: u16) -> io::Result<Message> {
    let bytes = encode(query, id)?;
    let mut stream = TcpStream::connect(upstream).await?;
    write_message(&mut stream, &bytes).await?;
    let response = read_message(&mut stream)
        .await?
        .ok_or_else(|| io::Error::from(io::ErrorKind::UnexpectedEof))?;
    let response = Message::from_vec(&response).map_err(invalid)?;
    if response.metadata.id != id || !answers(query, &response) {
        return Err(invalid("a response to another query"));
    }
    Ok(response)
}

fn encode(query: &Message, id: u16) -> io::Result<Vec<u8>> {
    let mut query = query.clone();
    query.metadata.id = id;
    query.to_vec().map_err(invalid)
}

/// Whether `response` answers `query`: a response with the query's
/// question. An error may come without the question.
fn answers(query: &Message, response: &Message) -> bool {
    if response.metadata.message_type != MessageType::Response
        || response.metadata.op_code != OpCode::Query
    {
        return false;
    }
    match (query.queries.as_slice(), response.queries.as_slice()) {
        ([asked], [answered]) => {
            asked.name() == answered.name()
                && asked.query_type() == answered.query_type()
                && asked.query_class() == answered.query_class()
        }
        (_, []) => response.metadata.response_code != ResponseCode::NoError,
        _ => false,
    }
}

fn invalid(error: impl ToString) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, error.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;
    use hickory_proto::rr::{Name, RecordType};

    fn question(name: &str, record_type: RecordType) -> Query {
        Query::query(Name::from_ascii(name).unwrap(), record_type)
    }

    fn response(query: Option<Query>, code: ResponseCode) -> Message {
        let mut message = Message::error_msg(0, OpCode::Query, code);
        message.add_queries(query);
        message
    }

    #[test]
    fn matches_the_question() {
        let request = Message::new(0, MessageType::Query, OpCode::Query);
        let asked = query(&request, question("www.example.com.", RecordType::AAAA));
        let same = question("WWW.Example.com.", RecordType::AAAA);
        assert!(answers(
            &asked,
            &response(Some(same), ResponseCode::NoError)
        ));
        let other_type = question("www.example.com.", RecordType::A);
        assert!(!answers(
            &asked,
            &response(Some(other_type), ResponseCode::NoError)
        ));
        let other_name = question("www.example.org.", RecordType::AAAA);
        assert!(!answers(
            &asked,
            &response(Some(other_name), ResponseCode::NoError)
        ));
        assert!(!answers(&asked, &response(None, ResponseCode::NoError)));
        assert!(answers(&asked, &response(None, ResponseCode::FormErr)));
        // A query is no answer.
        let mut echo = asked.clone();
        echo.metadata.message_type = MessageType::Query;
        assert!(!answers(&asked, &echo));
    }

    #[test]
    fn asks_with_the_requests_flags() {
        let mut request = Message::new(0, MessageType::Query, OpCode::Query);
        request.metadata.recursion_desired = true;
        request.metadata.checking_disabled = true;
        let mut edns = Edns::new();
        edns.set_dnssec_ok(true).set_max_payload(4096);
        request.set_edns(edns);
        let asked = query(&request, question("www.example.com.", RecordType::A));
        assert!(asked.metadata.recursion_desired);
        assert!(asked.metadata.checking_disabled);
        let edns = asked.edns.as_ref().unwrap();
        assert!(edns.flags().dnssec_ok);
        assert_eq!(edns.max_payload(), UDP_PAYLOAD);
    }
}
