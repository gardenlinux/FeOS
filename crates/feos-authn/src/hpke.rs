// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! HPKE (RFC 9180) in base mode, for the one suite of
//! `feos.fleet.v1.SealedBindings`: DHKEM(X25519, HKDF-SHA256), HKDF-SHA256,
//! ChaCha20-Poly1305, one message per context (sequence number 0).
//!
//! [`open`] is what feos-authn does with the box the control plane sealed
//! to its run's key. [`seal`] is the control plane's side
//! (`go/internal/pki/hpke.go`), here for tests and tools that play it.
//!
//! The primitives are ring's (HMAC-SHA256 for HKDF's extract, HKDF's
//! expand, the AEAD) and x25519-dalek's X25519, whose static keys open
//! more than one box: a run asks again with the same key after its
//! bindings change.

use ring::aead::{Aad, LessSafeKey, Nonce, UnboundKey, CHACHA20_POLY1305};
use ring::{hkdf, hmac};
use x25519_dalek::{PublicKey, StaticSecret};

/// The info of SealedBindings.
pub const BINDINGS_INFO: &[u8] = b"feos bindings v1";

const KEM_ID: u16 = 0x0020; // DHKEM(X25519, HKDF-SHA256)
const KDF_ID: u16 = 0x0001; // HKDF-SHA256
const AEAD_ID: u16 = 0x0003; // ChaCha20-Poly1305
const N_SECRET: usize = 32;
const N_K: usize = 32;
const N_N: usize = 12;
const MODE_BASE: u8 = 0;

/// The length of an encapsulated key and of a public key.
pub const N_ENC: usize = 32;

#[derive(Debug, PartialEq, Eq)]
pub enum Error {
    /// The encapsulated key is not 32 bytes.
    Enc,
    /// The Diffie-Hellman result is all zeros: a low-order point.
    LowOrder,
    /// The box does not open: another key, other additional data, or a
    /// changed ciphertext.
    Open,
    /// The AEAD refused to seal.
    Seal,
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Error::Enc => "the encapsulated key is not 32 bytes",
            Error::LowOrder => "the encapsulated key is a low-order point",
            Error::Open => "the box does not open with this run's key",
            Error::Seal => "the box cannot be sealed",
        })
    }
}

impl std::error::Error for Error {}

fn kem_suite() -> [u8; 5] {
    let k = KEM_ID.to_be_bytes();
    [b'K', b'E', b'M', k[0], k[1]]
}

fn hpke_suite() -> [u8; 10] {
    let (k, d, a) = (
        KEM_ID.to_be_bytes(),
        KDF_ID.to_be_bytes(),
        AEAD_ID.to_be_bytes(),
    );
    [b'H', b'P', b'K', b'E', k[0], k[1], d[0], d[1], a[0], a[1]]
}

/// LabeledExtract: HKDF-Extract(salt, "HPKE-v1" || suite || label || ikm),
/// which is HMAC-SHA256 keyed with the salt. An empty salt is HKDF's
/// default, a key of zeros, to HMAC.
fn labeled_extract(suite: &[u8], salt: &[u8], label: &[u8], ikm: &[u8]) -> [u8; 32] {
    let key = hmac::Key::new(hmac::HMAC_SHA256, salt);
    let mut ctx = hmac::Context::with_key(&key);
    ctx.update(b"HPKE-v1");
    ctx.update(suite);
    ctx.update(label);
    ctx.update(ikm);
    let tag = ctx.sign();
    let mut out = [0u8; 32];
    out.copy_from_slice(tag.as_ref());
    out
}

struct Len(usize);

impl hkdf::KeyType for Len {
    fn len(&self) -> usize {
        self.0
    }
}

/// LabeledExpand: HKDF-Expand(prk, I2OSP(L, 2) || "HPKE-v1" || suite ||
/// label || info, L).
fn labeled_expand(suite: &[u8], prk: &[u8; 32], label: &[u8], info: &[u8], out: &mut [u8]) {
    let prk = hkdf::Prk::new_less_safe(hkdf::HKDF_SHA256, prk);
    let len = (out.len() as u16).to_be_bytes();
    let parts: [&[u8]; 5] = [&len, b"HPKE-v1", suite, label, info];
    // The lengths here are HPKE's constants, far below HKDF's limit of
    // 255 hash lengths, so neither call can fail.
    if let Ok(okm) = prk.expand(&parts, Len(out.len())) {
        let _ = okm.fill(out);
    }
}

/// DHKEM's ExtractAndExpand over the X25519 result and enc || pkRm.
fn shared_secret(dh: &[u8; 32], enc: &[u8], pk_r: &[u8]) -> [u8; N_SECRET] {
    let suite = kem_suite();
    let eae_prk = labeled_extract(&suite, b"", b"eae_prk", dh);
    let mut ctx = Vec::with_capacity(2 * N_ENC);
    ctx.extend_from_slice(enc);
    ctx.extend_from_slice(pk_r);
    let mut out = [0u8; N_SECRET];
    labeled_expand(&suite, &eae_prk, b"shared_secret", &ctx, &mut out);
    out
}

/// The base mode's key schedule: the AEAD key and the base nonce.
fn key_schedule(shared: &[u8; N_SECRET], info: &[u8]) -> ([u8; N_K], [u8; N_N]) {
    let suite = hpke_suite();
    let psk_id_hash = labeled_extract(&suite, b"", b"psk_id_hash", b"");
    let info_hash = labeled_extract(&suite, b"", b"info_hash", info);
    let mut ctx = Vec::with_capacity(1 + 64);
    ctx.push(MODE_BASE);
    ctx.extend_from_slice(&psk_id_hash);
    ctx.extend_from_slice(&info_hash);
    let secret = labeled_extract(&suite, shared, b"secret", b"");
    let mut key = [0u8; N_K];
    let mut nonce = [0u8; N_N];
    labeled_expand(&suite, &secret, b"key", &ctx, &mut key);
    labeled_expand(&suite, &secret, b"base_nonce", &ctx, &mut nonce);
    (key, nonce)
}

fn dh(secret: &StaticSecret, public: [u8; 32]) -> Result<[u8; 32], Error> {
    let shared = secret.diffie_hellman(&PublicKey::from(public));
    if !shared.was_contributory() {
        return Err(Error::LowOrder);
    }
    Ok(shared.to_bytes())
}

fn aead(key: &[u8; N_K]) -> LessSafeKey {
    // A 32-byte key is ChaCha20-Poly1305's; UnboundKey::new fails only on
    // a wrong length.
    LessSafeKey::new(UnboundKey::new(&CHACHA20_POLY1305, key).expect("32-byte key"))
}

/// Opens a box sealed to `secret` with the encapsulated key `enc`, the info
/// and the additional data.
pub fn open(
    secret: &StaticSecret,
    enc: &[u8],
    info: &[u8],
    aad: &[u8],
    ciphertext: &[u8],
) -> Result<Vec<u8>, Error> {
    let pk_e: [u8; N_ENC] = enc.try_into().map_err(|_| Error::Enc)?;
    let pk_r = PublicKey::from(secret);
    let shared = shared_secret(&dh(secret, pk_e)?, enc, pk_r.as_bytes());
    let (key, nonce) = key_schedule(&shared, info);
    let mut buf = ciphertext.to_vec();
    let pt = aead(&key)
        .open_in_place(
            Nonce::assume_unique_for_key(nonce),
            Aad::from(aad),
            &mut buf,
        )
        .map_err(|_| Error::Open)?;
    let n = pt.len();
    buf.truncate(n);
    Ok(buf)
}

/// Seals `plaintext` to the public key `pk_r` with the ephemeral key
/// `ephemeral`; returns the encapsulated key and the ciphertext. A sender
/// draws a new ephemeral key for every box.
pub fn seal(
    pk_r: [u8; 32],
    ephemeral: &StaticSecret,
    info: &[u8],
    aad: &[u8],
    plaintext: &[u8],
) -> Result<([u8; N_ENC], Vec<u8>), Error> {
    let enc = PublicKey::from(ephemeral).to_bytes();
    let shared = shared_secret(&dh(ephemeral, pk_r)?, &enc, &pk_r);
    let (key, nonce) = key_schedule(&shared, info);
    let mut buf = plaintext.to_vec();
    aead(&key)
        .seal_in_place_append_tag(
            Nonce::assume_unique_for_key(nonce),
            Aad::from(aad),
            &mut buf,
        )
        .map_err(|_| Error::Seal)?;
    Ok((enc, buf))
}

/// The additional data of a SealedBindings: the pod's ID as it is written
/// (a short ID, or a UUID in lowercase hyphenated form), a zero byte and
/// the version.
pub fn bindings_aad(pod_id: &str, version: &str) -> Vec<u8> {
    let mut aad = Vec::with_capacity(pod_id.len() + 1 + version.len());
    aad.extend_from_slice(pod_id.as_bytes());
    aad.push(0);
    aad.extend_from_slice(version.as_bytes());
    aad
}

#[cfg(test)]
mod tests {
    use super::*;

    fn hex(s: &str) -> Vec<u8> {
        (0..s.len())
            .step_by(2)
            .map(|i| u8::from_str_radix(&s[i..i + 2], 16).unwrap())
            .collect()
    }

    fn key(s: &str) -> StaticSecret {
        StaticSecret::from(<[u8; 32]>::try_from(hex(s)).unwrap())
    }

    // RFC 9180, A.2.1: DHKEM(X25519, HKDF-SHA256), HKDF-SHA256,
    // ChaCha20Poly1305, base mode, the first encryption.
    const INFO: &str = "4f6465206f6e2061204772656369616e2055726e";
    const SK_EM: &str = "f4ec9b33b792c372c1d2c2063507b684ef925b8c75a42dbcbf57d63ccd381600";
    const SK_RM: &str = "8057991eef8f1f1af18f4a9491d16a1ce333f695d4db8e38da75975c4478e0fb";
    const PK_RM: &str = "4310ee97d88cc1f088a5576c77ab0cf5c3ac797f3d95139c6c84b5429c59662a";
    const ENC: &str = "1afa08d3dec047a643885163f1180476fa7ddb54c6a8029ea33f95796bf2ac4a";
    const AAD: &str = "436f756e742d30";
    const PT: &str = "4265617574792069732074727574682c20747275746820626561757479";
    const CT: &str = "1c5250d8034ec2b784ba2cfd69dbdb8af406cfe3ff938e131f0def8c8b60b4db21993c62ce81883d2dd1b51a28";

    #[test]
    fn rfc9180_vector_opens() {
        let sk = key(SK_RM);
        assert_eq!(PublicKey::from(&sk).as_bytes().to_vec(), hex(PK_RM));
        let pt = open(&sk, &hex(ENC), &hex(INFO), &hex(AAD), &hex(CT)).unwrap();
        assert_eq!(pt, hex(PT));
    }

    #[test]
    fn rfc9180_vector_seals() {
        let pk: [u8; 32] = hex(PK_RM).try_into().unwrap();
        let (enc, ct) = seal(pk, &key(SK_EM), &hex(INFO), &hex(AAD), &hex(PT)).unwrap();
        assert_eq!(enc.to_vec(), hex(ENC));
        assert_eq!(ct, hex(CT));
    }

    #[test]
    fn refuses_what_is_not_its_box() {
        let sk = key(SK_RM);
        let (enc, ct) = (hex(ENC), hex(CT));
        // Other additional data, another key, a changed byte, a short enc.
        assert_eq!(
            open(&sk, &enc, &hex(INFO), b"Count-1", &ct),
            Err(Error::Open)
        );
        assert_eq!(
            open(&key(SK_EM), &enc, &hex(INFO), &hex(AAD), &ct),
            Err(Error::Open)
        );
        let mut changed = ct.clone();
        changed[3] ^= 1;
        assert_eq!(
            open(&sk, &enc, &hex(INFO), &hex(AAD), &changed),
            Err(Error::Open)
        );
        assert_eq!(
            open(&sk, &enc[..31], &hex(INFO), &hex(AAD), &ct),
            Err(Error::Enc)
        );
        // The all-zero point is refused before the AEAD.
        assert_eq!(
            open(&sk, &[0u8; 32], &hex(INFO), &hex(AAD), &ct),
            Err(Error::LowOrder)
        );
    }

    #[test]
    fn a_static_key_opens_more_than_one_box() {
        let sk = key(SK_RM);
        let pk = PublicKey::from(&sk).to_bytes();
        for (i, eph) in [SK_EM, PK_RM].iter().enumerate() {
            let aad = bindings_aad("a1b2c3d4e5f", &format!("v{i}"));
            let (enc, ct) = seal(pk, &key(eph), BINDINGS_INFO, &aad, b"set").unwrap();
            assert_eq!(open(&sk, &enc, BINDINGS_INFO, &aad, &ct).unwrap(), b"set");
        }
    }

    #[test]
    fn aad_is_pod_zero_version() {
        assert_eq!(bindings_aad("pod", "v7"), b"pod\0v7");
        assert_eq!(bindings_aad("pod", ""), b"pod\0");
    }
}
