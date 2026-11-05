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
//! environment names them where neither the container's spec nor its
//! image's `Env` does: `SSL_CERT_FILE` and `REQUESTS_CA_BUNDLE` the bundle,
//! and `NODE_EXTRA_CA_CERTS` the CA file; and so does every process
//! started in it later, which takes the environment of its runtime spec.
//!
//! A container created before the guest held CAs, as a sandbox's are, has
//! no bundle mounted, and its runtime spec names none. A process started
//! in it later, by an exec, gets one in the share directory instead:
//! `intercept/<id>.pem`, which it sees in [`EXEC_DEST`], of the system
//! bundle its root filesystem has at that exec and the CAs, named by
//! `SSL_CERT_FILE` and `REQUESTS_CA_BUNDLE` where its environment names
//! none, beside `NODE_EXTRA_CA_CERTS`. Nothing covers its own bundle, so
//! its package manager can still replace it, and the next exec takes the
//! new one. The system bundle it was made of is kept in the container's
//! directory, as `intercept/exec-system.pem`.
//!
//! A change of the CAs rewrites the CA file and every container's mounted
//! bundle in place, truncated and written, never by a rename: a bind mount
//! of a file holds the inode it was made with. The exec bundles in the
//! share directory, which no mount holds, are replaced by a rename, so a
//! reader sees the old one or the new one, whole. A process that read its
//! store before reads the new CAs after its restart.
//!
//! Containers of a pod without CAs are left as they are.

use feos_utils::fs::openat2_confined;
use nix::fcntl::{OFlag, ResolveFlag};
use nix::sys::stat::Mode;
use serde_json::{json, Value};
use std::collections::HashMap;
use std::fs::{File, OpenOptions};
use std::io::{self, Read, Write};
use std::os::fd::AsRawFd;
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
/// The system bundle a container's exec bundle was made of, in `DIR`.
const EXEC_SYSTEM: &str = "exec-system.pem";
/// The exec bundles' directory in the share directory, and where a
/// container sees it.
const EXEC_DIR: &str = "intercept";
pub const EXEC_DEST: &str = "/run/feos/intercept";
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
    write_in_place(&dir.join(SYSTEM), &system)?;
    let bundle = dir.join(BUNDLE);
    write_in_place(&bundle, &bundle_of(&system, &cas))?;
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
/// states it) sets them, or `image` (its image's `Env`) does to a path of
/// its own: that one stays, or comes back where an earlier injection
/// replaced it. A value of an earlier injection is replaced.
pub fn set_env(
    env: &mut Vec<String>,
    injection: &Injection,
    spec: &HashMap<String, String>,
    image: &[String],
) {
    for (name, value) in &injection.env {
        if spec.contains_key(name) {
            continue;
        }
        let prefix = format!("{name}=");
        let value_in = |entries: &[String]| {
            entries
                .iter()
                .rev()
                .find_map(|entry| entry.strip_prefix(&prefix))
                .map(str::to_string)
        };
        let value = match value_in(image).filter(|own| !injected(own)) {
            // The image's: what is there stays, unless it is an injection's.
            Some(own) => match value_in(env) {
                Some(now) if !injected(&now) => continue,
                _ => own,
            },
            None => value.clone(),
        };
        env.retain(|entry| !entry.starts_with(&prefix));
        env.push(format!("{name}={value}"));
    }
}

/// Whether `value` is a path an injection names: a system bundle it
/// mounts over, the CA file, or an exec bundle.
fn injected(value: &str) -> bool {
    SYSTEM_BUNDLES.contains(&value)
        || value == CA_PATH
        || value
            .strip_prefix(EXEC_DEST)
            .is_some_and(|rest| rest.starts_with('/'))
}

/// An exec's environment `env` in the container `id`, whose directory is
/// `container` and whose root filesystem is `rootfs`, while the guest
/// holds CAs in `share`: the CA file where `env` names none, and where it
/// names no bundle, the container's exec bundle, made first or brought up
/// to date with its system bundle. A container created with the CAs names
/// its mounted bundle already. Without CAs, `env` stays as it is. Returns
/// why the exec bundle could not be made; `env` then names the CA file
/// alone.
pub fn set_exec_env(
    share: &Path,
    id: &str,
    container: &Path,
    rootfs: &Path,
    env: &mut Vec<String>,
) -> Result<(), String> {
    let _change = lock();
    let Some(cas) = cas_in(share) else {
        return Ok(());
    };
    if !names(env, CA_VARIABLE) {
        env.push(format!("{CA_VARIABLE}={CA_PATH}"));
    }
    let unnamed: Vec<&str> = BUNDLE_VARIABLES
        .iter()
        .copied()
        .filter(|name| !names(env, name))
        .collect();
    if unnamed.is_empty() {
        return Ok(());
    }
    let root = File::open(rootfs).map_err(|e| format!("{}: {e}", rootfs.display()))?;
    let system = match SYSTEM_BUNDLES.iter().find(|path| is_file_in(&root, path)) {
        Some(path) => read_in(&root, path)?,
        None => Vec::new(),
    };
    // The system bundle is kept for a change of the CAs, which has no
    // exec to read it at.
    let dir = container.join(DIR);
    let kept = dir.join(EXEC_SYSTEM);
    if !std::fs::read(&kept).is_ok_and(|kept| kept == system) {
        std::fs::create_dir_all(&dir).map_err(|e| format!("{}: {e}", dir.display()))?;
        write_in_place(&kept, &system)?;
    }
    let bundle = share.join(EXEC_DIR).join(exec_bundle_name(id));
    let content = bundle_of(&system, &cas);
    if !std::fs::read(&bundle).is_ok_and(|now| now == content) {
        write_replacing(&bundle, &content)?;
    }
    for name in unnamed {
        env.push(format!("{name}={EXEC_DEST}/{}", exec_bundle_name(id)));
    }
    Ok(())
}

/// Removes the exec bundle of the container `id` from `share`, if it has
/// one; its directory goes with the container.
pub fn forget(share: &Path, id: &str) -> io::Result<()> {
    match std::fs::remove_file(share.join(EXEC_DIR).join(exec_bundle_name(id))) {
        Err(e) if e.kind() != io::ErrorKind::NotFound => Err(e),
        _ => Ok(()),
    }
}

fn exec_bundle_name(id: &str) -> String {
    format!("{id}.pem")
}

/// Whether `env`, a process's `KEY=value` entries, names `name`.
fn names(env: &[String], name: &str) -> bool {
    let prefix = format!("{name}=");
    env.iter().any(|entry| entry.starts_with(&prefix))
}

/// Sets the guest's CAs, `cas` oldest first: the share directory's file
/// and the bundles of every container below `containers` that has one,
/// the mounted bundle in place, the exec bundle in the share directory
/// replaced. Returns the containers whose bundle could not be rewritten,
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
        let exec_bundle = share.join(EXEC_DIR).join(exec_bundle_name(&id));
        for result in [
            rewrite(&dir.join(SYSTEM), &joined, |bundle| {
                write_in_place(&dir.join(BUNDLE), bundle)
            }),
            rewrite(&dir.join(EXEC_SYSTEM), &joined, |bundle| {
                write_replacing(&exec_bundle, bundle)
            }),
        ] {
            if let Err(e) = result {
                failed.push((id.clone(), e));
            }
        }
    }
    Ok(failed)
}

/// Writes, with `write`, a bundle of the system bundle kept at `system`
/// and `cas`, if one is kept there.
fn rewrite(
    system: &Path,
    cas: &str,
    write: impl FnOnce(&[u8]) -> Result<(), String>,
) -> Result<(), String> {
    match std::fs::read(system) {
        Ok(kept) => write(&bundle_of(&kept, cas)),
        Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(()),
        Err(e) => Err(format!("{}: {e}", system.display())),
    }
}

/// A bundle of `system` and `cas`, each block on lines of its own. A
/// system bundle is bytes as the image has them, text or not.
fn bundle_of(system: &[u8], cas: &str) -> Vec<u8> {
    let mut bundle = system.to_vec();
    if !bundle.is_empty() && !bundle.ends_with(b"\n") {
        bundle.push(b'\n');
    }
    bundle.extend_from_slice(with_newline(cas).as_bytes());
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

/// Replaces the file at `path` by a new one with `content`, made 0644 in
/// its directory (0755, made if it is not there) and renamed over it: a
/// reader sees the old file or the new one, whole.
fn write_replacing(path: &Path, content: &[u8]) -> Result<(), String> {
    let (Some(dir), Some(name)) = (path.parent(), path.file_name()) else {
        return Err(format!("{}: not a file's path", path.display()));
    };
    feos_utils::share::make_share_dir(dir).map_err(|e| format!("{}: {e}", dir.display()))?;
    // A dot first and no ".pem" last: never another container's bundle.
    let staged = dir.join(format!(".{}.new", name.to_string_lossy()));
    write_in_place(&staged, content)?;
    std::fs::rename(&staged, path).map_err(|e| format!("{}: {e}", path.display()))
}

/// Whether `path`, as the container sees it in its root filesystem
/// `root`, is a regular file; its links are followed inside the root.
fn is_file_in(root: &File, path: &str) -> bool {
    open_in(root, path).is_ok()
}

fn read_in(root: &File, path: &str) -> Result<Vec<u8>, String> {
    let mut bytes = Vec::new();
    open_in(root, path)
        .and_then(|file| file.take(MAX_SYSTEM_BUNDLE).read_to_end(&mut bytes))
        .map_err(|e| format!("the image's {path}: {e}"))?;
    Ok(bytes)
}

/// Opens `path` in the root `root`, an image's or a running container's,
/// its links resolved inside it as the container would see them, if it is
/// a regular file. A container may have put anything there, so it is
/// opened as a path first and read only once it is known to be a file: a
/// FIFO would block the read, and a device node would read the guest's
/// device.
fn open_in(root: &File, path: &str) -> io::Result<File> {
    let relative = PathBuf::from(path.trim_start_matches('/'));
    let fd = openat2_confined(
        root,
        &relative,
        OFlag::O_PATH,
        Mode::empty(),
        ResolveFlag::RESOLVE_IN_ROOT | ResolveFlag::RESOLVE_NO_MAGICLINKS,
    )?;
    let found = File::from(fd);
    if !found.metadata()?.is_file() {
        return Err(io::Error::other("not a regular file"));
    }
    // The same inode, now open for reading.
    File::open(format!("/proc/self/fd/{}", found.as_raw_fd()))
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
        set_exec_env(&guest.share, "main", &dir, root.path(), &mut env).unwrap();
        assert_eq!(env, ["A=1"]);
        assert!(!guest.share.join(EXEC_DIR).exists());
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
        set_env(&mut env, &injection, &spec, &[]);
        assert_eq!(
            env,
            [
                "PATH=/bin",
                "REQUESTS_CA_BUNDLE=/mine.pem",
                "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt",
                "NODE_EXTRA_CA_CERTS=/run/feos/intercept-ca.pem",
            ]
        );
        // An exec there takes the spec's environment, which names the
        // bundle: no exec bundle.
        set_exec_env(&guest.share, "main", &dir, root.path(), &mut env).unwrap();
        assert_eq!(env.len(), 4);
        assert!(!guest.share.join(EXEC_DIR).exists());
    }

    #[test]
    fn the_images_own_variables_win_and_come_back() {
        let guest = guest();
        publish(&guest.share, &guest.containers, &[CA1.into()]).unwrap();
        let dir = guest.container("main");
        let root = image(&[("/etc/ssl/certs/ca-certificates.crt", "SYSTEM\n")], &[]);
        let injection = prepare(&guest.share, &dir, root.path()).unwrap();
        let image = [
            "PATH=/bin".to_string(),
            "SSL_CERT_FILE=/etc/custom.pem".to_string(),
            "NODE_EXTRA_CA_CERTS=/etc/node.pem".to_string(),
            // A path an injection names is no image's own.
            "REQUESTS_CA_BUNDLE=/etc/pki/tls/certs/ca-bundle.crt".to_string(),
        ];
        let injected = [
            "SSL_CERT_FILE=/etc/custom.pem",
            "NODE_EXTRA_CA_CERTS=/etc/node.pem",
            "PATH=/bin",
            "REQUESTS_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt",
        ];
        // A create: the image's environment.
        let mut env = image.to_vec();
        set_env(&mut env, &injection, &HashMap::new(), &image);
        let mut sorted = env.clone();
        sorted.sort();
        let mut want = injected.map(String::from).to_vec();
        want.sort();
        assert_eq!(sorted, want);
        // An adopted container an older guest set every variable of: the
        // image's come back, an exec bundle's is replaced.
        let mut env = vec![
            "PATH=/bin".to_string(),
            "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt".to_string(),
            "REQUESTS_CA_BUNDLE=/run/feos/intercept/main.pem".to_string(),
            format!("NODE_EXTRA_CA_CERTS={CA_PATH}"),
        ];
        set_env(&mut env, &injection, &HashMap::new(), &image);
        let mut sorted = env.clone();
        sorted.sort();
        assert_eq!(sorted, want);
        // Set again, nothing changes.
        set_env(&mut env, &injection, &HashMap::new(), &image);
        env.sort();
        assert_eq!(env, want);
        // The spec's own still win over the image's.
        let spec = HashMap::from([("SSL_CERT_FILE".to_string(), "/mine.pem".to_string())]);
        let mut env = vec!["SSL_CERT_FILE=/mine.pem".to_string()];
        set_env(&mut env, &injection, &spec, &image);
        assert!(env.contains(&"SSL_CERT_FILE=/mine.pem".to_string()));
    }

    /// The environment of an exec into the container `id`, whose own is
    /// `env`.
    fn exec(guest: &Guest, id: &str, root: &Path, env: &[&str]) -> Vec<String> {
        let mut env: Vec<String> = env.iter().map(|e| e.to_string()).collect();
        set_exec_env(&guest.share, id, &guest.container(id), root, &mut env).unwrap();
        env
    }

    #[test]
    fn an_exec_in_a_container_made_before_the_cas_gets_a_bundle_in_the_share_directory() {
        let guest = guest();
        let root = image(&[("/etc/ssl/certs/ca-certificates.crt", "SYSTEM\n")], &[]);
        // Created before the CAs: nothing.
        let dir = guest.container("early");
        assert_eq!(
            prepare(&guest.share, &dir, root.path()).unwrap(),
            Injection::default()
        );
        publish(&guest.share, &guest.containers, &[CA1.into()]).unwrap();

        let env = exec(&guest, "early", root.path(), &["A=1"]);
        assert_eq!(
            env,
            [
                "A=1",
                "NODE_EXTRA_CA_CERTS=/run/feos/intercept-ca.pem",
                "SSL_CERT_FILE=/run/feos/intercept/early.pem",
                "REQUESTS_CA_BUNDLE=/run/feos/intercept/early.pem",
            ]
        );
        let bundle = guest.share.join(EXEC_DIR).join("early.pem");
        assert_eq!(
            std::fs::read_to_string(&bundle).unwrap(),
            format!("SYSTEM\n{CA1}")
        );
        // Its own bundle is its own: nothing covers or changes it.
        assert_eq!(
            std::fs::read_to_string(root.path().join("etc/ssl/certs/ca-certificates.crt")).unwrap(),
            "SYSTEM\n"
        );
        // What the exec names stays.
        let env = exec(
            &guest,
            "early",
            root.path(),
            &["SSL_CERT_FILE=/mine.pem", "NODE_EXTRA_CA_CERTS=/x"],
        );
        assert_eq!(
            env,
            [
                "SSL_CERT_FILE=/mine.pem",
                "NODE_EXTRA_CA_CERTS=/x",
                "REQUESTS_CA_BUNDLE=/run/feos/intercept/early.pem",
            ]
        );

        // Its package manager replaces its bundle: the next exec takes it,
        // and the exec bundle is replaced, never rewritten under a reader.
        let held = File::open(&bundle).unwrap();
        std::fs::write(
            root.path().join("etc/ssl/certs/ca-certificates.crt"),
            "SYSTEM2\n",
        )
        .unwrap();
        exec(&guest, "early", root.path(), &[]);
        assert_eq!(
            std::fs::read_to_string(&bundle).unwrap(),
            format!("SYSTEM2\n{CA1}")
        );
        let mut seen = String::new();
        (&held).read_to_string(&mut seen).unwrap();
        assert_eq!(seen, format!("SYSTEM\n{CA1}"), "the old one, whole");

        // A change of the CAs replaces it with the system bundle it was
        // made of; without CAs it is that bundle alone.
        let failed = publish(&guest.share, &guest.containers, &[CA1.into(), CA2.into()]).unwrap();
        assert!(failed.is_empty(), "{failed:?}");
        assert_eq!(
            std::fs::read_to_string(&bundle).unwrap(),
            format!("SYSTEM2\n{CA1}{CA2}\n")
        );
        // A bundle a change could not rewrite is mended by the next exec.
        std::fs::write(&bundle, "STALE").unwrap();
        exec(&guest, "early", root.path(), &[]);
        assert_eq!(
            std::fs::read_to_string(&bundle).unwrap(),
            format!("SYSTEM2\n{CA1}{CA2}\n")
        );
        publish(&guest.share, &guest.containers, &[]).unwrap();
        assert_eq!(std::fs::read_to_string(&bundle).unwrap(), "SYSTEM2\n");
        assert_eq!(exec(&guest, "early", root.path(), &["A=1"]), ["A=1"]);
        let staged: Vec<_> = std::fs::read_dir(guest.share.join(EXEC_DIR))
            .unwrap()
            .map(|e| e.unwrap().file_name())
            .collect();
        assert_eq!(staged, ["early.pem"], "nothing staged is left");

        forget(&guest.share, "early").unwrap();
        assert!(!bundle.exists());
        forget(&guest.share, "early").unwrap();
    }

    #[test]
    fn a_container_reads_only_regular_files_of_its_root() {
        let guest = guest();
        publish(&guest.share, &guest.containers, &[CA1.into()]).unwrap();
        // A FIFO where the first bundle is would block a read; it is
        // passed over for the next.
        let root = image(&[("/etc/ssl/cert.pem", "LIBRE\n")], &[]);
        let fifo = root.path().join("etc/ssl/certs/ca-certificates.crt");
        std::fs::create_dir_all(fifo.parent().unwrap()).unwrap();
        nix::unistd::mkfifo(&fifo, Mode::from_bits_truncate(0o644)).unwrap();
        exec(&guest, "odd", root.path(), &[]);
        assert_eq!(
            std::fs::read_to_string(guest.share.join(EXEC_DIR).join("odd.pem")).unwrap(),
            format!("LIBRE\n{CA1}")
        );
        // A directory is no bundle either.
        let root = image(&[], &[]);
        std::fs::create_dir_all(root.path().join("etc/ssl/cert.pem")).unwrap();
        exec(&guest, "dir", root.path(), &[]);
        assert_eq!(
            std::fs::read_to_string(guest.share.join(EXEC_DIR).join("dir.pem")).unwrap(),
            CA1
        );
    }

    /// A bundle is bytes: one that is no UTF-8, as a comment in Latin-1
    /// makes it, is taken as it is, at a create, an exec and a change.
    #[test]
    fn a_system_bundle_that_is_no_text_is_taken_as_it_is() {
        let guest = guest();
        publish(&guest.share, &guest.containers, &[CA1.into()]).unwrap();
        let system: &[u8] = b"# Certificat racine \xe9mis\nSYSTEM";
        let root = tempfile::tempdir().unwrap();
        let path = root.path().join("etc/ssl/certs/ca-certificates.crt");
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        std::fs::write(&path, system).unwrap();
        let dir = guest.container("main");
        prepare(&guest.share, &dir, root.path()).unwrap();
        let with = |cas: &str| [system, b"\n", cas.as_bytes()].concat();
        assert_eq!(
            std::fs::read(dir.join(DIR).join(BUNDLE)).unwrap(),
            with(CA1)
        );
        let early = guest.container("early");
        let mut env = Vec::new();
        set_exec_env(&guest.share, "early", &early, root.path(), &mut env).unwrap();
        let exec = guest.share.join(EXEC_DIR).join("early.pem");
        assert_eq!(std::fs::read(&exec).unwrap(), with(CA1));
        let failed = publish(&guest.share, &guest.containers, &[CA2.into()]).unwrap();
        assert!(failed.is_empty(), "{failed:?}");
        let ca2 = format!("{CA2}\n");
        assert_eq!(
            std::fs::read(dir.join(DIR).join(BUNDLE)).unwrap(),
            with(&ca2)
        );
        assert_eq!(std::fs::read(&exec).unwrap(), with(&ca2));
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
