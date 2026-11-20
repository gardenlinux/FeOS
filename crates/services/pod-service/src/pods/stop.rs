// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A planned stop of a feosd whose microVMs end with it, as in a pod on
//! Kubernetes (ADR 0020), and the start after it. The stop refuses every
//! call that changes a pod from then on, suspends every running pod into
//! its directory (ADR 0008), as SuspendPod does, a few at a time, and
//! records which, in a file that outlives feosd. The next feosd resumes
//! them, a few at a time, and removes the record.
//!
//! A pod whose VM is busy (`vmm::is_busy`), as while Firecracker still
//! writes a snapshot that a suspend gave up on, is tried again after the
//! first pass over all pods, until the stop's grace ends.
//!
//! A record that a feosd before this one left also names the pods it
//! suspended and this one has not resumed yet: a stop during the resumes
//! keeps them. A pod whose resume fails stays suspended, and is not
//! resumed again by the next start: the control plane sees it suspended.

use super::Pods;
use feos_proto::pod_service::PodState;
use futures::StreamExt;
use log::{error, info, warn};
use serde_json::{json, Value};
use std::future::Future;
use std::io::{self, Write};
use std::path::Path;
use std::sync::atomic::Ordering;
use std::time::{Duration, SystemTime, UNIX_EPOCH};
use tokio::sync::mpsc;
use tokio::time::{sleep, sleep_until, Instant};
use tonic::Status;

/// Where a stop records the pods it suspended: beside the pods' directory,
/// on the node's storage.
pub const STOP_RECORD: &str = "/var/lib/feos/suspended-for-stop.json";
/// How many pods a stop suspends at once, and a start resumes: a suspend
/// writes the guest's memory at the disk's speed, so more at once only
/// share the disk.
const PARALLEL: usize = 4;
/// How long a start waits for diskd to answer before it resumes: the pods'
/// disks are diskd's, and diskd may start beside feosd.
const DISKD_WAIT: Duration = Duration::from_secs(120);
/// How often a stop tries a pod whose VM is busy again.
const BUSY_RETRY: Duration = Duration::from_secs(5);

/// The pods a stop suspended, and when.
#[derive(Debug, Default, Clone, PartialEq, Eq)]
pub struct StopRecord {
    /// Seconds since 1970 on the node's clock.
    pub stopped_at: u64,
    pub pods: Vec<String>,
}

impl StopRecord {
    fn to_json(&self) -> String {
        json!({ "stopped_at": self.stopped_at, "pods": self.pods }).to_string()
    }

    fn parse(text: &str) -> Result<Self, String> {
        let value: Value = serde_json::from_str(text).map_err(|e| e.to_string())?;
        let stopped_at = value
            .get("stopped_at")
            .and_then(Value::as_u64)
            .ok_or("no stopped_at")?;
        let pods = value
            .get("pods")
            .and_then(Value::as_array)
            .ok_or("no pods")?
            .iter()
            .map(|pod| pod.as_str().map(str::to_string).ok_or("a pod is no string"))
            .collect::<Result<_, _>>()?;
        Ok(Self { stopped_at, pods })
    }

    /// Seconds since the stop; 0 when the clock went back since.
    fn age(&self, now: u64) -> u64 {
        now.saturating_sub(self.stopped_at)
    }
}

/// The record at `path`, if there is one.
pub fn read_record(path: &Path) -> io::Result<Option<StopRecord>> {
    match std::fs::read_to_string(path) {
        Ok(text) => StopRecord::parse(&text)
            .map(Some)
            .map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e)),
        Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(None),
        Err(e) => Err(e),
    }
}

/// Writes the record, synced and replaced in one step, or removes it when
/// it names no pod.
fn write_record(path: &Path, record: &StopRecord) -> io::Result<()> {
    if record.pods.is_empty() {
        return remove_record(path);
    }
    let temporary = path.with_extension("json.new");
    let mut file = std::fs::File::create(&temporary)?;
    file.write_all(record.to_json().as_bytes())?;
    file.sync_all()?;
    std::fs::rename(&temporary, path)?;
    if let Some(dir) = path.parent() {
        std::fs::File::open(dir)?.sync_all()?;
    }
    Ok(())
}

/// `write_record` on the blocking pool: its fsyncs may take seconds while
/// the disk writes the snapshots of the stop, and the other suspends go on
/// meanwhile.
async fn keep_record(path: &Path, record: &StopRecord) -> io::Result<()> {
    let (path, record) = (path.to_path_buf(), record.clone());
    tokio::task::spawn_blocking(move || write_record(&path, &record))
        .await
        .unwrap_or_else(|e| Err(io::Error::other(e)))
}

fn remove_record(path: &Path) -> io::Result<()> {
    match std::fs::remove_file(path) {
        Err(e) if e.kind() != io::ErrorKind::NotFound => Err(e),
        _ => Ok(()),
    }
}

fn now() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|since| since.as_secs())
        .unwrap_or_default()
}

/// What became of a pod that a stop suspends.
#[derive(Debug, PartialEq)]
enum Done {
    /// Suspended: by the stop, with what its memory takes on disk, or, found
    /// so once its VM was no longer busy, by its microVM's suspend that went
    /// through after all (`Pods::follow_vm`).
    Suspended(Option<u64>),
    /// It does not run.
    NotRunning,
}

/// Suspends each of `candidates` with `suspend`, `PARALLEL` at a time, and
/// sends what became of each to `done` as it is done. A pod whose VM is
/// busy (`vmm::is_busy`) is tried again with `again` after the first pass
/// over all of them, every `retry`, until `until`; then its last answer
/// goes to `done`, as every other's at once.
async fn suspend_each<S, SF, A, AF>(
    candidates: Vec<String>,
    suspend: S,
    again: A,
    until: Instant,
    retry: Duration,
    done: mpsc::UnboundedSender<(String, Result<Done, Status>)>,
) where
    S: Fn(String) -> SF,
    SF: Future<Output = (String, Result<Done, Status>)>,
    A: Fn(String) -> AF,
    AF: Future<Output = (String, Result<Done, Status>)>,
{
    let mut busy = Vec::new();
    let mut first = futures::stream::iter(candidates)
        .map(&suspend)
        .buffer_unordered(PARALLEL);
    while let Some((pod_id, result)) = first.next().await {
        match result {
            Err(e) if microvm_service::vmm::is_busy(&e) && Instant::now() < until => {
                info!(
                    "Pods: {pod_id} is busy, tried again after the others: {}",
                    e.message()
                );
                busy.push((pod_id, e));
            }
            result => {
                let _ = done.send((pod_id, result));
            }
        }
    }
    while !busy.is_empty() {
        let now = Instant::now();
        if now >= until {
            break;
        }
        sleep_until((now + retry).min(until)).await;
        let tried: Vec<String> = busy.drain(..).map(|(pod_id, _)| pod_id).collect();
        let mut tries = futures::stream::iter(tried)
            .map(&again)
            .buffer_unordered(PARALLEL);
        while let Some((pod_id, result)) = tries.next().await {
            match result {
                Err(e) if microvm_service::vmm::is_busy(&e) => busy.push((pod_id, e)),
                result => {
                    let _ = done.send((pod_id, result));
                }
            }
        }
    }
    for (pod_id, e) in busy {
        let _ = done.send((pod_id, Err(e)));
    }
}

/// What a stop did.
#[derive(Debug, Default, PartialEq)]
pub struct Stopped {
    /// The pods it suspended, and those a stop before suspended that are
    /// still suspended: the record's.
    pub recorded: Vec<String>,
    /// The pods whose suspend failed: their microVMs end with feosd.
    pub failed: Vec<String>,
}

impl Pods {
    /// Refuses every call that changes a pod from now on: feosd stops.
    pub fn begin_stop(&self) {
        self.inner.stopping.store(true, Ordering::SeqCst);
    }

    /// Whether feosd stops.
    pub fn stopping(&self) -> bool {
        self.inner.stopping.load(Ordering::SeqCst)
    }

    /// Fails while feosd stops: for the API's calls that change pods.
    pub(crate) fn check_serving(&self) -> Result<(), Status> {
        if self.stopping() {
            return Err(Status::unavailable(
                "feosd stops: it suspends the node's pods",
            ));
        }
        Ok(())
    }

    /// Suspends every running pod for a stop of feosd, `PARALLEL` at a
    /// time, and records them in `record` as each is done: a stop cut off
    /// by a kill keeps what it suspended. A pod in a hibernation is left to
    /// it, which the stop waits for. A pod whose stop or delete is in
    /// flight is left to it, which the stop does not wait for: its
    /// containers may have a long grace, and its microVM ends with feosd.
    /// A pod whose VM is busy is tried again after the others, until
    /// `until`, the end of the stop's grace less what it needs to exit;
    /// one that is then suspended, by the stop or as its microVM's suspend
    /// went through after all, is recorded too. One whose suspend fails
    /// ends with feosd.
    pub async fn suspend_for_stop(&self, record: &Path, until: Instant) -> Stopped {
        self.begin_stop();
        let _record = self.inner.stop_record.lock().await;
        let mut stopped = Stopped {
            recorded: self.still_suspended(record),
            ..Default::default()
        };
        let mut entry = StopRecord {
            stopped_at: now(),
            pods: stopped.recorded.clone(),
        };
        if let Err(e) = keep_record(record, &entry).await {
            error!(
                "Pods: Failed to record the suspended pods in {}: {e}",
                record.display()
            );
        }
        let running: Vec<String> = self
            .pods()
            .iter()
            .filter(|(_, pod)| {
                pod.info.state() == PodState::Running
                    && pod.hibernation.is_none()
                    && pod.ending == 0
            })
            .map(|(pod_id, _)| pod_id.clone())
            .collect();
        info!(
            "Pods: feosd stops: suspending {} running pods, {PARALLEL} at a time",
            running.len()
        );
        // The recorded ones too, as a resume of one may be under way, and
        // last the hibernating ones, whose hibernations hold their locks:
        // the stop waits for them, as long as its grace lasts.
        let hibernating: Vec<String> = self
            .pods()
            .iter()
            .filter(|(_, pod)| pod.hibernation.is_some())
            .map(|(pod_id, _)| pod_id.clone())
            .collect();
        let mut candidates = running;
        for pod_id in entry.pods.iter().chain(&hibernating) {
            if !candidates.contains(pod_id) {
                candidates.push(pod_id.clone());
            }
        }
        let started = Instant::now();
        let (done, mut outcomes) = mpsc::unbounded_channel();
        let suspending = suspend_each(
            candidates,
            |pod_id| async move {
                let suspended = self.suspend_if_running(&pod_id).await;
                (pod_id, suspended)
            },
            |pod_id| async move {
                let suspended = self.suspend_again(&pod_id).await;
                (pod_id, suspended)
            },
            until,
            BUSY_RETRY,
            done,
        );
        let recording = async {
            while let Some((pod_id, suspended)) = outcomes.recv().await {
                match suspended {
                    Ok(Done::Suspended(bytes)) => {
                        match bytes {
                            Some(bytes) => info!(
                                "Pods: {pod_id} is suspended for the stop, {} MiB of memory",
                                bytes >> 20
                            ),
                            None => info!(
                                "Pods: {pod_id} is suspended for the stop: its microVM's \
                                 suspend went through after all"
                            ),
                        }
                        if !entry.pods.contains(&pod_id) {
                            entry.pods.push(pod_id);
                        }
                        if let Err(e) = keep_record(record, &entry).await {
                            error!(
                                "Pods: Failed to record the suspended pods in {}: {e}",
                                record.display()
                            );
                        }
                    }
                    Ok(Done::NotRunning) => {}
                    Err(e) => {
                        error!(
                            "Pods: {pod_id} is not suspended for the stop, its microVM ends with \
                             feosd: {}",
                            e.message()
                        );
                        if entry.pods.contains(&pod_id) {
                            entry.pods.retain(|recorded| *recorded != pod_id);
                            if let Err(e) = keep_record(record, &entry).await {
                                error!(
                                    "Pods: Failed to record the suspended pods in {}: {e}",
                                    record.display()
                                );
                            }
                        }
                        stopped.failed.push(pod_id);
                    }
                }
            }
        };
        tokio::join!(suspending, recording);
        info!(
            "Pods: feosd stops: {} pods recorded as suspended, after {}s; {} failed",
            entry.pods.len(),
            started.elapsed().as_secs(),
            stopped.failed.len()
        );
        stopped.recorded = entry.pods;
        stopped
    }

    /// The pods of the record of a stop before that are still suspended.
    fn still_suspended(&self, record: &Path) -> Vec<String> {
        match read_record(record) {
            Ok(Some(old)) => old
                .pods
                .into_iter()
                .filter(|pod_id| {
                    self.info(pod_id)
                        .is_ok_and(|info| info.state() == PodState::Suspended)
                })
                .collect(),
            Ok(None) => Vec::new(),
            Err(e) => {
                warn!(
                    "Pods: The record of a stop before, {}: {e}",
                    record.display()
                );
                Vec::new()
            }
        }
    }

    /// Suspends the pod if it still runs.
    async fn suspend_if_running(&self, pod_id: &str) -> Result<Done, Status> {
        let _lock = self.lock(pod_id).await?;
        if self.info(pod_id)?.state() != PodState::Running {
            return Ok(Done::NotRunning);
        }
        self.suspend_locked(pod_id)
            .await
            .map(|bytes| Done::Suspended(Some(bytes)))
    }

    /// Suspends the pod whose VM was busy, if it still runs; one that is
    /// suspended now, as its microVM's suspend went through after all
    /// (`Pods::follow_vm`), counts as suspended for the stop: the next feosd
    /// resumes it, and finds it as the stop found it.
    async fn suspend_again(&self, pod_id: &str) -> Result<Done, Status> {
        let _lock = self.lock(pod_id).await?;
        match self.info(pod_id)?.state() {
            PodState::Running => self
                .suspend_locked(pod_id)
                .await
                .map(|bytes| Done::Suspended(Some(bytes))),
            PodState::Suspended => Ok(Done::Suspended(None)),
            _ => Ok(Done::NotRunning),
        }
    }

    /// Resumes the pods that a stop of feosd suspended, as `record` names
    /// them, `PARALLEL` at a time, once diskd answers, and removes the
    /// record. With `within`, the pods of a stop longer ago than that stay
    /// suspended: the control plane may have given them up and brought
    /// their sandboxes back elsewhere, and sees them suspended. Returns the
    /// pods it resumed.
    pub async fn resume_after_stop(&self, record: &Path, within: Option<Duration>) -> Vec<String> {
        let entry = match read_record(record) {
            Ok(Some(entry)) => entry,
            Ok(None) => return Vec::new(),
            Err(e) => {
                error!(
                    "Pods: The record of the pods a stop suspended, {}, does not read: {e}; \
                     they stay suspended",
                    record.display()
                );
                self.forget_record(record).await;
                return Vec::new();
            }
        };
        let age = entry.age(now());
        if let Some(within) = within
            && age > within.as_secs()
        {
            warn!(
                "Pods: feosd stopped {age}s ago, more than {}s: the {} pods it suspended stay \
                 suspended, for the control plane",
                within.as_secs(),
                entry.pods.len()
            );
            self.forget_record(record).await;
            return Vec::new();
        }
        info!(
            "Pods: Resuming the {} pods that feosd suspended when it stopped {age}s ago",
            entry.pods.len()
        );
        self.wait_for_diskd().await;
        let started = Instant::now();
        let resumed: Vec<String> = futures::stream::iter(entry.pods)
            .map(|pod_id| async move {
                let resumed = self.resume_if_suspended(&pod_id).await;
                (pod_id, resumed)
            })
            .buffer_unordered(PARALLEL)
            .filter_map(|(pod_id, resumed)| async move {
                match resumed {
                    Ok(true) => Some(pod_id),
                    Ok(false) => None,
                    Err(e) => {
                        error!("Pods: {pod_id} stays suspended: {}", e.message());
                        None
                    }
                }
            })
            .collect()
            .await;
        info!(
            "Pods: Resumed {} pods after the stop, in {}s",
            resumed.len(),
            started.elapsed().as_secs()
        );
        self.forget_record(record).await;
        resumed
    }

    /// Resumes the pod if it is still suspended and feosd does not stop;
    /// false if not.
    async fn resume_if_suspended(&self, pod_id: &str) -> Result<bool, Status> {
        if self.stopping() {
            return Ok(false);
        }
        match self.info(pod_id) {
            Ok(info) if info.state() == PodState::Suspended => {}
            Ok(info) => {
                info!(
                    "Pods: {pod_id} is {} and not resumed after the stop",
                    info.state().as_str_name()
                );
                return Ok(false);
            }
            // Gone since.
            Err(_) => return Ok(false),
        }
        self.resume(pod_id).await.map(|()| true)
    }

    /// Removes the record, unless a stop has begun: it records anew.
    async fn forget_record(&self, record: &Path) {
        let _record = self.inner.stop_record.lock().await;
        if self.stopping() {
            return;
        }
        let removed = record.to_path_buf();
        let removed = tokio::task::spawn_blocking(move || remove_record(&removed))
            .await
            .unwrap_or_else(|e| Err(io::Error::other(e)));
        if let Err(e) = removed {
            error!("Pods: Failed to remove {}: {e}", record.display());
        }
    }

    /// Waits until diskd answers, at most `DISKD_WAIT`.
    async fn wait_for_diskd(&self) {
        let deadline = Instant::now() + DISKD_WAIT;
        let mut said = false;
        loop {
            match self.inner.disks.node_stats().await {
                Ok(_) => return,
                Err(e) if Instant::now() >= deadline => {
                    warn!(
                        "Pods: diskd does not answer, resuming all the same: {}",
                        e.message()
                    );
                    return;
                }
                Err(e) => {
                    if !said {
                        info!("Pods: Waiting for diskd to resume pods: {}", e.message());
                        said = true;
                    }
                    sleep(Duration::from_secs(1)).await;
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_record_reads_back() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("suspended-for-stop.json");
        assert_eq!(read_record(&path).unwrap(), None);
        let record = StopRecord {
            stopped_at: 1_790_000_000,
            pods: vec![
                "0b6f2a43-5a1e-4b8e-9d55-0f7b0d3e4a11".into(),
                "7c1d9e2f-3b4a-4c5d-8e6f-a0b1c2d3e4f5".into(),
            ],
        };
        write_record(&path, &record).unwrap();
        assert_eq!(read_record(&path).unwrap(), Some(record));
        assert!(!path.with_extension("json.new").exists());
        // A record without pods is none.
        write_record(
            &path,
            &StopRecord {
                stopped_at: 1,
                pods: Vec::new(),
            },
        )
        .unwrap();
        assert!(!path.exists());
        remove_record(&path).unwrap();
    }

    #[test]
    fn a_broken_record_is_an_error() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("suspended-for-stop.json");
        for text in [
            "",
            "{}",
            r#"{"stopped_at": 1}"#,
            r#"{"stopped_at": -1, "pods": []}"#,
            r#"{"stopped_at": 1, "pods": [1]}"#,
        ] {
            std::fs::write(&path, text).unwrap();
            assert!(read_record(&path).is_err(), "{text:?}");
        }
    }

    #[test]
    fn the_age_of_a_record_is_never_negative() {
        let record = StopRecord {
            stopped_at: 100,
            pods: Vec::new(),
        };
        assert_eq!(record.age(160), 60);
        assert_eq!(record.age(40), 0);
    }

    /// The record is written on the blocking pool: on a runtime of one
    /// thread, a write that hangs left the other suspends of the stop
    /// nothing to run on.
    #[tokio::test(flavor = "current_thread")]
    async fn a_record_that_hangs_leaves_the_runtime_running() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("suspended-for-stop.json");
        let hung = crate::hung::Hung::at(path.with_extension("json.new"));
        let (moved, watched) = std::sync::mpsc::channel::<()>();
        let watchdog = {
            let hung = hung.0.clone();
            std::thread::spawn(move || {
                let stalled = watched.recv_timeout(Duration::from_secs(3)).is_err();
                crate::hung::Hung(hung).open();
                stalled
            })
        };
        let record = StopRecord {
            stopped_at: 1,
            pods: vec!["p".to_string()],
        };
        let kept = tokio::spawn({
            let path = path.clone();
            async move { keep_record(&path, &record).await }
        });
        sleep(Duration::from_millis(100)).await;
        let _ = moved.send(());
        assert!(!watchdog.join().unwrap(), "the write held the runtime");
        assert!(kept.await.unwrap().is_err(), "a FIFO does not sync");
    }

    type Outcomes = Vec<(String, Result<Done, Status>)>;

    /// Runs `suspend_each` over `candidates` and collects what it sends.
    async fn each<S, SF, A, AF>(
        candidates: &[&str],
        suspend: S,
        again: A,
        until: Instant,
    ) -> Outcomes
    where
        S: Fn(String) -> SF,
        SF: Future<Output = (String, Result<Done, Status>)>,
        A: Fn(String) -> AF,
        AF: Future<Output = (String, Result<Done, Status>)>,
    {
        let (done, mut outcomes) = mpsc::unbounded_channel();
        let candidates = candidates.iter().map(|pod| pod.to_string()).collect();
        suspend_each(
            candidates,
            suspend,
            again,
            until,
            Duration::from_millis(20),
            done,
        )
        .await;
        let mut got = Vec::new();
        while let Ok(outcome) = outcomes.try_recv() {
            got.push(outcome);
        }
        got
    }

    fn outcome<'a>(got: &'a Outcomes, pod: &str) -> &'a Result<Done, Status> {
        &got.iter()
            .find(|(pod_id, _)| pod_id == pod)
            .unwrap_or_else(|| panic!("no outcome of {pod}: {got:?}"))
            .1
    }

    /// A planned stop gave up at once on a pod whose snapshot was late, and
    /// its microVM ended with feosd. A busy pod is now tried again, after
    /// the first pass over all pods, so that it holds no slot meanwhile,
    /// until it is suspended.
    #[tokio::test]
    async fn a_busy_pod_is_tried_again_after_the_others() {
        let log = std::sync::Mutex::new(Vec::<String>::new());
        let tries = std::sync::atomic::AtomicUsize::new(0);
        let got = each(
            &["busy", "a", "b", "c", "d", "e"],
            |pod_id| {
                let log = &log;
                async move {
                    log.lock().unwrap().push(pod_id.clone());
                    if pod_id == "busy" {
                        let busy = microvm_service::vmm::busy_status("Firecracker still writes");
                        return (pod_id, Err(busy));
                    }
                    sleep(Duration::from_millis(50)).await;
                    (pod_id, Ok(Done::Suspended(Some(1 << 30))))
                }
            },
            |pod_id| {
                let (log, tries) = (&log, &tries);
                async move {
                    log.lock().unwrap().push(format!("again {pod_id}"));
                    if tries.fetch_add(1, std::sync::atomic::Ordering::SeqCst) == 0 {
                        let busy = microvm_service::vmm::busy_status("Firecracker still writes");
                        return (pod_id, Err(busy));
                    }
                    (pod_id, Ok(Done::Suspended(None)))
                }
            },
            Instant::now() + Duration::from_secs(10),
        )
        .await;
        assert_eq!(got.len(), 6, "{got:?}");
        assert_eq!(
            outcome(&got, "busy").as_ref().ok(),
            Some(&Done::Suspended(None))
        );
        for pod in ["a", "b", "c", "d", "e"] {
            assert_eq!(
                outcome(&got, pod).as_ref().ok(),
                Some(&Done::Suspended(Some(1 << 30)))
            );
        }
        let log = log.into_inner().unwrap();
        assert_eq!(tries.into_inner(), 2, "{log:?}");
        let last_first = log.iter().position(|entry| entry == "e").unwrap();
        let first_again = log.iter().position(|entry| entry == "again busy").unwrap();
        assert!(
            last_first < first_again,
            "tried again in the first pass: {log:?}"
        );
        // The busy one's outcome came last, after the others'.
        assert_eq!(got.last().unwrap().0, "busy");
    }

    /// A pod that stays busy is given up on when the grace ends, with its
    /// busy answer; another UNAVAILABLE, even one that says the same, is
    /// not tried again.
    #[tokio::test]
    async fn a_busy_pod_is_given_up_on_when_the_grace_ends() {
        let tries = std::sync::atomic::AtomicUsize::new(0);
        let started = Instant::now();
        let got = each(
            &["busy", "other"],
            |pod_id| async move {
                let error = match pod_id.as_str() {
                    "busy" => microvm_service::vmm::busy_status("Firecracker still writes"),
                    _ => Status::unavailable("the VM is busy: or so it says"),
                };
                (pod_id, Err(error))
            },
            |pod_id| {
                let tries = &tries;
                async move {
                    tries.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
                    assert_eq!(pod_id, "busy", "tried again");
                    let busy = microvm_service::vmm::busy_status("Firecracker still writes");
                    (pod_id, Err(busy))
                }
            },
            Instant::now() + Duration::from_millis(150),
        )
        .await;
        let elapsed = started.elapsed();
        assert!(
            elapsed >= Duration::from_millis(150) && elapsed < Duration::from_secs(2),
            "{elapsed:?}"
        );
        assert!(tries.into_inner() >= 2);
        let busy = outcome(&got, "busy").as_ref().unwrap_err();
        assert!(microvm_service::vmm::is_busy(busy), "{busy:?}");
        let other = outcome(&got, "other").as_ref().unwrap_err();
        assert!(!microvm_service::vmm::is_busy(other), "{other:?}");
        assert_eq!(got.len(), 2);
    }
}
