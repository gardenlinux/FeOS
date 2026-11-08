# ADR 0027: A guest's clock follows its node's, slewed from measurements over vsock

**Status:** Accepted · **Date:** 2026-11-01

A guest's clock follows its node's. While a guest is attached, `feosd`
measures its clock once a minute over the vsock API it already has, by
the round trip of several time reads of which the shortest counts, and
sends the guest the offset. The guest steps its clock only when the offset
is large, slews it otherwise, and learns its frequency from the drift
since its last adjustment, with a gain of one half; the kernel holds the
frequency, so a snapshot keeps it. A guest that does not know the
adjustment call is set outright when it is far off.

## Context

A guest has no NTP and no RTC it reads. `feosd` sets its wall clock when
it attaches and after every resume (ADR 0007), and never again. From then
on the guest's clock is kvm-clock: KVM gives the guest a TSC reading, a
time and a scale, and the guest counts on from them. The scale comes from
the TSC's nominal frequency, and the time from the node's raw clock.
Neither carries the frequency correction that the node's NTP client
applies to the node's own clock. So a guest's clock runs at the node's
uncorrected rate.

On nodes that keep their clocks within about 1 ms of the pool with a
frequency correction of about +14 ppm, their guests lose 9 to 13 ppm:

| guest uptime | guest clock against the node's |
|---|---|
| 3 min | +10 ms |
| 5.8 h | −250 to −280 ms |
| 43 h | −1.4 to −1.6 s |

That is about one second a day. A trace whose spans come from pods of
different ages shows children before their parents. A log line carries a
time that is off by as much, and a query by time window misses lines at
its edges.

The standard fix for a KVM guest is `ptp_kvm`: the guest asks KVM, by a
hypercall, for the host's wall clock and the TSC that goes with it, and a
clock daemon in the guest follows `/dev/ptp0`. FeOS's guest kernel has it,
and Firecracker recommends it. But on x86 KVM answers the hypercall only
if the host's clocksource is the TSC. Our nodes are VMs themselves: their
clocksource is kvm-clock, and their CPUs do not report an invariant TSC.
So `/dev/ptp0` never appears in our guests.

## Decision

### `feosd` measures the guest's clock over the channel it already has

While a guest is attached, `feosd` measures its clock once a minute. It
reads the guest's time four times over the guest's vsock API and takes
the read with the shortest round trip. The guest's offset is its time
minus the middle of that read on the node's clock. If even the shortest
round trip took more than 10 ms, the measurement says too little and is
dropped.

The reference is the node's `CLOCK_REALTIME`. Keeping that right is the
node's job: its NTP client on a distribution, `feos-host-init`'s on bare
metal (ADR 0009).

### The guest corrects itself

`feosd` sends the offset to the guest's `AdjustTime`. The guest acts on it
as follows:

- **A large offset**, over 100 ms, it steps away. This happens only when
  something went wrong, such as a long pause without a resume.
- **Otherwise** it slews the clock by the offset. The kernel slews at up
  to 0.5 ms per second, so the clock never jumps.
- **Its frequency** it corrects as well, if its last adjustment was at
  least 16 s ago, so that the drift outweighs the measurement's noise: by
  half of the rate at which it drifted since then. The drift is the offset
  plus what the kernel had not yet slewed of the last correction.

The kernel holds the frequency. The guest itself remembers only when it
last adjusted, in memory, so a snapshot keeps it. A set of the time steps
the clock, drops a slew still running and starts the count again.

This is a frequency-locked loop with a gain of one half. A constant drift
is gone within a few minutes, and what is left is the measurement's noise.

### Guests without `AdjustTime` are set

A guest whose API does not have `AdjustTime`, as a guest suspended before
it has (ADR 0007), answers it with `UNIMPLEMENTED`. `feosd` sets such a
guest's clock whenever it is off by more than 20 ms. Its clock then jumps
by about 20 ms every half hour instead of drifting a second a day. Such a
guest is replaced by a new one when its pod is next created.

## Alternatives considered

- **`ptp_kvm` with a clock daemon in the guest.** This is the standard,
  and it is better than this ADR: no measurement over vsock, and an
  accuracy of microseconds. But it needs a node whose clocksource is the
  TSC, which we do not have.
- **NTP in the guest against a responder of the node's** on the node's
  link-local address. That needs an NTP server in `feosd`, a client in the
  guest and a port in every security group. vsock already carries the
  same exchange.
- **Setting the time once a minute.** It steps, and a clock that jumps,
  even backwards, is worse for traces and logs than one that is slightly
  off.
- **The kernel's PLL (`STA_PLL`).** It would discipline the frequency
  too, but at one measurement a minute it takes hours to settle, and its
  time constant needs tuning. The loop above settles in minutes and fits
  in a few lines.
- **The node's frequency correction copied into the guest.** A guest's
  error is not exactly the node's: on a node without a TSC clocksource,
  KVM refreshes the guest's kvm-clock from the node's corrected clock at
  some moments and counts with the raw TSC in between. The measured drift
  (9 to 13 ppm) differs from the node's correction (14 ppm) and from guest
  to guest.

## Consequences

- A guest's clock stays within about a millisecond of its node's.
- `feosd` makes five vsock calls per guest and minute.
- A suspended pod's guest is detached, so `feosd` does not measure it. A
  call that meets a microVM that is pausing for a snapshot times out and
  is dropped.
- While a guest slews, its `CLOCK_REALTIME` and `CLOCK_MONOTONIC` run up
  to 500 ppm fast or slow. `CLOCK_MONOTONIC_RAW` does not change.
- Should the nodes get an invariant TSC, the guest could follow
  `/dev/ptp0` where it appears, and this loop would be the fallback.

## History

Decided and accepted on 2026-11-01 (1b1c98d), after the drift was measured
on a two-node cluster whose guests had run for up to 43 hours. The
measurement loop, `AdjustTime` and the setting of older guests shipped in
the same commit.
