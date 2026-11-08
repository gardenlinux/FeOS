// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The key rustls signs with, which is not here: its signature is the
//! signer's answer to feid/1's `sign`. rustls hands an Ed25519 key the
//! whole CertificateVerify input, unhashed, which the signer checks before
//! it signs.

use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use rustls::sign::{Signer, SigningKey};
use rustls::{SignatureAlgorithm, SignatureScheme};

use crate::Inner;

/// The workload's key, held by the signer.
pub(crate) struct DelegatedKey {
    inner: Arc<Inner>,
}

impl DelegatedKey {
    pub(crate) fn new(inner: Arc<Inner>) -> DelegatedKey {
        DelegatedKey { inner }
    }
}

impl std::fmt::Debug for DelegatedKey {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "DelegatedKey({})", self.inner.socket.display())
    }
}

impl SigningKey for DelegatedKey {
    fn choose_scheme(&self, offered: &[SignatureScheme]) -> Option<Box<dyn Signer>> {
        offered
            .contains(&SignatureScheme::ED25519)
            .then(|| Box::new(Delegated(self.inner.clone())) as Box<dyn Signer>)
    }

    fn algorithm(&self) -> SignatureAlgorithm {
        SignatureAlgorithm::ED25519
    }
}

struct Delegated(Arc<Inner>);

impl std::fmt::Debug for Delegated {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("Delegated")
    }
}

impl Signer for Delegated {
    fn sign(&self, message: &[u8]) -> Result<Vec<u8>, rustls::Error> {
        self.0
            .call("sign", |c| c.sign(message))
            .map_err(|e| rustls::Error::General(format!("the signer: {e}")))
    }

    fn scheme(&self) -> SignatureScheme {
        SignatureScheme::ED25519
    }
}

/// `d` by 0.75 to 1.25, so that retries of many workloads do not fall in
/// step. Not for anything secret: the clock and a counter seed it.
pub(crate) fn jitter(d: Duration) -> Duration {
    static COUNTER: AtomicU64 = AtomicU64::new(0);
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |t| t.subsec_nanos() as u64);
    let mut x = nanos ^ COUNTER.fetch_add(0x9e37_79b9_7f4a_7c15, Ordering::Relaxed);
    x ^= x >> 33;
    x = x.wrapping_mul(0xff51_afd7_ed55_8ccd);
    x ^= x >> 33;
    let unit = (x % 1_000_000) as f64 / 1_000_000.0;
    d.mul_f64(0.75 + unit * 0.5)
}
