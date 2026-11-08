// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The run's keys, made in memory at every start and never written
//! anywhere (ADR 0014): the pod's Ed25519 identity key, whose signing
//! request goes to the control plane, and the X25519 key to which the
//! control plane seals the pod's bindings.

use rcgen::{PublicKeyData, SigningKey};
use ring::rand::{SecureRandom, SystemRandom};
use x25519_dalek::{PublicKey, StaticSecret};

use crate::hpke;

pub struct Keys {
    /// The pod's identity key, with which FeID's signer signs.
    identity: rcgen::KeyPair,
    csr_der: Vec<u8>,
    hpke: StaticSecret,
    hpke_public: [u8; 32],
}

impl Keys {
    /// Makes both key pairs and the signing request. Its subject and names
    /// are placeholders: the control plane ignores them and names the
    /// pod's SPIFFE ID itself.
    pub fn generate() -> Result<Keys, String> {
        let identity = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519)
            .map_err(|e| format!("make the identity key: {e}"))?;
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new())
            .map_err(|e| format!("signing request: {e}"))?;
        params
            .distinguished_name
            .push(rcgen::DnType::CommonName, "feos-authn");
        let csr_der = params
            .serialize_request(&identity)
            .map_err(|e| format!("signing request: {e}"))?
            .der()
            .to_vec();
        let mut seed = [0u8; 32];
        SystemRandom::new()
            .fill(&mut seed)
            .map_err(|_| "the system's random source failed".to_string())?;
        let hpke = StaticSecret::from(seed);
        let hpke_public = PublicKey::from(&hpke).to_bytes();
        Ok(Keys {
            identity,
            csr_der,
            hpke,
            hpke_public,
        })
    }

    /// Keys with a given HPKE secret, for tests that open a vector.
    pub fn with_hpke_secret(secret: [u8; 32]) -> Result<Keys, String> {
        let mut k = Keys::generate()?;
        k.hpke = StaticSecret::from(secret);
        k.hpke_public = PublicKey::from(&k.hpke).to_bytes();
        Ok(k)
    }

    pub fn csr_der(&self) -> &[u8] {
        &self.csr_der
    }

    pub fn hpke_public(&self) -> [u8; 32] {
        self.hpke_public
    }

    /// The identity key's public half, the 32 bytes of Ed25519.
    pub fn identity_public(&self) -> &[u8] {
        self.identity.der_bytes()
    }

    /// Signs with the identity key. Only FeID's signer calls it, with an
    /// input that passed feid-proto's check.
    pub fn sign_identity(&self, message: &[u8]) -> Result<Vec<u8>, String> {
        self.identity
            .sign(message)
            .map_err(|e| format!("sign with the identity key: {e}"))
    }

    /// Opens a SealedBindings of this run for the pod.
    pub fn open_bindings(
        &self,
        pod_id: &str,
        enc: &[u8],
        ciphertext: &[u8],
        version: &str,
    ) -> Result<Vec<u8>, hpke::Error> {
        hpke::open(
            &self.hpke,
            enc,
            hpke::BINDINGS_INFO,
            &hpke::bindings_aad(pod_id, version),
            ciphertext,
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use x509_parser::prelude::FromDer;

    #[test]
    fn signing_request_is_ed25519_and_self_signed() {
        let k = Keys::generate().unwrap();
        let (_, csr) =
            x509_parser::certification_request::X509CertificationRequest::from_der(k.csr_der())
                .unwrap();
        csr.verify_signature().unwrap();
        assert_eq!(
            csr.certification_request_info
                .subject_pki
                .algorithm
                .algorithm
                .to_id_string(),
            "1.3.101.112"
        );
        // Every run has keys of its own.
        let other = Keys::generate().unwrap();
        assert_ne!(k.hpke_public(), other.hpke_public());
        assert_ne!(k.identity_public(), other.identity_public());
    }

    #[test]
    fn the_identity_key_signs_ed25519() {
        let k = Keys::generate().unwrap();
        assert_eq!(k.identity_public().len(), 32);
        let sig = k.sign_identity(b"message").unwrap();
        ring::signature::UnparsedPublicKey::new(&ring::signature::ED25519, k.identity_public())
            .verify(b"message", &sig)
            .unwrap();
    }
}
