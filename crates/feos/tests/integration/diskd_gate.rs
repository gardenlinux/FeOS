// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A gate between the tests' feosd and diskd: feosd reaches diskd through
//! it, on a socket of the tests' own, and it holds the calls a test names,
//! or diskd's answers to them, so that the test can act while a
//! hibernation is at a step of its choosing. Every other call it forwards
//! as it is. A call that feosd drops drops the gate's own call to diskd,
//! whose handler stops as when feosd calls diskd directly.

use feos_proto::disk_service::disk_service_client::DiskServiceClient;
use feos_proto::disk_service::disk_service_server::{DiskService, DiskServiceServer};
use feos_proto::disk_service::{
    BakeRequest, CloseMemoryRequest, CreateDiskRequest, Device, Empty, EvictRequest, EvictStats,
    FsckReport, FsckRequest, GetHeadRequest, GetObjectRequest, Head, ImportBaseRequest, Layer,
    LayerRef, LiveLayersRequest, LiveLayersResponse, MaterializeRequest, Memory, OpenDiskRequest,
    OpenMemoryRequest, PutHeadRequest, PutObjectRequest, RemoveDeviceRequest, RemoveDeviceResponse,
    SealFileRequest, SealRequest, StatsRequest, StatsResponse, StopDiskRequest, SweepReport,
    SweepRequest,
};
use hyper_util::rt::TokioIo;
use pod_service::disk::DISKD_SOCKET;
use std::future::Future;
use std::path::Path;
use std::sync::{Arc, Mutex, PoisonError};
use tokio::net::{UnixListener, UnixStream};
use tokio::sync::watch;
use tokio_stream::wrappers::UnixListenerStream;
use tonic::transport::{Channel, Endpoint, Server, Uri};
use tonic::{Request, Response, Status};
use tower::service_fn;

static GATE: DiskdGate = DiskdGate {
    rules: Mutex::new(Vec::new()),
};

/// The gate of the tests' feosd.
pub fn diskd_gate() -> &'static DiskdGate {
    &GATE
}

/// A call of feosd's to diskd, as a test's rule sees it.
pub enum GateCall<'a> {
    Seal(&'a SealRequest),
    SealFile(&'a SealFileRequest),
    PutHead(&'a PutHeadRequest),
    /// Any other, by its method's name.
    Other(&'static str),
}

impl GateCall<'_> {
    /// Its method's name.
    fn method(&self) -> &'static str {
        match self {
            GateCall::Seal(_) => "Seal",
            GateCall::SealFile(_) => "SealFile",
            GateCall::PutHead(_) => "PutHead",
            GateCall::Other(method) => method,
        }
    }
}

pub struct DiskdGate {
    rules: Mutex<Vec<Rule>>,
}

/// What a test asked the gate to do with the first call that matches.
struct Rule {
    matches: Box<dyn Fn(&GateCall) -> bool + Send + Sync>,
    mode: Mode,
    passage: Arc<Passage>,
}

/// How the gate takes a call that a rule took.
#[derive(Clone, Copy, PartialEq)]
enum Mode {
    /// Holds it before diskd, until the test releases it.
    Hold,
    /// Forwards it at once, and answers feosd as diskd answers.
    Pass,
    /// Forwards it at once, and holds diskd's answer until the test
    /// releases it.
    HoldAnswer,
}

/// What became of a call that a rule took.
struct Passage {
    arrived: watch::Sender<bool>,
    released: watch::Sender<bool>,
    /// Whether diskd answered the forwarded call.
    answered: watch::Sender<bool>,
    /// Whether feosd dropped the call before the gate answered it; unset
    /// while it is under way.
    ended: watch::Sender<Option<bool>>,
}

/// A call that a test named, held or forwarded. Dropping it releases the
/// call, so that a test that fails leaves no hibernation hanging, and
/// takes its rule out of the gate if no call came for it.
pub struct Held(Arc<Passage>);

impl DiskdGate {
    /// Holds the first call that `matches` before it reaches diskd, until
    /// the test releases it.
    pub fn hold(&self, matches: impl Fn(&GateCall) -> bool + Send + Sync + 'static) -> Held {
        self.add(matches, Mode::Hold)
    }

    /// Forwards the first call that `matches` at once and reports it: if
    /// feosd drops it, the forwarded call is dropped with it, and diskd's
    /// handler where it is.
    pub fn pass(&self, matches: impl Fn(&GateCall) -> bool + Send + Sync + 'static) -> Held {
        self.add(matches, Mode::Pass)
    }

    /// Forwards the first call that `matches` at once, and holds diskd's
    /// answer until the test releases it: feosd is in the call until then,
    /// whether or not diskd ended it. If feosd drops it, the forwarded call
    /// is dropped with it, unless diskd answered.
    pub fn pass_holding_answer(
        &self,
        matches: impl Fn(&GateCall) -> bool + Send + Sync + 'static,
    ) -> Held {
        self.add(matches, Mode::HoldAnswer)
    }

    fn add(&self, matches: impl Fn(&GateCall) -> bool + Send + Sync + 'static, mode: Mode) -> Held {
        let passage = Arc::new(Passage {
            arrived: watch::channel(false).0,
            released: watch::channel(false).0,
            answered: watch::channel(false).0,
            ended: watch::channel(None).0,
        });
        self.rules
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .push(Rule {
                matches: Box::new(matches),
                mode,
                passage: passage.clone(),
            });
        Held(passage)
    }

    /// Takes the first rule that matches `call`.
    fn take(&self, call: &GateCall) -> Option<(Mode, Arc<Passage>)> {
        let mut rules = self.rules.lock().unwrap_or_else(PoisonError::into_inner);
        let index = rules.iter().position(|rule| (rule.matches)(call))?;
        let rule = rules.remove(index);
        Some((rule.mode, rule.passage))
    }
}

impl Held {
    /// Returns once the call came: it is held, or forwarded.
    pub async fn arrived(&self) {
        let _ = self
            .0
            .arrived
            .subscribe()
            .wait_for(|arrived| *arrived)
            .await;
    }

    /// Forwards a held call, or diskd's held answer.
    pub fn release(&self) {
        self.0.released.send_replace(true);
    }

    /// Whether diskd answered the forwarded call, so far.
    pub fn answered(&self) -> bool {
        *self.0.answered.borrow()
    }

    /// Whether feosd dropped the call before the gate answered it, as
    /// diskd did or, with its answer held, once released: returns once one
    /// of the two happened.
    pub async fn dropped(&self) -> bool {
        let mut ended = self.0.ended.subscribe();
        let ended = ended.wait_for(Option::is_some).await.map(|ended| *ended);
        ended.ok().flatten().unwrap_or(false)
    }
}

impl Drop for Held {
    fn drop(&mut self) {
        self.release();
        GATE.rules
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .retain(|rule| !Arc::ptr_eq(&rule.passage, &self.0));
    }
}

/// Marks a call that a rule took as dropped, unless the gate answered it.
struct Ending {
    passage: Arc<Passage>,
    answered: bool,
}

impl Drop for Ending {
    fn drop(&mut self) {
        self.passage.ended.send_replace(Some(!self.answered));
    }
}

/// Takes a call through the gate: forwards it, and holds it or diskd's
/// answer to it if its rule says so.
async fn through<T>(
    rule: Option<(Mode, Arc<Passage>)>,
    forward: impl Future<Output = Result<Response<T>, Status>>,
) -> Result<Response<T>, Status> {
    let Some((mode, passage)) = rule else {
        return forward
            .await
            .map(|response| Response::new(response.into_inner()));
    };
    let mut ending = Ending {
        passage: passage.clone(),
        answered: false,
    };
    passage.arrived.send_replace(true);
    if mode == Mode::Hold {
        released(&passage).await;
    }
    let answer = forward.await;
    passage.answered.send_replace(true);
    if mode == Mode::HoldAnswer {
        released(&passage).await;
    }
    ending.answered = true;
    answer.map(|response| Response::new(response.into_inner()))
}

/// Returns once the test released the call.
async fn released(passage: &Passage) {
    let _ = passage
        .released
        .subscribe()
        .wait_for(|released| *released)
        .await;
}

/// The gate's service: diskd's API, forwarded to diskd.
struct Gate {
    diskd: DiskServiceClient<Channel>,
}

/// Each of diskd's calls, all unary, through the gate.
macro_rules! forward {
    ($($method:ident($request:ty) -> $response:ty, $call:expr;)*) => {
        #[tonic::async_trait]
        impl DiskService for Gate {
            $(
                async fn $method(
                    &self,
                    request: Request<$request>,
                ) -> Result<Response<$response>, Status> {
                    let request = request.into_inner();
                    let call = ($call)(&request);
                    let rule = GATE.take(&call);
                    if let Some((mode, _)) = &rule {
                        let what = match mode {
                            Mode::Hold => "holds",
                            Mode::Pass => "passes",
                            Mode::HoldAnswer => "passes, holding the answer to,",
                        };
                        log::info!("The diskd gate {what} a call of {}", call.method());
                    }
                    let mut diskd = self.diskd.clone();
                    through(rule, async move { diskd.$method(request).await }).await
                }
            )*
        }
    };
}

forward! {
    import_base(ImportBaseRequest) -> Layer, |_| GateCall::Other("ImportBase");
    create_disk(CreateDiskRequest) -> Device, |_| GateCall::Other("CreateDisk");
    open_disk(OpenDiskRequest) -> Device, |_| GateCall::Other("OpenDisk");
    stop_disk(StopDiskRequest) -> Empty, |_| GateCall::Other("StopDisk");
    seal(SealRequest) -> Layer, GateCall::Seal;
    bake(BakeRequest) -> Layer, |_| GateCall::Other("Bake");
    seal_file(SealFileRequest) -> Layer, GateCall::SealFile;
    remove_device(RemoveDeviceRequest) -> RemoveDeviceResponse, |_| GateCall::Other("RemoveDevice");
    materialize(MaterializeRequest) -> Empty, |_| GateCall::Other("Materialize");
    push(LayerRef) -> Empty, |_| GateCall::Other("Push");
    pull(LayerRef) -> Empty, |_| GateCall::Other("Pull");
    evict(EvictRequest) -> EvictStats, |_| GateCall::Other("Evict");
    stats(StatsRequest) -> StatsResponse, |_| GateCall::Other("Stats");
    fsck(FsckRequest) -> FsckReport, |_| GateCall::Other("Fsck");
    sweep(SweepRequest) -> SweepReport, |_| GateCall::Other("Sweep");
    live_layers(LiveLayersRequest) -> LiveLayersResponse, |_| GateCall::Other("LiveLayers");
    put_object(PutObjectRequest) -> Empty, |_| GateCall::Other("PutObject");
    get_object(GetObjectRequest) -> Empty, |_| GateCall::Other("GetObject");
    put_head(PutHeadRequest) -> Head, GateCall::PutHead;
    get_head(GetHeadRequest) -> Head, |_| GateCall::Other("GetHead");
    open_memory(OpenMemoryRequest) -> Memory, |_| GateCall::Other("OpenMemory");
    close_memory(CloseMemoryRequest) -> Empty, |_| GateCall::Other("CloseMemory");
}

/// Serves the gate on `socket`, in front of diskd's, on the runtime it is
/// called in.
pub fn serve(socket: &Path) -> std::io::Result<()> {
    let listener = UnixListener::bind(socket)?;
    let diskd = Endpoint::from_static("http://diskd").connect_with_connector_lazy(service_fn(
        |_: Uri| async { UnixStream::connect(DISKD_SOCKET).await.map(TokioIo::new) },
    ));
    let gate = Gate {
        diskd: DiskServiceClient::new(diskd),
    };
    tokio::spawn(async move {
        let served = Server::builder()
            .add_service(DiskServiceServer::new(gate))
            .serve_with_incoming(UnixListenerStream::new(listener))
            .await;
        if let Err(e) = served {
            log::error!("The diskd gate ended: {e}");
        }
    });
    Ok(())
}
