// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The HTTP side of a pod's bindings (ADR 0028 §3–§5, §7): a request from
//! the pod, on a connection `feosd` accepted at 169.254.169.1 and passed
//! on, selects its binding (by `Host` in the name mode, by the TLS
//! connection's SNI in the intercept mode), is checked (path prefixes,
//! rate limit, requests in flight), loses what it may not carry and gains
//! the credential, and goes to the upstream over the binding's pool.
//!
//! The answer passes back with its `Location` rewritten to the binding's
//! name (name mode only); bodies are streamed, never rewritten, never
//! buffered whole. Upgrades (WebSocket) become a pipe. Every request is
//! recorded before its answer's head leaves ([`crate::audit`]); a request
//! whose record cannot be written is refused.
//!
//! The proxy's own refusals carry `FeOS-Binding-Error: <code>` and a JSON
//! body `{"error":{"code":…,"message":…}}`.

use std::convert::Infallible;
use std::sync::atomic::AtomicU64;
use std::sync::Arc;
use std::time::{Duration, Instant};

use http::header::{
    HeaderName, HeaderValue, AUTHORIZATION, CACHE_CONTROL, CONNECTION, CONTENT_LOCATION,
    CONTENT_TYPE, HOST, LOCATION, RETRY_AFTER, UPGRADE,
};
use http::uri::{Authority, PathAndQuery, Scheme};
use http::{HeaderMap, Method, Request, Response, StatusCode, Uri, Version};
use http_body::Body;
use hyper::body::Incoming;
use hyper::service::service_fn;
use hyper::upgrade::OnUpgrade;
use hyper_util::rt::{TokioExecutor, TokioIo, TokioTimer};
use tokio::io::{AsyncRead, AsyncWrite};
use tokio::net::TcpStream;
use tokio::sync::watch;

use crate::audit::{self, Audit, ErrorRec, HttpRecord};
use crate::bindings::{is_hop_by_hop, Binding, Credential, Mode, Snapshot, Upstream};
use crate::body::{EndGuard, ProxyBody};
use crate::dest::{ConnectErrorKind, Net};
use crate::oauth::TokenError;
use crate::trace::TraceContext;
use crate::upstream::{self, bare_host, HttpClient, UpstreamAddr};

/// How long a request waits for the pod's bindings before 503 not_ready.
pub const READY_WAIT: Duration = Duration::from_secs(5);
const HEADER_TIMEOUT: Duration = Duration::from_secs(30);
/// The header whose presence tells a refusal of the proxy's own from the
/// upstream's answers.
pub const BINDING_ERROR: &str = "feos-binding-error";
/// A record's path is cut here.
const MAX_RECORD_PATH: usize = 2048;

/// What every connection of the pod shares.
pub struct Ctx {
    pub audit: Arc<Audit>,
    pub net: Arc<Net>,
    pub ready: watch::Receiver<Option<Arc<Snapshot>>>,
}

impl Ctx {
    /// The current set, waiting for the first until `deadline`.
    pub async fn wait_ready(&self, deadline: Instant) -> Option<Arc<Snapshot>> {
        let mut rx = self.ready.clone();
        let wait = rx.wait_for(|s| s.is_some());
        match tokio::time::timeout_at(tokio::time::Instant::from_std(deadline), wait).await {
            Ok(Ok(s)) => s.clone(),
            _ => None,
        }
    }
}

/// How a connection reached the proxy.
pub enum Entry {
    /// Port 80: the binding is the request's `Host`.
    Name,
    /// Port 443: the binding is the one whose upstream the SNI named.
    Intercept { host: String },
}

/// Serves one connection to port 80.
pub async fn serve_name(ctx: Arc<Ctx>, stream: TcpStream) {
    serve_http(ctx, stream, Arc::new(Entry::Name), false).await;
}

/// Serves HTTP/1.1 (with upgrades) or HTTP/2 on one of the pod's
/// connections.
pub async fn serve_http<I>(ctx: Arc<Ctx>, io: I, entry: Arc<Entry>, h2: bool)
where
    I: AsyncRead + AsyncWrite + Unpin + Send + 'static,
{
    let svc = service_fn(move |req| {
        let (ctx, entry) = (ctx.clone(), entry.clone());
        async move { Ok::<_, Infallible>(handle(ctx, entry, req).await) }
    });
    let io = TokioIo::new(io);
    let result = if h2 {
        hyper::server::conn::http2::Builder::new(TokioExecutor::new())
            .timer(TokioTimer::new())
            .serve_connection(io, svc)
            .await
    } else {
        hyper::server::conn::http1::Builder::new()
            .timer(TokioTimer::new())
            .header_read_timeout(HEADER_TIMEOUT)
            .serve_connection(io, svc)
            .with_upgrades()
            .await
    };
    if let Err(e) = result {
        log::debug!("a pod's connection: {e}");
    }
}

/// A refusal of the proxy's own.
#[derive(Debug)]
pub struct Refusal {
    pub status: StatusCode,
    pub code: &'static str,
    pub message: String,
    pub retry_after: Option<u64>,
}

impl Refusal {
    pub fn new(status: StatusCode, code: &'static str, message: impl Into<String>) -> Refusal {
        Refusal {
            status,
            code,
            message: message.into(),
            retry_after: None,
        }
    }
}

/// What a request's records say about it.
struct Exchange {
    start: Instant,
    method: Method,
    path: String,
    trace: TraceContext,
    binding: Option<Arc<Binding>>,
    host: Option<String>,
    upstream_addr: Option<String>,
}

impl Exchange {
    fn write(
        &self,
        audit: &Audit,
        status: StatusCode,
        error: Option<ErrorRec>,
    ) -> std::io::Result<()> {
        let b = self.binding.as_deref();
        audit.write(&HttpRecord {
            ts: audit::now(),
            kind: "http",
            binding: b.map(|b| b.name.as_str()),
            mode: b.map(|b| b.mode.as_str()),
            method: self.method.as_str(),
            host: self.host.as_deref(),
            path: &self.path,
            status: status.as_u16(),
            duration_ms: self.start.elapsed().as_millis() as u64,
            upstream_addr: self.upstream_addr.clone(),
            credential: b.and_then(|b| b.credential_kind()),
            trace_id: self.trace.trace_id(),
            span_id: self.trace.span_id(),
            parent_span_id: self.trace.parent_span_id(),
            error,
        })
    }

    fn guard(&self, audit: &Arc<Audit>, bytes_in: Arc<AtomicU64>, status: StatusCode) -> EndGuard {
        let mut g = EndGuard::new(
            audit.clone(),
            self.binding.as_ref().map(|b| b.name.clone()),
            self.trace.trace_id(),
            self.trace.span_id(),
            self.start,
            bytes_in,
        );
        g.status = status.as_u16();
        g
    }
}

/// One request of the pod's: forwarded, or refused.
pub async fn handle(
    ctx: Arc<Ctx>,
    entry: Arc<Entry>,
    req: Request<Incoming>,
) -> Response<ProxyBody> {
    let mut path = req.uri().path().to_string();
    if path.len() > MAX_RECORD_PATH {
        let mut cut = MAX_RECORD_PATH;
        while !path.is_char_boundary(cut) {
            cut -= 1;
        }
        path.truncate(cut);
    }
    let mut x = Exchange {
        start: Instant::now(),
        method: req.method().clone(),
        path,
        trace: TraceContext::from_headers(req.headers()),
        binding: None,
        host: None,
        upstream_addr: None,
    };
    match forward(&ctx, &entry, req, &mut x).await {
        Ok(resp) => resp,
        Err(r) => refuse(&ctx, &x, r),
    }
}

fn refuse(ctx: &Ctx, x: &Exchange, r: Refusal) -> Response<ProxyBody> {
    let error = ErrorRec {
        code: r.code.to_string(),
        message: r.message.clone(),
    };
    let audited = x.write(&ctx.audit, r.status, Some(error)).is_ok();
    let r = if audited {
        r
    } else {
        log::error!("audit: the record of a refusal cannot be written");
        Refusal::new(
            StatusCode::SERVICE_UNAVAILABLE,
            "audit_unavailable",
            "the request's record cannot be written",
        )
    };
    let body = serde_json::json!({"error": {"code": r.code, "message": r.message}});
    let body = ProxyBody::full(serde_json::to_vec(&body).unwrap_or_default());
    let body = if audited {
        body.with_guard(x.guard(&ctx.audit, Arc::new(AtomicU64::new(0)), r.status))
    } else {
        body
    };
    let mut resp = Response::new(body);
    *resp.status_mut() = r.status;
    let h = resp.headers_mut();
    h.insert(CONTENT_TYPE, HeaderValue::from_static("application/json"));
    h.insert(CACHE_CONTROL, HeaderValue::from_static("no-store"));
    h.insert(BINDING_ERROR, HeaderValue::from_static(r.code));
    if let Some(s) = r.retry_after {
        h.insert(RETRY_AFTER, HeaderValue::from(s));
    }
    resp
}

/// The host a request asks for: its URI's authority (HTTP/2, or an
/// absolute-form request), else its `Host`; lowercase, without port or
/// final dot.
pub fn request_host(uri: &Uri, headers: &HeaderMap) -> Option<String> {
    let host = match uri.authority() {
        Some(a) => a.host().to_string(),
        None => {
            let h = headers.get(HOST)?.to_str().ok()?;
            h.parse::<Authority>().ok()?.host().to_string()
        }
    };
    let host = bare_host(&host);
    Some(host.strip_suffix('.').unwrap_or(host).to_ascii_lowercase())
}

/// Whether `path` may be reached under `prefixes` (all paths when there
/// are none). A prefix holds whole segments: "/api" holds "/api" and
/// "/api/x", not "/apix". With prefixes, a path with dot segments or
/// encoded separators or dots is refused, so that no upstream's
/// normalisation takes it out of its prefix; a dot segment with
/// parameters ("..;x"), which servlet containers take for one, too.
pub fn check_path(path: &str, prefixes: &[String]) -> Result<(), String> {
    if prefixes.is_empty() {
        return Ok(());
    }
    let lower = path.to_ascii_lowercase();
    if path.contains('\\') || ["%2f", "%5c", "%2e"].iter().any(|e| lower.contains(e)) {
        return Err(format!(
            "the path {path} has encoded separators or dots; the binding allows only {}",
            prefixes.join(", ")
        ));
    }
    let dots = |s: &str| matches!(s.split(';').next(), Some("." | ".."));
    if path.split('/').any(dots) {
        return Err(format!(
            "the path {path} has dot segments; the binding allows only {}",
            prefixes.join(", ")
        ));
    }
    let under = |p: &String| {
        path == p
            || (path.starts_with(p.as_str())
                && (p.ends_with('/') || path[p.len()..].starts_with('/')))
    };
    if prefixes.iter().any(under) {
        Ok(())
    } else {
        Err(format!(
            "the path {path} is outside the binding's prefixes {}",
            prefixes.join(", ")
        ))
    }
}

/// The upstream's URI for a request: the binding's origin, fixed, and
/// the request's path and query, which must begin with "/". Anything else
/// is refused: an asterisk, none (an authority-form request), or a path
/// without its "/", which HTTP/2 lets through and which after the origin
/// would name another host ("@other.example/", ".other.example/", ":1/").
pub fn upstream_uri(up: &Upstream, pq: Option<&PathAndQuery>) -> Result<Uri, Refusal> {
    let refused = || {
        Refusal::new(
            StatusCode::BAD_REQUEST,
            "bad_request",
            "the request's path does not begin with /",
        )
    };
    let pq = pq
        .filter(|pq| pq.as_str().starts_with('/'))
        .ok_or_else(refused)?;
    Uri::builder()
        .scheme(Scheme::HTTPS)
        .authority(up.authority.as_str())
        .path_and_query(pq.clone())
        .build()
        .map_err(|_| refused())
}

/// The lowercase tokens of a message's `Connection` headers.
fn connection_tokens(h: &HeaderMap) -> Vec<String> {
    h.get_all(CONNECTION)
        .iter()
        .filter_map(|v| v.to_str().ok())
        .flat_map(|v| v.split(','))
        .map(|t| t.trim().to_ascii_lowercase())
        .filter(|t| !t.is_empty())
        .collect()
}

/// Whether a request asks for the HTTP/1.1 upgrade a binding carries, to
/// WebSocket: `Connection: upgrade` with `Upgrade: websocket` alone. One
/// to another protocol is no upgrade here: it goes on as a request, and
/// its `Upgrade`, hop-by-hop, stays behind.
pub fn is_upgrade(h: &HeaderMap) -> bool {
    let mut upgrades = h.get_all(UPGRADE).iter();
    let websocket = match (upgrades.next(), upgrades.next()) {
        (Some(v), None) => v.as_bytes().trim_ascii().eq_ignore_ascii_case(b"websocket"),
        _ => false,
    };
    websocket && connection_tokens(h).iter().any(|t| t == "upgrade")
}

/// The request's headers for the upstream: without `Host` (the client sets
/// the upstream's), `Authorization`, `Expect`, every `Proxy-*`, the
/// hop-by-hop ones and those `Connection` names, the trace context (set
/// again by the caller) and every header the credential sets. `TE:
/// trailers` stays; an upgrade keeps `Upgrade` with `Connection: upgrade`.
pub fn upstream_headers(src: &HeaderMap, credential: &[HeaderName], upgrade: bool) -> HeaderMap {
    let listed = connection_tokens(src);
    let mut out = HeaderMap::with_capacity(src.len());
    for (n, v) in src {
        let s = n.as_str();
        let keep_upgrade = upgrade && s == "upgrade";
        let keep_te = s == "te" && v.as_bytes().eq_ignore_ascii_case(b"trailers");
        let dropped = matches!(
            s,
            "host" | "authorization" | "expect" | "traceparent" | "tracestate"
        ) || (is_hop_by_hop(n) && !keep_upgrade && !keep_te)
            || (listed.iter().any(|t| t == s) && !keep_upgrade)
            || credential.contains(n);
        if !dropped {
            out.append(n.clone(), v.clone());
        }
    }
    if upgrade {
        out.insert(CONNECTION, HeaderValue::from_static("upgrade"));
    }
    out
}

/// The answer's headers for the pod: without the hop-by-hop ones, those
/// `Connection` names and `Alt-Svc`, which names the upstream's endpoints,
/// not the proxy's. A 101 keeps `Upgrade` with `Connection: upgrade`.
pub fn downstream_headers(h: &mut HeaderMap, switching: bool) {
    let listed = connection_tokens(h);
    let names: Vec<HeaderName> = h.keys().cloned().collect();
    for n in names {
        let s = n.as_str();
        let keep = switching && s == "upgrade";
        if !keep && (is_hop_by_hop(&n) || listed.iter().any(|t| t == s) || s == "alt-svc") {
            h.remove(&n);
        }
    }
    if switching {
        h.insert(CONNECTION, HeaderValue::from_static("upgrade"));
    }
}

/// A URL on the upstream's origin, absolute or scheme-relative, as the
/// binding's: `http://<name>/…`. Anything else is left as it is (None).
pub fn rewrite_url(v: &str, up: &Upstream, name: &str) -> Option<String> {
    let (rest, scheme) = if v
        .get(..8)
        .is_some_and(|s| s.eq_ignore_ascii_case("https://"))
    {
        (&v[8..], "http://")
    } else {
        let r = v.strip_prefix("//")?;
        (r, "//")
    };
    let end = rest.find(['/', '?', '#']).unwrap_or(rest.len());
    let (auth, tail) = rest.split_at(end);
    if auth.is_empty() || auth.contains('@') {
        return None;
    }
    let a: Authority = auth.parse().ok()?;
    let host = bare_host(a.host()).to_ascii_lowercase();
    let host = host.strip_suffix('.').unwrap_or(&host);
    if host != up.host || a.port_u16().unwrap_or(443) != up.port {
        return None;
    }
    Some(format!("{scheme}{name}{tail}"))
}

fn rewrite_locations(h: &mut HeaderMap, up: &Upstream, name: &str) {
    for n in [LOCATION, CONTENT_LOCATION] {
        let new = h
            .get(&n)
            .and_then(|v| v.to_str().ok())
            .and_then(|v| rewrite_url(v, up, name))
            .and_then(|v| HeaderValue::from_str(&v).ok());
        if let Some(v) = new {
            h.insert(n, v);
        }
    }
}

fn is_idempotent(m: &Method) -> bool {
    matches!(
        *m,
        Method::GET | Method::HEAD | Method::OPTIONS | Method::TRACE | Method::PUT | Method::DELETE
    )
}

fn token_refusal(e: TokenError) -> Refusal {
    match e {
        TokenError::Unavailable(m) => {
            Refusal::new(StatusCode::SERVICE_UNAVAILABLE, "credential_unavailable", m)
        }
        TokenError::Audit(m) => {
            log::error!("audit: {m}");
            Refusal::new(StatusCode::SERVICE_UNAVAILABLE, "audit_unavailable", m)
        }
    }
}

struct Outbound<'a> {
    client: &'a HttpClient,
    method: &'a Method,
    uri: &'a Uri,
    headers: &'a HeaderMap,
}

impl Outbound<'_> {
    async fn send(
        &self,
        credential: &[(HeaderName, HeaderValue)],
        body: ProxyBody,
    ) -> Result<Response<Incoming>, Refusal> {
        let mut req = Request::new(body);
        *req.method_mut() = self.method.clone();
        *req.uri_mut() = self.uri.clone();
        *req.version_mut() = Version::HTTP_11;
        *req.headers_mut() = self.headers.clone();
        for (n, v) in credential {
            req.headers_mut().insert(n.clone(), v.clone());
        }
        self.client.request(req).await.map_err(|e| {
            let c = upstream::connect_error(&e);
            match c.kind {
                ConnectErrorKind::Denied => {
                    Refusal::new(StatusCode::FORBIDDEN, "denied_destination", c.message)
                }
                ConnectErrorKind::Unreachable => {
                    Refusal::new(StatusCode::BAD_GATEWAY, "unreachable", c.message)
                }
                ConnectErrorKind::Tls => Refusal::new(StatusCode::BAD_GATEWAY, "tls", c.message),
            }
        })
    }
}

async fn forward(
    ctx: &Arc<Ctx>,
    entry: &Entry,
    req: Request<Incoming>,
    x: &mut Exchange,
) -> Result<Response<ProxyBody>, Refusal> {
    let asked = request_host(req.uri(), req.headers());
    x.host = asked.clone();
    let Some(snap) = ctx.wait_ready(x.start + READY_WAIT).await else {
        return Err(Refusal::new(
            StatusCode::SERVICE_UNAVAILABLE,
            "not_ready",
            "the pod's bindings have not arrived yet",
        ));
    };
    let binding = match entry {
        Entry::Name => {
            let Some(name) = asked.as_deref() else {
                return Err(Refusal::new(
                    StatusCode::NOT_FOUND,
                    "unknown_binding",
                    "the request names no host",
                ));
            };
            snap.by_name.get(name).cloned().ok_or_else(|| {
                Refusal::new(
                    StatusCode::NOT_FOUND,
                    "unknown_binding",
                    format!("the pod has no binding named {name}"),
                )
            })?
        }
        Entry::Intercept { host } => {
            x.host = Some(host.clone());
            let b = snap.by_host.get(host).cloned().ok_or_else(|| {
                Refusal::new(
                    StatusCode::NOT_FOUND,
                    "unknown_binding",
                    format!("the pod no longer intercepts {host}"),
                )
            })?;
            if asked.as_deref().is_some_and(|a| a != host) {
                return Err(Refusal::new(
                    StatusCode::MISDIRECTED_REQUEST,
                    "misdirected",
                    format!("this connection is for {host}"),
                ));
            }
            b
        }
    };
    x.host = Some(binding.host.clone());
    x.binding = Some(binding.clone());
    let live = binding.live.as_ref().map_err(|e| {
        Refusal::new(
            StatusCode::SERVICE_UNAVAILABLE,
            "binding_invalid",
            format!("the binding cannot be served: {e}"),
        )
    })?;
    if req.method() == Method::CONNECT {
        return Err(Refusal::new(
            StatusCode::METHOD_NOT_ALLOWED,
            "method_not_allowed",
            "a binding does not take CONNECT",
        ));
    }
    let uri = upstream_uri(&live.upstream, req.uri().path_and_query())?;
    check_path(req.uri().path(), &live.prefixes)
        .map_err(|m| Refusal::new(StatusCode::FORBIDDEN, "denied_path", m))?;
    if let Some(limit) = &live.limit {
        let taken = limit
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .take(Instant::now());
        if let Err(wait) = taken {
            let mut r = Refusal::new(
                StatusCode::TOO_MANY_REQUESTS,
                "rate_limited",
                "the binding's rate limit is reached",
            );
            r.retry_after = Some(wait.as_secs_f64().ceil().max(1.0) as u64);
            return Err(r);
        }
    }
    let permit = live.requests.clone().try_acquire_owned().map_err(|_| {
        Refusal::new(
            StatusCode::TOO_MANY_REQUESTS,
            "rate_limited",
            "too many requests to the binding are in flight",
        )
    })?;

    let mut token_id = None;
    let credential: Vec<(HeaderName, HeaderValue)> = match &live.credential {
        Credential::Headers(h) => h.clone(),
        Credential::Basic(v) => vec![(AUTHORIZATION, v.clone())],
        Credential::OAuth2(o) => {
            let t = o.get().await.map_err(token_refusal)?;
            token_id = Some(t.id);
            vec![(AUTHORIZATION, t.header)]
        }
        Credential::ClientCertificate => Vec::new(),
    };

    let (mut parts, body) = req.into_parts();
    let upgrade = parts.version == Version::HTTP_11 && is_upgrade(&parts.headers);
    let pod_upgrade = if upgrade {
        parts.extensions.remove::<OnUpgrade>()
    } else {
        None
    };
    let mut headers = upstream_headers(&parts.headers, &live.credential.header_names(), upgrade);
    x.trace.apply(&mut headers);
    if matches!(entry, Entry::Name) && live.forwarded_host {
        if let Ok(v) = HeaderValue::from_str(&binding.name) {
            headers.insert("x-forwarded-host", v);
        }
        headers.insert("x-forwarded-proto", HeaderValue::from_static("http"));
    }

    let empty = body.is_end_stream();
    let replayable = token_id.is_some() && empty && is_idempotent(&parts.method);
    let bytes_in = Arc::new(AtomicU64::new(0));
    let req_body = if empty {
        ProxyBody::empty()
    } else {
        ProxyBody::counted(body, bytes_in.clone())
    };
    let out = Outbound {
        client: if upgrade {
            &live.client_h1
        } else {
            &live.client
        },
        method: &parts.method,
        uri: &uri,
        headers: &headers,
    };
    let mut resp = out.send(&credential, req_body).await?;
    if resp.status() == StatusCode::UNAUTHORIZED
        && let (Credential::OAuth2(o), Some(id)) = (&live.credential, token_id)
    {
        if replayable {
            if let Some(t) = o.renew_after_unauthorized(id).await {
                resp = out
                    .send(&[(AUTHORIZATION, t.header)], ProxyBody::empty())
                    .await?;
            }
        } else {
            // Not to be repeated: the next request gets the new token.
            let o = o.clone();
            tokio::spawn(async move {
                o.renew_after_unauthorized(id).await;
            });
        }
    }
    x.upstream_addr = resp
        .extensions()
        .get::<UpstreamAddr>()
        .map(|a| a.0.to_string());

    let status = resp.status();
    let (mut rparts, rbody) = resp.into_parts();
    let switching = upgrade && status == StatusCode::SWITCHING_PROTOCOLS;
    let tunnel_ends = if switching {
        match (pod_upgrade, rparts.extensions.remove::<OnUpgrade>()) {
            (Some(p), Some(u)) => Some((p, u)),
            _ => {
                return Err(Refusal::new(
                    StatusCode::BAD_GATEWAY,
                    "unreachable",
                    "the upstream's upgrade cannot be carried",
                ))
            }
        }
    } else {
        None
    };
    downstream_headers(&mut rparts.headers, switching);
    if binding.mode == Mode::Name {
        rewrite_locations(&mut rparts.headers, &live.upstream, &binding.name);
    }
    rparts.version = Version::HTTP_11;

    // The record, before the head leaves.
    if x.write(&ctx.audit, status, None).is_err() {
        log::error!("audit: the record of a request cannot be written; refused");
        return Err(Refusal::new(
            StatusCode::SERVICE_UNAVAILABLE,
            "audit_unavailable",
            "the request's record cannot be written",
        ));
    }
    let mut guard = x.guard(&ctx.audit, bytes_in, status);
    guard.permit = Some(permit);
    if let Some((pod, up)) = tunnel_ends {
        tokio::spawn(tunnel(pod, up, guard));
        return Ok(Response::from_parts(rparts, ProxyBody::empty()));
    }
    let counter = guard.bytes_out.clone();
    Ok(Response::from_parts(
        rparts,
        ProxyBody::counted(rbody, counter).with_guard(guard),
    ))
}

/// A tunnel's end that counts the bytes read from it.
struct Counted<T> {
    inner: T,
    read: Arc<AtomicU64>,
}

impl<T: AsyncRead + Unpin> AsyncRead for Counted<T> {
    fn poll_read(
        self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
        buf: &mut tokio::io::ReadBuf<'_>,
    ) -> std::task::Poll<std::io::Result<()>> {
        let this = self.get_mut();
        let before = buf.filled().len();
        let r = std::pin::Pin::new(&mut this.inner).poll_read(cx, buf);
        if let std::task::Poll::Ready(Ok(())) = &r {
            let n = (buf.filled().len() - before) as u64;
            this.read.fetch_add(n, std::sync::atomic::Ordering::Relaxed);
        }
        r
    }
}

impl<T: AsyncWrite + Unpin> AsyncWrite for Counted<T> {
    fn poll_write(
        self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
        buf: &[u8],
    ) -> std::task::Poll<std::io::Result<usize>> {
        std::pin::Pin::new(&mut self.get_mut().inner).poll_write(cx, buf)
    }

    fn poll_flush(
        self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<std::io::Result<()>> {
        std::pin::Pin::new(&mut self.get_mut().inner).poll_flush(cx)
    }

    fn poll_shutdown(
        self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<std::io::Result<()>> {
        std::pin::Pin::new(&mut self.get_mut().inner).poll_shutdown(cx)
    }
}

/// After a 101, both sides are a pipe until either ends. An upstream that
/// closes without TLS's close_notify, as many do, ends the tunnel as a
/// close would.
async fn tunnel(pod: OnUpgrade, up: OnUpgrade, guard: EndGuard) {
    let (pod, up) = match tokio::try_join!(pod, up) {
        Ok(ends) => ends,
        Err(e) => {
            guard.finish(Some(ErrorRec {
                code: "upgrade".into(),
                message: format!("the upgrade did not complete: {e}"),
            }));
            return;
        }
    };
    let mut pod = Counted {
        inner: TokioIo::new(pod),
        read: guard.bytes_in.clone(),
    };
    let mut up = Counted {
        inner: TokioIo::new(up),
        read: guard.bytes_out.clone(),
    };
    match tokio::io::copy_bidirectional(&mut pod, &mut up).await {
        Ok(_) => guard.finish(None),
        Err(e) if e.kind() == std::io::ErrorKind::UnexpectedEof => guard.finish(None),
        Err(e) => guard.finish(Some(ErrorRec {
            code: "tunnel".into(),
            message: format!("the tunnel failed: {e}"),
        })),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn up(s: &str) -> Upstream {
        Upstream::parse(s).unwrap()
    }

    #[test]
    fn location_rewrite() {
        let u = up("https://api.erp.example");
        let cases = [
            (
                "https://api.erp.example/odata/x?$skip=10",
                Some("http://erp/odata/x?$skip=10"),
            ),
            ("HTTPS://API.ERP.EXAMPLE/x", Some("http://erp/x")),
            ("https://api.erp.example:443/x#f", Some("http://erp/x#f")),
            ("https://api.erp.example", Some("http://erp")),
            ("https://api.erp.example?x=1", Some("http://erp?x=1")),
            ("//api.erp.example/x", Some("//erp/x")),
            ("https://api.erp.example:8443/x", None),
            ("https://other.example/x", None),
            ("https://api.erp.example.evil/x", None),
            ("https://user@api.erp.example/x", None),
            ("http://api.erp.example/x", None),
            ("/relative/path", None),
            ("relative", None),
        ];
        for (from, want) in cases {
            assert_eq!(rewrite_url(from, &u, "erp").as_deref(), want, "{from}");
        }
        let u = up("https://api.pay.example:8443");
        assert_eq!(
            rewrite_url("https://api.pay.example:8443/a", &u, "pay").as_deref(),
            Some("http://pay/a")
        );
        assert_eq!(rewrite_url("https://api.pay.example/a", &u, "pay"), None);

        let mut h = HeaderMap::new();
        h.insert(
            LOCATION,
            HeaderValue::from_static("https://api.pay.example:8443/next"),
        );
        h.insert(CONTENT_LOCATION, HeaderValue::from_static("/same"));
        rewrite_locations(&mut h, &u, "pay");
        assert_eq!(h[LOCATION], "http://pay/next");
        assert_eq!(h[CONTENT_LOCATION], "/same");
    }

    #[test]
    fn the_upstream_uri_keeps_the_bindings_origin() {
        let u = up("https://api.erp.example");
        let pq = |s: &'static str| PathAndQuery::from_static(s);
        let uri = upstream_uri(&u, Some(&pq("/odata/x?$top=1"))).unwrap();
        assert_eq!(uri, "https://api.erp.example/odata/x?$top=1");
        let uri = upstream_uri(&up("https://[2001:db8::1]:8443"), Some(&pq("/"))).unwrap();
        assert_eq!(uri, "https://[2001:db8::1]:8443/");
        // What would name another host after the origin, an asterisk, a
        // query alone, or no path at all: refused.
        for bad in [
            "@attacker.example/",
            ".attacker.example/",
            ":1/",
            "*",
            "?x=1",
        ] {
            let r = upstream_uri(&u, Some(&pq(bad))).unwrap_err();
            assert_eq!((r.status, r.code), (StatusCode::BAD_REQUEST, "bad_request"));
        }
        assert!(upstream_uri(&u, None).is_err());
    }

    #[test]
    fn path_prefixes() {
        let p = vec!["/odata/".to_string(), "/api".to_string()];
        for ok in ["/odata/x", "/odata/", "/api", "/api/v1/x"] {
            assert!(check_path(ok, &p).is_ok(), "{ok}");
        }
        for bad in [
            "/",
            "/odata",
            "/apix",
            "/admin",
            "/api/../admin",
            "/api/./x",
            "/api/%2e%2e/admin",
            "/api%2Fadmin",
            "/api/x%5cy",
            "/api\\..\\admin",
            "/odata/..",
            "/api/..;/admin",
            "/api/..;jsessionid=1/admin",
            "/api/.;x/y",
        ] {
            assert!(check_path(bad, &p).is_err(), "{bad}");
        }
        assert!(check_path("/api/x;v=1/y", &p).is_ok());
        assert!(check_path("/api/...;x", &p).is_ok());
        // No prefixes: every path.
        assert!(check_path("/api/../anything", &[]).is_ok());
    }

    #[test]
    fn request_hosts() {
        let mut h = HeaderMap::new();
        h.insert(HOST, HeaderValue::from_static("ERP:80"));
        assert_eq!(
            request_host(&Uri::from_static("/x"), &h).as_deref(),
            Some("erp")
        );
        h.insert(HOST, HeaderValue::from_static("erp."));
        assert_eq!(
            request_host(&Uri::from_static("/x"), &h).as_deref(),
            Some("erp")
        );
        // An absolute-form request's authority wins over Host.
        assert_eq!(
            request_host(&Uri::from_static("http://pay/x"), &h).as_deref(),
            Some("pay")
        );
        assert_eq!(
            request_host(&Uri::from_static("/x"), &HeaderMap::new()),
            None
        );
    }

    #[test]
    fn request_header_rewrite() {
        let mut h = HeaderMap::new();
        for (n, v) in [
            ("host", "erp"),
            ("authorization", "Bearer pod-own"),
            ("proxy-authorization", "Basic x"),
            ("proxy-connection", "keep-alive"),
            ("connection", "keep-alive, x-hop"),
            ("keep-alive", "timeout=5"),
            ("x-hop", "1"),
            ("transfer-encoding", "chunked"),
            ("te", "trailers"),
            ("trailer", "x-t"),
            ("upgrade", "h2c"),
            ("expect", "100-continue"),
            ("x-api-key", "pod-forged"),
            (
                "traceparent",
                "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
            ),
            ("accept", "application/json"),
            ("cookie", "a=1"),
        ] {
            h.append(HeaderName::from_static(n), HeaderValue::from_static(v));
        }
        let out = upstream_headers(&h, &[HeaderName::from_static("x-api-key")], false);
        let names: Vec<&str> = out.keys().map(|k| k.as_str()).collect();
        assert_eq!(names, vec!["te", "accept", "cookie"]);

        // An upgrade keeps Upgrade, with Connection: upgrade only.
        let mut ws = HeaderMap::new();
        ws.insert(CONNECTION, HeaderValue::from_static("Upgrade"));
        ws.insert(UPGRADE, HeaderValue::from_static("websocket"));
        ws.insert("sec-websocket-key", HeaderValue::from_static("k"));
        assert!(is_upgrade(&ws));
        let out = upstream_headers(&ws, &[], true);
        assert_eq!(out[UPGRADE], "websocket");
        assert_eq!(out[CONNECTION], "upgrade");
        assert_eq!(out["sec-websocket-key"], "k");
        assert!(!is_upgrade(&HeaderMap::new()));
        // WebSocket alone.
        ws.insert(UPGRADE, HeaderValue::from_static("WebSocket"));
        assert!(is_upgrade(&ws));
        for other in ["h2c", "websocket, h2c", "echo"] {
            ws.insert(UPGRADE, HeaderValue::from_static(other));
            assert!(!is_upgrade(&ws), "{other}");
        }
        ws.insert(UPGRADE, HeaderValue::from_static("websocket"));
        ws.append(UPGRADE, HeaderValue::from_static("websocket"));
        assert!(!is_upgrade(&ws), "two Upgrade headers");
    }

    #[test]
    fn response_header_rewrite() {
        let mut h = HeaderMap::new();
        h.insert(CONNECTION, HeaderValue::from_static("close, x-hop"));
        h.insert("x-hop", HeaderValue::from_static("1"));
        h.insert("keep-alive", HeaderValue::from_static("timeout=5"));
        h.insert("alt-svc", HeaderValue::from_static("h3=\":443\""));
        h.insert(UPGRADE, HeaderValue::from_static("websocket"));
        h.insert("content-type", HeaderValue::from_static("text/plain"));
        let mut plain = h.clone();
        downstream_headers(&mut plain, false);
        assert_eq!(
            plain.keys().map(|k| k.as_str()).collect::<Vec<_>>(),
            vec!["content-type"]
        );
        downstream_headers(&mut h, true);
        assert_eq!(h[UPGRADE], "websocket");
        assert_eq!(h[CONNECTION], "upgrade");
    }
}
