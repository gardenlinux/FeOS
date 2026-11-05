// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The node's side against a control plane of the test's own: one port,
//! TLS 1.3, its chain leaf, issuer and root, client certificates verified
//! when given. The uplink (`h2`) serves feos.fleet.v1, and on a node
//! connection (`feos-node/1`) the test calls the node's API.

use feos_proto::container_service::{log_entry::Source, LogEntry, StreamContainerLogsRequest};
use feos_proto::fleet_service::fleet_service_server::{FleetService, FleetServiceServer};
use feos_proto::fleet_service::{
    event, Certificate, ControlPlaneInstance, Event, EventAck, EventBatch,
    IssuePodCredentialsRequest, IssuePodCredentialsResponse, JoinMeshRequest, JoinMeshResponse,
    JoinRequest, JoinResponse, RenewCertificateRequest, RenewCertificateResponse, Telemetry,
    TelemetryResponse,
};
use feos_proto::opentelemetry::proto::collector::logs::v1::{
    ExportLogsServiceRequest, ExportLogsServiceResponse,
};
use feos_proto::opentelemetry::proto::common::v1::{any_value, AnyValue};
use feos_proto::pod_service::{PodEvent, PodInfo, PodState};
use feos_proto::sink_service::sink_service_server::{SinkService, SinkServiceServer};
use feos_proto::sink_service::{
    EndStreamsRequest, EndStreamsResponse, PositionsRequest, PositionsResponse, StreamKey,
};
use feos_proto::volume_service::volume_service_client::VolumeServiceClient;
use feos_proto::volume_service::volume_service_server::{VolumeService, VolumeServiceServer};
use feos_proto::volume_service::{
    CreateVolumeRequest, DeleteVolumeRequest, DeleteVolumeResponse, GetVolumeRequest,
    ListVolumesRequest, ListVolumesResponse, Volume,
};
use fleet_client::{Config, Diskd, Installed, Inventory, LogStream, Logs, ShippedPod, Shipper};
use hyper_util::rt::TokioIo;
use rcgen::{
    BasicConstraints, CertificateParams, CertificateSigningRequestParams, DnType,
    ExtendedKeyUsagePurpose, IsCa, Issuer, KeyPair, PublicKeyData, SanType,
};
use rustls::pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer};
use rustls::server::WebPkiClientVerifier;
use rustls::RootCertStore;
use std::os::unix::fs::PermissionsExt;
use std::path::Path;
use std::pin::Pin;
use std::sync::{Arc, Mutex};
use std::task::{Context, Poll};
use std::time::{Duration, SystemTime};
use tokio::io::{AsyncRead, AsyncWrite, ReadBuf};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::mpsc;
use tokio::time::{sleep, Instant};
use tokio_rustls::server::TlsStream;
use tokio_rustls::TlsAcceptor;
use tokio_stream::wrappers::ReceiverStream;
use tokio_stream::Stream;
use tonic::transport::server::Connected;
use tonic::transport::{Endpoint, Server, Uri};
use tonic::{Request, Response, Status, Streaming};

const TRUST_DOMAIN: &str = "feos.local";
/// The name of the control plane's certificate; the test dials 127.0.0.1.
const NAME: &str = "ferun.feos.test";
const NODE: &str = "node-1";
const POD: &str = "3f2c0b8e-1d2a-4c5b-9e7f-0a1b2c3d4e5f";

fn key() -> KeyPair {
    KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256).unwrap()
}

fn ca_params(name: &str) -> CertificateParams {
    let mut params = CertificateParams::new(Vec::<String>::new()).unwrap();
    params.distinguished_name.push(DnType::CommonName, name);
    params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
    params
}

fn sha256_hex(bytes: &[u8]) -> String {
    use sha2::Digest;
    sha2::Sha256::digest(bytes)
        .iter()
        .map(|b| format!("{b:02x}"))
        .collect()
}

fn timestamp(t: SystemTime) -> Option<prost_types::Timestamp> {
    Some(t.into())
}

/// The platform CA: a root, and the issuer of the control plane's and the
/// nodes' certificates.
struct Pki {
    root: CertificateDer<'static>,
    /// The SHA-256 of the root's SubjectPublicKeyInfo, from its key.
    pin: String,
    issuer_cert: CertificateDer<'static>,
    issuer: Issuer<'static, KeyPair>,
}

impl Pki {
    fn new() -> Self {
        let root_key = key();
        let pin = sha256_hex(&root_key.subject_public_key_info());
        let root_params = ca_params("root");
        let root = root_params.self_signed(&root_key).unwrap().der().clone();
        let root_issuer = Issuer::new(root_params, root_key);
        let issuer_key = key();
        let issuer_params = ca_params("issuer");
        let issuer_cert = issuer_params
            .signed_by(&issuer_key, &root_issuer)
            .unwrap()
            .der()
            .clone();
        Self {
            root,
            pin,
            issuer_cert,
            issuer: Issuer::new(issuer_params, issuer_key),
        }
    }

    /// The control plane's server configuration: leaf, issuer and root.
    fn server_config(&self) -> Arc<rustls::ServerConfig> {
        let key = key();
        let mut params = CertificateParams::new(vec![NAME.to_string()]).unwrap();
        params.subject_alt_names.push(SanType::URI(
            format!("spiffe://{TRUST_DOMAIN}/ferun").try_into().unwrap(),
        ));
        params.extended_key_usages = vec![ExtendedKeyUsagePurpose::ServerAuth];
        let leaf = params.signed_by(&key, &self.issuer).unwrap().der().clone();
        let provider = Arc::new(rustls::crypto::ring::default_provider());
        let mut roots = RootCertStore::empty();
        roots.add(self.root.clone()).unwrap();
        let clients =
            WebPkiClientVerifier::builder_with_provider(Arc::new(roots), provider.clone())
                .allow_unauthenticated()
                .build()
                .unwrap();
        let mut config = rustls::ServerConfig::builder_with_provider(provider)
            .with_protocol_versions(&[&rustls::version::TLS13])
            .unwrap()
            .with_client_cert_verifier(clients)
            .with_single_cert(
                vec![leaf, self.issuer_cert.clone(), self.root.clone()],
                PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(key.serialize_der())),
            )
            .unwrap();
        config.alpn_protocols = vec![b"feos-node/1".to_vec(), b"h2".to_vec()];
        Arc::new(config)
    }

    /// A sink's server configuration (ADR 0026): its URI, no name the node
    /// dials, and the node's certificate required.
    fn sink_config(&self) -> Arc<rustls::ServerConfig> {
        let key = key();
        let mut params = CertificateParams::new(vec!["sink.feos.test".to_string()]).unwrap();
        params.subject_alt_names.push(SanType::URI(
            format!("spiffe://{TRUST_DOMAIN}/sink/sink-0")
                .try_into()
                .unwrap(),
        ));
        params.extended_key_usages = vec![ExtendedKeyUsagePurpose::ServerAuth];
        let leaf = params.signed_by(&key, &self.issuer).unwrap().der().clone();
        let provider = Arc::new(rustls::crypto::ring::default_provider());
        let mut roots = RootCertStore::empty();
        roots.add(self.root.clone()).unwrap();
        let clients =
            WebPkiClientVerifier::builder_with_provider(Arc::new(roots), provider.clone())
                .build()
                .unwrap();
        let mut config = rustls::ServerConfig::builder_with_provider(provider)
            .with_protocol_versions(&[&rustls::version::TLS13])
            .unwrap()
            .with_client_cert_verifier(clients)
            .with_single_cert(
                vec![leaf, self.issuer_cert.clone()],
                PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(key.serialize_der())),
            )
            .unwrap();
        config.alpn_protocols = vec![b"h2".to_vec()];
        Arc::new(config)
    }

    /// A node's certificate for the key of `csr`.
    fn issue(&self, csr: &[u8], name: &str, renew_after: SystemTime) -> Certificate {
        let mut csr = CertificateSigningRequestParams::from_der(&csr.to_vec().into()).unwrap();
        let spiffe_id = format!("spiffe://{TRUST_DOMAIN}/node/{name}");
        csr.params.subject_alt_names = vec![SanType::URI(spiffe_id.clone().try_into().unwrap())];
        csr.params.extended_key_usages = vec![ExtendedKeyUsagePurpose::ClientAuth];
        let leaf = csr.signed_by(&self.issuer).unwrap();
        let now = SystemTime::now();
        Certificate {
            spiffe_id,
            chain: vec![leaf.der().to_vec(), self.issuer_cert.to_vec()],
            trust_bundle: vec![self.root.to_vec()],
            not_before: timestamp(now - Duration::from_secs(600)),
            not_after: timestamp(now + Duration::from_secs(3600)),
            renew_after: timestamp(renew_after),
        }
    }
}

/// What the test control plane saw.
#[derive(Default)]
struct Seen {
    joins: Vec<JoinRequest>,
    renewals: usize,
    /// The certificate issued last.
    issued: Vec<u8>,
    /// Each report, with the certificate it came with.
    reports: Vec<(Vec<u8>, Telemetry)>,
    events: Vec<Event>,
    last_seq: u64,
    /// ListVolumes answered on a node connection, with its certificate.
    volumes: Vec<(Vec<u8>, String)>,
    /// Refuse the node from now on.
    refuse: bool,
    /// The JoinMesh calls, with the certificate they came with.
    mesh_joins: Vec<(Vec<u8>, JoinMeshRequest)>,
    /// When each report came.
    reported_at: Vec<std::time::Instant>,
    /// The node connections the node opened.
    node_connections: usize,
    /// The channels of the node connections, for the test's calls.
    channels: Vec<tonic::transport::Channel>,
    /// The sink the control plane names.
    sink_endpoint: String,
    /// The pods' credentials asked for.
    credentials: Vec<IssuePodCredentialsRequest>,
}

struct ControlPlane {
    pki: Arc<Pki>,
    secret: String,
    address: String,
    seen: Arc<Mutex<Seen>>,
}

/// The connection's client certificate, for the handlers.
#[derive(Clone)]
struct Peer(Option<Vec<u8>>);

struct Uplink(TlsStream<TcpStream>);

impl Connected for Uplink {
    type ConnectInfo = Peer;

    fn connect_info(&self) -> Peer {
        Peer(
            self.0
                .get_ref()
                .1
                .peer_certificates()
                .and_then(|certs| certs.first())
                .map(|cert| cert.to_vec()),
        )
    }
}

impl AsyncRead for Uplink {
    fn poll_read(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &mut ReadBuf<'_>,
    ) -> Poll<std::io::Result<()>> {
        Pin::new(&mut self.0).poll_read(cx, buf)
    }
}

impl AsyncWrite for Uplink {
    fn poll_write(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<std::io::Result<usize>> {
        Pin::new(&mut self.0).poll_write(cx, buf)
    }

    fn poll_flush(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<std::io::Result<()>> {
        Pin::new(&mut self.0).poll_flush(cx)
    }

    fn poll_shutdown(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<std::io::Result<()>> {
        Pin::new(&mut self.0).poll_shutdown(cx)
    }
}

impl ControlPlane {
    fn peer<T>(&self, request: &Request<T>) -> Result<Vec<u8>, Status> {
        if self.seen.lock().unwrap().refuse {
            return Err(Status::permission_denied(format!(
                "node {NODE} was removed; join it again"
            )));
        }
        request
            .extensions()
            .get::<Peer>()
            .and_then(|peer| peer.0.clone())
            .ok_or_else(|| Status::unauthenticated("no client certificate"))
    }

    fn instances(&self) -> Vec<ControlPlaneInstance> {
        vec![ControlPlaneInstance {
            id: "i1".into(),
            node_address: self.address.clone(),
        }]
    }
}

#[tonic::async_trait]
impl FleetService for ControlPlane {
    type PublishEventsStream = Pin<Box<dyn Stream<Item = Result<EventAck, Status>> + Send>>;

    async fn join(&self, request: Request<JoinRequest>) -> Result<Response<JoinResponse>, Status> {
        let request = request.into_inner();
        self.seen.lock().unwrap().joins.push(request.clone());
        if request.token != self.secret {
            return Err(Status::permission_denied("unknown join token"));
        }
        // Renewed at once.
        let certificate = self
            .pki
            .issue(&request.csr, &request.name, SystemTime::now());
        {
            let mut seen = self.seen.lock().unwrap();
            seen.issued = certificate.chain[0].clone();
            seen.refuse = false;
        }
        Ok(Response::new(JoinResponse {
            name: request.name,
            certificate: Some(certificate),
            instances: self.instances(),
        }))
    }

    async fn renew_certificate(
        &self,
        request: Request<RenewCertificateRequest>,
    ) -> Result<Response<RenewCertificateResponse>, Status> {
        self.peer(&request)?;
        let later = SystemTime::now() + Duration::from_secs(3000);
        let certificate = self.pki.issue(&request.get_ref().csr, NODE, later);
        let mut seen = self.seen.lock().unwrap();
        seen.renewals += 1;
        seen.issued = certificate.chain[0].clone();
        Ok(Response::new(RenewCertificateResponse {
            certificate: Some(certificate),
        }))
    }

    async fn report_telemetry(
        &self,
        request: Request<Telemetry>,
    ) -> Result<Response<TelemetryResponse>, Status> {
        let peer = self.peer(&request)?;
        let mut seen = self.seen.lock().unwrap();
        seen.reports.push((peer, request.into_inner()));
        seen.reported_at.push(std::time::Instant::now());
        drop(seen);
        let sink_endpoint = self.seen.lock().unwrap().sink_endpoint.clone();
        Ok(Response::new(TelemetryResponse {
            interval_seconds: 1,
            instances: self.instances(),
            sink_endpoint,
        }))
    }

    async fn publish_events(
        &self,
        request: Request<Streaming<EventBatch>>,
    ) -> Result<Response<Self::PublishEventsStream>, Status> {
        self.peer(&request)?;
        let mut batches = request.into_inner();
        let seen = self.seen.clone();
        let (tx, rx) = mpsc::channel(4);
        tokio::spawn(async move {
            while let Ok(Some(batch)) = batches.message().await {
                let seq = {
                    let mut seen = seen.lock().unwrap();
                    for event in batch.events {
                        if event.seq > seen.last_seq {
                            seen.last_seq = event.seq;
                            seen.events.push(event);
                        }
                    }
                    seen.last_seq
                };
                let ack = EventAck {
                    start_id: batch.start_id,
                    seq,
                };
                if tx.send(Ok(ack)).await.is_err() {
                    return;
                }
            }
        });
        Ok(Response::new(Box::pin(ReceiverStream::new(rx))))
    }

    async fn issue_pod_credentials(
        &self,
        request: Request<IssuePodCredentialsRequest>,
    ) -> Result<Response<IssuePodCredentialsResponse>, Status> {
        self.peer(&request)?;
        let request = request.into_inner();
        if request.pod_id != POD {
            return Err(Status::not_found(format!("no pod {}", request.pod_id)));
        }
        self.seen.lock().unwrap().credentials.push(request.clone());
        Ok(Response::new(IssuePodCredentialsResponse {
            certificate: Some(Certificate {
                spiffe_id: format!("spiffe://{TRUST_DOMAIN}/pod/{POD}"),
                chain: vec![request.csr],
                ..Default::default()
            }),
            bindings: Some(feos_proto::fleet_service::SealedBindings {
                enc: request.hpke_public_key,
                ciphertext: b"sealed".to_vec(),
                version: "7".into(),
            }),
        }))
    }

    async fn join_mesh(
        &self,
        request: Request<JoinMeshRequest>,
    ) -> Result<Response<JoinMeshResponse>, Status> {
        let peer = self.peer(&request)?;
        self.seen
            .lock()
            .unwrap()
            .mesh_joins
            .push((peer, request.into_inner()));
        Ok(Response::new(JoinMeshResponse {
            pod_network: MESH_NETWORK.into(),
            pod_prefix: MESH_PREFIX.into(),
        }))
    }
}

/// Starts the test control plane; its token and its address.
async fn control_plane(pki: Arc<Pki>, seen: Arc<Mutex<Seen>>) -> (String, String) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap().to_string();
    let secret = format!("feos-join-{}", "Q".repeat(43));
    let fleet = FleetServiceServer::new(ControlPlane {
        pki: pki.clone(),
        secret: secret.clone(),
        address: address.clone(),
        seen: seen.clone(),
    });
    let (uplinks, incoming) = mpsc::channel::<Result<Uplink, std::io::Error>>(16);
    tokio::spawn(
        Server::builder()
            .add_service(fleet)
            .serve_with_incoming(ReceiverStream::new(incoming)),
    );
    let acceptor = TlsAcceptor::from(pki.server_config());
    tokio::spawn(async move {
        loop {
            let (tcp, _) = listener.accept().await.unwrap();
            let acceptor = acceptor.clone();
            let uplinks = uplinks.clone();
            let seen = seen.clone();
            tokio::spawn(async move {
                // A node that refuses the server ends the handshake.
                let Ok(tls) = acceptor.accept(tcp).await else {
                    return;
                };
                match tls.get_ref().1.alpn_protocol() {
                    Some(b"h2") => {
                        uplinks.send(Ok(Uplink(tls))).await.ok();
                    }
                    Some(b"feos-node/1") => {
                        seen.lock().unwrap().node_connections += 1;
                        call_node(tls, seen).await
                    }
                    _ => {}
                }
            });
        }
    });
    (format!("{secret}.{}", pki.pin), address)
}

/// The control plane's side of a node connection: it is the HTTP/2 client
/// on the connection the node opened.
async fn call_node(tls: TlsStream<TcpStream>, seen: Arc<Mutex<Seen>>) {
    let peer = tls
        .get_ref()
        .1
        .peer_certificates()
        .and_then(|certs| certs.first())
        .map(|cert| cert.to_vec())
        .unwrap_or_default();
    let stream = Mutex::new(Some(tls));
    // A node that does not answer pings within 3 s loses its connection,
    // as ferund's keepalive drops it.
    let channel = Endpoint::from_static("http://node")
        .http2_keep_alive_interval(Duration::from_secs(1))
        .keep_alive_timeout(Duration::from_secs(3))
        .keep_alive_while_idle(true)
        .connect_with_connector(tower::service_fn(move |_: Uri| {
            let stream = stream.lock().unwrap().take();
            async move {
                stream
                    .map(TokioIo::new)
                    .ok_or_else(|| std::io::Error::other("the node connection was used"))
            }
        }))
        .await
        .unwrap();
    seen.lock().unwrap().channels.push(channel.clone());
    let mut client = VolumeServiceClient::new(channel.clone());
    let volumes = client.list_volumes(ListVolumesRequest {}).await;
    if let Ok(volumes) = volumes {
        for volume in volumes.into_inner().volumes {
            seen.lock()
                .unwrap()
                .volumes
                .push((peer.clone(), volume.volume_id));
        }
    }
    // The connection stays, as the control plane's do.
    sleep(Duration::from_secs(600)).await;
    drop(channel);
}

/// The node's volumes, as its API answers on the node connection.
struct Volumes;

/// The GetVolume calls that came, and the volumes whose calls may answer.
#[derive(Default)]
struct Gates {
    entered: Vec<String>,
    opened: Vec<String>,
}

static GATES: Mutex<Gates> = Mutex::new(Gates {
    entered: Vec::new(),
    opened: Vec::new(),
});

#[tonic::async_trait]
impl VolumeService for Volumes {
    async fn create_volume(
        &self,
        _request: Request<CreateVolumeRequest>,
    ) -> Result<Response<Volume>, Status> {
        Err(Status::unimplemented("not in this test"))
    }

    async fn delete_volume(
        &self,
        _request: Request<DeleteVolumeRequest>,
    ) -> Result<Response<DeleteVolumeResponse>, Status> {
        Err(Status::unimplemented("not in this test"))
    }

    /// Answers once the test opens the volume's gate (`GATES`).
    async fn get_volume(
        &self,
        request: Request<GetVolumeRequest>,
    ) -> Result<Response<Volume>, Status> {
        let volume_id = request.into_inner().volume_id;
        GATES.lock().unwrap().entered.push(volume_id.clone());
        while !GATES.lock().unwrap().opened.contains(&volume_id) {
            sleep(Duration::from_millis(10)).await;
        }
        Ok(Response::new(Volume {
            volume_id,
            ..Default::default()
        }))
    }

    async fn list_volumes(
        &self,
        _request: Request<ListVolumesRequest>,
    ) -> Result<Response<ListVolumesResponse>, Status> {
        Ok(Response::new(ListVolumesResponse {
            volumes: vec![Volume {
                volume_id: "v1".into(),
                ..Default::default()
            }],
        }))
    }
}

/// The cluster's ULA and the node's /64 of it, as the test control plane
/// gives them.
const MESH_NETWORK: &str = "fd5e:1b2c:93a0::/48";
const MESH_PREFIX: &str = "fd5e:1b2c:93a0:7::/64";

/// What the node was told of the mesh, the last answer.
static JOINED: Mutex<Option<(String, String)>> = Mutex::new(None);

struct Node;

impl Inventory for Node {
    fn pods(&self) -> Vec<PodInfo> {
        vec![PodInfo {
            pod_id: POD.into(),
            state: PodState::Running as i32,
            ..Default::default()
        }]
    }

    fn installed(&self) -> Installed {
        Installed {
            firecracker: vec!["1.12.0".into()],
            guest: vec!["0.6.0".into()],
        }
    }

    async fn diskd(&self) -> Option<Diskd> {
        Some(Diskd {
            object_store: true,
            object_store_id: "abc".into(),
            version: "0.6.0".into(),
        })
    }

    fn pod_prefix(&self) -> Option<String> {
        JOINED
            .lock()
            .unwrap()
            .as_ref()
            .map(|(_, prefix)| prefix.clone())
    }

    fn mesh_key(&self) -> Option<([u8; 32], u16)> {
        Some(([7; 32], 51821))
    }

    async fn joined_mesh(&self, pod_network: String, pod_prefix: String) -> Result<(), String> {
        *JOINED.lock().unwrap() = Some((pod_network, pod_prefix));
        Ok(())
    }
}

/// Starts the node's side with the token in `dir`, serving `Volumes` on
/// its node connections.
fn node(address: &str, dir: &Path) -> fleet_client::Fleet {
    node_with(address, dir, Node, fleet_client::STALL_LIMIT)
}

/// `node` with another inventory and stall limit. The node's API runs on
/// the test's runtime, as feosd's does on feosd's, and is served as feosd
/// serves it.
fn node_with<I: Inventory>(
    address: &str,
    dir: &Path,
    inventory: I,
    stall_limit: Duration,
) -> fleet_client::Fleet {
    let mut config = Config::new(address, Some(NAME), TRUST_DOMAIN).unwrap();
    config.join_token_file = dir.join("join-token");
    config.node_dir = dir.join("node");
    config.node_name = Some(NODE.into());
    config.pod_prefix = "fd00:fe05:1::/64".into();
    config.version = "0.6.0".into();
    config.stall_limit = stall_limit;
    let calls = fleet_client::NodeCalls::new();
    let (fleet, incoming) = fleet_client::start(config, inventory, calls.clone()).unwrap();
    fleet.serve(
        fleet_client::server(tokio::runtime::Handle::current(), &calls)
            .add_service(VolumeServiceServer::new(Volumes)),
        incoming,
    );
    fleet
}

/// Waits until `done` holds of what the control plane saw.
async fn wait_for(seen: &Mutex<Seen>, what: &str, done: impl Fn(&Seen) -> bool) {
    let deadline = Instant::now() + Duration::from_secs(30);
    while !done(&seen.lock().unwrap()) {
        assert!(Instant::now() < deadline, "timed out waiting for {what}");
        sleep(Duration::from_millis(50)).await;
    }
}

fn uri_of(cert: &[u8]) -> Vec<String> {
    let (_, cert) = x509_parser::parse_x509_certificate(cert).unwrap();
    cert.subject_alternative_name()
        .unwrap()
        .map(|names| {
            names
                .value
                .general_names
                .iter()
                .filter_map(|name| match name {
                    x509_parser::extensions::GeneralName::URI(uri) => Some(uri.to_string()),
                    _ => None,
                })
                .collect()
        })
        .unwrap_or_default()
}

#[tokio::test]
async fn a_node_joins_reports_renews_and_serves_its_api() {
    let pki = Arc::new(Pki::new());
    let seen = Arc::new(Mutex::new(Seen::default()));
    let (token, address) = control_plane(pki, seen.clone()).await;
    let dir = tempfile::tempdir().unwrap();
    std::fs::write(dir.path().join("join-token"), format!("{token}\n")).unwrap();
    let fleet = node(&address, dir.path());

    // The join: the secret alone, the node's name, a signing request.
    wait_for(&seen, "the join", |seen| !seen.joins.is_empty()).await;
    let join = seen.lock().unwrap().joins[0].clone();
    assert_eq!(join.token, token.split_once('.').unwrap().0);
    assert_eq!(join.name, NODE);
    assert!(!join.csr.is_empty());

    // The identity is kept, the key for root alone.
    let node_dir = dir.path().join("node");
    wait_for(&seen, "a renewal", |seen| seen.renewals > 0).await;
    for file in ["key.pem", "chain.pem", "bundle.pem", "node.json"] {
        assert!(node_dir.join(file).exists(), "{file}");
    }
    let mode = |path: &Path| std::fs::metadata(path).unwrap().permissions().mode() & 0o777;
    assert_eq!(mode(&node_dir), 0o700);
    assert_eq!(mode(&node_dir.join("key.pem")), 0o600);

    // Telemetry, with the renewed certificate.
    wait_for(&seen, "a report with the renewed certificate", |seen| {
        seen.reports.iter().any(|(cert, _)| *cert == seen.issued)
    })
    .await;
    let (cert, report) = seen.lock().unwrap().reports.last().unwrap().clone();
    assert_eq!(
        uri_of(&cert),
        [format!("spiffe://{TRUST_DOMAIN}/node/{NODE}")]
    );
    assert!(uuid::Uuid::parse_str(&report.start_id).is_ok());
    assert_eq!(report.pods_running, 1);
    assert_eq!(
        report.inventory_hash,
        sha256_hex(format!("{POD} POD_STATE_RUNNING\n").as_bytes())
    );
    // The configured prefix, until the mesh gives the node one.
    let first = seen.lock().unwrap().reports[0].1.clone();
    assert_eq!(first.pod_prefix, "fd00:fe05:1::/64");
    assert!(report.object_store);
    assert_eq!(report.object_store_id, "abc");
    let versions = report.versions.unwrap();
    assert_eq!(versions.feosd, "0.6.0");
    assert_eq!(versions.firecracker, ["1.12.0"]);
    assert_eq!(versions.diskd, "0.6.0");
    assert!(report.memory_total_bytes > 0);
    let state = std::fs::read_to_string(node_dir.join("node.json")).unwrap();
    assert!(state.contains(&address), "{state}");

    // The node joins the mesh once, with its key and the address from
    // which it reaches the control plane, and takes the answer: its /64 is
    // its prefix from then on.
    wait_for(&seen, "a report with the mesh's /64", |seen| {
        seen.reports
            .iter()
            .any(|(_, report)| report.pod_prefix == MESH_PREFIX)
    })
    .await;
    assert_eq!(
        *JOINED.lock().unwrap(),
        Some((MESH_NETWORK.to_string(), MESH_PREFIX.to_string()))
    );
    let joins = seen.lock().unwrap().mesh_joins.clone();
    assert_eq!(joins.len(), 1, "{joins:?}");
    assert_eq!(joins[0].1.public_key, [7; 32]);
    assert_eq!(joins[0].1.endpoint, "127.0.0.1:51821");

    // The node's API on a node connection, with the renewed certificate.
    wait_for(&seen, "ListVolumes on a node connection", |seen| {
        seen.volumes
            .iter()
            .any(|(cert, volume)| *cert == seen.issued && volume == "v1")
    })
    .await;

    // Events: feosd started, then a pod's; acknowledged.
    fleet.events().pod(PodEvent {
        pod_id: POD.into(),
        state: PodState::Running as i32,
        ..Default::default()
    });
    wait_for(&seen, "the pod's event", |seen| seen.events.len() == 2).await;
    {
        let seen = seen.lock().unwrap();
        let seqs: Vec<u64> = seen.events.iter().map(|event| event.seq).collect();
        assert_eq!(seqs, [1, 2]);
        assert!(matches!(
            &seen.events[0].body,
            Some(event::Body::Node(node)) if node.kind == "started"
        ));
        assert!(matches!(&seen.events[1].body, Some(event::Body::Pod(_))));
    }
    fleet.stopping("restart").await;
    assert!(fleet.events().flush(Duration::from_millis(1)).await);
    assert_eq!(seen.lock().unwrap().events.len(), 3);
}

#[tokio::test]
async fn a_pods_credentials_come_over_the_uplink() {
    let pki = Arc::new(Pki::new());
    let seen = Arc::new(Mutex::new(Seen::default()));
    let (token, address) = control_plane(pki, seen.clone()).await;
    let dir = tempfile::tempdir().unwrap();
    std::fs::write(dir.path().join("join-token"), format!("{token}\n")).unwrap();
    let fleet = node(&address, dir.path());
    // Asked before the uplink is up: it waits for it.
    let response = fleet
        .issue_pod_credentials(IssuePodCredentialsRequest {
            pod_id: POD.into(),
            csr: b"csr".to_vec(),
            hpke_public_key: [7; 32].to_vec(),
        })
        .await
        .unwrap();
    let certificate = response.certificate.unwrap();
    assert_eq!(certificate.chain, [b"csr".to_vec()]);
    let bindings = response.bindings.unwrap();
    assert_eq!(
        (bindings.enc, bindings.version),
        ([7; 32].to_vec(), "7".into())
    );
    let asked = seen.lock().unwrap().credentials.clone();
    assert_eq!(asked.len(), 1);
    assert_eq!(asked[0].pod_id, POD);
    // The control plane's refusal is the answer.
    let refused = fleet
        .issue_pod_credentials(IssuePodCredentialsRequest {
            pod_id: "other".into(),
            ..Default::default()
        })
        .await
        .unwrap_err();
    assert_eq!(refused.code(), tonic::Code::NotFound);
}

#[tokio::test]
async fn a_refused_node_waits_for_a_new_token() {
    let pki = Arc::new(Pki::new());
    let seen = Arc::new(Mutex::new(Seen::default()));
    let (token, address) = control_plane(pki, seen.clone()).await;
    let dir = tempfile::tempdir().unwrap();
    let token_file = dir.path().join("join-token");
    // A token that pins another root: the node never sends it.
    let (secret, _) = token.split_once('.').unwrap();
    std::fs::write(&token_file, format!("{secret}.{}", "0".repeat(64))).unwrap();
    let _fleet = node(&address, dir.path());
    sleep(Duration::from_secs(2)).await;
    assert!(seen.lock().unwrap().joins.is_empty());

    std::fs::write(&token_file, &token).unwrap();
    wait_for(&seen, "a report", |seen| !seen.reports.is_empty()).await;

    // Removed: the identity goes aside, and the node waits for a new token.
    seen.lock().unwrap().refuse = true;
    let node_dir = dir.path().join("node");
    let deadline = Instant::now() + Duration::from_secs(30);
    while node_dir.join("node.json").exists() {
        assert!(Instant::now() < deadline, "the identity was not set aside");
        sleep(Duration::from_millis(50)).await;
    }
    let aside: Vec<_> = std::fs::read_dir(&node_dir)
        .unwrap()
        .flatten()
        .filter(|entry| entry.file_name().to_string_lossy().starts_with("removed-"))
        .collect();
    assert_eq!(aside.len(), 1);
    assert!(aside[0].path().join("key.pem").exists());
    sleep(Duration::from_secs(2)).await;
    assert_eq!(seen.lock().unwrap().joins.len(), 1);

    // A new token joins it again.
    let secret = format!("feos-join-{}", "R".repeat(43));
    std::fs::write(
        &token_file,
        format!("{secret}.{}", token.split_once('.').unwrap().1),
    )
    .unwrap();
    wait_for(&seen, "the second join", |seen| seen.joins.len() == 2).await;
    assert_eq!(seen.lock().unwrap().joins[1].token, secret);
}

/// A runtime of the test's own, as the control plane is another process;
/// shut down without waiting when the test ends.
struct Elsewhere(Option<tokio::runtime::Runtime>);

impl Elsewhere {
    fn new() -> Self {
        Self(Some(
            tokio::runtime::Builder::new_multi_thread()
                .worker_threads(2)
                .enable_all()
                .build()
                .unwrap(),
        ))
    }

    fn handle(&self) -> tokio::runtime::Handle {
        self.0.as_ref().unwrap().handle().clone()
    }
}

impl Drop for Elsewhere {
    fn drop(&mut self) {
        if let Some(runtime) = self.0.take() {
            runtime.shutdown_background();
        }
    }
}

/// The node's pods, which do not answer while the lock is held, as the
/// pods' records' lock is while one of them is synced.
struct Gated(Arc<Mutex<()>>);

impl Inventory for Gated {
    fn pods(&self) -> Vec<PodInfo> {
        let _held = self.0.lock().unwrap_or_else(|e| e.into_inner());
        Node.pods()
    }

    fn installed(&self) -> Installed {
        Node.installed()
    }

    async fn diskd(&self) -> Option<Diskd> {
        Node.diskd().await
    }
}

/// On 2026-10-28 a suspend whose snapshot was late removed the snapshot
/// while Firecracker still wrote it; the unlink(2) blocked a thread of
/// feosd's runtime for 50 s, which stalled the whole runtime: no report
/// went out, the node connections' pings went unanswered, and the control
/// plane counted the node down. The node's side runs on threads of its
/// own: a blocked thread of feosd's runtime, which holds what the pods'
/// read needs, stops neither the reports nor the node connections. The
/// test's runtime, feosd's here, has the one thread: blocking it stalls it
/// as a whole, its timers and its I/O, as the incident did.
#[tokio::test(flavor = "current_thread")]
async fn a_blocked_runtime_stops_neither_the_reports_nor_the_node_connections() {
    let plane = Elsewhere::new();
    let pki = Arc::new(Pki::new());
    let seen = Arc::new(Mutex::new(Seen::default()));
    let (token, address) = plane
        .handle()
        .spawn(control_plane(pki, seen.clone()))
        .await
        .unwrap();
    let dir = tempfile::tempdir().unwrap();
    std::fs::write(dir.path().join("join-token"), format!("{token}\n")).unwrap();
    let gate = Arc::new(Mutex::new(()));
    let _fleet = node_with(
        &address,
        dir.path(),
        Gated(gate.clone()),
        fleet_client::STALL_LIMIT,
    );

    // Settled: renewed, reporting, and serving with the new certificate.
    wait_for(&seen, "reports with the renewed certificate", |seen| {
        seen.reports
            .iter()
            .filter(|(cert, _)| *cert == seen.issued)
            .count()
            >= 2
    })
    .await;
    wait_for(&seen, "ListVolumes with the renewed certificate", |seen| {
        seen.volumes.iter().any(|(cert, _)| *cert == seen.issued)
    })
    .await;
    let connections = seen.lock().unwrap().node_connections;

    // A task of feosd's runtime that a timer woke, as the suspend's
    // deadline did, blocks its thread and holds the pods' lock meanwhile.
    const BLOCKED: Duration = Duration::from_secs(8);
    let held = gate.clone();
    let started = std::time::Instant::now();
    tokio::spawn(async move {
        sleep(Duration::from_millis(100)).await;
        let _held = held.lock().unwrap();
        std::thread::sleep(BLOCKED);
    });
    // The test's own timer waits out the block, and a redial after it.
    sleep(BLOCKED + Duration::from_secs(5)).await;

    let seen = seen.lock().unwrap();
    let window = (
        started + Duration::from_millis(1500),
        started + BLOCKED - Duration::from_millis(500),
    );
    let during: Vec<&Telemetry> = seen
        .reports
        .iter()
        .zip(&seen.reported_at)
        .filter(|(_, at)| **at > window.0 && **at < window.1)
        .map(|((_, report), _)| report)
        .collect();
    assert!(
        during.len() >= 2,
        "{} reports while feosd's runtime was blocked",
        during.len()
    );
    // The pods as they were, since they did not answer.
    assert!(during.iter().all(|report| report.pods_running == 1));
    assert_eq!(
        seen.node_connections, connections,
        "a node connection was lost and dialed again"
    );
}

/// A feosd that stands still for longer than the stall limit takes no
/// calls: the node stops reporting, so that the control plane counts it
/// down, and reports again once feosd moves.
#[tokio::test(flavor = "current_thread")]
async fn a_runtime_stalled_past_the_limit_stops_the_reports_until_it_moves() {
    let plane = Elsewhere::new();
    let pki = Arc::new(Pki::new());
    let seen = Arc::new(Mutex::new(Seen::default()));
    let (token, address) = plane
        .handle()
        .spawn(control_plane(pki, seen.clone()))
        .await
        .unwrap();
    let dir = tempfile::tempdir().unwrap();
    std::fs::write(dir.path().join("join-token"), format!("{token}\n")).unwrap();
    let _fleet = node_with(&address, dir.path(), Node, Duration::from_secs(2));
    wait_for(&seen, "reports with the renewed certificate", |seen| {
        seen.reports
            .iter()
            .filter(|(cert, _)| *cert == seen.issued)
            .count()
            >= 2
    })
    .await;

    // feosd's runtime, the test's, stands still for longer than the limit.
    const BLOCKED: Duration = Duration::from_secs(8);
    let started = std::time::Instant::now();
    tokio::spawn(async move {
        sleep(Duration::from_millis(100)).await;
        std::thread::sleep(BLOCKED);
    });
    sleep(BLOCKED + Duration::from_secs(5)).await;

    let seen = seen.lock().unwrap();
    let between = |from: Duration, to: Duration| {
        seen.reported_at
            .iter()
            .filter(|at| **at > started + from && **at < started + to)
            .count()
    };
    assert_eq!(
        between(Duration::from_secs(4), BLOCKED - Duration::from_millis(500)),
        0,
        "reports while feosd stood still past the limit"
    );
    assert!(
        between(BLOCKED, BLOCKED + Duration::from_secs(5)) >= 2,
        "no reports once feosd moved again"
    );
}

/// A drill on 2026-10-28: the feos-node pod restarted while an exec ran,
/// and the exec ended with "error reading from server: EOF": the stop
/// dropped the node connections. A stop now sends GOAWAY: the control
/// plane starts no new call on the connection, and the node dials no
/// more; a call in flight that ends within the bound answers, and one that
/// does not ends with UNAVAILABLE "the node stops".
#[tokio::test]
async fn a_stop_lets_the_calls_in_flight_end_and_cuts_the_rest() {
    let pki = Arc::new(Pki::new());
    let seen = Arc::new(Mutex::new(Seen::default()));
    let (token, address) = control_plane(pki, seen.clone()).await;
    let dir = tempfile::tempdir().unwrap();
    std::fs::write(dir.path().join("join-token"), format!("{token}\n")).unwrap();
    let fleet = node(&address, dir.path());
    wait_for(&seen, "a node connection", |seen| !seen.channels.is_empty()).await;
    let channel = seen.lock().unwrap().channels[0].clone();
    let get = |id: &'static str| {
        let mut client = VolumeServiceClient::new(channel.clone());
        tokio::spawn(async move {
            client
                .get_volume(GetVolumeRequest {
                    volume_id: id.into(),
                })
                .await
                .map(|volume| volume.into_inner().volume_id)
        })
    };
    let entered = |id: &str| GATES.lock().unwrap().entered.iter().any(|e| e == id);
    let quick = get("stop-quick");
    let slow = get("stop-slow");
    let deadline = Instant::now() + Duration::from_secs(10);
    while !(entered("stop-quick") && entered("stop-slow")) {
        assert!(Instant::now() < deadline, "the calls did not come");
        sleep(Duration::from_millis(10)).await;
    }
    let connections = seen.lock().unwrap().node_connections;

    let started = Instant::now();
    let stopping = {
        let fleet = fleet.clone();
        tokio::spawn(async move { fleet.stop(Duration::from_secs(1)).await })
    };
    sleep(Duration::from_millis(300)).await;
    // No new call starts on the connection.
    let late = get("stop-late");
    GATES.lock().unwrap().opened.push("stop-quick".into());
    assert_eq!(quick.await.unwrap().unwrap(), "stop-quick");
    let cut = slow.await.unwrap().expect_err("the slow call answered");
    assert_eq!(cut.code(), tonic::Code::Unavailable, "{cut:?}");
    assert!(
        cut.message().starts_with("the node stops: "),
        "{}",
        cut.message()
    );
    stopping.await.unwrap();
    assert!(
        started.elapsed() < Duration::from_secs(4),
        "the stop took {:?}",
        started.elapsed()
    );
    assert!(
        late.await.unwrap().is_err(),
        "a call after the stop answered"
    );
    assert!(!entered("stop-late"), "a call after the stop came");
    // The node dials no more.
    sleep(Duration::from_secs(2)).await;
    assert_eq!(seen.lock().unwrap().node_connections, connections);
}

/// A stop without calls in flight does not wait for its bound: the server
/// lets go of an idle connection at once.
#[tokio::test]
async fn a_stop_without_calls_ends_at_once() {
    let pki = Arc::new(Pki::new());
    let seen = Arc::new(Mutex::new(Seen::default()));
    let (token, address) = control_plane(pki, seen.clone()).await;
    let dir = tempfile::tempdir().unwrap();
    std::fs::write(dir.path().join("join-token"), format!("{token}\n")).unwrap();
    let fleet = node(&address, dir.path());
    wait_for(&seen, "a node connection", |seen| !seen.volumes.is_empty()).await;
    let started = Instant::now();
    fleet.stop(Duration::from_secs(20)).await;
    assert!(
        started.elapsed() < Duration::from_secs(5),
        "the stop took {:?}",
        started.elapsed()
    );
}

/// What the test's sink saw (ADR 0026).
#[derive(Default)]
struct SinkSeen {
    /// Where it is with each stream, (pod, container, log ID).
    positions: std::collections::HashMap<(String, String, String), u64>,
    /// Each export, with the certificate it came with.
    exports: Vec<(Vec<u8>, ExportLogsServiceRequest)>,
    ended: Vec<StreamKey>,
    /// Exports to refuse, UNAVAILABLE, before it takes one.
    refuse: usize,
}

#[derive(Clone)]
struct Sink(Arc<Mutex<SinkSeen>>);

fn peer_of<T>(request: &Request<T>) -> Result<Vec<u8>, Status> {
    request
        .extensions()
        .get::<Peer>()
        .and_then(|peer| peer.0.clone())
        .ok_or_else(|| Status::unauthenticated("no client certificate"))
}

#[tonic::async_trait]
impl SinkService for Sink {
    async fn positions(
        &self,
        request: Request<PositionsRequest>,
    ) -> Result<Response<PositionsResponse>, Status> {
        peer_of(&request)?;
        let seen = self.0.lock().unwrap();
        let next_offsets = request
            .into_inner()
            .streams
            .into_iter()
            .map(|key| {
                let key = (key.pod, key.container, key.log_id);
                seen.positions.get(&key).copied().unwrap_or(0)
            })
            .collect();
        Ok(Response::new(PositionsResponse { next_offsets }))
    }

    async fn end_streams(
        &self,
        request: Request<EndStreamsRequest>,
    ) -> Result<Response<EndStreamsResponse>, Status> {
        peer_of(&request)?;
        let mut seen = self.0.lock().unwrap();
        seen.ended.extend(request.into_inner().streams);
        Ok(Response::new(EndStreamsResponse {}))
    }
}

/// OTLP's LogsService of the test's sink, by hand: feos-proto generates
/// its client only.
#[derive(Clone)]
struct Otlp(Sink);

impl tonic::server::NamedService for Otlp {
    const NAME: &'static str = "opentelemetry.proto.collector.logs.v1.LogsService";
}

impl tonic::server::UnaryService<ExportLogsServiceRequest> for Otlp {
    type Response = ExportLogsServiceResponse;
    type Future =
        Pin<Box<dyn std::future::Future<Output = Result<Response<Self::Response>, Status>> + Send>>;

    fn call(&mut self, request: Request<ExportLogsServiceRequest>) -> Self::Future {
        let sink = self.0.clone();
        Box::pin(async move {
            let peer = peer_of(&request)?;
            let mut seen = sink.0.lock().unwrap();
            if seen.refuse > 0 {
                seen.refuse -= 1;
                return Err(Status::unavailable("the sink's database is away"));
            }
            seen.exports.push((peer, request.into_inner()));
            Ok(Response::new(ExportLogsServiceResponse::default()))
        })
    }
}

impl tonic::codegen::Service<tonic::codegen::http::Request<tonic::body::Body>> for Otlp {
    type Response = tonic::codegen::http::Response<tonic::body::Body>;
    type Error = std::convert::Infallible;
    type Future =
        Pin<Box<dyn std::future::Future<Output = Result<Self::Response, Self::Error>> + Send>>;

    fn poll_ready(&mut self, _: &mut Context<'_>) -> Poll<Result<(), Self::Error>> {
        Poll::Ready(Ok(()))
    }

    fn call(&mut self, request: tonic::codegen::http::Request<tonic::body::Body>) -> Self::Future {
        let otlp = self.clone();
        Box::pin(async move {
            if request.uri().path() != "/opentelemetry.proto.collector.logs.v1.LogsService/Export" {
                return Ok(Status::unimplemented("").into_http());
            }
            let mut grpc = tonic::server::Grpc::new(tonic::codec::ProstCodec::default());
            Ok(grpc.unary(otlp, request).await)
        })
    }
}

/// The test's sink, on a port of its own; returns its address.
async fn sink(pki: &Pki, seen: Arc<Mutex<SinkSeen>>) -> String {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap().to_string();
    let sink = Sink(seen);
    let (connections, incoming) = mpsc::channel::<Result<Uplink, std::io::Error>>(16);
    tokio::spawn(
        Server::builder()
            .add_service(SinkServiceServer::new(sink.clone()))
            .add_service(Otlp(sink))
            .serve_with_incoming(ReceiverStream::new(incoming)),
    );
    let acceptor = TlsAcceptor::from(pki.sink_config());
    tokio::spawn(async move {
        loop {
            let (tcp, _) = listener.accept().await.unwrap();
            let acceptor = acceptor.clone();
            let connections = connections.clone();
            tokio::spawn(async move {
                if let Ok(tls) = acceptor.accept(tcp).await {
                    connections.send(Ok(Uplink(tls))).await.ok();
                }
            });
        }
    });
    address
}

/// A guest's log of one container, in memory, that grows as the test says.
#[derive(Clone, Default)]
struct GuestLog {
    entries: Arc<Mutex<Vec<LogEntry>>>,
    grown: Arc<tokio::sync::Notify>,
}

impl GuestLog {
    fn append(&self, text: &str) {
        let mut entries = self.entries.lock().unwrap();
        let offset = entries.last().map_or(0, |e| e.next_offset);
        entries.push(LogEntry {
            line: text.as_bytes().to_vec(),
            source: Source::Stdout as i32,
            offset,
            next_offset: offset + 41 + text.len() as u64,
            time_unix_nano: 1_000 + offset as i64,
            partial: false,
            log_id: "log-1".into(),
        });
        self.grown.notify_waiters();
    }
}

struct Pods(GuestLog, Arc<std::sync::atomic::AtomicBool>);

impl Pods {
    /// The pod runs with `log`, until the flag says it no longer does.
    fn new(log: GuestLog) -> (Self, Arc<std::sync::atomic::AtomicBool>) {
        let running = Arc::new(std::sync::atomic::AtomicBool::new(true));
        (Self(log, running.clone()), running)
    }
}

impl Logs for Pods {
    fn pods(&self) -> Vec<ShippedPod> {
        if !self.1.load(std::sync::atomic::Ordering::Relaxed) {
            return Vec::new();
        }
        vec![ShippedPod {
            id: POD.into(),
            labels: [("ferun.tenant".to_string(), "acme".to_string())].into(),
            redact: vec!["hunter2".into()],
            containers: vec!["main".into()],
        }]
    }

    async fn logs(
        &self,
        pod_id: &str,
        request: StreamContainerLogsRequest,
    ) -> Result<LogStream, Status> {
        assert_eq!((pod_id, request.container_id.as_str()), (POD, "main"));
        let log = self.0.clone();
        if !request.follow {
            // The probe: the last entry.
            let last = log.entries.lock().unwrap().last().cloned();
            assert_eq!(request.tail_lines, 1);
            return Ok(Box::pin(tokio_stream::iter(last.map(Ok))));
        }
        assert_eq!(request.log_id, "log-1");
        let (tx, rx) = mpsc::channel(4);
        tokio::spawn(async move {
            let mut from = request.from_offset;
            loop {
                let grown = log.grown.notified();
                let next: Vec<LogEntry> = log
                    .entries
                    .lock()
                    .unwrap()
                    .iter()
                    .filter(|e| e.offset >= from)
                    .cloned()
                    .collect();
                for entry in next {
                    from = entry.next_offset;
                    if tx.send(Ok(entry)).await.is_err() {
                        return;
                    }
                }
                tokio::select! {
                    () = grown => {}
                    () = tx.closed() => return,
                }
            }
        });
        Ok(Box::pin(ReceiverStream::new(rx)))
    }
}

/// The offsets of the records the sink took, and the resource of the last.
fn exported(seen: &SinkSeen) -> Vec<i64> {
    seen.exports
        .iter()
        .flat_map(|(_, request)| &request.resource_logs)
        .flat_map(|logs| &logs.scope_logs)
        .flat_map(|scope| &scope.log_records)
        .filter_map(|record| {
            record.attributes.iter().find_map(|kv| match &kv.value {
                Some(AnyValue {
                    value: Some(any_value::Value::IntValue(offset)),
                }) if kv.key == "log.offset" => Some(*offset),
                _ => None,
            })
        })
        .collect()
}

async fn wait_for_sink(seen: &Mutex<SinkSeen>, what: &str, done: impl Fn(&SinkSeen) -> bool) {
    let deadline = Instant::now() + Duration::from_secs(30);
    while !done(&seen.lock().unwrap()) {
        assert!(Instant::now() < deadline, "timed out waiting for {what}");
        sleep(Duration::from_millis(50)).await;
    }
}

#[tokio::test]
async fn a_node_ships_its_pods_logs_to_the_sink_it_is_told_of() {
    let pki = Arc::new(Pki::new());
    let seen = Arc::new(Mutex::new(Seen::default()));
    let sink_seen = Arc::new(Mutex::new(SinkSeen::default()));
    let sink_address = sink(&pki, sink_seen.clone()).await;
    let (token, address) = control_plane(pki, seen.clone()).await;
    let dir = tempfile::tempdir().unwrap();
    std::fs::write(dir.path().join("join-token"), format!("{token}\n")).unwrap();
    let fleet = node(&address, dir.path());
    let log = GuestLog::default();
    for text in ["first", "second", "third hunter2"] {
        log.append(text);
    }
    let shipper = Shipper::start(Pods::new(log.clone()).0, fleet.sink());

    // Without a sink, nothing is shipped.
    wait_for(&seen, "a report", |seen| seen.reports.len() >= 2).await;
    assert!(fleet.sink().borrow().is_none());
    assert!(sink_seen.lock().unwrap().exports.is_empty());

    // The sink has the first line already; it refuses the first export.
    let second = log.entries.lock().unwrap()[1].offset;
    {
        let mut sink = sink_seen.lock().unwrap();
        let key = (POD.to_string(), "main".to_string(), "log-1".to_string());
        sink.positions.insert(key, second);
        sink.refuse = 1;
    }
    seen.lock().unwrap().sink_endpoint = sink_address.clone();
    wait_for_sink(&sink_seen, "the export", |seen| !seen.exports.is_empty()).await;
    let third = log.entries.lock().unwrap()[2].offset;
    {
        let sink = sink_seen.lock().unwrap();
        assert_eq!(sink.refuse, 0, "the refused export is sent again");
        assert_eq!(exported(&sink), [second as i64, third as i64]);
        let (cert, request) = &sink.exports[0];
        assert_eq!(
            uri_of(cert),
            [format!("spiffe://{TRUST_DOMAIN}/node/{NODE}")]
        );
        assert_eq!(request.resource_logs.len(), 1);
        let resource = request.resource_logs[0].resource.as_ref().unwrap();
        let attributes: Vec<(String, String)> = resource
            .attributes
            .iter()
            .map(|kv| match &kv.value {
                Some(AnyValue {
                    value: Some(any_value::Value::StringValue(value)),
                }) => (kv.key.clone(), value.clone()),
                other => panic!("{other:?}"),
            })
            .collect();
        let want: Vec<(String, String)> = [
            ("ferun.container", "main"),
            ("ferun.log_id", "log-1"),
            ("ferun.node", NODE),
            ("ferun.pod", POD),
            ("ferun.tenant", "acme"),
        ]
        .iter()
        .map(|(k, v)| (k.to_string(), v.to_string()))
        .collect();
        assert_eq!(attributes, want);
        let record = &request.resource_logs[0].scope_logs[0].log_records[0];
        assert_eq!(record.time_unix_nano, 1_000 + second);
        assert!(record.observed_time_unix_nano > 0);
        assert_eq!(
            record.body,
            Some(AnyValue {
                value: Some(any_value::Value::StringValue("second".into())),
            })
        );
        // A secret goes redacted.
        let record = &request.resource_logs[0].scope_logs[0].log_records[1];
        assert_eq!(
            record.body,
            Some(AnyValue {
                value: Some(any_value::Value::StringValue("third [redacted]".into())),
            })
        );
    }

    // A drain sends what the container wrote since, before it returns.
    log.append("fourth");
    let fourth = log.entries.lock().unwrap()[3].offset;
    tokio::time::timeout(Duration::from_secs(5), shipper.drain(POD))
        .await
        .expect("the drain did not end");
    assert_eq!(
        exported(&sink_seen.lock().unwrap()),
        [second as i64, third as i64, fourth as i64]
    );

    // A deleted pod's streams end.
    shipper.deleted(POD);
    wait_for_sink(&sink_seen, "the end of the stream", |seen| {
        !seen.ended.is_empty()
    })
    .await;
    assert_eq!(
        sink_seen.lock().unwrap().ended,
        [StreamKey {
            pod: POD.into(),
            container: "main".into(),
            log_id: "log-1".into(),
        }]
    );

    // Without a sink again, the node lets go of it.
    seen.lock().unwrap().sink_endpoint.clear();
    let mut sink = fleet.sink();
    tokio::time::timeout(Duration::from_secs(10), sink.wait_for(|t| t.is_none()))
        .await
        .expect("the sink stays")
        .unwrap();
}

/// A stream whose pod stops running (it is stopped or deleted) is let go
/// of: what it read and has not sent yet still goes, and the rest of the
/// log that the guest sent it.
#[tokio::test]
async fn a_stream_let_go_of_sends_what_it_read() {
    let pki = Arc::new(Pki::new());
    let seen = Arc::new(Mutex::new(Seen::default()));
    let sink_seen = Arc::new(Mutex::new(SinkSeen::default()));
    let sink_address = sink(&pki, sink_seen.clone()).await;
    let (token, address) = control_plane(pki, seen.clone()).await;
    let dir = tempfile::tempdir().unwrap();
    std::fs::write(dir.path().join("join-token"), format!("{token}\n")).unwrap();
    seen.lock().unwrap().sink_endpoint = sink_address;
    let fleet = node(&address, dir.path());
    let log = GuestLog::default();
    log.append("started");
    let (pods, running) = Pods::new(log.clone());
    let shipper = Shipper::start(pods, fleet.sink());
    wait_for_sink(&sink_seen, "the first line", |seen| {
        exported(seen).len() == 1
    })
    .await;

    // The container's last words, and the pod gone at once: they are read
    // into a batch that would wait 500 ms for more.
    log.append("shutting down");
    log.append("bye");
    sleep(Duration::from_millis(50)).await;
    running.store(false, std::sync::atomic::Ordering::Relaxed);
    shipper.changed();
    wait_for_sink(&sink_seen, "the last lines", |seen| {
        exported(seen).len() == 3
    })
    .await;
    let offsets: Vec<i64> = log
        .entries
        .lock()
        .unwrap()
        .iter()
        .map(|e| e.offset as i64)
        .collect();
    assert_eq!(exported(&sink_seen.lock().unwrap()), offsets);

    // Stopped, then deleted, as when an instance is replaced: its stream
    // ends at the sink.
    shipper.deleted(POD);
    wait_for_sink(&sink_seen, "the end of the stream", |seen| {
        !seen.ended.is_empty()
    })
    .await;
    assert_eq!(sink_seen.lock().unwrap().ended[0].log_id, "log-1");
}

/// A deleted pod's stream sends what it read before it is ended.
#[tokio::test]
async fn a_deleted_pods_stream_sends_what_it_read_then_ends() {
    let pki = Arc::new(Pki::new());
    let seen = Arc::new(Mutex::new(Seen::default()));
    let sink_seen = Arc::new(Mutex::new(SinkSeen::default()));
    let sink_address = sink(&pki, sink_seen.clone()).await;
    let (token, address) = control_plane(pki, seen.clone()).await;
    let dir = tempfile::tempdir().unwrap();
    std::fs::write(dir.path().join("join-token"), format!("{token}\n")).unwrap();
    seen.lock().unwrap().sink_endpoint = sink_address;
    let fleet = node(&address, dir.path());
    let log = GuestLog::default();
    log.append("started");
    let (pods, running) = Pods::new(log.clone());
    let shipper = Shipper::start(pods, fleet.sink());
    wait_for_sink(&sink_seen, "the first line", |seen| {
        exported(seen).len() == 1
    })
    .await;
    log.append("last");
    sleep(Duration::from_millis(50)).await;
    running.store(false, std::sync::atomic::Ordering::Relaxed);
    shipper.deleted(POD);
    wait_for_sink(&sink_seen, "the end of the stream", |seen| {
        !seen.ended.is_empty()
    })
    .await;
    let sink = sink_seen.lock().unwrap();
    assert_eq!(
        exported(&sink).len(),
        2,
        "the last line came before the end"
    );
    assert_eq!(sink.ended.len(), 1);
}
