// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A binding's rate limit (ADR 0028 §7): a token bucket of `burst`
//! requests that fills at `per_second`.

use std::time::{Duration, Instant};

pub struct Bucket {
    rate: f64,
    burst: f64,
    tokens: f64,
    last: Instant,
}

impl Bucket {
    /// A bucket that starts full. A burst of 0 is one second's worth, at
    /// least one request.
    pub fn new(per_second: u32, burst: u32, now: Instant) -> Bucket {
        let rate = f64::from(per_second.max(1));
        let burst = if burst == 0 {
            rate.max(1.0)
        } else {
            f64::from(burst)
        };
        Bucket {
            rate,
            burst,
            tokens: burst,
            last: now,
        }
    }

    /// Takes one request's token, or says how long until there is one.
    pub fn take(&mut self, now: Instant) -> Result<(), Duration> {
        let elapsed = now.saturating_duration_since(self.last).as_secs_f64();
        self.last = now.max(self.last);
        self.tokens = (self.tokens + elapsed * self.rate).min(self.burst);
        if self.tokens >= 1.0 {
            self.tokens -= 1.0;
            Ok(())
        } else {
            Err(Duration::from_secs_f64((1.0 - self.tokens) / self.rate))
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn burst_then_rate() {
        let t0 = Instant::now();
        let mut b = Bucket::new(2, 3, t0);
        for _ in 0..3 {
            assert!(b.take(t0).is_ok());
        }
        let wait = b.take(t0).unwrap_err();
        assert!(wait > Duration::from_millis(400) && wait <= Duration::from_millis(500));
        // Half a second at 2/s is one more request, and only one.
        let t1 = t0 + Duration::from_millis(500);
        assert!(b.take(t1).is_ok());
        assert!(b.take(t1).is_err());
        // A long pause fills the bucket to its burst, not beyond.
        let t2 = t1 + Duration::from_secs(60);
        for _ in 0..3 {
            assert!(b.take(t2).is_ok());
        }
        assert!(b.take(t2).is_err());
    }

    #[test]
    fn zero_burst_is_one_second() {
        let t0 = Instant::now();
        let mut b = Bucket::new(5, 0, t0);
        for _ in 0..5 {
            assert!(b.take(t0).is_ok());
        }
        assert!(b.take(t0).is_err());
    }
}
