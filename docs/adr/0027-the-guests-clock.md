# ADR 0027: A guest's clock follows its node's: feosd measures it over vsock, the guest slews it and learns its frequency

**Status:** Accepted (Malte, 2026-11-01) · **Date:** 2026-11-01

## Context

A guest has no NTP and no RTC it reads. `feosd` sets its wall clock
(`SetTime`) when it attaches and after every resume, and never again.
From then on the guest's clock is kvm-clock: KVM gives the guest a
TSC reading, a time and a scale, and the guest counts on from them. The
scale comes from the TSC's nominal frequency, and the time from the
node's raw clock. Neither carries the frequency correction that the
node's NTP client applies to the node's own clock. So a guest's clock
runs at the node's uncorrected rate.

On shoot `fecode` (2026-11-01) the nodes keep their clocks with
systemd-timesyncd, within about 1 ms of the pool, with a frequency
correction of about +14 ppm. Their guests lose 9 to 13 ppm:

| guest uptime | guest clock against the node's |
|---|---|
| 3 min | +10 ms |
| 5.8 h | −250 to −280 ms |
| 43 h | −1.4 to −1.6 s |

That is about one second a day. A trace whose spans come from pods of
different ages shows children before their parents. A log line carries
a time that is off by as much, and a query by time window misses lines
at its edges.

The standard fix for a KVM guest is `ptp_kvm`. The guest asks KVM, by a
hypercall, for the host's wall clock and the TSC that goes with it, and
a clock daemon in the guest follows `/dev/ptp0`. FeOS's guest kernel has
it (`CONFIG_PTP_1588_CLOCK_KVM`), and Firecracker recommends it. But on
x86 KVM answers the hypercall only if the host's clocksource is the TSC.
Our nodes are VMs themselves: on IronCore their clocksource is
kvm-clock, and their CPUs do not report an invariant TSC
(`constant_tsc`, `nonstop_tsc`). So `/dev/ptp0` never appears in our
guests.

`feos-host-init` has an SNTP client. It runs on a bare-metal host, not in
a guest, and it steps the clock once a day.

## Decision

### 1. `feosd` measures the guest's clock over the channel it already has

While a guest is attached, `feosd` measures its clock once a minute. It
calls `GetTime` four times over the guest's vsock API, and takes the
call with the shortest round trip. The guest's offset is its time minus
the middle of that call on the node's clock. If even the shortest
round trip took more than 10 ms, the measurement says too little and is
dropped.

The reference is the node's `CLOCK_REALTIME`. Keeping that right is the
node's job: its NTP client on Gardener, `feos-host-init`'s on bare metal.

### 2. The guest corrects itself: `AdjustTime`

`feosd` sends the offset to a new call, `AdjustTime`. The guest acts on
it as follows:

- **Over 100 ms:** it steps the clock by the offset (`adjtimex`
  `ADJ_SETOFFSET`). This happens only when something went wrong, such as
  a long pause without a resume.
- **Otherwise:** it slews the clock by the offset (`ADJ_OFFSET_SINGLESHOT`).
  The kernel slews at up to 0.5 ms per second, so the clock never jumps.
- **Its frequency:** if its last adjustment was at least 16 s ago, it
  also corrects its frequency (`ADJ_FREQUENCY`). The correction is half
  of the rate at which it drifted since then. The drift is the offset
  plus what the kernel had not yet slewed of the last correction.

The kernel holds the frequency. The guest itself remembers only when it
last adjusted, in memory, so a snapshot keeps it. `SetTime` steps the
clock, drops a slew still running and starts the count again.

This is a frequency-locked loop with a gain of one half. A constant
drift is gone within a few minutes, and what is left is the
measurement's noise.

### 3. Older guests are stepped

A guest from before this ADR answers `AdjustTime` with `UNIMPLEMENTED`.
`feosd` sets such a guest's clock with `SetTime` whenever it is off by
more than 20 ms. Its clock then jumps by about 20 ms every half hour
instead of drifting a second a day. Such a guest is replaced by a new
one when its pod is next created.

## Alternatives considered

- **`ptp_kvm` with a clock daemon in the guest.** This is the standard,
  and it is better than this ADR: no measurement over vsock, and an
  accuracy of microseconds. But it needs a node whose clocksource is the
  TSC, which we do not have (Context).
- **NTP in the guest against a responder of the node's on
  `169.254.169.1`.** That needs an NTP server in `feosd`, a client in the
  guest and a port in every security group. vsock already carries the
  same exchange.
- **`SetTime` once a minute.** It steps, and a clock that jumps, even
  backwards, is worse for traces and logs than one that is slightly off.
- **The kernel's PLL (`STA_PLL`).** It would discipline the frequency
  too, but at one measurement a minute it takes hours to settle, and its
  time constant needs tuning. The loop of §2 settles in minutes and fits
  in a few lines.
- **The node's frequency correction copied into the guest.** A guest's
  error is not exactly the node's: on a node without a TSC clocksource,
  KVM refreshes the guest's kvm-clock from the node's corrected clock at
  some moments and counts with the raw TSC in between. The measured
  drift (9 to 13 ppm) differs from the node's correction (14 ppm) and
  from guest to guest.

## Consequences

- A guest's clock stays within about a millisecond of its node's.
- `feosd` makes five vsock calls per guest and minute.
- A suspended pod's guest is detached, so `feosd` does not measure it. A
  call that meets a microVM that is pausing for a snapshot times out and
  is dropped.
- While a guest slews, its `CLOCK_REALTIME` and `CLOCK_MONOTONIC` run up
  to 500 ppm fast or slow. `CLOCK_MONOTONIC_RAW` does not change.

## Open questions

- If our nodes get an invariant TSC, the guest could follow `/dev/ptp0`
  when it is there, and `feosd`'s loop would be needed only as a
  fallback.
