// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A pod's `feos-authn`, started in its jail (ADR 0014, "The jail"): a
//! session of its own, a mount namespace whose root is an empty read-only
//! tmpfs, its descriptors 0 and 1 on `/dev/null`, 2 `feosd`'s stderr, 3 the
//! audit file and 4 the control socket's listener and no other, rlimits, no
//! capabilities in any set, `no_new_privs`, and a user and group of its own
//! per running pod, with no supplementary groups. The binary is opened
//! before and executed from its descriptor, since the new root has none.
//!
//! Not yet: the seccomp allowlist, and the network namespace that reaches
//! only the internet (a veth and blackhole routes); the process dials from
//! the node's namespace until then, and checks every address itself.
//!
//! Between the fork and the exec the child makes raw system calls alone,
//! each async-signal-safe, as the child of a process with threads must:
//! everything it needs is allocated before the fork.

use nix::fcntl::{open, OFlag};
use nix::sys::stat::Mode;
use std::ffi::CString;
use std::io;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd, RawFd};
use std::os::unix::ffi::OsStrExt;
use std::path::{Path, PathBuf};

/// The first user and group ID of the processes; one per running pod
/// above it. Far above the microVMs' (from 1,000,000).
pub const UID_BASE: u32 = 2_000_000_000;
/// The processes' IDs at most.
pub const UID_COUNT: u32 = 1_000_000;
/// The descriptors the process is handed.
pub const AUDIT_FD: RawFd = 3;
pub const CONTROL_FD: RawFd = 4;
/// Where the binary's descriptor sits until the exec, which closes it.
const BINARY_FD: RawFd = 5;

/// The limits of the process: (resource, soft and hard).
const RLIMITS: [(libc::__rlimit_resource_t, u64); 5] = [
    (libc::RLIMIT_CORE, 0),
    (libc::RLIMIT_NOFILE, 4096),
    // Threads count; the user is the process's alone.
    (libc::RLIMIT_NPROC, 512),
    (libc::RLIMIT_AS, 4 << 30),
    (libc::RLIMIT_MEMLOCK, 64 << 10),
];

/// What a start needs.
pub struct Start<'a> {
    pub binary: &'a Path,
    pub pod_id: &'a str,
    pub audit: &'a OwnedFd,
    pub control: &'a OwnedFd,
    /// The empty directory the new root is mounted on.
    pub root: &'a Path,
    /// The process's user and group; none: this process's, without the
    /// mount namespace, for tests.
    pub uid: Option<u32>,
}

/// The step of the start a child that exits with `128 - code` failed at.
pub fn failed_step(code: i32) -> Option<&'static str> {
    STEPS.get(usize::try_from(128 - code).ok()?).copied()
}

const STEPS: [&str; 13] = [
    "",
    "setsid",
    "the mount namespace",
    "making the mounts private",
    "mounting the empty root",
    "pivot_root",
    "its descriptors",
    "the rlimits",
    "dropping the bounding set",
    "the groups",
    "the user",
    "no_new_privs and the capabilities",
    "the exec",
];

/// Starts the process; returns its pid.
pub fn spawn(start: &Start<'_>) -> io::Result<i32> {
    let binary = open(
        start.binary,
        OFlag::O_RDONLY | OFlag::O_CLOEXEC,
        Mode::empty(),
    )
    .map_err(|e| {
        io::Error::new(
            io::Error::from(e).kind(),
            format!("{}: {e}", start.binary.display()),
        )
    })?;
    let null = open("/dev/null", OFlag::O_RDWR | OFlag::O_CLOEXEC, Mode::empty())?;
    let cstring = |s: &str| CString::new(s).map_err(io::Error::other);
    let argv: Vec<CString> = [
        "feos-authn",
        "--pod-id",
        start.pod_id,
        "--control-fd",
        "4",
        "--audit-fd",
        "3",
    ]
    .into_iter()
    .map(cstring)
    .collect::<io::Result<_>>()?;
    let envp: Vec<CString> = ["RUST_LOG", "RUST_BACKTRACE"]
        .into_iter()
        .filter_map(|name| Some(format!("{name}={}", std::env::var(name).ok()?)))
        .map(|entry| cstring(&entry))
        .collect::<io::Result<_>>()?;
    let pointers = |strings: &[CString]| -> Vec<*const libc::c_char> {
        strings
            .iter()
            .map(|s| s.as_ptr())
            .chain(std::iter::once(std::ptr::null()))
            .collect()
    };
    let (argv_p, envp_p) = (pointers(&argv), pointers(&envp));
    let root = CString::new(start.root.as_os_str().as_bytes()).map_err(io::Error::other)?;
    let (tmpfs, dot, slash, empty) = (c"tmpfs", c".", c"/", c"");
    let options = c"size=4k,mode=0555";
    let fds = [
        start.audit.as_raw_fd(),
        start.control.as_raw_fd(),
        binary.as_raw_fd(),
    ];
    let jail = start.uid;

    // SAFETY: the child makes async-signal-safe calls alone, on memory
    // allocated before the fork, and execs or exits.
    let pid = unsafe { libc::fork() };
    if pid < 0 {
        return Err(io::Error::last_os_error());
    }
    if pid > 0 {
        return Ok(pid);
    }
    // The child.
    unsafe {
        let fail = |step: i32| -> ! {
            let message = b"feos-authn: the jail failed\n";
            libc::write(2, message.as_ptr().cast(), message.len());
            libc::_exit(128 - step)
        };
        let mut signals = std::mem::MaybeUninit::<libc::sigset_t>::uninit();
        libc::sigemptyset(signals.as_mut_ptr());
        libc::sigprocmask(libc::SIG_SETMASK, signals.as_ptr(), std::ptr::null_mut());
        libc::signal(libc::SIGPIPE, libc::SIG_DFL);
        if libc::setsid() < 0 {
            fail(1);
        }
        if jail.is_some() {
            if libc::unshare(libc::CLONE_NEWNS) != 0 {
                fail(2);
            }
            if libc::mount(
                std::ptr::null(),
                slash.as_ptr(),
                std::ptr::null(),
                libc::MS_REC | libc::MS_PRIVATE,
                std::ptr::null(),
            ) != 0
            {
                fail(3);
            }
            if libc::mount(
                tmpfs.as_ptr(),
                root.as_ptr(),
                tmpfs.as_ptr(),
                libc::MS_NOSUID | libc::MS_NODEV | libc::MS_NOEXEC | libc::MS_RDONLY,
                options.as_ptr().cast(),
            ) != 0
            {
                fail(4);
            }
            if libc::chdir(root.as_ptr()) != 0
                || libc::syscall(libc::SYS_pivot_root, dot.as_ptr(), dot.as_ptr()) != 0
                || libc::umount2(dot.as_ptr(), libc::MNT_DETACH) != 0
                || libc::chdir(slash.as_ptr()) != 0
            {
                fail(5);
            }
        }
        // Above every target first, so that moving one closes no other.
        let mut copies = [-1; 3];
        for (copy, fd) in copies.iter_mut().zip(fds) {
            *copy = libc::fcntl(fd, libc::F_DUPFD_CLOEXEC, 16);
            if *copy < 0 {
                fail(6);
            }
        }
        // dup2 leaves the new descriptor open across the exec. The
        // binary's is closed on exec, which an ELF binary does not need;
        // without the jail it stays open, for a script's interpreter.
        if libc::dup2(null.as_raw_fd(), 0) < 0
            || libc::dup2(null.as_raw_fd(), 1) < 0
            || libc::dup2(copies[0], AUDIT_FD) < 0
            || libc::dup2(copies[1], CONTROL_FD) < 0
            || libc::dup2(copies[2], BINARY_FD) < 0
            || (jail.is_some() && libc::fcntl(BINARY_FD, libc::F_SETFD, libc::FD_CLOEXEC) < 0)
        {
            fail(6);
        }
        // Everything else; an old kernel without close_range keeps what is
        // closed on exec anyway.
        libc::syscall(libc::SYS_close_range, BINARY_FD + 1, u32::MAX, 0);
        for (resource, limit) in RLIMITS {
            let limit = libc::rlimit64 {
                rlim_cur: limit,
                rlim_max: limit,
            };
            if libc::syscall(
                libc::SYS_prlimit64,
                0,
                resource,
                &limit as *const libc::rlimit64,
                std::ptr::null_mut::<libc::rlimit64>(),
            ) != 0
            {
                fail(7);
            }
        }
        if let Some(uid) = jail {
            for capability in 0..64 {
                // Past the last capability the kernel knows: EINVAL.
                if libc::prctl(libc::PR_CAPBSET_DROP, capability, 0, 0, 0) != 0
                    && *libc::__errno_location() != libc::EINVAL
                {
                    fail(8);
                }
            }
            if libc::syscall(libc::SYS_setgroups, 0, std::ptr::null::<libc::gid_t>()) != 0
                || libc::syscall(libc::SYS_setresgid, uid, uid, uid) != 0
            {
                fail(9);
            }
            if libc::syscall(libc::SYS_setresuid, uid, uid, uid) != 0 {
                fail(10);
            }
        }
        if libc::prctl(libc::PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != 0 {
            fail(11);
        }
        if jail.is_some() {
            // The user change cleared the permitted and effective sets;
            // the inheritable one goes too.
            let mut header = CapHeader {
                version: CAPABILITY_VERSION_3,
                pid: 0,
            };
            let data = [CapData::default(); 2];
            if libc::syscall(
                libc::SYS_capset,
                &mut header as *mut CapHeader,
                data.as_ptr(),
            ) != 0
            {
                fail(11);
            }
        }
        libc::syscall(
            libc::SYS_execveat,
            BINARY_FD,
            empty.as_ptr(),
            argv_p.as_ptr(),
            envp_p.as_ptr(),
            libc::AT_EMPTY_PATH,
        );
        fail(12)
    }
}

const CAPABILITY_VERSION_3: u32 = 0x2008_0522;

#[repr(C)]
struct CapHeader {
    version: u32,
    pid: i32,
}

#[repr(C)]
#[derive(Clone, Copy, Default)]
struct CapData {
    effective: u32,
    permitted: u32,
    inheritable: u32,
}

/// The time the process `pid` started, in clock ticks since the boot
/// (`/proc/<pid>/stat`'s 22nd field): with the pid, what names a process.
pub fn start_time(pid: i32) -> Option<u64> {
    let stat = std::fs::read_to_string(format!("/proc/{pid}/stat")).ok()?;
    // "pid (comm) state ppid …": comm may hold spaces and parentheses.
    let (_, rest) = stat.rsplit_once(')')?;
    rest.split_whitespace().nth(19)?.parse().ok()
}

/// A descriptor of the process `pid` that is readable once it exited:
/// for a child as for one a `feosd` before this one started.
pub fn pidfd(pid: i32) -> io::Result<OwnedFd> {
    // SAFETY: a plain system call; the descriptor is new and ours.
    let fd = unsafe { libc::syscall(libc::SYS_pidfd_open, pid, 0) };
    if fd < 0 {
        return Err(io::Error::last_os_error());
    }
    // SAFETY: pidfd_open returned it, open and ours alone.
    Ok(unsafe { OwnedFd::from_raw_fd(fd as RawFd) })
}

/// The process `pid` started at `start`, if it still runs, as a pidfd.
pub fn find(pid: i32, start: u64) -> Option<OwnedFd> {
    // The descriptor first: a pid that is reused after it names another
    // process than the descriptor.
    let fd = pidfd(pid).ok()?;
    (start_time(pid) == Some(start)).then_some(fd)
}

/// The empty directory the process's root is mounted on, in the pod's
/// directory.
pub fn root_dir(pod_dir: &Path) -> io::Result<PathBuf> {
    use std::os::unix::fs::DirBuilderExt;
    let root = pod_dir.join("authn-root");
    match std::fs::DirBuilder::new().mode(0o555).create(&root) {
        Ok(()) => Ok(root),
        Err(e) if e.kind() == io::ErrorKind::AlreadyExists => Ok(root),
        Err(e) => Err(e),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Read;

    #[test]
    fn a_start_time_names_a_process() {
        let own = std::process::id() as i32;
        let start = start_time(own).unwrap();
        assert!(find(own, start).is_some());
        assert!(find(own, start + 1).is_none());
        assert!(start_time(i32::MAX).is_none());
        assert_eq!(failed_step(128 - 5), Some("pivot_root"));
        assert_eq!(failed_step(0), None);
    }

    /// A start without the jail, as the tests run it: the arguments, and
    /// the descriptors where the process expects them and no others.
    #[test]
    fn the_process_gets_its_arguments_and_its_descriptors_alone() {
        let dir = tempfile::tempdir().unwrap();
        let script = dir.path().join("fake");
        std::fs::write(
            &script,
            "#!/bin/sh\necho \"$*\" >&3\nfor fd in 4 6 7 8 9 16 17 18; do\n  \
             [ -e /proc/$$/fd/$fd ] && echo \"open $fd\" >&3\ndone\nexit 0\n",
        )
        .unwrap();
        std::fs::set_permissions(&script, std::os::unix::fs::PermissionsExt::from_mode(0o755))
            .unwrap();
        let (mut ours, audit) = std::os::unix::net::UnixStream::pair().unwrap();
        let (_control_ours, control) = std::os::unix::net::UnixStream::pair().unwrap();
        let (audit, control) = (OwnedFd::from(audit), OwnedFd::from(control));
        let root = root_dir(dir.path()).unwrap();
        let pid = spawn(&Start {
            binary: &script,
            pod_id: "p",
            audit: &audit,
            control: &control,
            root: &root,
            uid: None,
        })
        .unwrap();
        drop((audit, control));
        let mut said = String::new();
        ours.read_to_string(&mut said).unwrap();
        let status = nix::sys::wait::waitpid(nix::unistd::Pid::from_raw(pid), None).unwrap();
        assert_eq!(
            status,
            nix::sys::wait::WaitStatus::Exited(nix::unistd::Pid::from_raw(pid), 0)
        );
        assert_eq!(said, "--pod-id p --control-fd 4 --audit-fd 3\nopen 4\n");
    }

    /// As root (`cargo test -p pod-service jail -- --ignored`): every step
    /// of the jail is taken, up to the exec of a binary that needs a
    /// loader the empty root does not have; a static one runs.
    #[test]
    #[ignore]
    fn the_jail_takes_every_step_as_root() {
        let dir = tempfile::tempdir().unwrap();
        let (_ours, audit) = std::os::unix::net::UnixStream::pair().unwrap();
        let (_control_ours, control) = std::os::unix::net::UnixStream::pair().unwrap();
        let (audit, control) = (OwnedFd::from(audit), OwnedFd::from(control));
        let root = root_dir(dir.path()).unwrap();
        let pid = spawn(&Start {
            binary: Path::new("/bin/true"),
            pod_id: "p",
            audit: &audit,
            control: &control,
            root: &root,
            uid: Some(UID_BASE + UID_COUNT - 1),
        })
        .unwrap();
        let status = nix::sys::wait::waitpid(nix::unistd::Pid::from_raw(pid), None).unwrap();
        let nix::sys::wait::WaitStatus::Exited(_, code) = status else {
            panic!("{status:?}");
        };
        assert!(
            code == 0 || failed_step(code) == Some("the exec"),
            "failed at {:?}",
            failed_step(code)
        );
        // A static binary runs there: glibc's ldconfig, which refuses the
        // options it is given, as itself.
        let ldconfig = Path::new("/sbin/ldconfig");
        if ldconfig.exists() {
            let pid = spawn(&Start {
                binary: ldconfig,
                pod_id: "p",
                audit: &audit,
                control: &control,
                root: &root,
                uid: Some(UID_BASE + UID_COUNT - 1),
            })
            .unwrap();
            let status = nix::sys::wait::waitpid(nix::unistd::Pid::from_raw(pid), None).unwrap();
            let nix::sys::wait::WaitStatus::Exited(_, code) = status else {
                panic!("{status:?}");
            };
            assert_eq!(failed_step(code), None, "exit {code}");
        }
    }
}
