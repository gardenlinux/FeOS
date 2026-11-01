// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The wall clock as the host sets and corrects it (ADR 0027). A guest has no
//! NTP and does not read its RTC. Its host sets the time after every boot and
//! every resume. While the guest is attached, the host also measures the clock
//! once a minute and sends the offset to `AdjustTime`. A small offset is
//! slewed, and the clock's frequency learns from it, so that kvm-clock's
//! drift goes away.

use crate::error::HostError;
use feos_proto::host_service::{
    AdjustTimeRequest, AdjustTimeResponse, GetTimeResponse, SetTimeRequest, SetTimeResponse,
};
use log::{error, info};
use nix::sys::time::TimeSpec;
use nix::time::{clock_gettime, clock_settime, ClockId};
use prost_types::Timestamp;
use std::sync::{Mutex, MutexGuard};
use std::time::{Duration, Instant};
use tokio::sync::oneshot;

/// An offset larger than this steps the clock instead of slewing it.
const STEP_THRESHOLD_NS: i64 = 100_000_000;
/// An offset tells the frequency only if at least this much time has passed
/// since the last adjustment; before that, the measurement's noise outweighs
/// the drift.
const MIN_INTERVAL: Duration = Duration::from_secs(16);
/// The kernel's bound on the frequency correction, ±500 ppm, in its unit of
/// 2^-16 ppm.
const MAX_FREQUENCY: i64 = 500 << 16;

/// When the clock was last adjusted. `None` after a boot and after
/// `SetTime`: no drift can be measured from those yet. It lives in memory, so
/// a snapshot keeps it.
static LAST_ADJUSTMENT: Mutex<Option<Instant>> = Mutex::new(None);

fn last_adjustment() -> MutexGuard<'static, Option<Instant>> {
    LAST_ADJUSTMENT.lock().unwrap_or_else(|e| e.into_inner())
}

pub async fn handle_set_time(
    req: SetTimeRequest,
    responder: oneshot::Sender<Result<SetTimeResponse, HostError>>,
) {
    let result = set_time(req.time);
    if result.is_ok() {
        info!("HostWorker: Clock set by host.");
    }
    if responder.send(result).is_err() {
        error!("HostWorker: Failed to send response for SetTime.");
    }
}

pub async fn handle_get_time(responder: oneshot::Sender<Result<GetTimeResponse, HostError>>) {
    let result = clock_gettime(ClockId::CLOCK_REALTIME)
        .map(|now| GetTimeResponse {
            time: Some(Timestamp {
                seconds: now.tv_sec(),
                nanos: now.tv_nsec() as i32,
            }),
        })
        .map_err(|e| HostError::Failed(format!("clock_gettime: {e}")));
    if responder.send(result).is_err() {
        error!("HostWorker: Failed to send response for GetTime.");
    }
}

pub async fn handle_adjust_time(
    req: AdjustTimeRequest,
    responder: oneshot::Sender<Result<AdjustTimeResponse, HostError>>,
) {
    let result = adjust_time(req.offset_ns);
    if let Ok(response) = &result
        && response.stepped
    {
        info!(
            "HostWorker: Clock stepped by host, it was {} ms off.",
            req.offset_ns / 1_000_000
        );
    }
    if responder.send(result).is_err() {
        error!("HostWorker: Failed to send response for AdjustTime.");
    }
}

fn set_time(time: Option<Timestamp>) -> Result<SetTimeResponse, HostError> {
    let time = time.ok_or_else(|| HostError::InvalidArgument("missing time".to_string()))?;
    if time.seconds < 0 || !(0..1_000_000_000).contains(&time.nanos) {
        return Err(HostError::InvalidArgument(format!(
            "invalid time: seconds={} nanos={}",
            time.seconds, time.nanos
        )));
    }
    let mut last = last_adjustment();
    let spec = TimeSpec::new(time.seconds, i64::from(time.nanos));
    clock_settime(ClockId::CLOCK_REALTIME, spec)
        .map_err(|e| HostError::Failed(format!("clock_settime: {e}")))?;
    // A slew of the clock before is wrong now.
    slew(0)?;
    *last = None;
    Ok(SetTimeResponse {})
}

/// What to do about an offset.
#[derive(Debug, PartialEq)]
enum Correction {
    Step,
    /// Slew the offset away and, if given, set the frequency to this.
    Slew {
        frequency: Option<i64>,
    },
}

/// Decides on an offset of `offset_ns` (the clock ahead of the host's).
/// `pending_us` is what the kernel has not yet slewed of the last
/// correction, `since` the time since the last adjustment, and `frequency`
/// the current frequency correction, in 2^-16 ppm.
fn plan(offset_ns: i64, pending_us: i64, since: Option<Duration>, frequency: i64) -> Correction {
    if offset_ns.unsigned_abs() > STEP_THRESHOLD_NS as u64 {
        return Correction::Step;
    }
    let frequency = since.filter(|since| *since >= MIN_INTERVAL).map(|since| {
        // The last correction was the offset then. What is left now, less
        // what the kernel has still to slew of it, is what the clock
        // drifted since.
        let drift_ns = offset_ns as f64 + pending_us as f64 * 1_000.0;
        let rate_ppm = drift_ns / since.as_nanos() as f64 * 1e6;
        // Half of the error, so that one noisy measurement does not throw
        // the frequency off.
        let correction = (rate_ppm * 65_536.0 / 2.0).round() as i64;
        (frequency - correction).clamp(-MAX_FREQUENCY, MAX_FREQUENCY)
    });
    Correction::Slew { frequency }
}

fn adjust_time(offset_ns: i64) -> Result<AdjustTimeResponse, HostError> {
    let mut last = last_adjustment();
    let now = Instant::now();
    let mut state = timex(0);
    adjtimex(&mut state)?;
    let mut pending = timex(libc::ADJ_OFFSET_SS_READ);
    adjtimex(&mut pending)?;
    let correction = plan(
        offset_ns,
        pending.offset,
        last.map(|last| now.duration_since(last)),
        state.freq,
    );
    let mut frequency = state.freq;
    let stepped = correction == Correction::Step;
    match correction {
        Correction::Step => {
            step(-offset_ns)?;
            slew(0)?;
        }
        Correction::Slew {
            frequency: new_frequency,
        } => {
            if let Some(new_frequency) = new_frequency {
                let mut set = timex(libc::ADJ_FREQUENCY);
                set.freq = new_frequency;
                adjtimex(&mut set)?;
                frequency = new_frequency;
            }
            slew(-offset_ns / 1_000)?;
        }
    }
    *last = Some(now);
    Ok(AdjustTimeResponse {
        stepped,
        frequency_ppb: frequency * 1_000 / 65_536,
    })
}

/// Has the kernel slew the clock by `us` microseconds, in place of what it
/// still had to slew.
fn slew(us: i64) -> Result<(), HostError> {
    let mut tx = timex(libc::ADJ_OFFSET_SINGLESHOT);
    tx.offset = us;
    adjtimex(&mut tx)
}

/// Moves the clock by `ns` nanoseconds at once.
fn step(ns: i64) -> Result<(), HostError> {
    let mut tx = timex(libc::ADJ_SETOFFSET | libc::ADJ_NANO);
    tx.time.tv_sec = ns.div_euclid(1_000_000_000);
    tx.time.tv_usec = ns.rem_euclid(1_000_000_000);
    adjtimex(&mut tx)
}

fn timex(modes: libc::c_uint) -> libc::timex {
    // SAFETY: timex is plain data, and all zeroes is a valid value of it.
    let mut tx: libc::timex = unsafe { std::mem::zeroed() };
    tx.modes = modes;
    tx
}

fn adjtimex(tx: &mut libc::timex) -> Result<(), HostError> {
    // SAFETY: tx is a valid timex, which the kernel reads and fills.
    if unsafe { libc::adjtimex(tx) } < 0 {
        return Err(HostError::Failed(format!(
            "adjtimex: {}",
            std::io::Error::last_os_error()
        )));
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rejects_bad_timestamps() {
        for time in [
            None,
            Some(Timestamp {
                seconds: -1,
                nanos: 0,
            }),
            Some(Timestamp {
                seconds: 0,
                nanos: 1_000_000_000,
            }),
        ] {
            assert!(matches!(set_time(time), Err(HostError::InvalidArgument(_))));
        }
    }

    const MINUTE: Option<Duration> = Some(Duration::from_secs(60));

    #[test]
    fn a_large_offset_steps() {
        assert_eq!(plan(150_000_000, 0, MINUTE, 0), Correction::Step);
        assert_eq!(plan(-150_000_000, 0, MINUTE, 0), Correction::Step);
    }

    #[test]
    fn the_first_offset_is_only_slewed() {
        assert_eq!(
            plan(5_000_000, 0, None, 0),
            Correction::Slew { frequency: None }
        );
        assert_eq!(
            plan(5_000_000, 0, Some(Duration::from_secs(5)), 0),
            Correction::Slew { frequency: None }
        );
    }

    #[test]
    fn a_clock_behind_speeds_up_by_half_its_drift() {
        // 600 µs behind in a minute: 10 ppm slow. Half of it, 5 ppm, goes
        // into the frequency.
        assert_eq!(
            plan(-600_000, 0, MINUTE, 0),
            Correction::Slew {
                frequency: Some(5 << 16)
            }
        );
    }

    #[test]
    fn a_clock_ahead_slows_down() {
        assert_eq!(
            plan(600_000, 0, MINUTE, 2 << 16),
            Correction::Slew {
                frequency: Some(-(3 << 16))
            }
        );
    }

    #[test]
    fn what_is_still_to_be_slewed_is_no_drift() {
        // The last correction was +1 ms (the clock was behind), of which the
        // kernel still has 400 µs to slew: the clock is 400 µs behind
        // without having drifted.
        assert_eq!(
            plan(-400_000, 400, MINUTE, 7 << 16),
            Correction::Slew {
                frequency: Some(7 << 16)
            }
        );
    }

    #[test]
    fn the_frequency_stays_within_the_kernels_bounds() {
        assert_eq!(
            plan(-90_000_000, 0, Some(MIN_INTERVAL), 0),
            Correction::Slew {
                frequency: Some(MAX_FREQUENCY)
            }
        );
    }
}
