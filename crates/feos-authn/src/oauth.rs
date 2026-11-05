// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! OAuth 2.0 client credentials (RFC 6749 §4.4) for a binding (ADR 0028
//! §4): the token is fetched at first use, kept in memory, renewed at three
//! quarters of its lifetime, and renewed once after a 401 from the
//! upstream. The token endpoint is dialed under the same destination rule
//! as the upstream; its server is checked against the WebPKI and the
//! binding's CA.
//!
//! One fetch at a time per binding. While the token is valid it serves,
//! and a renewal after three quarters of its lifetime runs in the
//! background, one at a time; only a request without a valid token waits
//! for a fetch. A failed fetch is not tried again for a few seconds, so
//! that a failing endpoint is not hammered by a pod's retries; a token
//! still valid serves meanwhile.

use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::{Duration, Instant};

use base64::Engine;
use bytes::Bytes;
use http::header::{ACCEPT, AUTHORIZATION, CONTENT_TYPE};
use http::{HeaderValue, Method, Request, Uri};
use http_body_util::{BodyExt, Limited};
use serde::Deserialize;

use crate::audit::{self, Audit, ErrorRec, TokenRecord};
use crate::body::ProxyBody;
use crate::upstream::{self, HttpClient, UpstreamAddr};

/// A token endpoint's answer may be this large.
const MAX_TOKEN_RESPONSE: usize = 64 * 1024;
const FETCH_TIMEOUT: Duration = Duration::from_secs(15);
/// After a failed fetch, the next one waits this long.
const FAILURE_PAUSE: Duration = Duration::from_secs(5);
/// A 401 renews the token at most this often.
const UNAUTHORIZED_RENEWAL_PAUSE: Duration = Duration::from_secs(10);
/// A token whose lifetime the endpoint does not say.
const DEFAULT_LIFETIME: Duration = Duration::from_secs(15 * 60);

pub struct Config {
    pub token_url: Uri,
    pub client_id: String,
    pub client_secret: String,
    pub scope: String,
    pub audience: String,
    pub auth_in_body: bool,
}

#[derive(Clone)]
pub struct Token {
    /// `Authorization: Bearer …`, marked sensitive.
    pub header: HeaderValue,
    /// Which fetch it came from, to tell a renewed token from the one a
    /// request used.
    pub id: u64,
    renew_at: Instant,
    expires_at: Instant,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum TokenError {
    /// No token can be had: 503 credential_unavailable.
    Unavailable(String),
    /// The token's record could not be written: 503 audit_unavailable.
    Audit(String),
}

#[derive(Default)]
struct State {
    token: Option<Token>,
    failed: Option<(Instant, TokenError)>,
    unauthorized_renewal: Option<Instant>,
    /// A renewal runs in the background while the token serves.
    renewing: bool,
}

pub struct OAuth {
    binding: String,
    cfg: Config,
    client: HttpClient,
    audit: Arc<Audit>,
    /// Never held across a fetch.
    state: Mutex<State>,
    /// Held across a fetch: one at a time.
    fetching: tokio::sync::Mutex<()>,
    fetches: AtomicU64,
}

#[derive(Deserialize)]
struct TokenResponse {
    access_token: String,
    #[serde(default)]
    token_type: String,
    #[serde(default)]
    expires_in: Option<serde_json::Value>,
}

#[derive(Deserialize)]
struct ErrorResponse {
    error: String,
}

impl OAuth {
    pub fn new(binding: String, cfg: Config, client: HttpClient, audit: Arc<Audit>) -> OAuth {
        OAuth {
            binding,
            cfg,
            client,
            audit,
            state: Mutex::new(State::default()),
            fetching: tokio::sync::Mutex::new(()),
            fetches: AtomicU64::new(0),
        }
    }

    fn state(&self) -> MutexGuard<'_, State> {
        self.state.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// The token for a request: the kept one while it is valid, which
    /// past three quarters of its lifetime starts a renewal in the
    /// background; without a valid one, a fetch this request waits for.
    pub async fn get(self: &Arc<Self>) -> Result<Token, TokenError> {
        {
            let mut st = self.state();
            let now = Instant::now();
            if let Some(t) = st.token.clone().filter(|t| now < t.expires_at) {
                let paused = st
                    .failed
                    .as_ref()
                    .is_some_and(|(at, _)| now < *at + FAILURE_PAUSE);
                if now >= t.renew_at && !st.renewing && !paused {
                    st.renewing = true;
                    let o = self.clone();
                    tokio::spawn(async move { o.renew().await });
                }
                return Ok(t);
            }
        }
        let _one = self.fetching.lock().await;
        let reason = {
            let st = self.state();
            let now = Instant::now();
            // Fetched while this request waited.
            if let Some(t) = st.token.clone().filter(|t| now < t.expires_at) {
                return Ok(t);
            }
            if let Some((at, e)) = &st.failed
                && now < *at + FAILURE_PAUSE
            {
                return Err(e.clone());
            }
            if st.token.is_some() {
                "renewal"
            } else {
                "first_use"
            }
        };
        let fetched = self.fetch(reason).await;
        let mut st = self.state();
        match fetched {
            Ok(t) => {
                st.token = Some(t.clone());
                st.failed = None;
                Ok(t)
            }
            Err(e) => {
                st.failed = Some((Instant::now(), e.clone()));
                Err(e)
            }
        }
    }

    /// Renews the token while it serves; a failure leaves it serving
    /// until it expires.
    async fn renew(self: Arc<Self>) {
        let _one = self.fetching.lock().await;
        let fresh = self
            .state()
            .token
            .as_ref()
            .is_some_and(|t| Instant::now() < t.renew_at);
        let fetched = match fresh {
            // Renewed after a 401 meanwhile.
            true => None,
            false => Some(self.fetch("renewal").await),
        };
        let mut st = self.state();
        st.renewing = false;
        match fetched {
            None => {}
            Some(Ok(t)) => {
                st.token = Some(t);
                st.failed = None;
            }
            Some(Err(e)) => {
                log::warn!(
                    "binding {}: renew its token: {e:?}; the current one serves",
                    self.binding
                );
                st.failed = Some((Instant::now(), e));
            }
        }
    }

    /// After a 401 to a request that carried the token `used`: a token to
    /// repeat it with, if there is a newer one or one can be fetched; None
    /// to pass the 401 on. The refused token serves no other request: they
    /// wait for the new one.
    pub async fn renew_after_unauthorized(&self, used: u64) -> Option<Token> {
        let _one = self.fetching.lock().await;
        {
            let mut st = self.state();
            if let Some(t) = &st.token
                && t.id != used
            {
                return Some(t.clone());
            }
            let now = Instant::now();
            if st
                .unauthorized_renewal
                .is_some_and(|at| now < at + UNAUTHORIZED_RENEWAL_PAUSE)
            {
                return None;
            }
            st.unauthorized_renewal = Some(now);
            st.token = None;
        }
        let fetched = self.fetch("unauthorized").await;
        let mut st = self.state();
        match fetched {
            Ok(t) => {
                st.token = Some(t.clone());
                st.failed = None;
                Some(t)
            }
            Err(e) => {
                log::warn!(
                    "binding {}: renew its token after a 401: {e:?}",
                    self.binding
                );
                st.failed = Some((Instant::now(), e));
                None
            }
        }
    }

    fn form(&self) -> String {
        let mut f = form_urlencoded::Serializer::new(String::new());
        f.append_pair("grant_type", "client_credentials");
        if !self.cfg.scope.is_empty() {
            f.append_pair("scope", &self.cfg.scope);
        }
        if !self.cfg.audience.is_empty() {
            f.append_pair("audience", &self.cfg.audience);
        }
        if self.cfg.auth_in_body {
            f.append_pair("client_id", &self.cfg.client_id);
            f.append_pair("client_secret", &self.cfg.client_secret);
        }
        f.finish()
    }

    /// client_secret_basic: the ID and secret form-encoded, then Basic
    /// (RFC 6749 §2.3.1).
    fn basic(&self) -> Result<HeaderValue, String> {
        let enc = |s: &str| form_urlencoded::byte_serialize(s.as_bytes()).collect::<String>();
        let pair = format!(
            "{}:{}",
            enc(&self.cfg.client_id),
            enc(&self.cfg.client_secret)
        );
        let mut v = HeaderValue::from_str(&format!(
            "Basic {}",
            base64::engine::general_purpose::STANDARD.encode(pair)
        ))
        .map_err(|_| "the client's credentials make no header".to_string())?;
        v.set_sensitive(true);
        Ok(v)
    }

    async fn fetch(&self, reason: &'static str) -> Result<Token, TokenError> {
        let start = Instant::now();
        let mut addr = None;
        let result = tokio::time::timeout(FETCH_TIMEOUT, self.request(&mut addr)).await;
        let (status, outcome) = match result {
            Ok(r) => r,
            Err(_) => (
                None,
                Err("the token endpoint did not answer in time".to_string()),
            ),
        };
        let (expires_in, error) = match &outcome {
            Ok((_, lifetime)) => (Some(lifetime.as_secs()), None),
            Err(m) => (
                None,
                Some(ErrorRec {
                    code: "credential_unavailable".into(),
                    message: m.clone(),
                }),
            ),
        };
        let host = self.cfg.token_url.host().unwrap_or_default();
        let rec = TokenRecord {
            ts: audit::now(),
            kind: "token",
            binding: &self.binding,
            host,
            reason,
            status,
            expires_in,
            duration_ms: start.elapsed().as_millis() as u64,
            upstream_addr: addr.map(|a: UpstreamAddr| a.0.to_string()),
            error,
        };
        if let Err(e) = self.audit.write(&rec) {
            return Err(TokenError::Audit(format!("write the token's record: {e}")));
        }
        let (token, lifetime) = outcome.map_err(TokenError::Unavailable)?;
        let mut header = HeaderValue::from_str(&format!("Bearer {token}"))
            .map_err(|_| TokenError::Unavailable("the token makes no header".into()))?;
        header.set_sensitive(true);
        let now = Instant::now();
        Ok(Token {
            header,
            id: self.fetches.fetch_add(1, Ordering::Relaxed) + 1,
            renew_at: now + lifetime.mul_f64(0.75),
            expires_at: now + lifetime,
        })
    }

    /// One request to the token endpoint: its status, and the token and its
    /// lifetime or why not. Error messages never hold the secret, the
    /// token or the answer's body beyond its OAuth error code.
    async fn request(
        &self,
        addr: &mut Option<UpstreamAddr>,
    ) -> (Option<u16>, Result<(String, Duration), String>) {
        let mut req = Request::builder()
            .method(Method::POST)
            .uri(self.cfg.token_url.clone())
            .header(CONTENT_TYPE, "application/x-www-form-urlencoded")
            .header(ACCEPT, "application/json");
        if !self.cfg.auth_in_body {
            match self.basic() {
                Ok(v) => req = req.header(AUTHORIZATION, v),
                Err(e) => return (None, Err(e)),
            }
        }
        let req = match req.body(ProxyBody::full(Bytes::from(self.form()))) {
            Ok(r) => r,
            Err(e) => return (None, Err(format!("the token request: {e}"))),
        };
        let resp = match self.client.request(req).await {
            Ok(r) => r,
            Err(e) => {
                let c = upstream::connect_error(&e);
                return (None, Err(format!("the token endpoint: {}", c.message)));
            }
        };
        *addr = resp.extensions().get::<UpstreamAddr>().copied();
        let status = resp.status();
        let body = match Limited::new(resp.into_body(), MAX_TOKEN_RESPONSE)
            .collect()
            .await
        {
            Ok(b) => b.to_bytes(),
            Err(e) => {
                return (
                    Some(status.as_u16()),
                    Err(format!("the token endpoint's answer: {e}")),
                )
            }
        };
        if !status.is_success() {
            let code = serde_json::from_slice::<ErrorResponse>(&body)
                .map(|e| format!(" ({})", e.error.chars().take(64).collect::<String>()))
                .unwrap_or_default();
            return (
                Some(status.as_u16()),
                Err(format!("the token endpoint answered {status}{code}")),
            );
        }
        let parsed: TokenResponse = match serde_json::from_slice(&body) {
            Ok(t) => t,
            Err(_) => {
                return (
                    Some(status.as_u16()),
                    Err("the token endpoint's answer is no token".into()),
                )
            }
        };
        if !parsed.token_type.is_empty() && !parsed.token_type.eq_ignore_ascii_case("bearer") {
            return (
                Some(status.as_u16()),
                Err(format!(
                    "the token endpoint gave a token of type {:?}, not Bearer",
                    parsed.token_type.chars().take(32).collect::<String>()
                )),
            );
        }
        let secs = match &parsed.expires_in {
            Some(serde_json::Value::Number(n)) => n.as_u64(),
            Some(serde_json::Value::String(s)) => s.parse().ok(),
            _ => None,
        };
        let lifetime = secs
            .filter(|&s| s > 0)
            .map(Duration::from_secs)
            .unwrap_or(DEFAULT_LIFETIME);
        (Some(status.as_u16()), Ok((parsed.access_token, lifetime)))
    }
}
