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
use tokio::io::AsyncWriteExt;

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
        Arc::new(Net::allowing_loopback(Arc::new(resolver))),
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

    // An upgrade to another protocol than WebSocket is no upgrade: the
    // request goes on without it.
    let req = Request::get("/ws")
        .header("host", "erp")
        .header("connection", "upgrade")
        .header("upgrade", "h2c")
        .body(Full::new(Bytes::new()))
        .unwrap();
    let (status, _, body) = read(send1(&mut s, req).await).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(&body[..], b"ok");
    assert!(header(&up.last(), "upgrade").is_empty());

    // An upgrade becomes a pipe, over HTTP/1.1 to the upstream.
    let mut s = h1(h.node.connect(&h.feosd, 80).await).await;
    let req = Request::get("/ws")
        .header("host", "erp")
        .header("connection", "upgrade")
        .header("upgrade", "websocket")
        .body(Full::new(Bytes::new()))
        .unwrap();
    let mut resp = send1(&mut s, req).await;
    assert_eq!(resp.status(), StatusCode::SWITCHING_PROTOCOLS);
    assert_eq!(resp.headers()["upgrade"], "websocket");
    let io = hyper::upgrade::on(&mut resp).await.unwrap();
    let mut io = TokioIo::new(io);
    io.write_all(&ws_client_frame(b"ping")).await.unwrap();
    assert_eq!(ws_read(&mut io).await.unwrap(), b"ping");
    assert_eq!(up.last().version, http::Version::HTTP_11);
    drop(io);
    let rec = await_record(&h.audit, |r| {
        r["type"] == "http" && r["path"] == "/ws" && r["status"] == 101
    })
    .await;
    let end = await_record(&h.audit, |r| {
        r["type"] == "http_end" && r["spanId"] == rec["spanId"]
    })
    .await;
    // A client's frame of "ping" is 10 bytes, a server's 6.
    assert_eq!(end["bytesIn"], 10, "{end}");
    assert_eq!(end["bytesOut"], 6);

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
    // Past three quarters of its 2 s it serves on, and is renewed in the
    // background; the next request has the new one.
    tokio::time::sleep(Duration::from_millis(1600)).await;
    let (status, _, _) = read(send1(&mut s, get("graph", "/protected")).await).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(header(&up.last(), "authorization"), vec!["Bearer tok-2"]);
    await_record(&h.audit, |r| {
        r["type"] == "token" && r["reason"] == "renewal"
    })
    .await;
    assert_eq!(tokens.fetches.load(std::sync::atomic::Ordering::SeqCst), 3);
    let (status, _, _) = read(send1(&mut s, get("graph", "/protected")).await).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(header(&up.last(), "authorization"), vec!["Bearer tok-3"]);

    let all = std::fs::read_to_string(&h.audit).unwrap();
    assert!(!all.contains("tok-") && !all.contains("se:cret") && !all.contains("se%3Acret"));
}

/// A slow token endpoint stalls no request while the token is valid: the
/// renewal runs in the background, one at a time.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_renewal_does_not_stall_requests() {
    let pki = Pki::new();
    let up = Upstream::start(pki.server_config(&["api.graph.test"])).await;
    let tokens = TokenServer::start(pki.server_config(&["login.test"]), 4).await;
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
            client_secret: "secret".into(),
            ..Default::default()
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
    let fetches = || tokens.fetches.load(std::sync::atomic::Ordering::SeqCst);
    let (status, _, _) = read(send1(&mut s, get("graph", "/")).await).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(fetches(), 1);

    // Past three quarters of its 4 s, the endpoint takes 700 ms.
    tokens
        .delay_ms
        .store(700, std::sync::atomic::Ordering::SeqCst);
    tokio::time::sleep(Duration::from_millis(3100)).await;
    for _ in 0..3 {
        let t0 = Instant::now();
        let (status, _, _) = read(send1(&mut s, get("graph", "/")).await).await;
        assert_eq!(status, StatusCode::OK);
        assert!(
            t0.elapsed() < Duration::from_millis(400),
            "{:?}",
            t0.elapsed()
        );
        assert_eq!(header(&up.last(), "authorization"), vec!["Bearer tok-1"]);
    }
    await_record(&h.audit, |r| {
        r["type"] == "token" && r["reason"] == "renewal"
    })
    .await;
    assert_eq!(fetches(), 2, "one renewal");
    let (status, _, _) = read(send1(&mut s, get("graph", "/")).await).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(header(&up.last(), "authorization"), vec!["Bearer tok-2"]);
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

/// The answer to a request, as the pod gets it: its head and body.
async fn answer(
    s: &mut hyper::client::conn::http1::SendRequest<Full<Bytes>>,
    req: Request<Full<Bytes>>,
) -> (http::response::Parts, Bytes) {
    let (parts, body) = send1(s, req).await.into_parts();
    let body = http_body_util::BodyExt::collect(body).await.unwrap();
    (parts, body.to_bytes())
}

fn fault_body(credential: &str) -> String {
    format!(
        r#"{{"fault":{{"faultstring":"Rate limit quota violation. Quota limit exceeded. Identifier : {credential}","detail":{{"errorcode":"policies.ratelimit.QuotaViolation"}}}}}}"#
    )
}

/// The incident: an upstream repeated the API key in a 500's body, which
/// went to the pod as it came. Nothing of the key reaches the pod now: not
/// in the body (cut in the middle of the key), a header, a trailer, the
/// reason or a WebSocket frame; an answer that cannot be checked is
/// refused; what was replaced is counted, never recorded.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn answers_never_carry_the_credential() {
    // 24 bytes: its mask is the marker exactly.
    const KEY: &str = "erp-key-0123456789abcdef";
    const MASK: &str = "redacted-by-FeOS-binding";
    let pki = Pki::new();
    let up = Upstream::start(pki.server_config(&["api.erp.test", "api.erp2.test"])).await;
    let up1 = Upstream::start(pki.server_config_alpn(&["api.erp1.test"], &[b"http/1.1"])).await;
    let mut r = MapResolver::default();
    r.0.insert(("api.erp.test".into(), up.addr.port()), local(up.addr));
    r.0.insert(("api.erp2.test".into(), 443), local(up.addr));
    r.0.insert(("api.erp1.test".into(), up1.addr.port()), local(up1.addr));
    let h = start(r).await;
    let erp = format!("https://api.erp.test:{}", up.addr.port());
    let (ca, ca_key) = intercept_ca(&["api.erp2.test"]);
    let mut erp2 = headers(
        "erp2",
        "https://api.erp2.test",
        &pki.ca_pem,
        &[("X-API-Key", KEY)],
    );
    erp2.mode = HttpBindingMode::Intercept as i32;
    let set = PodBindings {
        http: vec![
            erp2,
            headers("erp", &erp, &pki.ca_pem, &[("X-API-Key", KEY)]),
            headers(
                "erp1",
                &format!("https://api.erp1.test:{}", up1.addr.port()),
                &pki.ca_pem,
                &[("X-API-Key", KEY)],
            ),
            // Too short to be redacted: its answers pass as they came.
            headers("short", &erp, &pki.ca_pem, &[("X-API-Key", "k3y")]),
        ],
        intercept_cas: vec![InterceptCa {
            cert_pem: ca.clone(),
            key_pem: ca_key,
        }],
        ..Default::default()
    };
    h.feosd.hello(&[], None);
    h.feosd.answer(&set, "v1");
    h.feosd.ready("v1");
    let mut s = h1(h.node.connect(&h.feosd, 80).await).await;

    // The body and a header. The pod asked for gzip and a range; the
    // upstream was asked for neither.
    let req = Request::get("/fault")
        .header("host", "erp")
        .header("accept-encoding", "gzip, br")
        .header("range", "bytes=0-99")
        .body(Full::new(Bytes::new()))
        .unwrap();
    let (parts, body) = answer(&mut s, req).await;
    assert_eq!(parts.status, StatusCode::INTERNAL_SERVER_ERROR);
    assert_eq!(std::str::from_utf8(&body).unwrap(), fault_body(MASK));
    assert_eq!(parts.headers["x-quota-key"], MASK);
    let seen = up.last();
    assert_eq!(header(&seen, "accept-encoding"), vec!["identity"]);
    assert!(header(&seen, "range").is_empty());
    assert_eq!(header(&seen, "x-api-key"), vec![KEY]);
    let rec = await_record(&h.audit, |r| r["type"] == "http" && r["path"] == "/fault").await;
    assert_eq!(rec["redacted"], 1, "the header: {rec}");
    let end = await_record(&h.audit, |r| {
        r["type"] == "http_end" && r["spanId"] == rec["spanId"]
    })
    .await;
    // The trailer too, which an HTTP/1.1 pod does not get.
    assert_eq!(
        end["redacted"], 3,
        "the header, the body, the trailer: {end}"
    );
    assert_eq!(end["bytesOut"], fault_body(KEY).len());

    // HTTP/2 under the upstream's own name: the trailer as well.
    let tls = tls_connect(
        h.node.connect(&h.feosd, 443).await,
        tls_client(&ca, &[b"h2"]),
        "api.erp2.test",
    )
    .await
    .unwrap();
    let (mut send2, conn) = hyper::client::conn::http2::handshake(
        hyper_util::rt::TokioExecutor::new(),
        TokioIo::new(tls),
    )
    .await
    .unwrap();
    tokio::spawn(conn);
    let req = Request::get("https://api.erp2.test/fault")
        .body(Full::new(Bytes::new()))
        .unwrap();
    let (parts, body) = send2.send_request(req).await.unwrap().into_parts();
    let collected = http_body_util::BodyExt::collect(body).await.unwrap();
    let trailers = collected.trailers().cloned().unwrap();
    assert_eq!(trailers["x-debug"], format!("key={MASK}").as_str());
    assert_eq!(parts.headers["x-quota-key"], MASK);
    assert_eq!(
        std::str::from_utf8(&collected.to_bytes()).unwrap(),
        fault_body(MASK)
    );

    // WWW-Authenticate and the reason (HTTP/1.1 upstreams have one).
    for name in ["erp", "erp1"] {
        let (parts, _) = answer(&mut s, get(name, "/echo-header")).await;
        assert_eq!(parts.status, StatusCode::UNAUTHORIZED);
        assert_eq!(
            parts.headers["www-authenticate"],
            format!(r#"Bearer error="invalid_token", error_description="{MASK} is refused""#)
                .as_str()
        );
        assert_eq!(parts.headers["x-echo"], MASK);
        if name == "erp1" {
            let reason = parts.extensions.get::<hyper::ext::ReasonPhrase>().unwrap();
            assert_eq!(reason.as_bytes(), format!("Refused {MASK}").as_bytes());
        }
    }
    assert_eq!(up1.last().version, http::Version::HTTP_11);

    // gzip, sent although identity was asked for, is unpacked and
    // searched: the pod gets it plain, without the key, also when it
    // asked for gzip itself.
    for ask in [None, Some("gzip, br")] {
        let mut req = Request::get("/gzip").header("host", "erp");
        if let Some(a) = ask {
            req = req.header("accept-encoding", a);
        }
        let (parts, body) = answer(&mut s, req.body(Full::new(Bytes::new())).unwrap()).await;
        assert_eq!(parts.status, StatusCode::OK);
        assert!(!parts.headers.contains_key("content-encoding"));
        assert!(!parts.headers.contains_key("content-length"));
        assert_eq!(parts.headers["etag"], r#"W/"v1""#);
        assert_eq!(std::str::from_utf8(&body).unwrap(), gzip_body(MASK));
        assert_eq!(header(&up.last(), "accept-encoding"), vec!["identity"]);
    }

    // An answer that cannot be searched is refused; one without a body is
    // not.
    for path in ["/br", "/partial"] {
        let (parts, body) = answer(&mut s, get("erp", path)).await;
        assert_eq!(parts.status, StatusCode::BAD_GATEWAY, "{path}");
        assert_eq!(refusal_code(&parts.headers, &body), "unredactable");
    }
    let head = Request::head("/gzip")
        .header("host", "erp")
        .body(Full::new(Bytes::new()))
        .unwrap();
    let (parts, _) = answer(&mut s, head).await;
    assert_eq!(parts.status, StatusCode::OK);
    assert_eq!(parts.headers["content-encoding"], "gzip");

    // Nothing to redact: as it came, gzip asked for and all.
    let req = Request::get("/fault")
        .header("host", "short")
        .header("accept-encoding", "gzip")
        .body(Full::new(Bytes::new()))
        .unwrap();
    let (_, body) = answer(&mut s, req).await;
    assert_eq!(std::str::from_utf8(&body).unwrap(), fault_body("k3y"));
    assert_eq!(header(&up.last(), "accept-encoding"), vec!["gzip"]);

    // A large body without the key passes whole, its length announced.
    let (parts, body) = answer(&mut s, get("erp", "/big")).await;
    assert_eq!(parts.headers["content-length"], BIG.to_string().as_str());
    assert_eq!(body.len(), BIG);
    assert!(body.iter().all(|b| *b == b'x'));

    // A WebSocket: no extensions asked for, frames redacted.
    let mut s = h1(h.node.connect(&h.feosd, 80).await).await;
    let req = Request::get("/ws")
        .header("host", "erp")
        .header("connection", "upgrade")
        .header("upgrade", "websocket")
        .header("sec-websocket-extensions", "permessage-deflate")
        .body(Full::new(Bytes::new()))
        .unwrap();
    let mut resp = send1(&mut s, req).await;
    assert_eq!(resp.status(), StatusCode::SWITCHING_PROTOCOLS);
    assert!(header(&up.last(), "sec-websocket-extensions").is_empty());
    let io = hyper::upgrade::on(&mut resp).await.unwrap();
    let mut io = TokioIo::new(io);
    io.write_all(&ws_client_frame(b"key")).await.unwrap();
    assert_eq!(
        ws_read(&mut io).await.unwrap(),
        format!("your key is {MASK}").as_bytes()
    );
    io.write_all(&ws_client_frame(b"hello")).await.unwrap();
    assert_eq!(ws_read(&mut io).await.unwrap(), b"hello");
    drop(io);
    let rec = await_record(&h.audit, |r| {
        r["type"] == "http" && r["path"] == "/ws" && r["status"] == 101
    })
    .await;
    let end = await_record(&h.audit, |r| {
        r["type"] == "http_end" && r["spanId"] == rec["spanId"]
    })
    .await;
    assert_eq!(end["redacted"], 1, "{end}");

    let all = std::fs::read_to_string(&h.audit).unwrap();
    assert!(!all.contains(KEY));
}

/// Basic's user and password in every form, an OAuth token, and the
/// client secret in what a token endpoint says: none reaches the pod.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn basic_and_oauth_credentials_are_redacted() {
    const TOKEN: &str = "access-token-0123456789-";
    const SECRET: &str = "client-s3cret-value";
    let pki = Pki::new();
    let up = Upstream::start(pki.server_config(&["api.test"])).await;
    let tokens = TokenServer::start_with(pki.server_config(&["login.test"]), 3600, TOKEN).await;
    let failing = TokenServer::start(pki.server_config(&["login.test"]), 3600).await;
    let basic_of_client = base64::engine::general_purpose::STANDARD.encode(format!("id:{SECRET}"));
    // The secret again where the code is cut, at 64 characters.
    *failing.error.lock().unwrap() = Some(format!("bad {SECRET} {basic_of_client} {SECRET}"));
    let mut r = MapResolver::default();
    r.0.insert(("api.test".into(), up.addr.port()), local(up.addr));
    r.0.insert(
        ("login.test".into(), tokens.addr.port()),
        local(tokens.addr),
    );
    r.0.insert(
        ("login.test".into(), failing.addr.port()),
        local(failing.addr),
    );
    let h = start(r).await;
    let upstream = format!("https://api.test:{}", up.addr.port());
    let oauth = |name: &str, port: u16| HttpBinding {
        name: name.into(),
        upstream: upstream.clone(),
        ca_pem: pki.ca_pem.clone(),
        credential: Some(http_binding::Credential::Oauth2(OAuthClientCredentials {
            token_url: format!("https://login.test:{port}/token"),
            client_id: "id".into(),
            client_secret: SECRET.into(),
            ..Default::default()
        })),
        ..Default::default()
    };
    let set = PodBindings {
        http: vec![
            HttpBinding {
                name: "pay".into(),
                upstream: upstream.clone(),
                ca_pem: pki.ca_pem.clone(),
                credential: Some(http_binding::Credential::Basic(BasicCredential {
                    user: "svc-user".into(),
                    password: "pa55-w0rd-xyz!".into(),
                })),
                ..Default::default()
            },
            oauth("graph", tokens.addr.port()),
            oauth("fail", failing.addr.port()),
        ],
        ..Default::default()
    };
    h.feosd.hello(&[], None);
    h.feosd.answer(&set, "v1");
    h.feosd.ready("v1");
    let mut s = h1(h.node.connect(&h.feosd, 80).await).await;

    // Basic: the header's value, the pair and the password.
    let (parts, body) = answer(&mut s, get("pay", "/fault")).await;
    let forms = echoed(&up.last().headers);
    assert_eq!(forms.len(), 3, "{forms:?}");
    let body = String::from_utf8(body.to_vec()).unwrap();
    assert_eq!(body.len(), fault_body(&forms.join(" / ")).len());
    for f in &forms {
        assert!(!body.contains(f.as_str()), "{f} in {body}");
        assert!(!parts.headers["x-quota-key"]
            .to_str()
            .unwrap()
            .contains(f.as_str()));
    }
    assert!(!body.contains("pa55-w0rd"), "{body}");

    // OAuth: the token, with its scheme and alone.
    let (parts, body) = answer(&mut s, get("graph", "/fault")).await;
    let forms = echoed(&up.last().headers);
    assert_eq!(forms, vec![format!("Bearer {TOKEN}1"), format!("{TOKEN}1")]);
    let body = String::from_utf8(body.to_vec()).unwrap();
    assert!(!body.contains(TOKEN), "{body}");
    assert!(!parts.headers["x-quota-key"]
        .to_str()
        .unwrap()
        .contains(TOKEN));
    let (parts, _) = answer(&mut s, get("graph", "/echo-header")).await;
    assert!(!parts.headers["www-authenticate"]
        .to_str()
        .unwrap()
        .contains(TOKEN));

    // A token endpoint that repeats the client secret: the refusal says
    // what it said, without it.
    let (parts, body) = answer(&mut s, get("fail", "/")).await;
    assert_eq!(parts.status, StatusCode::SERVICE_UNAVAILABLE);
    assert_eq!(
        refusal_code(&parts.headers, &body),
        "credential_unavailable"
    );
    let body = String::from_utf8(body.to_vec()).unwrap();
    assert!(body.contains("(bad redacted"), "{body}");
    assert!(!body.contains(&basic_of_client), "{body}");
    // Not even the part before the cut.
    assert!(!body.contains("client-"), "{body}");

    let all = std::fs::read_to_string(&h.audit).unwrap();
    for v in [SECRET, &basic_of_client, TOKEN, "pa55-w0rd"] {
        assert!(!all.contains(v), "{v} in the audit");
    }
}
