// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! End to end, in one process: a fake feosd on a real control socket, the
//! pod's connections passed as descriptors, HTTPS upstreams and a token
//! endpoint with test certificates, a resolver of fixed answers.

use std::fs::OpenOptions;
use std::net::SocketAddr;
use std::path::PathBuf;
use std::sync::Arc;
use std::time::{Duration, Instant};

use base64::Engine;
use bytes::Bytes;
use feos_proto::fleet_service::{
    http_binding, BasicCredential, HeaderCredential, HttpBinding, HttpBindingMode, InterceptCa,
    OAuthClientCredentials, PodBindings, RateLimit,
};
use http::{Request, StatusCode};
use http_body_util::Full;
use hyper_util::rt::TokioIo;
use serde_json::Value;
use tokio::io::{AsyncReadExt, AsyncWriteExt};

use crate::audit::Audit;
use crate::keys::Keys;
use crate::testkit::*;
use crate::{Authn, Net};

struct Harness {
    _dir: tempfile::TempDir,
    sock: PathBuf,
    feosd: Feosd,
    node: Node,
    audit: PathBuf,
}

async fn start(resolver: MapResolver) -> Harness {
    let dir = tempfile::tempdir().unwrap();
    let sock = dir.path().join("authn.sock");
    let listener = seqpacket_listener(&sock);
    let audit = dir.path().join("authn-audit.ndjson");
    let file = OpenOptions::new()
        .create(true)
        .append(true)
        .open(&audit)
        .unwrap();
    let authn = Arc::new(Authn::new(
        POD_ID.into(),
        Keys::generate().unwrap(),
        Arc::new(Audit::new(file.into())),
        Arc::new(Net::new(Arc::new(resolver))),
    ));
    tokio::spawn(async move {
        let _ = authn.run(listener).await;
    });
    let feosd = Feosd::connect(&sock);
    Harness {
        _dir: dir,
        sock,
        feosd,
        node: Node::new().await,
        audit,
    }
}

fn local(addr: SocketAddr) -> Vec<SocketAddr> {
    vec![addr]
}

fn headers(name: &str, upstream: &str, ca: &str, h: &[(&str, &str)]) -> HttpBinding {
    HttpBinding {
        name: name.into(),
        upstream: upstream.into(),
        ca_pem: ca.into(),
        credential: Some(http_binding::Credential::Headers(HeaderCredential {
            headers: h
                .iter()
                .map(|(n, v)| (n.to_string(), v.to_string()))
                .collect(),
        })),
        ..Default::default()
    }
}

fn header<'a>(s: &'a Seen, n: &str) -> Vec<&'a str> {
    s.headers
        .get_all(n)
        .iter()
        .map(|v| v.to_str().unwrap())
        .collect()
}

fn refusal_code(h: &http::HeaderMap, body: &Bytes) -> String {
    let v: Value = serde_json::from_slice(body).unwrap();
    let code = v["error"]["code"].as_str().unwrap().to_string();
    assert_eq!(h["feos-binding-error"], code.as_str());
    assert_eq!(h["content-type"], "application/json");
    code
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn name_mode_end_to_end() {
    let pki = Pki::new();
    let up = Upstream::start(pki.server_config(&["api.erp.test"])).await;
    let port = up.addr.port();
    let mut r = MapResolver::default();
    r.0.insert(("api.erp.test".into(), port), local(up.addr));
    let h = start(r).await;
    let mut b = headers(
        "erp",
        &format!("https://api.erp.test:{port}"),
        &pki.ca_pem,
        &[("X-API-Key", "s3cret-key")],
    );
    b.path_prefixes = vec![
        "/api".into(),
        "/redirect".into(),
        "/ws".into(),
        "/big".into(),
    ];
    b.forwarded_host = true;
    h.feosd.hello(&["10.0.0.0/8"], Some("127.0.0.1"));
    h.feosd.answer(
        &PodBindings {
            http: vec![b],
            ..Default::default()
        },
        "v1",
    );
    h.feosd.ready("v1");

    let mut s = h1(h.node.connect(&h.feosd, 80).await).await;
    let tp = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01";
    let req = Request::get("/api/echo?q=1")
        .header("host", "erp")
        .header("authorization", "Bearer the-pods-own")
        .header("x-api-key", "forged")
        .header("proxy-authorization", "Basic x")
        .header("traceparent", tp)
        .header("tracestate", "vendor=x")
        .body(Full::new(Bytes::new()))
        .unwrap();
    let (status, hdrs, body) = read(send1(&mut s, req).await).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(&body[..], b"ok");
    assert!(hdrs.get("alt-svc").is_none());
    let seen = up.last();
    // ALPN chose h2 with the upstream.
    assert_eq!(seen.version, http::Version::HTTP_2);
    assert!(seen.uri.ends_with("/api/echo?q=1"), "{}", seen.uri);
    assert_eq!(authority(&seen), format!("api.erp.test:{port}"));
    assert_eq!(header(&seen, "x-api-key"), vec!["s3cret-key"]);
    assert!(header(&seen, "authorization").is_empty());
    assert!(header(&seen, "proxy-authorization").is_empty());
    assert_eq!(header(&seen, "x-forwarded-host"), vec!["erp"]);
    assert_eq!(header(&seen, "x-forwarded-proto"), vec!["http"]);
    let up_tp = header(&seen, "traceparent")[0].to_string();
    assert!(up_tp.starts_with("00-0af7651916cd43dd8448eb211c80319c-"));
    assert!(!up_tp.contains("b7ad6b7169203331"));
    assert_eq!(header(&seen, "tracestate"), vec!["vendor=x"]);
    let rec = await_record(&h.audit, |r| {
        r["type"] == "http" && r["path"] == "/api/echo"
    })
    .await;
    assert_eq!(rec["binding"], "erp");
    assert_eq!(rec["mode"], "name");
    assert_eq!(rec["method"], "GET");
    assert_eq!(rec["host"], "api.erp.test");
    assert_eq!(rec["status"], 200);
    assert_eq!(rec["credential"], "headers");
    assert_eq!(rec["upstreamAddr"], up.addr.to_string());
    assert_eq!(rec["traceId"], "0af7651916cd43dd8448eb211c80319c");
    assert_eq!(rec["parentSpanId"], "b7ad6b7169203331");
    assert_eq!(
        up_tp,
        format!(
            "00-{}-{}-01",
            rec["traceId"].as_str().unwrap(),
            rec["spanId"].as_str().unwrap()
        )
    );
    let end = await_record(&h.audit, |r| {
        r["type"] == "http_end" && r["spanId"] == rec["spanId"]
    })
    .await;
    assert_eq!(end["bytesOut"], 2);
    assert_eq!(end["error"], Value::Null);

    // Location on the upstream's origin becomes the binding's.
    let (status, hdrs, _) = read(send1(&mut s, get("erp", "/redirect")).await).await;
    assert_eq!(status, StatusCode::FOUND);
    assert_eq!(hdrs["location"], "http://erp/next?page=2");
    assert_eq!(hdrs["content-location"], "https://elsewhere.example/x");

    // Outside the prefixes, and an unknown name: refused here.
    let (status, hdrs, body) = read(send1(&mut s, get("erp", "/admin")).await).await;
    assert_eq!(status, StatusCode::FORBIDDEN);
    assert_eq!(refusal_code(&hdrs, &body), "denied_path");
    let (status, hdrs, body) = read(send1(&mut s, get("nope", "/api")).await).await;
    assert_eq!(status, StatusCode::NOT_FOUND);
    assert_eq!(refusal_code(&hdrs, &body), "unknown_binding");
    let n = up.count();
    assert_eq!(n, 2, "refusals never reach the upstream");

    // A request body passes and is counted.
    let post = Request::post("/api/post")
        .header("host", "erp")
        .body(Full::new(Bytes::from_static(b"hello")))
        .unwrap();
    let (status, _, _) = read(send1(&mut s, post).await).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(&up.last().body[..], b"hello");
    let rec = await_record(&h.audit, |r| {
        r["type"] == "http" && r["path"] == "/api/post"
    })
    .await;
    let end = await_record(&h.audit, |r| {
        r["type"] == "http_end" && r["spanId"] == rec["spanId"]
    })
    .await;
    assert_eq!(end["bytesIn"], 5);

    // A large answer streams through and is counted.
    let (status, _, body) = read(send1(&mut s, get("erp", "/big")).await).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(body.len(), BIG);
    let rec = await_record(&h.audit, |r| r["type"] == "http" && r["path"] == "/big").await;
    let end = await_record(&h.audit, |r| {
        r["type"] == "http_end" && r["spanId"] == rec["spanId"]
    })
    .await;
    assert_eq!(end["bytesOut"], BIG as u64);

    // An upgrade becomes a pipe, over HTTP/1.1 to the upstream.
    let mut s = h1(h.node.connect(&h.feosd, 80).await).await;
    let req = Request::get("/ws")
        .header("host", "erp")
        .header("connection", "upgrade")
        .header("upgrade", "echo")
        .body(Full::new(Bytes::new()))
        .unwrap();
    let mut resp = send1(&mut s, req).await;
    assert_eq!(resp.status(), StatusCode::SWITCHING_PROTOCOLS);
    assert_eq!(resp.headers()["upgrade"], "echo");
    let io = hyper::upgrade::on(&mut resp).await.unwrap();
    let mut io = TokioIo::new(io);
    io.write_all(b"ping").await.unwrap();
    let mut buf = [0u8; 4];
    io.read_exact(&mut buf).await.unwrap();
    assert_eq!(&buf, b"ping");
    assert_eq!(up.last().version, http::Version::HTTP_11);
    drop(io);
    let rec = await_record(&h.audit, |r| r["type"] == "http" && r["path"] == "/ws").await;
    assert_eq!(rec["status"], 101);
    let end = await_record(&h.audit, |r| {
        r["type"] == "http_end" && r["spanId"] == rec["spanId"]
    })
    .await;
    assert_eq!(end["bytesIn"], 4, "{end}");
    assert_eq!(end["bytesOut"], 4);

    // No record holds the credential.
    let all = std::fs::read_to_string(&h.audit).unwrap();
    assert!(!all.contains("s3cret-key"));
    assert!(records(&h.audit)
        .iter()
        .any(|r| r["type"] == "bindings" && r["version"] == "v1"));
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn intercept_mode_end_to_end() {
    let pki = Pki::new();
    let up = Upstream::start(pki.server_config(&["api.partner.test"])).await;
    let mut r = MapResolver::default();
    r.0.insert(("api.partner.test".into(), 443), local(up.addr));
    let h = start(r).await;
    let (old_ca, old_key) = intercept_ca(&["api.partner.test"]);
    let (new_ca, new_key) = intercept_ca(&["api.partner.test"]);
    let b = HttpBinding {
        name: "partner".into(),
        upstream: "https://api.partner.test".into(),
        ca_pem: pki.ca_pem.clone(),
        mode: HttpBindingMode::Intercept as i32,
        credential: Some(http_binding::Credential::Basic(BasicCredential {
            user: "u".into(),
            password: "p@ss".into(),
        })),
        ..Default::default()
    };
    h.feosd.hello(&[], None);
    h.feosd.answer(
        &PodBindings {
            http: vec![b],
            intercept_cas: vec![
                InterceptCa {
                    cert_pem: old_ca.clone(),
                    key_pem: old_key,
                },
                InterceptCa {
                    cert_pem: new_ca.clone(),
                    key_pem: new_key,
                },
            ],
            ..Default::default()
        },
        "v1",
    );
    h.feosd.ready("v1");
    let basic = format!(
        "Basic {}",
        base64::engine::general_purpose::STANDARD.encode("u:p@ss")
    );

    // HTTP/2 from the pod, under the upstream's own name.
    let tls = tls_connect(
        h.node.connect(&h.feosd, 443).await,
        tls_client(&new_ca, &[b"h2", b"http/1.1"]),
        "api.partner.test",
    )
    .await
    .unwrap();
    assert_eq!(tls.get_ref().1.alpn_protocol(), Some(&b"h2"[..]));
    let (mut send, conn) = hyper::client::conn::http2::handshake(
        hyper_util::rt::TokioExecutor::new(),
        TokioIo::new(tls),
    )
    .await
    .unwrap();
    tokio::spawn(conn);
    let req = Request::get("https://api.partner.test/x?y=1")
        .header("authorization", "Bearer the-pods-own")
        .body(Full::new(Bytes::new()))
        .unwrap();
    let (status, _, body) = read(send.send_request(req).await.unwrap()).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(&body[..], b"ok");
    let seen = up.last();
    assert_eq!(authority(&seen), "api.partner.test");
    assert_eq!(header(&seen, "authorization"), vec![basic.as_str()]);
    assert!(seen.uri.ends_with("/x?y=1"));
    // The real name needs no Location rewrite.
    let req = Request::get("https://api.partner.test/redirect")
        .body(Full::new(Bytes::new()))
        .unwrap();
    let (status, hdrs, _) = read(send.send_request(req).await.unwrap()).await;
    assert_eq!(status, StatusCode::FOUND);
    assert_eq!(hdrs["location"], "https://api.partner.test/next?page=2");
    let rec = await_record(&h.audit, |r| r["type"] == "http" && r["path"] == "/x").await;
    assert_eq!(rec["mode"], "intercept");
    assert_eq!(rec["binding"], "partner");
    assert_eq!(rec["credential"], "basic");

    // HTTP/1.1 over TLS; a Host other than the SNI is misdirected.
    let tls = tls_connect(
        h.node.connect(&h.feosd, 443).await,
        tls_client(&new_ca, &[b"http/1.1"]),
        "api.partner.test",
    )
    .await
    .unwrap();
    let (mut send, conn) = hyper::client::conn::http1::handshake(TokioIo::new(tls))
        .await
        .unwrap();
    tokio::spawn(conn);
    let (status, _, _) = read(send1(&mut send, get("api.partner.test", "/h1")).await).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(up.last().version, http::Version::HTTP_2);
    let (status, hdrs, body) = read(send1(&mut send, get("other.test", "/h1")).await).await;
    assert_eq!(status, StatusCode::MISDIRECTED_REQUEST);
    assert_eq!(refusal_code(&hdrs, &body), "misdirected");

    // The leaf is the newest CA's: a client of the old one refuses it.
    let old = tls_connect(
        h.node.connect(&h.feosd, 443).await,
        tls_client(&old_ca, &[b"h2"]),
        "api.partner.test",
    )
    .await;
    assert!(old.is_err());
    // A name that is no intercept binding's gets no certificate at all.
    let other = tls_connect(
        h.node.connect(&h.feosd, 443).await,
        tls_client(&new_ca, &[b"h2"]),
        "other.test",
    )
    .await;
    let e = other.unwrap_err().to_string();
    assert!(!e.contains("certificate"), "{e}");
}

/// HTTP/2 lets a request's `:path` go without its "/": after the
/// upstream's origin it would name another host ("@attacker.test/"
/// makes `https://api.partner.test@attacker.test/`). It is refused, and
/// nothing dials anywhere else.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_path_without_its_slash_never_reaches_another_host() {
    let pki = Pki::new();
    let up = Upstream::start(pki.server_config(&["api.partner.test"])).await;
    let attacker = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let dialed = Arc::new(std::sync::atomic::AtomicUsize::new(0));
    let mut r = MapResolver::default();
    r.0.insert(("api.partner.test".into(), 443), local(up.addr));
    for other in [
        ("attacker.test", 443),
        ("api.partner.test.attacker.test", 443),
        ("api.partner.test", 1),
    ] {
        r.0.insert(
            (other.0.into(), other.1),
            local(attacker.local_addr().unwrap()),
        );
    }
    {
        let dialed = dialed.clone();
        tokio::spawn(async move {
            while attacker.accept().await.is_ok() {
                dialed.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
            }
        });
    }
    let h = start(r).await;
    let (ca, ca_key) = intercept_ca(&["api.partner.test"]);
    let b = HttpBinding {
        name: "partner".into(),
        upstream: "https://api.partner.test".into(),
        ca_pem: pki.ca_pem.clone(),
        mode: HttpBindingMode::Intercept as i32,
        credential: Some(http_binding::Credential::Basic(BasicCredential {
            user: "u".into(),
            password: "p".into(),
        })),
        ..Default::default()
    };
    h.feosd.hello(&[], None);
    h.feosd.answer(
        &PodBindings {
            http: vec![b],
            intercept_cas: vec![InterceptCa {
                cert_pem: ca.clone(),
                key_pem: ca_key,
            }],
            ..Default::default()
        },
        "v1",
    );
    h.feosd.ready("v1");
    let tls = tls_connect(
        h.node.connect(&h.feosd, 443).await,
        tls_client(&ca, &[b"h2"]),
        "api.partner.test",
    )
    .await
    .unwrap();
    let (mut send, conn) = hyper::client::conn::http2::handshake(
        hyper_util::rt::TokioExecutor::new(),
        TokioIo::new(tls),
    )
    .await
    .unwrap();
    tokio::spawn(conn);
    for path in ["@attacker.test/", ".attacker.test/", ":1/", "?x=1"] {
        let mut parts = http::uri::Parts::default();
        parts.scheme = Some(http::uri::Scheme::HTTPS);
        parts.authority = Some(http::uri::Authority::from_static("api.partner.test"));
        parts.path_and_query = Some(http::uri::PathAndQuery::from_static(path));
        let mut req = Request::new(Full::new(Bytes::new()));
        *req.uri_mut() = http::Uri::from_parts(parts).unwrap();
        let (status, hdrs, body) = read(send.send_request(req).await.unwrap()).await;
        assert_eq!(status, StatusCode::BAD_REQUEST, "{path}");
        assert_eq!(refusal_code(&hdrs, &body), "bad_request", "{path}");
    }
    // The connection serves on; what was refused reached nobody.
    let req = Request::get("https://api.partner.test/ok")
        .body(Full::new(Bytes::new()))
        .unwrap();
    let (status, _, _) = read(send.send_request(req).await.unwrap()).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(up.count(), 1);
    assert_eq!(authority(&up.last()), "api.partner.test");
    tokio::time::sleep(Duration::from_millis(100)).await;
    assert_eq!(dialed.load(std::sync::atomic::Ordering::SeqCst), 0);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn oauth_tokens_renew() {
    let pki = Pki::new();
    let up = Upstream::start(pki.server_config(&["api.graph.test"])).await;
    let tokens = TokenServer::start(pki.server_config(&["login.test"]), 2).await;
    let mut r = MapResolver::default();
    r.0.insert(("api.graph.test".into(), up.addr.port()), local(up.addr));
    r.0.insert(
        ("login.test".into(), tokens.addr.port()),
        local(tokens.addr),
    );
    let h = start(r).await;
    let b = HttpBinding {
        name: "graph".into(),
        upstream: format!("https://api.graph.test:{}", up.addr.port()),
        ca_pem: pki.ca_pem.clone(),
        credential: Some(http_binding::Credential::Oauth2(OAuthClientCredentials {
            token_url: format!("https://login.test:{}/token", tokens.addr.port()),
            client_id: "id".into(),
            client_secret: "se:cret".into(),
            scope: "read".into(),
            audience: "graph".into(),
            client_auth_in_body: false,
        })),
        ..Default::default()
    };
    h.feosd.hello(&[], None);
    h.feosd.answer(
        &PodBindings {
            http: vec![b],
            ..Default::default()
        },
        "v1",
    );
    h.feosd.ready("v1");
    let mut s = h1(h.node.connect(&h.feosd, 80).await).await;

    // tok-1 is refused: renewed once and the GET repeated with tok-2.
    let (status, _, _) = read(send1(&mut s, get("graph", "/protected")).await).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(tokens.fetches.load(std::sync::atomic::Ordering::SeqCst), 2);
    assert_eq!(up.count(), 2);
    assert_eq!(header(&up.last(), "authorization"), vec!["Bearer tok-2"]);
    let (form, auth) = tokens.requests.lock().unwrap()[0].clone();
    assert_eq!(
        form,
        "grant_type=client_credentials&scope=read&audience=graph"
    );
    assert_eq!(
        auth.unwrap(),
        format!(
            "Basic {}",
            base64::engine::general_purpose::STANDARD.encode("id:se%3Acret")
        )
    );
    let first = await_record(&h.audit, |r| {
        r["type"] == "token" && r["reason"] == "first_use"
    })
    .await;
    assert_eq!(first["status"], 200);
    assert_eq!(first["expiresIn"], 2);
    assert_eq!(first["host"], "login.test");
    await_record(&h.audit, |r| {
        r["type"] == "token" && r["reason"] == "unauthorized"
    })
    .await;
    let rec = await_record(&h.audit, |r| {
        r["type"] == "http" && r["path"] == "/protected"
    })
    .await;
    assert_eq!(rec["credential"], "oauth2");
    assert_eq!(rec["status"], 200);

    // Kept while young.
    let (status, _, _) = read(send1(&mut s, get("graph", "/protected")).await).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(tokens.fetches.load(std::sync::atomic::Ordering::SeqCst), 2);
    // Renewed after three quarters of its 2 s.
    tokio::time::sleep(Duration::from_millis(1600)).await;
    let (status, _, _) = read(send1(&mut s, get("graph", "/protected")).await).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(tokens.fetches.load(std::sync::atomic::Ordering::SeqCst), 3);
    assert_eq!(header(&up.last(), "authorization"), vec!["Bearer tok-3"]);
    await_record(&h.audit, |r| {
        r["type"] == "token" && r["reason"] == "renewal"
    })
    .await;

    let all = std::fs::read_to_string(&h.audit).unwrap();
    assert!(!all.contains("tok-") && !all.contains("se:cret") && !all.contains("se%3Acret"));
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn refusals() {
    let pki = Pki::new();
    let up = Upstream::start(pki.server_config(&["api.erp.test"])).await;
    let port = up.addr.port();
    let closed = {
        let l = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        l.local_addr().unwrap()
    };
    let mut r = MapResolver::default();
    r.0.insert(("api.erp.test".into(), port), local(up.addr));
    r.0.insert(
        ("internal.test".into(), 443),
        vec!["10.1.2.3:443".parse().unwrap()],
    );
    r.0.insert(("down.test".into(), closed.port()), local(closed));
    let h = start(r).await;
    let upstream = format!("https://api.erp.test:{port}");
    let mut limited = headers("limited", &upstream, &pki.ca_pem, &[("X-Key", "k")]);
    limited.rate_limit = Some(RateLimit {
        per_second: 1,
        burst: 2,
    });
    let set = PodBindings {
        http: vec![
            headers("internal", "https://internal.test", "", &[("X-Key", "k")]),
            headers(
                "down",
                &format!("https://down.test:{}", closed.port()),
                "",
                &[("X-Key", "k")],
            ),
            headers("untrusted", &upstream, "", &[("X-Key", "k")]),
            headers("broken", &upstream, &pki.ca_pem, &[("X-Key", "bad\nvalue")]),
            limited,
        ],
        ..Default::default()
    };
    h.feosd.hello(&["10.0.0.0/8"], None);
    h.feosd.answer(&set, "v1");
    h.feosd.ready("v1");
    let mut s = h1(h.node.connect(&h.feosd, 80).await).await;
    for (name, status, code) in [
        ("internal", StatusCode::FORBIDDEN, "denied_destination"),
        ("down", StatusCode::BAD_GATEWAY, "unreachable"),
        ("untrusted", StatusCode::BAD_GATEWAY, "tls"),
        ("broken", StatusCode::SERVICE_UNAVAILABLE, "binding_invalid"),
    ] {
        let (got, hdrs, body) = read(send1(&mut s, get(name, "/")).await).await;
        assert_eq!(got, status, "{name}");
        assert_eq!(refusal_code(&hdrs, &body), code, "{name}");
        let rec = await_record(&h.audit, |r| r["type"] == "http" && r["binding"] == name).await;
        assert_eq!(rec["error"]["code"], code);
        assert_eq!(rec["status"], status.as_u16());
    }
    let all = std::fs::read_to_string(&h.audit).unwrap();
    assert!(!all.contains("bad\\nvalue"));
    // Two at once, then 429 until the bucket fills again.
    for _ in 0..2 {
        let (got, _, _) = read(send1(&mut s, get("limited", "/")).await).await;
        assert_eq!(got, StatusCode::OK);
    }
    let (got, hdrs, body) = read(send1(&mut s, get("limited", "/")).await).await;
    assert_eq!(got, StatusCode::TOO_MANY_REQUESTS);
    assert_eq!(refusal_code(&hdrs, &body), "rate_limited");
    assert_eq!(hdrs["retry-after"], "1");
    assert_eq!(up.count(), 2);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn control_protocol() {
    let pki = Pki::new();
    let up = Upstream::start(pki.server_config(&["api.erp.test"])).await;
    let port = up.addr.port();
    let mut r = MapResolver::default();
    r.0.insert(("api.erp.test".into(), port), local(up.addr));
    let h = start(r).await;
    let upstream = format!("https://api.erp.test:{port}");
    let set = |name: &str| PodBindings {
        http: vec![headers(name, &upstream, &pki.ca_pem, &[("X-Key", "k")])],
        ..Default::default()
    };

    // Nothing before hello; then the request for credentials.
    h.feosd.hello(&[], None);
    let req = h.feosd.recv();
    assert_eq!(req["op"], "credentials");
    let key = req["hpkePublicKey"].as_str().unwrap().to_string();
    let csr = base64::engine::general_purpose::STANDARD
        .decode(req["csr"].as_str().unwrap())
        .unwrap();
    use x509_parser::prelude::FromDer;
    x509_parser::certification_request::X509CertificationRequest::from_der(&csr).unwrap();

    // Without bindings, a request waits, then gets 503 not_ready.
    let mut s = h1(h.node.connect(&h.feosd, 80).await).await;
    let t0 = Instant::now();
    let (status, hdrs, body) = read(send1(&mut s, get("one", "/")).await).await;
    assert_eq!(status, StatusCode::SERVICE_UNAVAILABLE);
    assert_eq!(refusal_code(&hdrs, &body), "not_ready");
    assert!(t0.elapsed() >= crate::proxy::READY_WAIT - Duration::from_millis(100));

    // The answer to that request: sealed to its key.
    let b64 = base64::engine::general_purpose::STANDARD;
    let mut eph = [0u8; 32];
    ring::rand::SecureRandom::fill(&ring::rand::SystemRandom::new(), &mut eph).unwrap();
    let pk: [u8; 32] = b64.decode(&key).unwrap().try_into().unwrap();
    let (enc, ct) = crate::hpke::seal(
        pk,
        &x25519_dalek::StaticSecret::from(eph),
        crate::hpke::BINDINGS_INFO,
        &crate::hpke::bindings_aad(POD_ID, "v1"),
        &prost::Message::encode_to_vec(&set("one")),
    )
    .unwrap();
    h.feosd.send(
        &serde_json::json!({"v": 1, "op": "credentials", "certificate": {"spiffeId": "spiffe://x"},
            "bindings": {"enc": b64.encode(enc), "ciphertext": b64.encode(ct), "version": "v1"}}),
        &[],
    );
    h.feosd.ready("v1");
    let (status, _, _) = read(send1(&mut s, get("one", "/")).await).await;
    assert_eq!(status, StatusCode::OK);

    // refetch: the same key asks again, and the new set replaces the old.
    h.feosd
        .send(&serde_json::json!({"v": 1, "op": "refetch"}), &[]);
    let again = h.feosd.answer(&set("two"), "v2");
    assert_eq!(again["hpkePublicKey"], key.as_str());
    h.feosd.ready("v2");
    let (status, _, _) = read(send1(&mut s, get("one", "/")).await).await;
    assert_eq!(status, StatusCode::NOT_FOUND);
    let (status, _, _) = read(send1(&mut s, get("two", "/")).await).await;
    assert_eq!(status, StatusCode::OK);

    // An error from the control plane: asked again after a pause.
    h.feosd
        .send(&serde_json::json!({"v": 1, "op": "refetch"}), &[]);
    assert_eq!(h.feosd.recv()["op"], "credentials");
    let t0 = Instant::now();
    h.feosd.send(
        &serde_json::json!({"v": 1, "op": "error", "message": "RESOURCE_EXHAUSTED: too many"}),
        &[],
    );
    // A box for another pod does not open: said so, asked again later,
    // and the set in place keeps serving.
    h.feosd.answer_for(&set("three"), "v3", "k2v8d0h3q9y");
    assert!(t0.elapsed() >= Duration::from_millis(1900));
    let err = h.feosd.recv();
    assert_eq!(err["op"], "error");
    let (status, _, _) = read(send1(&mut s, get("two", "/")).await).await;
    assert_eq!(status, StatusCode::OK);
    h.feosd.answer(&set("three"), "v3");
    h.feosd.ready("v3");

    // Unknown ops and other versions are ignored.
    h.feosd
        .send(&serde_json::json!({"v": 1, "op": "future"}), &[]);
    h.feosd
        .send(&serde_json::json!({"v": 2, "op": "refetch"}), &[]);

    // A restarted feosd connects again: its hello is answered with ready.
    let second = Feosd::connect(&h.sock);
    second.hello(&[], None);
    second.ready("v3");
    // Connections now come over the new one.
    let mut s = h1(h.node.connect(&second, 80).await).await;
    let (status, _, _) = read(send1(&mut s, get("three", "/")).await).await;
    assert_eq!(status, StatusCode::OK);
    // A hello for another pod is closed.
    let third = Feosd::connect(&h.sock);
    third.send(
        &serde_json::json!({"v": 1, "op": "hello", "podId": "k2v8d0h3q9y", "denyPrefixes": []}),
        &[],
    );
    assert!(third.closed());
}
