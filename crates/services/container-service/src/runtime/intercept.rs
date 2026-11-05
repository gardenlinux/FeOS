// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A pod's interception CAs in every container (ADR 0028 §5).
//!
//! feosd pushes the CAs (`GuestService.SetInterceptCas`); the guest writes
//! them to [`CA_FILE`] in the share directory, which every container sees
//! read-only at [`CA_PATH`]. A container created while there are CAs gets,
//! in its directory, `intercept/system.pem`, a copy of its image's system
//! bundle (the first of [`SYSTEM_BUNDLES`] its image has), and
//! `intercept/ca-bundle.pem`, that bundle and the CAs, which its runtime
//! spec mounts read-only over every one of [`SYSTEM_BUNDLES`] the image
//! has, or over the first when it has none (distroless, scratch). Its
//! environment names them where the container's spec does not:
//! `SSL_CERT_FILE` and `REQUESTS_CA_BUNDLE` the bundle, and
//! `NODE_EXTRA_CA_CERTS` the CA file; and so does every process started in
//! it later, which takes the environment of its runtime spec.
//!
//! A change of the CAs rewrites the CA file and every container's bundle
//! in place, truncated and written, never by a rename: a bind mount of a
//! file holds the inode it was made with. A process that read its store
//! before reads the new CAs after its restart.
//!
//! Containers of a pod without CAs are left as they are.

use feos_utils::fs::openat2_confined;
use nix::fcntl::{OFlag, ResolveFlag};
use nix::sys::stat::Mode;
use serde_json::{json, Value};
use std::collections::HashMap;
use std::fs::{File, OpenOptions};
use std::io::{self, Read, Write};
use std::os::unix::fs::OpenOptionsExt;
use std::path::{Path, PathBuf};
use std::sync::{Mutex, MutexGuard};

/// The CAs' file in the share directory, and where a container sees it.
pub const CA_FILE: &str = "intercept-ca.pem";
pub const CA_PATH: &str = "/run/feos/intercept-ca.pem";
/// The system bundles of Debian and Alpine, Red Hat, and LibreSSL and
/// Alpine, in the order a bundle is taken from.
pub const SYSTEM_BUNDLES: [&str; 3] = [
    "/etc/ssl/certs/ca-certificates.crt",
    "/etc/pki/tls/certs/ca-bundle.crt",
    "/etc/ssl/cert.pem",
];
/// A container's files, in its directory.
const DIR: &str = "intercept";
const SYSTEM: &str = "system.pem";
const BUNDLE: &str = "ca-bundle.pem";
/// A system bundle larger than this is not one.
const MAX_SYSTEM_BUNDLE: u64 = 16 << 20;
/// The variables, and what each names: the bundle, or the CA file.
const BUNDLE_VARIABLES: [&str; 2] = ["SSL_CERT_FILE", "REQUESTS_CA_BUNDLE"];
const CA_VARIABLE: &str = "NODE_EXTRA_CA_CERTS";

/// Serializes a container's provisioning with a change of the CAs: one
/// that reads the CAs before the change writes its bundle after it.
static CHANGE: Mutex<()> = Mutex::new(());

fn lock() -> MutexGuard<'static, ()> {
    CHANGE.lock().unwrap_or_else(|e| e.into_inner())
}

/// The CAs the guest holds, PEM, from the share directory `share`; None
/// when it holds none.
pub fn cas_in(share: &Path) -> Option<String> {
    let cas = std::fs::read_to_string(share.join(CA_FILE)).ok()?;
    (!cas.trim().is_empty()).then_some(cas)
}

/// What a container gets of the CAs: the mounts of its bundle, and the
/// variables its environment gets.
#[derive(Debug, Default, PartialEq)]
pub struct Injection {
    pub mounts: Vec<Value>,
    pub env: Vec<(String, String)>,
}

/// Prepares the CAs for the container whose directory is `container`, of
/// the image whose root filesystem is `rootfs`, if the guest holds CAs in
/// `share`: writes its files and returns what its runtime spec gets.
/// Nothing without CAs.
pub fn prepare(share: &Path, container: &Path, rootfs: &Path) -> Result<Injection, String> {
    let _change = lock();
    let Some(cas) = cas_in(share) else {
        return Ok(Injection::default());
    };
    let root = File::open(rootfs).map_err(|e| format!("{}: {e}", rootfs.display()))?;
    let present: Vec<&str> = SYSTEM_BUNDLES
        .iter()
        .copied()
        .filter(|path| is_file_in(&root, path))
        .collect();
    let system = present
        .first()
        .map(|path| read_in(&root, path))
        .transpose()?
        .unwrap_or_default();
    let dir = container.join(DIR);
    std::fs::create_dir_all(&dir).map_err(|e| format!("{}: {e}", dir.display()))?;
    write_in_place(&dir.join(SYSTEM), system.as_bytes())?;
    let bundle = dir.join(BUNDLE);
    write_in_place(&bundle, bundle_of(&system, &cas).as_bytes())?;
    let targets = match present.is_empty() {
        true => vec![SYSTEM_BUNDLES[0]],
        false => present,
    };
    let mounts = targets
        .iter()
        .map(|target| {
            json!({
                "destination": target,
                "type": "bind",
                "source": bundle,
                "options": ["bind", "ro", "nosuid", "nodev", "noexec"],
            })
        })
        .collect();
    let mut env: Vec<(String, String)> = BUNDLE_VARIABLES
        .iter()
        .map(|name| (name.to_string(), targets[0].to_string()))
        .collect();
    env.push((CA_VARIABLE.to_string(), CA_PATH.to_string()));
    Ok(Injection { mounts, env })
}

/// Sets the injection's variables in `env`, a process's `KEY=value`
/// entries, unless `spec` (the container's own environment as its spec
/// states it) sets them; a value of an earlier injection is replaced.
pub fn set_env(env: &mut Vec<String>, injection: &Injection, spec: &HashMap<String, String>) {
    for (name, value) in &injection.env {
        if spec.contains_key(name) {
            continue;
        }
        let prefix = format!("{name}=");
        env.retain(|entry| !entry.starts_with(&prefix));
        env.push(format!("{name}={value}"));
    }
}

/// An exec's environment `env`, in a container that may have been created
/// before the guest held CAs: the CA file where the guest holds CAs and
/// `env` names none. The bundle is the container's only if it was created
/// with it, and then its runtime spec names it already.
pub fn set_exec_env(share: &Path, env: &mut Vec<String>) {
    let prefix = format!("{CA_VARIABLE}=");
    if env.iter().any(|entry| entry.starts_with(&prefix)) {
        return;
    }
    if cas_in(share).is_some() {
        env.push(format!("{CA_VARIABLE}={CA_PATH}"));
    }
}

/// Sets the guest's CAs, `cas` oldest first: the share directory's file
/// and the bundle of every container below `containers` that has one, all
/// in place. Returns the containers whose bundle could not be rewritten,
/// with why; a failure to write the CA file is an error.
pub fn publish(
    share: &Path,
    containers: &Path,
    cas: &[String],
) -> io::Result<Vec<(String, String)>> {
    let _change = lock();
    feos_utils::share::make_share_dir(share)?;
    let joined: String = cas.iter().map(|ca| with_newline(ca)).collect();
    write_in_place(&share.join(CA_FILE), joined.as_bytes()).map_err(io::Error::other)?;
    let mut failed = Vec::new();
    let entries = match std::fs::read_dir(containers) {
        Ok(entries) => entries,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(failed),
        Err(e) => return Err(e),
    };
    for entry in entries.flatten() {
        let dir = entry.path().join(DIR);
        let id = entry.file_name().to_string_lossy().into_owned();
        let system = match std::fs::read_to_string(dir.join(SYSTEM)) {
            Ok(system) => system,
            Err(e) if e.kind() == io::ErrorKind::NotFound => continue,
            Err(e) => {
                failed.push((id, format!("{}: {e}", dir.join(SYSTEM).display())));
                continue;
            }
        };
        if let Err(e) = write_in_place(&dir.join(BUNDLE), bundle_of(&system, &joined).as_bytes()) {
            failed.push((id, e));
        }
    }
    Ok(failed)
}

/// A bundle of `system` and `cas`, each block on lines of its own.
fn bundle_of(system: &str, cas: &str) -> String {
    let mut bundle = with_newline(system);
    bundle.push_str(&with_newline(cas));
    bundle
}

fn with_newline(text: &str) -> String {
    let mut text = text.to_string();
    if !text.is_empty() && !text.ends_with('\n') {
        text.push('\n');
    }
    text
}

/// Replaces the content of the file at `path`, made 0644 if it is not
/// there, in place: truncated and written, so that a bind mount of it sees
/// the new content.
fn write_in_place(path: &Path, content: &[u8]) -> Result<(), String> {
    let mut file = OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(0o644)
        .custom_flags(OFlag::O_NOFOLLOW.bits())
        .open(path)
        .map_err(|e| format!("{}: {e}", path.display()))?;
    file.write_all(content)
        .map_err(|e| format!("{}: {e}", path.display()))
}

/// Whether `path`, as the container sees it in its root filesystem
/// `root`, is a regular file; its links are followed inside the root.
fn is_file_in(root: &File, path: &str) -> bool {
    open_in(root, path)
        .and_then(|file| file.metadata())
        .is_ok_and(|meta| meta.is_file())
}

fn read_in(root: &File, path: &str) -> Result<String, String> {
    let mut text = String::new();
    open_in(root, path)
        .and_then(|file| file.take(MAX_SYSTEM_BUNDLE).read_to_string(&mut text))
        .map_err(|e| format!("the image's {path}: {e}"))?;
    Ok(text)
}

/// Opens `path` in the image's root `root`, its links resolved inside it,
/// as the container would see them.
fn open_in(root: &File, path: &str) -> io::Result<File> {
    let relative = PathBuf::from(path.trim_start_matches('/'));
    let fd = openat2_confined(
        root,
        &relative,
        OFlag::O_RDONLY,
        Mode::empty(),
        ResolveFlag::RESOLVE_IN_ROOT | ResolveFlag::RESOLVE_NO_MAGICLINKS,
    )?;
    Ok(File::from(fd))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::{symlink, MetadataExt};

    const CA1: &str = "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n";
    const CA2: &str = "-----BEGIN CERTIFICATE-----\nBBBB\n-----END CERTIFICATE-----";

    struct Guest {
        _dir: tempfile::TempDir,
        share: PathBuf,
        containers: PathBuf,
    }

    fn guest() -> Guest {
        let dir = tempfile::tempdir().unwrap();
        let share = dir.path().join("share");
        let containers = dir.path().join("containers");
        std::fs::create_dir_all(&containers).unwrap();
        Guest {
            share,
            containers,
            _dir: dir,
        }
    }

    impl Guest {
        fn container(&self, id: &str) -> PathBuf {
            let dir = self.containers.join(id);
            std::fs::create_dir_all(&dir).unwrap();
            dir
        }
    }

    /// An image's root with `files` at their paths and `links`.
    fn image(files: &[(&str, &str)], links: &[(&str, &str)]) -> tempfile::TempDir {
        let root = tempfile::tempdir().unwrap();
        for (path, content) in files {
            let path = root.path().join(path.trim_start_matches('/'));
            std::fs::create_dir_all(path.parent().unwrap()).unwrap();
            std::fs::write(path, content).unwrap();
        }
        for (path, target) in links {
            let path = root.path().join(path.trim_start_matches('/'));
            std::fs::create_dir_all(path.parent().unwrap()).unwrap();
            symlink(target, path).unwrap();
        }
        root
    }

    fn destinations(injection: &Injection) -> Vec<&str> {
        injection
            .mounts
            .iter()
            .map(|m| m["destination"].as_str().unwrap())
            .collect()
    }

    #[test]
    fn without_cas_a_container_is_untouched() {
        let guest = guest();
        let dir = guest.container("main");
        let root = image(&[("/etc/ssl/certs/ca-certificates.crt", "SYSTEM\n")], &[]);
        let injection = prepare(&guest.share, &dir, root.path()).unwrap();
        assert_eq!(injection, Injection::default());
        assert!(!dir.join(DIR).exists());
        // Emptied CAs count as none.
        publish(&guest.share, &guest.containers, &[]).unwrap();
        assert_eq!(cas_in(&guest.share), None);
        assert_eq!(
            prepare(&guest.share, &dir, root.path()).unwrap(),
            Injection::default()
        );
        let mut env = vec!["A=1".to_string()];
        set_exec_env(&guest.share, &mut env);
        assert_eq!(env, ["A=1"]);
    }

    #[test]
    fn a_debian_image_gets_its_bundle_with_the_cas_mounted_over_its_own() {
        let guest = guest();
        publish(&guest.share, &guest.containers, &[CA1.into(), CA2.into()]).unwrap();
        assert_eq!(
            std::fs::read_to_string(guest.share.join(CA_FILE)).unwrap(),
            format!("{CA1}{CA2}\n")
        );
        let dir = guest.container("main");
        let root = image(&[("/etc/ssl/certs/ca-certificates.crt", "SYSTEM")], &[]);
        let injection = prepare(&guest.share, &dir, root.path()).unwrap();
        assert_eq!(
            destinations(&injection),
            ["/etc/ssl/certs/ca-certificates.crt"]
        );
        let bundle = dir.join(DIR).join(BUNDLE);
        assert_eq!(injection.mounts[0]["source"], json!(bundle));
        assert_eq!(injection.mounts[0]["options"][1], "ro");
        assert_eq!(
            std::fs::read_to_string(&bundle).unwrap(),
            format!("SYSTEM\n{CA1}{CA2}\n")
        );
        assert_eq!(
            injection.env,
            [
                (
                    "SSL_CERT_FILE".into(),
                    "/etc/ssl/certs/ca-certificates.crt".into()
                ),
                (
                    "REQUESTS_CA_BUNDLE".into(),
                    "/etc/ssl/certs/ca-certificates.crt".into()
                ),
                ("NODE_EXTRA_CA_CERTS".into(), CA_PATH.into()),
            ]
        );
    }

    #[test]
    fn every_bundle_an_image_has_is_covered_its_links_followed_inside_it() {
        let guest = guest();
        publish(&guest.share, &guest.containers, &[CA1.into()]).unwrap();
        let dir = guest.container("main");
        // Alpine's cert.pem links to its Debian path; Red Hat's bundle is
        // an absolute link, resolved in the image's root, not the guest's.
        let root = image(
            &[
                ("/etc/ssl/certs/ca-certificates.crt", "ALPINE\n"),
                ("/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", "RH\n"),
            ],
            &[
                ("/etc/ssl/cert.pem", "certs/ca-certificates.crt"),
                (
                    "/etc/pki/tls/certs/ca-bundle.crt",
                    "/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
                ),
            ],
        );
        let injection = prepare(&guest.share, &dir, root.path()).unwrap();
        assert_eq!(destinations(&injection), SYSTEM_BUNDLES);
        let bundle = std::fs::read_to_string(dir.join(DIR).join(BUNDLE)).unwrap();
        assert_eq!(bundle, format!("ALPINE\n{CA1}"), "the first one's");

        // A link out of the image is not followed onto the guest.
        let dir = guest.container("other");
        let root = image(&[], &[("/etc/ssl/cert.pem", "/../../../../etc/passwd")]);
        let injection = prepare(&guest.share, &dir, root.path()).unwrap();
        assert_eq!(destinations(&injection), [SYSTEM_BUNDLES[0]]);
    }

    #[test]
    fn an_image_without_a_bundle_gets_one_at_debians_path() {
        let guest = guest();
        publish(&guest.share, &guest.containers, &[CA1.into()]).unwrap();
        let dir = guest.container("main");
        let root = image(&[("/app", "binary")], &[]);
        let injection = prepare(&guest.share, &dir, root.path()).unwrap();
        assert_eq!(destinations(&injection), [SYSTEM_BUNDLES[0]]);
        assert_eq!(
            std::fs::read_to_string(dir.join(DIR).join(BUNDLE)).unwrap(),
            CA1
        );
        assert_eq!(injection.env[0].1, SYSTEM_BUNDLES[0]);
    }

    #[test]
    fn the_specs_own_variables_win_and_an_earlier_injection_is_replaced() {
        let guest = guest();
        publish(&guest.share, &guest.containers, &[CA1.into()]).unwrap();
        let dir = guest.container("main");
        let root = image(&[], &[]);
        let injection = prepare(&guest.share, &dir, root.path()).unwrap();
        let spec = HashMap::from([("REQUESTS_CA_BUNDLE".to_string(), "/mine.pem".to_string())]);
        let mut env = vec![
            "PATH=/bin".to_string(),
            "REQUESTS_CA_BUNDLE=/mine.pem".to_string(),
            // An adopted container's, from its first create.
            "SSL_CERT_FILE=/old".to_string(),
        ];
        set_env(&mut env, &injection, &spec);
        assert_eq!(
            env,
            [
                "PATH=/bin",
                "REQUESTS_CA_BUNDLE=/mine.pem",
                "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt",
                "NODE_EXTRA_CA_CERTS=/run/feos/intercept-ca.pem",
            ]
        );
        // An exec in a container made before the CAs: the CA file, unless
        // it names one.
        let mut exec = vec!["A=1".to_string()];
        set_exec_env(&guest.share, &mut exec);
        assert_eq!(
            exec,
            ["A=1", "NODE_EXTRA_CA_CERTS=/run/feos/intercept-ca.pem"]
        );
        let mut exec = vec!["NODE_EXTRA_CA_CERTS=/x".to_string()];
        set_exec_env(&guest.share, &mut exec);
        assert_eq!(exec, ["NODE_EXTRA_CA_CERTS=/x"]);
    }

    #[test]
    fn a_change_rewrites_the_files_in_place() {
        let guest = guest();
        publish(&guest.share, &guest.containers, &[CA1.into()]).unwrap();
        let dir = guest.container("main");
        let root = image(&[("/etc/ssl/certs/ca-certificates.crt", "SYSTEM\n")], &[]);
        prepare(&guest.share, &dir, root.path()).unwrap();
        // A container without a bundle, made before the CAs.
        guest.container("plain");
        let bundle = dir.join(DIR).join(BUNDLE);
        let inodes = |paths: &[&Path]| -> Vec<u64> {
            paths
                .iter()
                .map(|p| std::fs::metadata(p).unwrap().ino())
                .collect()
        };
        let ca_file = guest.share.join(CA_FILE);
        let before = inodes(&[&ca_file, &bundle]);
        // A reader that opened the files before sees the new content.
        let held = File::open(&bundle).unwrap();

        let failed = publish(&guest.share, &guest.containers, &[CA1.into(), CA2.into()]).unwrap();
        assert!(failed.is_empty(), "{failed:?}");
        assert_eq!(inodes(&[&ca_file, &bundle]), before, "never renamed");
        let mut seen = String::new();
        (&held).read_to_string(&mut seen).unwrap();
        assert_eq!(seen, format!("SYSTEM\n{CA1}{CA2}\n"));
        assert!(!guest.containers.join("plain").join(DIR).exists());

        // Fewer CAs: the files shrink.
        publish(&guest.share, &guest.containers, &[CA2.into()]).unwrap();
        assert_eq!(
            std::fs::read_to_string(&bundle).unwrap(),
            format!("SYSTEM\n{CA2}\n")
        );
        assert_eq!(
            std::fs::read_to_string(&ca_file).unwrap(),
            format!("{CA2}\n")
        );
    }
}
