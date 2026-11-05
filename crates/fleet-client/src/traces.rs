// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The pods' spans (ADR 0026 §10, docs/architecture/traces.md, "The
//! pod"): the node takes OTLP from its pods, over gRPC at [`GRPC_PORT`]
//! and over HTTP at [`HTTP_PORT`], knows a pod by the request's source
//! address, stamps its labels on every resource, and sends the request on
//! to the sink's `TraceService/Export` over the logs' channel. It keeps
//! nothing: the pod's SDK batches and tries again, and the sink's answer
//! is the pod's.

use crate::sink::Shipper;
use feos_proto::opentelemetry::proto::collector::trace::v1::trace_service_client::TraceServiceClient;
use feos_proto::opentelemetry::proto::collector::trace::v1::trace_service_server::{
    TraceService, TraceServiceServer,
};
use feos_proto::opentelemetry::proto::collector::trace::v1::{
    ExportTraceServiceRequest, ExportTraceServiceResponse,
};
use feos_proto::opentelemetry::proto::common::v1::{any_value::Value, AnyValue, KeyValue};
use hyper::body::{Body, Bytes, Frame, Incoming, SizeHint};
use hyper::header::{self, HeaderMap, HeaderValue};
use hyper::{Method, Request, Response, StatusCode};
use hyper_util::rt::TokioIo;
use log::{debug, info, warn};
use prost::Message;
use std::collections::{BTreeMap, HashMap, HashSet};
use std::convert::Infallible;
use std::io::Read;
use std::net::{IpAddr, Ipv4Addr, SocketAddr};
use std::pin::Pin;
use std::sync::{Arc, Mutex, MutexGuard, PoisonError, RwLock};
use std::task::{Context, Poll};
use std::time::Duration;
use tokio::net::TcpListener;
use tokio::sync::Notify;
use tokio::time::{sleep, timeout, Instant};
use tonic::transport::server::TcpIncoming;
use tonic::transport::Channel;
use tonic::{Code, Status};

/// OTLP's ports: gRPC and HTTP.
pub const GRPC_PORT: u16 = 4317;
pub const HTTP_PORT: u16 = 4318;
/// The path of OTLP/HTTP's traces.
const TRACES_PATH: &str = "/v1/traces";
const PROTOBUF: &str = "application/x-protobuf";
/// A request's most bytes on the wire, and decompressed: stamped, it fits
/// in what the sink takes.
const MAX_WIRE: usize = 4 << 20;
const MAX_DECODED: usize = 6 << 20;
/// A pod's rate and burst, in bytes of requests, decompressed.
const RATE: u64 = 1 << 20;
const BURST: u64 = 8 << 20;
/// How long the sink has to answer.
const DEADLINE: Duration = Duration::from_secs(8);
/// How often the pods are looked at without an event.
const RESCAN: Duration = Duration::from_secs(10);
/// The wait before a port is bound again.
const REBIND: Duration = Duration::from_secs(10);
/// Of a pod's labels, those of a log stream alone.
const STREAM_LABELS: [&str; 3] = ["ferun.container", "ferun.log_id", "ferun.retention_days"];

/// A pod as its spans tell it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct TracedPod {
    pub id: String,
    pub labels: HashMap<String, String>,
    /// Its addresses, from which it sends.
    pub addresses: Vec<IpAddr>,
}

/// What the node has of its pods for their spans.
pub trait Senders: Send + Sync + 'static {
    /// The pods on the node that have labels, in whatever state: a pod
    /// that is starting or ending sends too (an SDK sends what it has as
    /// its process exits). It may block briefly, on the lock of the pods'
    /// records; it runs on the blocking pool.
    fn pods(&self) -> Vec<TracedPod>;
}

/// The node's channel to the sink and its name, or why there is none.
type Sink = Box<dyn Fn() -> Result<(Channel, String), Status> + Send + Sync>;

/// The pods' spans; its tasks run on the runtime it was started on, for as
/// long as the process does.
#[derive(Clone)]
pub struct Traces {
    inner: Arc<Inner>,
}

struct Inner {
    sink: Sink,
    /// Where the pods come from.
    senders: Arc<dyn Senders>,
    /// The pods with labels by their addresses.
    pods: RwLock<HashMap<IpAddr, Arc<Sender>>>,
    /// Held while the pods are looked at for an address not known yet, so
    /// that the requests of a pod that just started look once.
    lookup: tokio::sync::Mutex<()>,
    /// The pods' limits, by their IDs.
    buckets: Mutex<HashMap<String, Bucket>>,
    /// A nudge to look at the pods now.
    rescan: Notify,
}

/// A pod that sends spans: what its resources get.
#[derive(Debug, PartialEq)]
struct Sender {
    id: String,
    /// Its labels but those of a log stream, and `ferun.pod`.
    labels: BTreeMap<String, String>,
    /// The `service.name` of a resource without one.
    service: String,
}

impl Sender {
    fn new(id: String, labels: &HashMap<String, String>) -> Self {
        let mut stamped: BTreeMap<String, String> = labels
            .iter()
            .filter(|(key, _)| !STREAM_LABELS.contains(&key.as_str()))
            .map(|(key, value)| (key.clone(), value.clone()))
            .collect();
        stamped.insert("ferun.pod".to_string(), id.clone());
        let service = ["ferun.object", "ferun.job"]
            .iter()
            .filter_map(|key| labels.get(*key))
            .find(|name| !name.is_empty())
            .cloned()
            .unwrap_or_else(|| "unknown_service".to_string());
        Self {
            id,
            labels: stamped,
            service,
        }
    }
}

/// A request refused, or the sink's refusal as the pod gets it.
#[derive(Debug, PartialEq)]
enum Refusal {
    /// It does not come from a pod with labels.
    Unknown,
    TooLarge,
    /// The pod is beyond its rate.
    Limited,
    /// It does not decode.
    Invalid(String),
    /// The sink's answer, its code the pod's ([`pod_code`]).
    Sink(Code, String),
}

impl Refusal {
    fn status(&self) -> Status {
        match self {
            Self::Unknown => {
                Status::permission_denied("the source is no pod of this node with labels")
            }
            Self::TooLarge => Status::resource_exhausted(format!(
                "a request is at most {MAX_WIRE} bytes, {MAX_DECODED} decompressed"
            )),
            Self::Limited => Status::resource_exhausted("the pod sends beyond its rate"),
            Self::Invalid(message) => Status::invalid_argument(message.clone()),
            Self::Sink(code, message) => Status::new(*code, message.clone()),
        }
    }

    /// The HTTP status, and the seconds of `Retry-After`.
    fn http(&self) -> (StatusCode, Option<u32>) {
        match self {
            Self::Unknown => (StatusCode::FORBIDDEN, None),
            Self::TooLarge => (StatusCode::PAYLOAD_TOO_LARGE, None),
            Self::Limited => (StatusCode::TOO_MANY_REQUESTS, Some(1)),
            Self::Invalid(_) => (StatusCode::BAD_REQUEST, None),
            Self::Sink(Code::ResourceExhausted, _) => (StatusCode::TOO_MANY_REQUESTS, Some(30)),
            Self::Sink(Code::Unavailable, _) => (StatusCode::SERVICE_UNAVAILABLE, Some(2)),
            Self::Sink(Code::InvalidArgument, _) => (StatusCode::BAD_REQUEST, None),
            Self::Sink(_, _) => (StatusCode::INTERNAL_SERVER_ERROR, None),
        }
    }
}

/// The pod's code for the sink's.
fn pod_code(sink: Code) -> Code {
    match sink {
        Code::Ok => Code::Ok,
        Code::ResourceExhausted => Code::ResourceExhausted,
        Code::Unavailable | Code::DeadlineExceeded => Code::Unavailable,
        Code::InvalidArgument => Code::InvalidArgument,
        _ => Code::Internal,
    }
}

/// A pod's limit: a bucket of bytes, [`RATE`] a second up to [`BURST`].
#[derive(Debug)]
struct Bucket {
    tokens: u64,
    at: Instant,
}

impl Bucket {
    fn new(now: Instant) -> Self {
        Self {
            tokens: BURST,
            at: now,
        }
    }

    /// Whether `bytes` may pass at `now`; they are taken if so.
    fn take(&mut self, bytes: usize, now: Instant) -> bool {
        let elapsed = now.saturating_duration_since(self.at).as_nanos();
        let added = u64::try_from(elapsed * u128::from(RATE) / 1_000_000_000).unwrap_or(u64::MAX);
        self.tokens = self.tokens.saturating_add(added).min(BURST);
        self.at = now;
        let cost = u64::try_from(bytes).unwrap_or(u64::MAX);
        if cost > self.tokens {
            return false;
        }
        self.tokens -= cost;
        true
    }
}

fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(PoisonError::into_inner)
}

fn string(key: &str, value: &str) -> KeyValue {
    KeyValue {
        key: key.to_string(),
        value: Some(AnyValue {
            value: Some(Value::StringValue(value.to_string())),
        }),
    }
}

/// Stamps the pod's labels and the node's name on every resource of the
/// request, in place of the `ferun.*` attributes it has; one without
/// `service.name` gets the pod's.
fn stamp(request: &mut ExportTraceServiceRequest, sender: &Sender, node: &str) {
    for spans in &mut request.resource_spans {
        let resource = spans.resource.get_or_insert_with(Default::default);
        resource
            .attributes
            .retain(|kv| !kv.key.starts_with("ferun.") && !sender.labels.contains_key(&kv.key));
        if !resource
            .attributes
            .iter()
            .any(|kv| kv.key == "service.name")
        {
            resource
                .attributes
                .push(string("service.name", &sender.service));
        }
        resource
            .attributes
            .extend(sender.labels.iter().map(|(key, value)| string(key, value)));
        resource.attributes.push(string("ferun.node", node));
    }
}

impl Traces {
    /// Takes the spans of the pods of `senders` and sends them on to the
    /// sink of `shipper`, once [`Traces::serve`] serves.
    pub fn start<S: Senders>(senders: S, shipper: Shipper) -> Self {
        Self::with_sink(senders, Box::new(move || shipper.sink()))
    }

    fn with_sink<S: Senders>(senders: S, sink: Sink) -> Self {
        let traces = Self {
            inner: Arc::new(Inner {
                sink,
                senders: Arc::new(senders),
                pods: RwLock::default(),
                lookup: tokio::sync::Mutex::default(),
                buckets: Mutex::default(),
                rescan: Notify::new(),
            }),
        };
        tokio::spawn(watch(traces.inner.clone()));
        traces
    }

    /// The pods may have changed: they are looked at now.
    pub fn changed(&self) {
        self.inner.rescan.notify_one();
    }

    /// Sends the client spans of a pod's requests through its HTTP
    /// bindings (ADR 0028, `egress`) to the sink, as the pod's own spans
    /// go: stamped with its labels, and with `ferun.source` `egress` and
    /// `ferun.container` `feos-authn` on their resource. A pod without
    /// labels sends none: they are dropped.
    pub async fn export_egress(
        &self,
        pod_id: &str,
        spans: Vec<feos_proto::opentelemetry::proto::trace::v1::Span>,
    ) -> Result<(), Status> {
        use feos_proto::opentelemetry::proto::common::v1::InstrumentationScope;
        use feos_proto::opentelemetry::proto::trace::v1::{ResourceSpans, ScopeSpans};
        if spans.is_empty() {
            return Ok(());
        }
        let Some(sender) = self.inner.sender_of(pod_id).await else {
            return Ok(());
        };
        let (channel, node) = (self.inner.sink)()?;
        let mut request = ExportTraceServiceRequest {
            resource_spans: vec![ResourceSpans {
                scope_spans: vec![ScopeSpans {
                    scope: Some(InstrumentationScope {
                        name: crate::egress::EGRESS_CONTAINER.to_string(),
                        ..Default::default()
                    }),
                    spans,
                    ..Default::default()
                }],
                ..Default::default()
            }],
        };
        stamp(&mut request, &sender, &node);
        for spans in &mut request.resource_spans {
            let resource = spans.resource.get_or_insert_with(Default::default);
            resource
                .attributes
                .push(string("ferun.source", crate::egress::SOURCE));
            resource
                .attributes
                .push(string("ferun.container", crate::egress::EGRESS_CONTAINER));
        }
        let mut call = tonic::Request::new(request);
        call.set_timeout(DEADLINE);
        timeout(DEADLINE, TraceServiceClient::new(channel).export(call))
            .await
            .map_err(|_| Status::unavailable("the sink did not answer in time"))??;
        Ok(())
    }

    /// Serves OTLP at `address`, gRPC and HTTP, for as long as the process
    /// runs. A port that does not bind is tried again; nothing here ends
    /// feosd.
    pub async fn serve(&self, address: Ipv4Addr) {
        let grpc = SocketAddr::new(address.into(), GRPC_PORT);
        let http = SocketAddr::new(address.into(), HTTP_PORT);
        let inner = self.inner.clone();
        let grpc = tokio::spawn(async move {
            loop {
                let listener = bind(grpc).await;
                if let Err(e) = serve_grpc(inner.clone(), listener).await {
                    warn!("Traces: Serving gRPC at {grpc} failed, again in a while: {e}");
                }
                sleep(REBIND).await;
            }
        });
        let inner = self.inner.clone();
        let http = tokio::spawn(async move {
            loop {
                let listener = bind(http).await;
                serve_http(inner.clone(), listener).await;
            }
        });
        let _ = tokio::join!(grpc, http);
    }
}

/// Keeps the pods' addresses up to date.
async fn watch(inner: Arc<Inner>) {
    loop {
        inner.rescan_now().await;
        tokio::select! {
            () = sleep(RESCAN) => {}
            () = inner.rescan.notified() => {}
        }
    }
}

/// Binds `address`, trying again until it does.
async fn bind(address: SocketAddr) -> TcpListener {
    let mut warned = false;
    loop {
        match TcpListener::bind(address).await {
            Ok(listener) => {
                info!("Traces: Taking the pods' spans at {address}");
                return listener;
            }
            Err(e) if !warned => {
                warn!("Traces: Binding {address} failed, trying again: {e}");
                warned = true;
            }
            Err(e) => debug!("Traces: Binding {address} failed: {e}"),
        }
        sleep(REBIND).await;
    }
}

impl Inner {
    fn set_pods(&self, pods: Vec<TracedPod>) {
        let mut by_address = HashMap::new();
        let mut ids = HashSet::new();
        for pod in pods.into_iter().filter(|pod| !pod.labels.is_empty()) {
            let sender = Arc::new(Sender::new(pod.id.clone(), &pod.labels));
            for address in pod.addresses {
                by_address.insert(address.to_canonical(), sender.clone());
            }
            ids.insert(pod.id);
        }
        lock(&self.buckets).retain(|id, _| ids.contains(id));
        *self.pods.write().unwrap_or_else(PoisonError::into_inner) = by_address;
    }

    /// Looks at the pods now.
    async fn rescan_now(&self) {
        let senders = self.senders.clone();
        let pods = tokio::task::spawn_blocking(move || senders.pods())
            .await
            .unwrap_or_default();
        self.set_pods(pods);
    }

    /// The pod `pod_id`, if it has labels; looked for among the pods now
    /// when it is not known yet.
    async fn sender_of(&self, pod_id: &str) -> Option<Arc<Sender>> {
        let known = |pods: &HashMap<IpAddr, Arc<Sender>>| {
            pods.values().find(|sender| sender.id == pod_id).cloned()
        };
        if let Some(sender) = known(&self.pods.read().unwrap_or_else(PoisonError::into_inner)) {
            return Some(sender);
        }
        let _one = self.lookup.lock().await;
        self.rescan_now().await;
        known(&self.pods.read().unwrap_or_else(PoisonError::into_inner))
    }

    /// The pod that sends from `address`; one not known yet is looked for
    /// among the pods now, since a pod sends as it starts, before the
    /// event of its start has reached the watch.
    async fn sender_now(&self, address: Option<IpAddr>) -> Result<Arc<Sender>, Refusal> {
        match self.sender(address) {
            Err(Refusal::Unknown) if address.is_some() => {
                let _one = self.lookup.lock().await;
                if let Ok(sender) = self.sender(address) {
                    return Ok(sender);
                }
                self.rescan_now().await;
                self.sender(address)
            }
            known => known,
        }
    }

    /// The pod that sends from `address`.
    fn sender(&self, address: Option<IpAddr>) -> Result<Arc<Sender>, Refusal> {
        let address = address.ok_or(Refusal::Unknown)?.to_canonical();
        self.pods
            .read()
            .unwrap_or_else(PoisonError::into_inner)
            .get(&address)
            .cloned()
            .ok_or(Refusal::Unknown)
    }

    /// Takes `bytes` of the pod's rate.
    fn admit(&self, pod: &str, bytes: usize) -> Result<(), Refusal> {
        let now = Instant::now();
        let mut buckets = lock(&self.buckets);
        let bucket = buckets
            .entry(pod.to_string())
            .or_insert_with(|| Bucket::new(now));
        match bucket.take(bytes, now) {
            true => Ok(()),
            false => Err(Refusal::Limited),
        }
    }

    /// Sends the pod's request on to the sink, stamped, and answers with
    /// the sink's answer.
    async fn export(
        &self,
        sender: &Sender,
        mut request: ExportTraceServiceRequest,
    ) -> Result<ExportTraceServiceResponse, Refusal> {
        let (channel, node) = (self.sink)().map_err(|e| sink_refusal(&e))?;
        stamp(&mut request, sender, &node);
        let mut call = tonic::Request::new(request);
        call.set_timeout(DEADLINE);
        let mut client = TraceServiceClient::new(channel);
        match timeout(DEADLINE, client.export(call)).await {
            Ok(Ok(response)) => Ok(response.into_inner()),
            Ok(Err(status)) => {
                debug!(
                    "Traces: The sink refused pod {}'s spans: {:?} {}",
                    sender.id,
                    status.code(),
                    status.message()
                );
                Err(sink_refusal(&status))
            }
            Err(_) => Err(Refusal::Sink(
                Code::Unavailable,
                "the sink did not answer in time".to_string(),
            )),
        }
    }

    /// What a request from `address` of `bytes` gets: checks and
    /// [`Inner::export`].
    async fn take(
        &self,
        address: Option<IpAddr>,
        sender: Arc<Sender>,
        bytes: usize,
        request: ExportTraceServiceRequest,
    ) -> Result<ExportTraceServiceResponse, Refusal> {
        self.admit(&sender.id, bytes).inspect_err(|_| {
            debug!(
                "Traces: Pod {} at {address:?} sends beyond its rate",
                sender.id
            )
        })?;
        self.export(&sender, request).await
    }
}

fn sink_refusal(status: &Status) -> Refusal {
    Refusal::Sink(pod_code(status.code()), status.message().to_string())
}

/// gRPC: `TraceService`, alone; the other services answer `UNIMPLEMENTED`.
async fn serve_grpc(inner: Arc<Inner>, listener: TcpListener) -> Result<(), String> {
    let service = TraceServiceServer::new(Grpc(inner)).max_decoding_message_size(MAX_WIRE);
    tonic::transport::Server::builder()
        .add_service(service)
        .serve_with_incoming(TcpIncoming::from(listener))
        .await
        .map_err(|e| e.to_string())
}

struct Grpc(Arc<Inner>);

#[tonic::async_trait]
impl TraceService for Grpc {
    async fn export(
        &self,
        request: tonic::Request<ExportTraceServiceRequest>,
    ) -> Result<tonic::Response<ExportTraceServiceResponse>, Status> {
        let address = request.remote_addr().map(|address| address.ip());
        let sender = self.0.sender_now(address).await.map_err(|e| e.status())?;
        let request = request.into_inner();
        let bytes = request.encoded_len();
        self.0
            .take(address, sender, bytes, request)
            .await
            .map(tonic::Response::new)
            .map_err(|e| e.status())
    }
}

/// HTTP/1.1: `POST /v1/traces`, protobuf.
async fn serve_http(inner: Arc<Inner>, listener: TcpListener) {
    loop {
        let (stream, peer) = match listener.accept().await {
            Ok(accepted) => accepted,
            Err(e) => {
                debug!("Traces: Accepting a connection failed: {e}");
                sleep(Duration::from_millis(100)).await;
                continue;
            }
        };
        let inner = inner.clone();
        tokio::spawn(async move {
            let service = hyper::service::service_fn(move |request| {
                let inner = inner.clone();
                async move { Ok::<_, Infallible>(answer(&inner, peer.ip(), request).await) }
            });
            if let Err(e) = hyper::server::conn::http1::Builder::new()
                .serve_connection(TokioIo::new(stream), service)
                .await
            {
                debug!("Traces: A connection from {peer} ended: {e}");
            }
        });
    }
}

/// A body of bytes, at once.
struct Full(Option<Bytes>);

impl Body for Full {
    type Data = Bytes;
    type Error = Infallible;

    fn poll_frame(
        self: Pin<&mut Self>,
        _: &mut Context<'_>,
    ) -> Poll<Option<Result<Frame<Bytes>, Infallible>>> {
        Poll::Ready(self.get_mut().0.take().map(|data| Ok(Frame::data(data))))
    }

    fn is_end_stream(&self) -> bool {
        self.0.is_none()
    }

    fn size_hint(&self) -> SizeHint {
        SizeHint::with_exact(self.0.as_ref().map_or(0, |data| data.len() as u64))
    }
}

/// `google.rpc.Status`, which OTLP/HTTP answers an error with.
#[derive(Clone, PartialEq, prost::Message)]
struct RpcStatus {
    #[prost(int32, tag = "1")]
    code: i32,
    #[prost(string, tag = "2")]
    message: String,
}

fn response(status: StatusCode, body: Vec<u8>) -> Response<Full> {
    let mut response = Response::new(Full(Some(body.into())));
    *response.status_mut() = status;
    response
        .headers_mut()
        .insert(header::CONTENT_TYPE, HeaderValue::from_static(PROTOBUF));
    response
}

/// The answer of a request refused for its path, method or type.
fn plain(status: StatusCode, message: &str) -> Response<Full> {
    let mut response = Response::new(Full(Some(message.to_string().into())));
    *response.status_mut() = status;
    let headers = response.headers_mut();
    headers.insert(
        header::CONTENT_TYPE,
        HeaderValue::from_static("text/plain; charset=utf-8"),
    );
    if status == StatusCode::METHOD_NOT_ALLOWED {
        headers.insert(header::ALLOW, HeaderValue::from_static("POST"));
    }
    response
}

fn refused(refusal: &Refusal) -> Response<Full> {
    let (status, retry_after) = refusal.http();
    let grpc = refusal.status();
    let body = RpcStatus {
        code: grpc.code() as i32,
        message: grpc.message().to_string(),
    };
    let mut response = response(status, body.encode_to_vec());
    if let Some(seconds) = retry_after {
        response
            .headers_mut()
            .insert(header::RETRY_AFTER, HeaderValue::from(seconds));
    }
    response
}

/// Whether a request is one of OTLP/HTTP's traces, by its head: whether
/// its body is gzip, or what it answers.
fn check_head(method: &Method, path: &str, headers: &HeaderMap) -> Result<bool, Response<Full>> {
    if path != TRACES_PATH {
        return Err(plain(StatusCode::NOT_FOUND, "only /v1/traces is served"));
    }
    if method != Method::POST {
        return Err(plain(StatusCode::METHOD_NOT_ALLOWED, "only POST"));
    }
    let value = |name| {
        headers
            .get(name)
            .map(|value: &HeaderValue| value.to_str().unwrap_or_default().trim().to_lowercase())
    };
    let media = value(header::CONTENT_TYPE).and_then(|value| {
        value
            .split(';')
            .next()
            .map(|media| media.trim().to_string())
    });
    if media.as_deref() != Some(PROTOBUF) {
        return Err(plain(
            StatusCode::UNSUPPORTED_MEDIA_TYPE,
            "only application/x-protobuf",
        ));
    }
    let gzip = match value(header::CONTENT_ENCODING).as_deref() {
        None | Some("" | "identity") => false,
        Some("gzip") => true,
        Some(_) => {
            return Err(plain(
                StatusCode::UNSUPPORTED_MEDIA_TYPE,
                "only gzip or no encoding",
            ));
        }
    };
    let length = value(header::CONTENT_LENGTH).and_then(|value| value.parse::<u64>().ok());
    if length.is_some_and(|length| length > MAX_WIRE as u64) {
        return Err(refused(&Refusal::TooLarge));
    }
    Ok(gzip)
}

/// The body, at most `limit` bytes of it.
async fn read_body(mut body: Incoming, limit: usize) -> Result<Vec<u8>, Refusal> {
    let mut read = Vec::new();
    while let Some(frame) = std::future::poll_fn(|cx| Pin::new(&mut body).poll_frame(cx)).await {
        let frame = frame.map_err(|e| Refusal::Invalid(format!("reading the body: {e}")))?;
        if let Ok(data) = frame.into_data() {
            if read.len() + data.len() > limit {
                return Err(Refusal::TooLarge);
            }
            read.extend_from_slice(&data);
        }
    }
    Ok(read)
}

/// The body decompressed, at most [`MAX_DECODED`] bytes of it.
fn gunzip(body: &[u8]) -> Result<Vec<u8>, Refusal> {
    let mut decoded = Vec::new();
    flate2::read::GzDecoder::new(body)
        .take(MAX_DECODED as u64 + 1)
        .read_to_end(&mut decoded)
        .map_err(|e| Refusal::Invalid(format!("the body is not gzip: {e}")))?;
    if decoded.len() > MAX_DECODED {
        return Err(Refusal::TooLarge);
    }
    Ok(decoded)
}

async fn answer(inner: &Inner, peer: IpAddr, request: Request<Incoming>) -> Response<Full> {
    let gzip = match check_head(request.method(), request.uri().path(), request.headers()) {
        Ok(gzip) => gzip,
        Err(response) => return response,
    };
    let sender = match inner.sender_now(Some(peer)).await {
        Ok(sender) => sender,
        Err(refusal) => return refused(&refusal),
    };
    let decoded = async {
        let body = read_body(request.into_body(), MAX_WIRE).await?;
        match gzip {
            true => gunzip(&body),
            false => Ok(body),
        }
    };
    let result = match decoded.await {
        Ok(body) => match ExportTraceServiceRequest::decode(body.as_slice()) {
            Ok(request) => inner.take(Some(peer), sender, body.len(), request).await,
            Err(e) => Err(Refusal::Invalid(format!(
                "not an ExportTraceServiceRequest: {e}"
            ))),
        },
        Err(refusal) => Err(refusal),
    };
    match result {
        Ok(answer) => response(StatusCode::OK, answer.encode_to_vec()),
        Err(refusal) => refused(&refusal),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use feos_proto::opentelemetry::proto::collector::logs::v1::logs_service_client::LogsServiceClient;
    use feos_proto::opentelemetry::proto::collector::logs::v1::ExportLogsServiceRequest;
    use feos_proto::opentelemetry::proto::collector::trace::v1::ExportTracePartialSuccess;
    use feos_proto::opentelemetry::proto::resource::v1::Resource;
    use feos_proto::opentelemetry::proto::trace::v1::ResourceSpans;
    use std::io::Write;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tonic::transport::Endpoint;

    fn labels(pairs: &[(&str, &str)]) -> HashMap<String, String> {
        pairs
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect()
    }

    fn attributes(resource: &Resource) -> Vec<(String, String)> {
        let mut pairs: Vec<_> = resource
            .attributes
            .iter()
            .map(|kv| {
                let value = match kv.value.as_ref().and_then(|v| v.value.as_ref()) {
                    Some(Value::StringValue(s)) => s.clone(),
                    other => format!("{other:?}"),
                };
                (kv.key.clone(), value)
            })
            .collect();
        pairs.sort();
        pairs
    }

    fn pairs(pairs: &[(&str, &str)]) -> Vec<(String, String)> {
        let mut pairs: Vec<_> = pairs
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect();
        pairs.sort();
        pairs
    }

    fn spans(resources: Vec<Option<Vec<KeyValue>>>) -> ExportTraceServiceRequest {
        ExportTraceServiceRequest {
            resource_spans: resources
                .into_iter()
                .map(|attributes| ResourceSpans {
                    resource: attributes.map(|attributes| Resource {
                        attributes,
                        ..Default::default()
                    }),
                    ..Default::default()
                })
                .collect(),
        }
    }

    const POD_LABELS: [(&str, &str); 7] = [
        ("ferun.tenant", "acme"),
        ("ferun.instance", "i1"),
        ("ferun.environment", "e1"),
        ("ferun.kind", "service"),
        ("ferun.object", "web"),
        ("ferun.retention_days", "7"),
        ("ferun.container", "forged"),
    ];

    #[test]
    fn a_pods_resources_get_its_labels_in_place_of_their_own() {
        let sender = Sender::new("p1".into(), &labels(&POD_LABELS));
        let mut request = spans(vec![
            Some(vec![
                string("service.name", "shop"),
                string("ferun.tenant", "evil"),
                string("ferun.source", "ingress"),
                string("host.name", "h"),
            ]),
            Some(vec![string("ferun.pod", "other")]),
            None,
        ]);
        stamp(&mut request, &sender, "node-1");
        let ours = [
            ("ferun.environment", "e1"),
            ("ferun.instance", "i1"),
            ("ferun.kind", "service"),
            ("ferun.node", "node-1"),
            ("ferun.object", "web"),
            ("ferun.pod", "p1"),
            ("ferun.tenant", "acme"),
        ];
        let resources: Vec<_> = request
            .resource_spans
            .iter()
            .map(|spans| attributes(spans.resource.as_ref().unwrap()))
            .collect();
        let with = |more: &[(&str, &str)]| pairs(&[&ours[..], more].concat());
        assert_eq!(
            resources[0],
            with(&[("service.name", "shop"), ("host.name", "h")])
        );
        // Without service.name: the pod's object's.
        assert_eq!(resources[1], with(&[("service.name", "web")]));
        assert_eq!(resources[2], with(&[("service.name", "web")]));
    }

    #[test]
    fn a_service_name_is_the_objects_the_jobs_or_unknown() {
        let job = Sender::new("p".into(), &labels(&[("ferun.job", "j1")]));
        assert_eq!(job.service, "j1");
        let both = labels(&[("ferun.job", "j1"), ("ferun.object", "o")]);
        assert_eq!(Sender::new("p".into(), &both).service, "o");
        let none = Sender::new("p".into(), &labels(&[("ferun.tenant", "t")]));
        assert_eq!(none.service, "unknown_service");
    }

    #[test]
    fn the_sinks_codes_are_the_pods() {
        for (sink, pod, http, retry) in [
            (
                Code::ResourceExhausted,
                Code::ResourceExhausted,
                429,
                Some(30),
            ),
            (Code::Unavailable, Code::Unavailable, 503, Some(2)),
            (Code::DeadlineExceeded, Code::Unavailable, 503, Some(2)),
            (Code::InvalidArgument, Code::InvalidArgument, 400, None),
            (Code::PermissionDenied, Code::Internal, 500, None),
            (Code::Unknown, Code::Internal, 500, None),
            (Code::Unauthenticated, Code::Internal, 500, None),
        ] {
            let refusal = sink_refusal(&Status::new(sink, "m"));
            assert_eq!(refusal.status().code(), pod, "{sink:?}");
            let (status, retry_after) = refusal.http();
            assert_eq!((status.as_u16(), retry_after), (http, retry), "{sink:?}");
        }
        assert_eq!(pod_code(Code::Ok), Code::Ok);
        // The node's own.
        for (refusal, code, http, retry) in [
            (Refusal::Unknown, Code::PermissionDenied, 403, None),
            (Refusal::TooLarge, Code::ResourceExhausted, 413, None),
            (Refusal::Limited, Code::ResourceExhausted, 429, Some(1)),
            (
                Refusal::Invalid("x".into()),
                Code::InvalidArgument,
                400,
                None,
            ),
        ] {
            assert_eq!(refusal.status().code(), code, "{refusal:?}");
            let (status, retry_after) = refusal.http();
            assert_eq!((status.as_u16(), retry_after), (http, retry), "{refusal:?}");
        }
    }

    #[test]
    fn a_pod_sends_a_mib_a_second_with_a_burst_of_eight() {
        let start = Instant::now();
        let mut bucket = Bucket::new(start);
        assert!(bucket.take(5 << 20, start));
        assert!(bucket.take(3 << 20, start));
        assert!(!bucket.take(1, start), "the burst is used");
        let later = start + Duration::from_millis(500);
        assert!(!bucket.take((1 << 20) / 2 + 1, later));
        assert!(bucket.take((1 << 20) / 2, later));
        // Full again after 8 s, and no fuller.
        let full = later + Duration::from_secs(60);
        assert!(bucket.take(8 << 20, full));
        assert!(!bucket.take(1, full));
    }

    #[test]
    fn heads_are_checked() {
        let headers = |pairs: &[(&str, &str)]| {
            let mut map = HeaderMap::new();
            for (k, v) in pairs {
                map.insert(
                    header::HeaderName::from_bytes(k.as_bytes()).unwrap(),
                    HeaderValue::from_str(v).unwrap(),
                );
            }
            map
        };
        let protobuf = headers(&[("content-type", "application/x-protobuf")]);
        let status = |method: Method, path: &str, headers: &HeaderMap| {
            check_head(&method, path, headers).map_err(|response| response.status())
        };
        assert_eq!(status(Method::POST, "/v1/traces", &protobuf), Ok(false));
        let gzip = headers(&[
            ("content-type", "Application/X-Protobuf; charset=binary"),
            ("content-encoding", "gzip"),
        ]);
        assert_eq!(status(Method::POST, "/v1/traces", &gzip), Ok(true));
        for (method, path, headers, answer) in [
            (Method::POST, "/v1/logs", &protobuf, 404),
            (Method::POST, "/v1/metrics", &protobuf, 404),
            (Method::GET, "/", &protobuf, 404),
            (Method::GET, "/v1/traces", &protobuf, 405),
            (Method::PUT, "/v1/traces", &protobuf, 405),
            (
                Method::POST,
                "/v1/traces",
                &headers(&[("content-type", "application/json")]),
                415,
            ),
            (Method::POST, "/v1/traces", &HeaderMap::new(), 415),
            (
                Method::POST,
                "/v1/traces",
                &headers(&[
                    ("content-type", "application/x-protobuf"),
                    ("content-encoding", "br"),
                ]),
                415,
            ),
            (
                Method::POST,
                "/v1/traces",
                &headers(&[
                    ("content-type", "application/x-protobuf"),
                    ("content-length", "4194305"),
                ]),
                413,
            ),
        ] {
            assert_eq!(
                status(method.clone(), path, headers).map_err(|s| s.as_u16()),
                Err(answer),
                "{method} {path} {headers:?}"
            );
        }
        let allow = check_head(&Method::GET, "/v1/traces", &protobuf).unwrap_err();
        assert_eq!(allow.headers()[header::ALLOW], "POST");
    }

    #[test]
    fn gzip_is_decompressed_up_to_6_mib() {
        let gzip = |bytes: &[u8]| {
            let mut encoder =
                flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::fast());
            encoder.write_all(bytes).unwrap();
            encoder.finish().unwrap()
        };
        assert_eq!(gunzip(&gzip(b"spans")).unwrap(), b"spans");
        assert_eq!(
            gunzip(&gzip(&vec![0; MAX_DECODED])).unwrap().len(),
            MAX_DECODED
        );
        assert_eq!(
            gunzip(&gzip(&vec![0; MAX_DECODED + 1])),
            Err(Refusal::TooLarge)
        );
        assert!(matches!(gunzip(b"plain"), Err(Refusal::Invalid(_))));
    }

    struct Fixed(Vec<TracedPod>);

    impl Senders for Fixed {
        fn pods(&self) -> Vec<TracedPod> {
            self.0.clone()
        }
    }

    #[tokio::test]
    async fn pods_are_known_by_their_addresses() {
        let traces = Traces::with_sink(
            Fixed(Vec::new()),
            Box::new(|| Err(Status::unavailable("none"))),
        );
        let inner = &traces.inner;
        let pod = |id: &str, labelled: bool, addresses: &[&str]| TracedPod {
            id: id.into(),
            labels: match labelled {
                true => labels(&[("ferun.tenant", "acme")]),
                false => HashMap::new(),
            },
            addresses: addresses.iter().map(|a| a.parse().unwrap()).collect(),
        };
        inner.set_pods(vec![
            pod("a", true, &["169.254.16.1", "fd00::2"]),
            pod("b", false, &["169.254.16.2"]),
        ]);
        let id = |address: &str| {
            inner
                .sender(Some(address.parse().unwrap()))
                .map(|sender| sender.id.clone())
        };
        assert_eq!(id("169.254.16.1"), Ok("a".to_string()));
        assert_eq!(id("::ffff:169.254.16.1"), Ok("a".to_string()));
        assert_eq!(id("fd00::2"), Ok("a".to_string()));
        // Without labels, or of no pod.
        assert_eq!(id("169.254.16.2"), Err(Refusal::Unknown));
        assert_eq!(id("169.254.16.3"), Err(Refusal::Unknown));
        assert_eq!(inner.sender(None).map(|_| ()), Err(Refusal::Unknown));
        // A pod that left is no longer, nor its limit.
        inner.admit("a", 1).unwrap();
        inner.set_pods(vec![pod("c", true, &["169.254.16.3"])]);
        assert_eq!(id("169.254.16.1"), Err(Refusal::Unknown));
        assert_eq!(id("169.254.16.3"), Ok("c".to_string()));
        assert!(lock(&inner.buckets).is_empty());
    }

    /// A pod that sends as it starts, before the watch has seen it, is
    /// found at its first request; an address of no pod stays refused.
    #[tokio::test]
    async fn a_pod_not_seen_yet_is_looked_for() {
        struct Later(Mutex<Vec<TracedPod>>);
        impl Senders for Later {
            fn pods(&self) -> Vec<TracedPod> {
                lock(&self.0).clone()
            }
        }
        let later = Arc::new(Later(Mutex::default()));
        struct Shared(Arc<Later>);
        impl Senders for Shared {
            fn pods(&self) -> Vec<TracedPod> {
                self.0.pods()
            }
        }
        let traces = Traces::with_sink(
            Shared(later.clone()),
            Box::new(|| Err(Status::unavailable("none"))),
        );
        let inner = &traces.inner;
        inner.rescan_now().await;
        let address: IpAddr = "169.254.16.7".parse().unwrap();
        assert_eq!(
            inner.sender(Some(address)).map(|_| ()),
            Err(Refusal::Unknown)
        );
        lock(&later.0).push(TracedPod {
            id: "new".into(),
            labels: labels(&[("ferun.tenant", "acme")]),
            addresses: vec![address],
        });
        let sender = inner.sender_now(Some(address)).await.unwrap();
        assert_eq!(sender.id, "new");
        let other: IpAddr = "169.254.16.8".parse().unwrap();
        assert_eq!(
            inner.sender_now(Some(other)).await.map(|_| ()),
            Err(Refusal::Unknown)
        );
    }

    /// A sink that answers with `code`, and keeps what it took.
    struct FakeSink {
        code: Code,
        took: Arc<Mutex<Vec<ExportTraceServiceRequest>>>,
    }

    #[tonic::async_trait]
    impl TraceService for FakeSink {
        async fn export(
            &self,
            request: tonic::Request<ExportTraceServiceRequest>,
        ) -> Result<tonic::Response<ExportTraceServiceResponse>, Status> {
            lock(&self.took).push(request.into_inner());
            match self.code {
                Code::Ok => Ok(tonic::Response::new(ExportTraceServiceResponse {
                    partial_success: Some(ExportTracePartialSuccess {
                        rejected_spans: 2,
                        error_message: "two bad".into(),
                    }),
                })),
                code => Err(Status::new(code, "no")),
            }
        }
    }

    /// The node at 127.0.0.1, which is pod `p1`'s address, before a sink
    /// that answers `code`.
    async fn node(
        code: Code,
    ) -> (
        SocketAddr,
        SocketAddr,
        Arc<Mutex<Vec<ExportTraceServiceRequest>>>,
    ) {
        let took = Arc::new(Mutex::new(Vec::new()));
        let sink = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let at = sink.local_addr().unwrap();
        let fake = FakeSink {
            code,
            took: took.clone(),
        };
        tokio::spawn(
            tonic::transport::Server::builder()
                .add_service(TraceServiceServer::new(fake))
                .serve_with_incoming(TcpIncoming::from(sink)),
        );
        let channel = Endpoint::from_shared(format!("http://{at}"))
            .unwrap()
            .connect_lazy();
        let pods = Fixed(vec![TracedPod {
            id: "p1".into(),
            labels: labels(&[("ferun.tenant", "acme"), ("ferun.job", "j1")]),
            addresses: vec!["127.0.0.1".parse().unwrap()],
        }]);
        let traces = Traces::with_sink(pods, Box::new(move || Ok((channel.clone(), "n1".into()))));
        // The pods are known before it serves.
        tokio::time::timeout(Duration::from_secs(5), async {
            while traces.inner.pods.read().unwrap().is_empty() {
                sleep(Duration::from_millis(5)).await;
            }
        })
        .await
        .unwrap();
        let grpc = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let http = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let (grpc_at, http_at) = (grpc.local_addr().unwrap(), http.local_addr().unwrap());
        tokio::spawn(serve_grpc(traces.inner.clone(), grpc));
        tokio::spawn(serve_http(traces.inner.clone(), http));
        (grpc_at, http_at, took)
    }

    /// Posts `body` and reads the answer: its status, headers and body.
    async fn post(at: SocketAddr, head: &str, body: &[u8]) -> (u16, String, Vec<u8>) {
        let head = format!("{head}\r\nContent-Length: {}", body.len());
        send(at, &head, body).await
    }

    async fn send(at: SocketAddr, head: &str, body: &[u8]) -> (u16, String, Vec<u8>) {
        let mut stream = tokio::net::TcpStream::connect(at).await.unwrap();
        let request = format!("{head}\r\nHost: node\r\nConnection: close\r\n\r\n");
        stream.write_all(request.as_bytes()).await.unwrap();
        stream.write_all(body).await.unwrap();
        let mut answer = Vec::new();
        stream.read_to_end(&mut answer).await.unwrap();
        let split = answer.windows(4).position(|w| w == b"\r\n\r\n").unwrap();
        let head = String::from_utf8_lossy(&answer[..split]).to_lowercase();
        let status = head[9..12].parse().unwrap();
        (status, head, answer[split + 4..].to_vec())
    }

    fn request() -> ExportTraceServiceRequest {
        spans(vec![Some(vec![string("ferun.tenant", "evil")])])
    }

    #[tokio::test]
    async fn http_goes_on_to_the_sink_and_answers_its_answer() {
        let (_, at, took) = node(Code::Ok).await;
        let body = request().encode_to_vec();
        let head = "POST /v1/traces HTTP/1.1\r\nContent-Type: application/x-protobuf";
        let (status, headers, answer) = post(at, head, &body).await;
        assert_eq!(status, 200);
        assert!(headers.contains("content-type: application/x-protobuf"));
        let answer = ExportTraceServiceResponse::decode(answer.as_slice()).unwrap();
        assert_eq!(answer.partial_success.unwrap().rejected_spans, 2);
        let sent = lock(&took).pop().unwrap();
        assert_eq!(
            attributes(sent.resource_spans[0].resource.as_ref().unwrap()),
            pairs(&[
                ("ferun.job", "j1"),
                ("ferun.node", "n1"),
                ("ferun.pod", "p1"),
                ("ferun.tenant", "acme"),
                ("service.name", "j1"),
            ])
        );

        // Gzip, the same.
        let mut encoder = flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::fast());
        encoder.write_all(&body).unwrap();
        let gzip = "POST /v1/traces HTTP/1.1\r\nContent-Type: application/x-protobuf\r\n\
                    Content-Encoding: gzip";
        let (status, _, _) = post(at, gzip, &encoder.finish().unwrap()).await;
        assert_eq!(status, 200);
        assert_eq!(lock(&took).len(), 1);

        // What is not a request of spans.
        let (status, headers, answer) = post(at, head, b"\xff\xff\xff").await;
        assert_eq!(status, 400);
        assert!(headers.contains("content-type: application/x-protobuf"));
        assert_eq!(
            RpcStatus::decode(answer.as_slice()).unwrap().code,
            Code::InvalidArgument as i32
        );
        let json = "POST /v1/traces HTTP/1.1\r\nContent-Type: application/json";
        assert_eq!(post(at, json, b"{}").await.0, 415);
        let logs = "POST /v1/logs HTTP/1.1\r\nContent-Type: application/x-protobuf";
        assert_eq!(post(at, logs, &body).await.0, 404);
        // Too large, by its length or as it is read.
        let large = format!("{head}\r\nContent-Length: {}", MAX_WIRE + 1);
        assert_eq!(send(at, &large, b"").await.0, 413);
        let chunked = format!("{head}\r\nTransfer-Encoding: chunked");
        let mut body = format!("{:x}\r\n", MAX_WIRE + 1).into_bytes();
        body.extend(vec![0; MAX_WIRE + 1]);
        body.extend(b"\r\n0\r\n\r\n");
        assert_eq!(send(at, &chunked, &body).await.0, 413);

        // Beyond the rate: 8 MiB of burst, then 429 and nothing sent on.
        let big = spans(vec![Some(vec![string("x", &"y".repeat(3 << 20))])]).encode_to_vec();
        lock(&took).clear();
        for _ in 0..2 {
            assert_eq!(post(at, head, &big).await.0, 200);
        }
        let (status, headers, _) = post(at, head, &big).await;
        assert_eq!(status, 429);
        assert!(headers.contains("retry-after: 1"), "{headers}");
        assert_eq!(lock(&took).len(), 2);
    }

    #[tokio::test]
    async fn the_sinks_refusal_is_the_pods() {
        let (grpc, at, _) = node(Code::PermissionDenied).await;
        let head = "POST /v1/traces HTTP/1.1\r\nContent-Type: application/x-protobuf";
        let (status, _, answer) = post(at, head, &request().encode_to_vec()).await;
        assert_eq!(status, 500);
        assert_eq!(
            RpcStatus::decode(answer.as_slice()).unwrap().code,
            Code::Internal as i32
        );
        let mut client = TraceServiceClient::connect(format!("http://{grpc}"))
            .await
            .unwrap();
        let status = client.export(request()).await.unwrap_err();
        assert_eq!(status.code(), Code::Internal);

        let (_, at, _) = node(Code::ResourceExhausted).await;
        let (status, headers, _) = post(at, head, &request().encode_to_vec()).await;
        assert_eq!(status, 429);
        assert!(headers.contains("retry-after: 30"), "{headers}");
    }

    #[tokio::test]
    async fn grpc_goes_on_to_the_sink() {
        let (grpc, _, took) = node(Code::Ok).await;
        let mut client = TraceServiceClient::connect(format!("http://{grpc}"))
            .await
            .unwrap();
        let answer = client.export(request()).await.unwrap().into_inner();
        assert_eq!(answer.partial_success.unwrap().rejected_spans, 2);
        let sent = lock(&took).pop().unwrap();
        let resource = sent.resource_spans[0].resource.as_ref().unwrap();
        assert!(attributes(resource).contains(&("ferun.tenant".into(), "acme".into())));
        // The other services are not served.
        let channel = Endpoint::from_shared(format!("http://{grpc}"))
            .unwrap()
            .connect()
            .await
            .unwrap();
        let mut logs = LogsServiceClient::new(channel);
        let status = logs
            .export(ExportLogsServiceRequest::default())
            .await
            .unwrap_err();
        assert_eq!(status.code(), Code::Unimplemented);
    }

    #[tokio::test]
    async fn a_pods_egress_spans_go_on_stamped_with_their_source() {
        let took = Arc::new(Mutex::new(Vec::new()));
        let sink = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let at = sink.local_addr().unwrap();
        let fake = FakeSink {
            code: Code::Ok,
            took: took.clone(),
        };
        tokio::spawn(
            tonic::transport::Server::builder()
                .add_service(TraceServiceServer::new(fake))
                .serve_with_incoming(TcpIncoming::from(sink)),
        );
        let channel = Endpoint::from_shared(format!("http://{at}"))
            .unwrap()
            .connect_lazy();
        let pods = Fixed(vec![TracedPod {
            id: "p1".into(),
            labels: labels(&[("ferun.tenant", "acme"), ("ferun.object", "shop")]),
            addresses: vec!["169.254.16.5".parse().unwrap()],
        }]);
        let traces = Traces::with_sink(pods, Box::new(move || Ok((channel.clone(), "n1".into()))));
        let span = feos_proto::opentelemetry::proto::trace::v1::Span {
            name: "GET erp".into(),
            ..Default::default()
        };
        traces.export_egress("p1", vec![span]).await.unwrap();
        let sent = lock(&took).pop().unwrap();
        let resource = sent.resource_spans[0].resource.as_ref().unwrap();
        let attributes = attributes(resource);
        for pair in [
            ("ferun.tenant", "acme"),
            ("ferun.pod", "p1"),
            ("ferun.node", "n1"),
            ("service.name", "shop"),
            ("ferun.source", "egress"),
            ("ferun.container", "feos-authn"),
        ] {
            assert!(
                attributes.contains(&(pair.0.into(), pair.1.into())),
                "{pair:?} in {attributes:?}"
            );
        }
        assert_eq!(
            sent.resource_spans[0].scope_spans[0].spans[0].name,
            "GET erp"
        );
        // A pod without labels sends none.
        traces
            .export_egress("unlabelled", vec![Default::default()])
            .await
            .unwrap();
        assert!(lock(&took).is_empty());
    }

    #[tokio::test]
    async fn without_a_sink_or_as_no_pod_the_node_refuses() {
        let traces = Traces::with_sink(
            Fixed(Vec::new()),
            Box::new(|| Err(Status::unavailable("the control plane names no sink"))),
        );
        let sender = Sender::new("p".into(), &labels(&[("ferun.tenant", "t")]));
        let refusal = traces.inner.export(&sender, request()).await.unwrap_err();
        assert_eq!(refusal.http(), (StatusCode::SERVICE_UNAVAILABLE, Some(2)));

        // From an address of no pod: refused before the sink is asked.
        let asked = Arc::new(Mutex::new(false));
        let ask = asked.clone();
        let traces = Traces::with_sink(
            Fixed(Vec::new()),
            Box::new(move || {
                *lock(&ask) = true;
                Err(Status::unavailable("none"))
            }),
        );
        let http = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let at = http.local_addr().unwrap();
        tokio::spawn(serve_http(traces.inner.clone(), http));
        let head = "POST /v1/traces HTTP/1.1\r\nContent-Type: application/x-protobuf";
        let (status, _, answer) = post(at, head, &request().encode_to_vec()).await;
        assert_eq!(status, 403);
        assert_eq!(
            RpcStatus::decode(answer.as_slice()).unwrap().code,
            Code::PermissionDenied as i32
        );
        assert!(!*lock(&asked));
    }
}
