// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A container's directory and OCI bundle.
//!
//! Layout below [`CONTAINERS_DIR`]/<id>/:
//!
//! ```text
//! bundle/rootfs/       overlay: the image's rootfs below, upper/ above
//! bundle/config.json   OCI runtime spec
//! upper/, work/        the container's writes, on the data root
//! logs/container.log   stdout and stderr of the container's process, in the
//!                      CRI format (crate::output); container.log.id its ID
//! logs/stdout, stderr  FIFOs the container's process writes to
//! ```
//!
//! Every container sees the FeOS toolbox read-only at /feos-tools and the
//! share directory read-only at /run/feos; its /etc/resolv.conf links to
//! the resolvers in the share directory (feos_utils::resolver). The pod's
//! volumes it mounts are bind mounts of where the guest mounted them.

use feos_proto::container_service::VolumeMount;
use feos_utils::filesystem::{volume, CONTAINERS_DIR};
use feos_utils::resolver;
use nix::mount::{mount, umount2, MntFlags, MsFlags};
use serde_json::{json, Value};
use std::collections::HashMap;
use std::path::{Path, PathBuf};

/// The toolbox in the guest: static uutils coreutils.
const TOOLS_DIR: &str = "/usr/lib/feos-tools";
/// Where a container sees the toolbox.
const TOOLS_DEST: &str = "feos-tools";
const DEFAULT_PATH: &str = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin";
const TOOLBOX_PATH: &str = "/feos-tools/bin";

pub fn container_dir(id: &str) -> PathBuf {
    Path::new(CONTAINERS_DIR).join(id)
}

pub fn bundle_dir(id: &str) -> PathBuf {
    container_dir(id).join("bundle")
}

pub fn log_file(id: &str) -> PathBuf {
    container_dir(id).join("logs/container.log")
}

/// Whether a container's directory holds a finished bundle that a reboot
/// left behind and that can be adopted.
pub fn adoptable(id: &str) -> bool {
    bundle_dir(id).join("config.json").is_file()
}

/// What the container runs.
pub struct Process {
    pub args: Vec<String>,
    pub env: Vec<String>,
    pub cwd: String,
}

/// The process of a container from the image config and the request:
/// `command` replaces the image's entrypoint and command, `env` is merged
/// onto the image's environment. The toolbox goes last on the PATH, so that
/// the image's own binaries win.
pub fn process(
    image_dir: &Path,
    command: &[String],
    env: &HashMap<String, String>,
) -> Result<Process, String> {
    let raw = std::fs::read_to_string(image_dir.join("config.json"))
        .map_err(|e| format!("image config: {e}"))?;
    let image: Value = serde_json::from_str(&raw).map_err(|e| format!("image config: {e}"))?;
    let config = &image["config"];
    let strings = |v: &Value| -> Vec<String> {
        v.as_array()
            .map(|a| {
                a.iter()
                    .filter_map(|x| x.as_str().map(String::from))
                    .collect()
            })
            .unwrap_or_default()
    };

    let mut args = strings(&config["Entrypoint"]);
    args.extend(strings(&config["Cmd"]));
    if !command.is_empty() {
        args = command.to_vec();
    }
    if args.is_empty() {
        return Err("no command in the request, and the image has no entrypoint or command".into());
    }

    let mut vars = strings(&config["Env"]);
    let mut overrides: Vec<_> = env.iter().collect();
    overrides.sort();
    for (key, value) in overrides {
        vars.retain(|v| !v.starts_with(&format!("{key}=")));
        vars.push(format!("{key}={value}"));
    }
    match vars.iter_mut().find(|v| v.starts_with("PATH=")) {
        Some(path) if !path.contains(TOOLBOX_PATH) => path.push_str(&format!(":{TOOLBOX_PATH}")),
        Some(_) => {}
        None => vars.push(format!("PATH={DEFAULT_PATH}:{TOOLBOX_PATH}")),
    }

    feos_utils::share::set_ssh_agent_env(&mut vars);

    let cwd = config["WorkingDir"]
        .as_str()
        .filter(|dir| !dir.is_empty())
        .unwrap_or("/")
        .to_string();
    Ok(Process {
        args,
        env: vars,
        cwd,
    })
}

/// The mounts of the pod's volumes into a container (ADR 0016): bind
/// mounts of the directories the guest mounted them at, read-only where the
/// container says so, without devices and setuid. A volume that is not
/// mounted is an error: the container would write into memory.
pub fn volume_mounts(mounts: &[VolumeMount]) -> Result<Vec<Value>, String> {
    volume_mounts_in(Path::new(volume::VOLUMES_DIR), mounts)
}

fn volume_mounts_in(dir: &Path, mounts: &[VolumeMount]) -> Result<Vec<Value>, String> {
    let mut paths = std::collections::HashSet::new();
    mounts
        .iter()
        .map(|mount| {
            if !volume::valid_name(&mount.volume) {
                return Err(format!("invalid volume name {:?}", mount.volume));
            }
            if !volume::valid_mount_path(&mount.path) {
                return Err(format!(
                    "volume {} cannot be mounted at {:?}: an absolute path other than \"/\"",
                    mount.volume, mount.path
                ));
            }
            if !paths.insert(mount.path.as_str()) {
                return Err(format!("two volumes are mounted at {}", mount.path));
            }
            let source = dir.join(&mount.volume);
            if !volume::is_mount_point(&source) {
                return Err(format!("volume {} is not mounted", mount.volume));
            }
            let access = if mount.read_only { "ro" } else { "rw" };
            Ok(json!({
                "destination": mount.path,
                "type": "bind",
                "source": source,
                "options": ["bind", access, "nosuid", "nodev"],
            }))
        })
        .collect()
}

/// The OCI runtime spec, with the volumes' mounts from [`volume_mounts`]
/// after FeOS's own. No network namespace: containers share the host's
/// network.
pub fn runtime_spec(id: &str, process: &Process, volumes: &[Value]) -> Value {
    let namespace = |kind: &str| json!({ "type": kind });
    let mut spec = json!({
        "ociVersion": "1.0.2",
        "process": {
            "terminal": false,
            "user": { "uid": 0, "gid": 0 },
            "args": process.args,
            "env": process.env,
            "cwd": process.cwd,
        },
        "root": { "path": "rootfs", "readonly": false },
        "hostname": id,
        "mounts": [
            { "destination": "/proc", "type": "proc", "source": "proc", "options": [] },
            { "destination": "/dev", "type": "tmpfs", "source": "tmpfs",
              "options": ["nosuid", "strictatime", "mode=755", "size=65536k"] },
            { "destination": "/dev/pts", "type": "devpts", "source": "devpts",
              "options": ["nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"] },
            { "destination": "/dev/shm", "type": "tmpfs", "source": "shm",
              "options": ["nosuid", "noexec", "nodev", "mode=1777", "size=65536k"] },
            { "destination": "/sys", "type": "sysfs", "source": "sysfs",
              "options": ["nosuid", "noexec", "nodev", "ro"] },
            // The directory, not the files in it, so that files created
            // later show up inside too. Read-only: a container can neither
            // remove them nor plant anything next to them.
            { "destination": resolver::SHARE_DEST, "type": "bind", "source": resolver::SHARE_DIR,
              "options": ["bind", "ro", "nosuid", "nodev", "noexec"] },
        ],
        "linux": {
            "namespaces": [namespace("pid"), namespace("ipc"), namespace("uts"), namespace("mount")],
            "maskedPaths": [
                "/proc/acpi", "/proc/asound", "/proc/kcore", "/proc/keys",
                "/proc/latency_stats", "/proc/timer_list", "/proc/scsi"
            ],
            "readonlyPaths": [
                "/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger"
            ],
        },
    });
    if let Some(mounts) = spec["mounts"].as_array_mut() {
        mounts.extend(volumes.iter().cloned());
    }
    spec
}

/// The process of an existing bundle: an adopted container keeps the
/// command and environment it was created with, unless the request sets
/// new ones.
pub fn previous_process(id: &str) -> Option<(Vec<String>, Vec<String>)> {
    let raw = std::fs::read_to_string(bundle_dir(id).join("config.json")).ok()?;
    let spec: Value = serde_json::from_str(&raw).ok()?;
    let strings = |v: &Value| -> Vec<String> {
        v.as_array()
            .map(|a| {
                a.iter()
                    .filter_map(|x| x.as_str().map(String::from))
                    .collect()
            })
            .unwrap_or_default()
    };
    Some((
        strings(&spec["process"]["args"]),
        strings(&spec["process"]["env"]),
    ))
}

/// Sets up the container's directory, mounts and spec. With `adopt`, what a
/// previous boot left (upper/, work/, logs/) stays, and a resolver link that
/// cannot be made is returned instead of failing: the container's data is
/// worth more than its resolver configuration. Returns that reason, or an
/// empty string.
pub fn provision(id: &str, image_dir: &Path, spec: &Value, adopt: bool) -> Result<String, String> {
    let dir = container_dir(id);
    let rootfs = dir.join("bundle/rootfs");
    if adopt {
        // Mounts of a previous life, e.g. of an adoption that failed halfway.
        unmount(id);
    }
    for d in [
        rootfs.clone(),
        dir.join("upper"),
        dir.join("work"),
        dir.join("logs"),
    ] {
        std::fs::create_dir_all(&d).map_err(|e| format!("{}: {e}", d.display()))?;
    }

    let lower = image_dir.join("rootfs");
    let options = format!(
        "lowerdir={},upperdir={},workdir={}",
        lower.display(),
        dir.join("upper").display(),
        dir.join("work").display()
    );
    mount(
        Some("overlay"),
        &rootfs,
        Some("overlay"),
        MsFlags::empty(),
        Some(options.as_str()),
    )
    .map_err(|e| format!("overlay mount: {e}"))?;

    let result = (|| {
        mount_toolbox(&rootfs)?;
        // The spec bind-mounts the share directory; it has to exist then.
        resolver::make_share_dir(Path::new(resolver::SHARE_DIR))
            .map_err(|e| format!("{}: {e}", resolver::SHARE_DIR))?;
        let resolver_error = match resolver::provision(&rootfs) {
            Ok(()) => String::new(),
            Err(e) if adopt => {
                log::warn!("Container {id}: {e} (adopted with what is there)");
                e
            }
            Err(e) => return Err(e),
        };
        let spec = serde_json::to_string_pretty(spec).map_err(|e| format!("spec: {e}"))?;
        std::fs::write(dir.join("bundle/config.json"), spec)
            .map_err(|e| format!("config.json: {e}"))?;
        Ok(resolver_error)
    })();
    if result.is_err() {
        unmount(id);
    }
    result
}

/// The toolbox, bind-mounted read-only. A guest without one (the host
/// profile) simply has none.
fn mount_toolbox(rootfs: &Path) -> Result<(), String> {
    let tools = Path::new(TOOLS_DIR);
    if !tools.is_dir() {
        return Ok(());
    }
    let dest = rootfs.join(TOOLS_DEST);
    std::fs::create_dir_all(&dest).map_err(|e| format!("{}: {e}", dest.display()))?;
    mount(
        Some(tools),
        &dest,
        None::<&str>,
        MsFlags::MS_BIND,
        None::<&str>,
    )
    .map_err(|e| format!("toolbox mount: {e}"))?;
    // MS_RDONLY takes effect only on a remount of the bind mount.
    mount(
        None::<&str>,
        &dest,
        None::<&str>,
        MsFlags::MS_BIND | MsFlags::MS_REMOUNT | MsFlags::MS_RDONLY,
        None::<&str>,
    )
    .map_err(|e| format!("toolbox read-only: {e}"))
}

/// Unmounts the toolbox and the overlay. What is not mounted is ignored.
pub fn unmount(id: &str) {
    let rootfs = bundle_dir(id).join("rootfs");
    let _ = umount2(&rootfs.join(TOOLS_DEST), MntFlags::MNT_DETACH);
    let _ = umount2(&rootfs, MntFlags::MNT_DETACH);
}

/// Unmounts everything and removes the container's directory and its exec
/// bundle of the interception CAs.
pub fn remove(id: &str) -> Result<(), String> {
    unmount(id);
    super::intercept::forget(Path::new(resolver::SHARE_DIR), id)
        .map_err(|e| format!("the exec bundle of {id}: {e}"))?;
    match std::fs::remove_dir_all(container_dir(id)) {
        Err(e) if e.kind() != std::io::ErrorKind::NotFound => {
            Err(format!("{}: {e}", container_dir(id).display()))
        }
        _ => Ok(()),
    }
}

/// The pod's interception CAs for the container `id` of the image in
/// `image_dir` (ADR 0028 §5, `intercept`): its bundle's mounts appended
/// to `mounts`, and their variables set in `env` where `spec`, the
/// container's environment as its spec states it, does not, nor its
/// image's own. Nothing while the guest holds no CAs.
pub fn intercept(
    id: &str,
    image_dir: &Path,
    spec: &HashMap<String, String>,
    env: &mut Vec<String>,
    mounts: &mut Vec<Value>,
) -> Result<(), String> {
    let injection = super::intercept::prepare(
        Path::new(resolver::SHARE_DIR),
        &container_dir(id),
        &image_dir.join("rootfs"),
    )?;
    super::intercept::set_env(env, &injection, spec, &image_env(image_dir));
    mounts.extend(injection.mounts);
    Ok(())
}

/// The image's own environment, its config's `Env`; none when it does not
/// read.
fn image_env(image_dir: &Path) -> Vec<String> {
    let image: Option<Value> = std::fs::read_to_string(image_dir.join("config.json"))
        .ok()
        .and_then(|raw| serde_json::from_str(&raw).ok());
    image
        .as_ref()
        .and_then(|image| image["config"]["Env"].as_array())
        .map(|env| {
            env.iter()
                .filter_map(|entry| entry.as_str().map(String::from))
                .collect()
        })
        .unwrap_or_default()
}

/// The environment of an exec: the container's own, `overrides` on top, and
/// a PATH if neither sets one; where the guest holds interception CAs, the
/// pod's CA file and the container's exec bundle where neither names them
/// (ADR 0028 §5). An exec bundle that cannot be made is logged; the exec
/// runs without it.
pub fn exec_env(id: &str, overrides: &HashMap<String, String>) -> Vec<String> {
    let mut env = previous_process(id).map(|(_, env)| env).unwrap_or_default();
    let mut overrides: Vec<_> = overrides.iter().collect();
    overrides.sort();
    for (key, value) in overrides {
        env.retain(|v| !v.starts_with(&format!("{key}=")));
        env.push(format!("{key}={value}"));
    }
    if !env.iter().any(|v| v.starts_with("PATH=")) {
        env.push(format!("PATH={DEFAULT_PATH}:{TOOLBOX_PATH}"));
    }
    feos_utils::share::set_ssh_agent_env(&mut env);
    if let Err(e) = super::intercept::set_exec_env(
        Path::new(resolver::SHARE_DIR),
        id,
        &container_dir(id),
        &bundle_dir(id).join("rootfs"),
        &mut env,
    ) {
        log::warn!("Container {id}: An exec without the interception CAs' bundle: {e}");
    }
    env
}

#[cfg(test)]
mod tests {
    use super::*;

    fn image(config: Value) -> tempfile::TempDir {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(
            dir.path().join("config.json"),
            json!({ "config": config }).to_string(),
        )
        .unwrap();
        dir
    }

    #[test]
    fn the_image_defines_the_process() {
        let dir = image(json!({
            "Entrypoint": ["/entry"], "Cmd": ["serve"],
            "Env": ["PATH=/usr/bin", "A=1"], "WorkingDir": "/app"
        }));
        let p = process(dir.path(), &[], &HashMap::new()).unwrap();
        assert_eq!(p.args, ["/entry", "serve"]);
        assert_eq!(
            p.env,
            [
                "PATH=/usr/bin:/feos-tools/bin",
                "A=1",
                "SSH_AUTH_SOCK=/run/feos/ssh-agent.sock"
            ]
        );
        assert_eq!(p.cwd, "/app");
    }

    #[test]
    fn the_request_overrides_the_image() {
        let dir = image(json!({ "Cmd": ["sh"], "Env": ["A=1", "B=2"] }));
        let env = HashMap::from([("A".to_string(), "9".to_string())]);
        let p = process(dir.path(), &["sleep".to_string(), "1".to_string()], &env).unwrap();
        assert_eq!(p.args, ["sleep", "1"]);
        assert_eq!(
            p.env,
            [
                "B=2",
                "A=9",
                "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/feos-tools/bin",
                "SSH_AUTH_SOCK=/run/feos/ssh-agent.sock"
            ]
        );
        assert_eq!(p.cwd, "/");
    }

    #[test]
    fn volumes_are_bind_mounted_once_they_are_mounted() {
        let mount = |volume: &str, path: &str, read_only: bool| VolumeMount {
            volume: volume.to_string(),
            path: path.to_string(),
            read_only,
        };
        // /proc and /sys stand for volumes the guest mounted.
        let root = Path::new("/");
        let mounts = volume_mounts_in(
            root,
            &[mount("proc", "/data", false), mount("sys", "/ro", true)],
        )
        .unwrap();
        assert_eq!(
            mounts[0],
            json!({"destination": "/data", "type": "bind", "source": "/proc",
                   "options": ["bind", "rw", "nosuid", "nodev"]})
        );
        assert_eq!(mounts[1]["options"][1], "ro");

        let process = Process {
            args: vec!["sh".to_string()],
            env: vec![],
            cwd: "/".to_string(),
        };
        let spec = runtime_spec("c", &process, &mounts);
        let all = spec["mounts"].as_array().unwrap();
        assert_eq!(all[all.len() - 2..], mounts[..]);

        // A volume the guest did not mount would be memory.
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir(dir.path().join("db")).unwrap();
        let error = volume_mounts_in(dir.path(), &[mount("db", "/data", false)]).unwrap_err();
        assert!(error.contains("not mounted"), "{error}");
        for bad in [
            vec![mount("db", "/", false)],
            vec![mount("db", "data", false)],
            vec![mount("../db", "/data", false)],
            vec![mount("proc", "/data", false), mount("sys", "/data", true)],
        ] {
            assert!(volume_mounts_in(root, &bad).is_err(), "{bad:?}");
        }
    }

    #[test]
    fn a_process_needs_a_command() {
        let dir = image(json!({}));
        assert!(process(dir.path(), &[], &HashMap::new()).is_err());
    }
}
