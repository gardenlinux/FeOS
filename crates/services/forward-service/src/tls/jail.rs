// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The TLS bindings' jail. The process is PID 1's child and starts as
//! root with every capability; it reads plaintext from the pod's
//! containers and TLS from its upstreams, so a fault in it must not hand
//! a peer the guest. The pod's key is never in it: feos-authn signs on
//! the host. In the order it runs, while the process has one thread:
//!
//! - [`close_inherited`]: every descriptor but stdin, stdout and stderr
//!   closes, before the process opens its own;
//! - the bindings' file is read and their listeners bound ([`super::listen`]),
//!   as root: the file is root's alone, and a port may be below 1024;
//! - [`enter`]: a mount namespace of its own whose root is a read-only
//!   tmpfs that holds the share directory at `/run/feos` and
//!   `/etc/resolv.conf` linked to `/run/feos/resolv.conf`, as a container
//!   sees them: FeID's socket and the pod's resolvers, and nothing else of
//!   the guest, no `/proc`, no containers, no volumes, no FeOS sockets;
//!   rlimits (no core file, no locked memory, no message queues, 4096
//!   descriptors); the bounding set emptied; no supplementary groups, and
//!   [`ID`] as its user and group; no capabilities in any other set; not
//!   dumpable; `no_new_privs`.
//!
//! Then the runtime starts its threads, which inherit all of it, and
//! [`Jail::seal`] installs feos-authn's seccomp allowlist on every thread,
//! with Unix sockets for FeID's (`feos_jail`), and checks what the kernel
//! says of the process in `/proc/self/status`. Any step that fails ends the
//! process with its error before it serves; PID 1 starts it again a second
//! later, and it never serves outside its jail.

use std::fs::File;
use std::io::{Read, Seek, SeekFrom};
use std::path::Path;

use nix::mount::{mount, umount2, MntFlags, MsFlags};
use nix::sched::{unshare, CloneFlags};
use nix::unistd::{chdir, pivot_root, setgroups, setresgid, setresuid, Gid, Uid};

use feos_utils::share::{FEID_SOCK, SHARE_DEST, SHARE_DIR};

/// The process's user and group: far above the IDs images name (below
/// 65,536 as a rule), as feosd's jails on the node are. Nothing else in a
/// guest has it, so its signals reach no container's process.
pub const ID: u32 = 2_000_000_000;

/// Where the jail's root is mounted before it becomes the root: an empty
/// directory in FeOS's own, which the mount covers in the jail's namespace
/// alone.
const ROOT: &str = "/run/feos/tls-bindings.root";

/// What the root may hold: its directories, the link and the mount point.
const ROOT_OPTIONS: &str = "mode=0755,size=16k,nr_inodes=16";

/// FeID's socket in the jail: where a container finds it too.
pub const FEID_SOCKET: &str = FEID_SOCK;

/// Closes every descriptor PID 1 left open but stdin, stdout and stderr.
pub fn close_inherited() -> Result<(), String> {
    feos_jail::close_other_fds(&[]).map_err(|e| format!("close inherited descriptors: {e}"))
}

/// The jail, all but its seccomp filter.
pub struct Jail {
    /// `/proc/self/status`, opened before the root changed: the check
    /// reads it again in the end.
    status: File,
}

/// Enters the jail but its seccomp filter, which needs the runtime's
/// threads and descriptors first. The process must have one thread: a
/// thread's capabilities are its own.
pub fn enter() -> Result<Jail, String> {
    let mut status =
        File::open("/proc/self/status").map_err(|e| format!("/proc/self/status: {e}"))?;
    let threads = field(&read(&mut status)?, "Threads").map(str::to_string);
    if threads.as_deref() != Some("1") {
        return Err(format!(
            "the jail needs a process of one thread, not {threads:?}"
        ));
    }
    root().map_err(|e| format!("the jail's root: {e}"))?;
    let step = |what: &str, r: std::io::Result<()>| r.map_err(|e| format!("{what}: {e}"));
    step("rlimits", feos_jail::limits())?;
    step("the bounding set", feos_jail::drop_bounding_set())?;
    let (uid, gid) = (Uid::from_raw(ID), Gid::from_raw(ID));
    setgroups(&[]).map_err(|e| format!("the groups: {e}"))?;
    setresgid(gid, gid, gid).map_err(|e| format!("the group: {e}"))?;
    setresuid(uid, uid, uid).map_err(|e| format!("the user: {e}"))?;
    step("the capabilities", feos_jail::clear_capabilities())?;
    step("dumpable", feos_jail::not_dumpable())?;
    step("no_new_privs", feos_jail::no_new_privs())?;
    Ok(Jail { status })
}

impl Jail {
    /// Installs the seccomp filter on every thread and checks the jail as
    /// the kernel reports it; a line for the log.
    pub fn seal(mut self) -> Result<String, String> {
        feos_jail::seccomp(feos_jail::INET_AND_UNIX).map_err(|e| format!("seccomp: {e}"))?;
        check(&read(&mut self.status)?)
    }
}

/// A new mount namespace whose root holds the share directory and the
/// resolvers' link, read-only, and nothing else.
fn root() -> Result<(), String> {
    let at = |what: &'static str| move |e: nix::Error| format!("{what}: {e}");
    unshare(CloneFlags::CLONE_NEWNS).map_err(at("unshare"))?;
    // Nothing mounted here reaches PID 1's namespace.
    mount(
        None::<&str>,
        "/",
        None::<&str>,
        MsFlags::MS_REC | MsFlags::MS_PRIVATE,
        None::<&str>,
    )
    .map_err(at("private mounts"))?;
    std::fs::create_dir_all(ROOT).map_err(|e| format!("{ROOT}: {e}"))?;
    let hardened = MsFlags::MS_NOSUID | MsFlags::MS_NODEV | MsFlags::MS_NOEXEC;
    mount(
        Some("tmpfs"),
        ROOT,
        Some("tmpfs"),
        hardened,
        Some(ROOT_OPTIONS),
    )
    .map_err(at("the root's tmpfs"))?;
    let root = Path::new(ROOT);
    let share = root.join(SHARE_DEST.trim_start_matches('/'));
    for dir in [root.join("etc"), share.clone()] {
        std::fs::create_dir_all(&dir).map_err(|e| format!("{}: {e}", dir.display()))?;
    }
    std::os::unix::fs::symlink(
        Path::new(SHARE_DEST).join("resolv.conf"),
        root.join("etc/resolv.conf"),
    )
    .map_err(|e| format!("/etc/resolv.conf: {e}"))?;
    mount(
        Some(SHARE_DIR),
        &share,
        None::<&str>,
        MsFlags::MS_BIND,
        None::<&str>,
    )
    .map_err(at("the share directory"))?;
    let read_only = MsFlags::MS_BIND | MsFlags::MS_REMOUNT | MsFlags::MS_RDONLY | hardened;
    mount(None::<&str>, &share, None::<&str>, read_only, None::<&str>)
        .map_err(at("the share directory, read-only"))?;
    mount(None::<&str>, ROOT, None::<&str>, read_only, None::<&str>)
        .map_err(at("the root, read-only"))?;
    // The old root goes under the new one and is detached from there.
    chdir(ROOT).map_err(at("chdir"))?;
    pivot_root(".", ".").map_err(at("pivot_root"))?;
    umount2(".", MntFlags::MNT_DETACH).map_err(at("detach the old root"))?;
    chdir("/").map_err(at("chdir /"))?;
    Ok(())
}

fn read(status: &mut File) -> Result<String, String> {
    let mut text = String::new();
    status
        .seek(SeekFrom::Start(0))
        .and_then(|_| status.read_to_string(&mut text))
        .map_err(|e| format!("/proc/self/status: {e}"))?;
    Ok(text)
}

/// The value of a line `name:\tvalue` of `/proc/<pid>/status`.
fn field<'a>(status: &'a str, name: &str) -> Option<&'a str> {
    status.lines().find_map(|line| {
        let (key, value) = line.split_once(':')?;
        (key == name).then_some(value.trim())
    })
}

/// The capability sets of `/proc/<pid>/status`.
const CAPABILITY_SETS: [&str; 5] = ["CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"];

/// Whether `status` is the jail's: [`ID`] as every user and group ID, no
/// supplementary groups, no capability in any set, `no_new_privs`, a
/// seccomp filter. The line for the log, or what is not.
fn check(status: &str) -> Result<String, String> {
    let get = |name: &str| field(status, name).ok_or_else(|| format!("no {name} in the status"));
    let id = ID.to_string();
    for name in ["Uid", "Gid"] {
        let ids: Vec<&str> = get(name)?.split_whitespace().collect();
        if ids.len() != 4 || ids.iter().any(|i| *i != id) {
            return Err(format!("{name} is {ids:?}, not {id} four times"));
        }
    }
    let groups = get("Groups")?;
    if !groups.is_empty() {
        return Err(format!("supplementary groups {groups}"));
    }
    for set in CAPABILITY_SETS {
        let value = get(set)?;
        if u64::from_str_radix(value, 16) != Ok(0) {
            return Err(format!("{set} is {value}"));
        }
    }
    let no_new_privs = get("NoNewPrivs")?;
    if no_new_privs != "1" {
        return Err(format!("NoNewPrivs is {no_new_privs}"));
    }
    // 2: a filter.
    let seccomp = get("Seccomp")?;
    if seccomp != "2" {
        return Err(format!("Seccomp is {seccomp}"));
    }
    Ok(format!(
        "jailed: Uid {id}, Gid {id}, no groups, CapInh CapPrm CapEff CapBnd CapAmb 0, \
         NoNewPrivs 1, Seccomp 2; root: the share directory at {SHARE_DEST}"
    ))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn status(replace: &[(&str, &str)]) -> String {
        let id = ID.to_string();
        let mut fields = vec![
            ("Name", "feos-guest".to_string()),
            ("Threads", "4".to_string()),
            ("Uid", format!("{id}\t{id}\t{id}\t{id}")),
            ("Gid", format!("{id}\t{id}\t{id}\t{id}")),
            ("Groups", String::new()),
            ("CapInh", "0000000000000000".to_string()),
            ("CapPrm", "0000000000000000".to_string()),
            ("CapEff", "0000000000000000".to_string()),
            ("CapBnd", "0000000000000000".to_string()),
            ("CapAmb", "0000000000000000".to_string()),
            ("NoNewPrivs", "1".to_string()),
            ("Seccomp", "2".to_string()),
            ("Seccomp_filters", "1".to_string()),
        ];
        for (name, value) in replace {
            match fields.iter_mut().find(|(n, _)| n == name) {
                Some(field) => field.1 = value.to_string(),
                None => panic!("no field {name}"),
            }
        }
        fields.iter().map(|(n, v)| format!("{n}:\t{v}\n")).collect()
    }

    #[test]
    fn the_jail_is_what_the_kernel_reports() {
        let line = check(&status(&[])).unwrap();
        assert!(line.contains("Uid 2000000000"), "{line}");
        assert!(line.contains("Seccomp 2"), "{line}");
    }

    // Root, a group, a capability anywhere, a process without
    // no_new_privs or its filter: no jail.
    #[test]
    fn anything_less_is_refused() {
        let root = "0\t0\t0\t0".to_string();
        let saved = format!("{ID}\t{ID}\t0\t{ID}");
        let bad = [
            ("Uid", root.as_str()),
            ("Uid", saved.as_str()),
            ("Gid", root.as_str()),
            ("Groups", "0"),
            ("CapInh", "0000000000000400"),
            ("CapPrm", "000001ffffffffff"),
            ("CapEff", "0000000000000001"),
            ("CapBnd", "000001ffffffffff"),
            ("CapAmb", "0000000000000400"),
            ("CapEff", "zz"),
            ("NoNewPrivs", "0"),
            ("Seccomp", "0"),
            ("Seccomp", "1"),
        ];
        for (name, value) in bad {
            assert!(
                check(&status(&[(name, value)])).is_err(),
                "{name} {value:?} passed"
            );
        }
        // A kernel that does not report a field.
        let text = status(&[]).replace("Seccomp:\t2\n", "");
        assert!(check(&text).is_err());
    }

    #[test]
    fn fields_are_read_by_their_whole_name() {
        let text = status(&[]);
        assert_eq!(field(&text, "Seccomp"), Some("2"));
        assert_eq!(field(&text, "Seccomp_filters"), Some("1"));
        assert_eq!(field(&text, "Groups"), Some(""));
        assert_eq!(field(&text, "Cap"), None);
    }
}
