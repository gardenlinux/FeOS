// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! What a FeOS process that serves untrusted peers does to itself: the
//! pod's `feos-authn` on the node (ADR 0014, "The jail") and the guest's
//! TLS bindings (ADR 0036). Both are a tokio proxy with rustls and the libc
//! resolver. `feosd` starts feos-authn as the pod's own user, in a mount
//! namespace with an empty root and in the network namespace of the node's
//! proxies; the guest's TLS bindings start as root and set up their user
//! and root themselves (`forward_service::tls::jail`). The process then:
//!
//! - first, [`close_other_fds`]: closes every descriptor but stdin,
//!   stdout, stderr and the ones it was handed;
//! - is no longer dumpable (no core dump, no ptrace by its own user) and
//!   gets rlimits: no core file, no locked memory, no message queues, at
//!   most [`NOFILE`] descriptors;
//! - drops every capability: the ambient set, the bounding set where it
//!   may, and the permitted, effective and inheritable sets. A thread's
//!   capabilities are its own: a process that has any confines itself
//!   before it starts a second thread;
//! - sets `no_new_privs` and, once its runtime's threads and descriptors
//!   exist, installs a seccomp allowlist for all its threads: what a tokio
//!   proxy with rustls and the libc resolver calls, with `socket` limited to
//!   the families it is given, `clone` to threads without new namespaces,
//!   `open`/`openat` to reading, `mmap`/`mprotect` to memory that is not
//!   executable, `ioctl` to FIONBIO and FIOCLEX, `prctl` to thread names,
//!   `prlimit64` to reading. `clone3` fails with ENOSYS, so that the libc
//!   falls back to `clone`, whose flags the filter sees. Every other call
//!   fails with EPERM: the code that made it returns an error, and the
//!   process keeps serving. The filter covers every thread (TSYNC), those
//!   of the runtime that exist and those it starts later.
//!
//! [`enter`] is feos-authn's whole sequence, once its runtime exists.

use std::io;

/// The descriptors a run may hold.
pub const NOFILE: u64 = 4096;

/// The socket families of a process that dials IPv4 and IPv6 alone.
pub const INET: &[libc::c_int] = &[libc::AF_INET, libc::AF_INET6];
/// And Unix sockets, for one that dials FeID's socket, as the guest's TLS
/// bindings do.
pub const INET_AND_UNIX: &[libc::c_int] = &[libc::AF_INET, libc::AF_INET6, libc::AF_UNIX];

const AUDIT_ARCH_X86_64: u32 = 0xc000_003e;
const X32_SYSCALL_BIT: u32 = 0x4000_0000;

// struct seccomp_data: nr, arch, instruction_pointer, args[6].
const OFF_NR: u32 = 0;
const OFF_ARCH: u32 = 4;
const fn off_arg_lo(i: u32) -> u32 {
    16 + 8 * i
}
const fn off_arg_hi(i: u32) -> u32 {
    16 + 8 * i + 4
}

const RET_ALLOW: u32 = libc::SECCOMP_RET_ALLOW;
const RET_EPERM: u32 = libc::SECCOMP_RET_ERRNO | libc::EPERM as u32;
const RET_ENOSYS: u32 = libc::SECCOMP_RET_ERRNO | libc::ENOSYS as u32;
const RET_KILL: u32 = libc::SECCOMP_RET_KILL_PROCESS;

/// feos-authn's jail, everything but the closing of descriptors, for a
/// process that dials IPv4 and IPv6 alone; the first step that fails stops
/// the start.
pub fn enter() -> io::Result<()> {
    not_dumpable()?;
    limits()?;
    drop_bounding_set()?;
    clear_capabilities()?;
    no_new_privs()?;
    seccomp(INET)
}

/// No core dump, and no ptrace by the process's own user. A change of
/// the user resets it: it comes after one.
pub fn not_dumpable() -> io::Result<()> {
    // SAFETY: prctl with integer arguments only.
    if unsafe { libc::prctl(libc::PR_SET_DUMPABLE, 0, 0, 0, 0) } != 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}

/// No privileges through an exec, of setuid binaries or file capabilities,
/// for this thread and the threads it starts.
pub fn no_new_privs() -> io::Result<()> {
    // SAFETY: prctl with integer arguments only.
    if unsafe { libc::prctl(libc::PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) } != 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}

/// Closes every descriptor but 0, 1, 2 and `keep`.
pub fn close_other_fds(keep: &[i32]) -> io::Result<()> {
    let mut keep: Vec<u32> = keep.iter().map(|&f| f as u32).collect();
    keep.extend([0, 1, 2]);
    keep.sort_unstable();
    keep.dedup();
    // Close the gaps between the kept descriptors, and all above the last.
    let mut from = 0u32;
    for k in keep.iter().copied().chain([u32::MAX]) {
        if k > from {
            let to = if k == u32::MAX { u32::MAX } else { k - 1 };
            // SAFETY: close_range on numbers; the kept ones are outside.
            let r = unsafe { libc::syscall(libc::SYS_close_range, from, to, 0) };
            if r != 0 {
                return Err(io::Error::last_os_error());
            }
        }
        from = k.saturating_add(1);
        if k == u32::MAX {
            break;
        }
    }
    Ok(())
}

/// prlimit64(2) on this process: sets `new` when given, returns the limit
/// before. The syscall, since glibc and musl type the resource apart.
fn prlimit(resource: u32, new: Option<(u64, u64)>) -> io::Result<libc::rlimit64> {
    let new = new.map(|(soft, hard)| libc::rlimit64 {
        rlim_cur: soft,
        rlim_max: hard,
    });
    let mut old = libc::rlimit64 {
        rlim_cur: 0,
        rlim_max: 0,
    };
    let new_ptr = new
        .as_ref()
        .map_or(std::ptr::null(), |n| n as *const libc::rlimit64);
    // SAFETY: pointers to a valid rlimit64 (or NULL) and a writable one.
    let r = unsafe {
        libc::syscall(
            libc::SYS_prlimit64,
            0,
            resource,
            new_ptr,
            &mut old as *mut libc::rlimit64,
        )
    };
    if r != 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(old)
}

/// No core file, no locked memory, no message queues, at most [`NOFILE`]
/// descriptors: as many, when the process may raise its hard limit.
// The resources are u32 to glibc and c_int to musl.
#[allow(clippy::unnecessary_cast)]
pub fn limits() -> io::Result<()> {
    prlimit(libc::RLIMIT_CORE as u32, Some((0, 0)))?;
    prlimit(libc::RLIMIT_MEMLOCK as u32, Some((0, 0)))?;
    prlimit(libc::RLIMIT_MSGQUEUE as u32, Some((0, 0)))?;
    let cur = prlimit(libc::RLIMIT_NOFILE as u32, None)?;
    let n = NOFILE.min(cur.rlim_max);
    prlimit(libc::RLIMIT_NOFILE as u32, Some((n, n)))?;
    Ok(())
}

#[repr(C)]
struct CapHeader {
    version: u32,
    pid: i32,
}

#[repr(C)]
#[derive(Clone, Copy)]
struct CapData {
    effective: u32,
    permitted: u32,
    inheritable: u32,
}

const LINUX_CAPABILITY_VERSION_3: u32 = 0x2008_0522;

/// Empties this thread's bounding set, which needs CAP_SETPCAP: a process
/// that changes its user does it before. Without CAP_SETPCAP it leaves the
/// set, since there is nothing in the other sets to keep anyway.
pub fn drop_bounding_set() -> io::Result<()> {
    for cap in 0..64 {
        // SAFETY: prctl with integer arguments only.
        let r = unsafe { libc::prctl(libc::PR_CAPBSET_DROP, cap, 0, 0, 0) };
        if r != 0 {
            let e = io::Error::last_os_error();
            match e.raw_os_error() {
                Some(libc::EINVAL) => break,
                Some(libc::EPERM) => break,
                _ => return Err(e),
            }
        }
    }
    Ok(())
}

/// Empties this thread's ambient, permitted, effective and inheritable
/// sets.
pub fn clear_capabilities() -> io::Result<()> {
    // SAFETY: prctl with integer arguments only.
    unsafe {
        libc::prctl(
            libc::PR_CAP_AMBIENT,
            libc::PR_CAP_AMBIENT_CLEAR_ALL,
            0,
            0,
            0,
        )
    };
    let mut header = CapHeader {
        version: LINUX_CAPABILITY_VERSION_3,
        pid: 0,
    };
    let data = [CapData {
        effective: 0,
        permitted: 0,
        inheritable: 0,
    }; 2];
    // SAFETY: capset with a version 3 header and its two data structs.
    let r = unsafe { libc::syscall(libc::SYS_capset, &mut header, data.as_ptr()) };
    if r != 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}

fn stmt(code: u32, k: u32) -> libc::sock_filter {
    libc::sock_filter {
        code: code as u16,
        jt: 0,
        jf: 0,
        k,
    }
}

fn jump(code: u32, k: u32, jt: u8, jf: u8) -> libc::sock_filter {
    libc::sock_filter {
        code: code as u16,
        jt,
        jf,
        k,
    }
}

const LD_W_ABS: u32 = libc::BPF_LD | libc::BPF_W | libc::BPF_ABS;
const JEQ_K: u32 = libc::BPF_JMP | libc::BPF_JEQ | libc::BPF_K;
const JSET_K: u32 = libc::BPF_JMP | libc::BPF_JSET | libc::BPF_K;
const RET_K: u32 = libc::BPF_RET | libc::BPF_K;

/// The system calls allowed without looking at their arguments.
const ALLOWED: &[libc::c_long] = &[
    libc::SYS_read,
    libc::SYS_write,
    libc::SYS_readv,
    libc::SYS_writev,
    libc::SYS_pread64,
    libc::SYS_close,
    libc::SYS_close_range,
    libc::SYS_fstat,
    libc::SYS_newfstatat,
    libc::SYS_statx,
    libc::SYS_lseek,
    libc::SYS_fcntl,
    libc::SYS_dup,
    libc::SYS_dup3,
    libc::SYS_fsync,
    libc::SYS_fdatasync,
    libc::SYS_getrandom,
    // Memory.
    libc::SYS_brk,
    libc::SYS_munmap,
    libc::SYS_mremap,
    libc::SYS_madvise,
    // Threads, signals, time.
    libc::SYS_futex,
    libc::SYS_set_robust_list,
    libc::SYS_rseq,
    libc::SYS_set_tid_address,
    libc::SYS_sched_yield,
    libc::SYS_sched_getaffinity,
    libc::SYS_rt_sigaction,
    libc::SYS_rt_sigprocmask,
    libc::SYS_rt_sigreturn,
    libc::SYS_sigaltstack,
    libc::SYS_tkill,
    libc::SYS_tgkill,
    libc::SYS_gettid,
    libc::SYS_getpid,
    libc::SYS_getuid,
    libc::SYS_geteuid,
    libc::SYS_getgid,
    libc::SYS_getegid,
    libc::SYS_getrlimit,
    libc::SYS_clock_gettime,
    libc::SYS_clock_getres,
    libc::SYS_gettimeofday,
    libc::SYS_nanosleep,
    libc::SYS_clock_nanosleep,
    libc::SYS_restart_syscall,
    libc::SYS_exit,
    libc::SYS_exit_group,
    libc::SYS_uname,
    libc::SYS_sysinfo,
    // The reactor.
    libc::SYS_epoll_create1,
    libc::SYS_epoll_ctl,
    libc::SYS_epoll_wait,
    libc::SYS_epoll_pwait,
    libc::SYS_epoll_pwait2,
    libc::SYS_eventfd2,
    libc::SYS_pipe2,
    libc::SYS_poll,
    libc::SYS_ppoll,
    libc::SYS_select,
    libc::SYS_pselect6,
    // Sockets that exist, or that `socket` below made.
    libc::SYS_connect,
    libc::SYS_bind,
    libc::SYS_accept,
    libc::SYS_accept4,
    libc::SYS_getsockname,
    libc::SYS_getpeername,
    libc::SYS_setsockopt,
    libc::SYS_getsockopt,
    libc::SYS_sendto,
    libc::SYS_recvfrom,
    libc::SYS_sendmsg,
    libc::SYS_recvmsg,
    libc::SYS_sendmmsg,
    libc::SYS_recvmmsg,
    libc::SYS_shutdown,
];

/// A call allowed when `arg`'s low 32 bits are one of `values`.
fn allow_if_in(nr: libc::c_long, arg: u32, values: &[u32]) -> Vec<libc::sock_filter> {
    let n = values.len();
    let mut f = vec![jump(JEQ_K, nr as u32, 0, (n + 3) as u8)];
    f.push(stmt(LD_W_ABS, off_arg_lo(arg)));
    for (i, v) in values.iter().enumerate() {
        // Equal: jump over the remaining checks and the EPERM to ALLOW.
        f.push(jump(JEQ_K, *v, (n - i) as u8, 0));
    }
    f.push(stmt(RET_K, RET_EPERM));
    f.push(stmt(RET_K, RET_ALLOW));
    // The block reloads nr for the checks after it only when skipped,
    // and a skip never entered it.
    f
}

/// A call allowed when `arg`'s low 32 bits have none of `mask`'s bits.
fn allow_if_none(nr: libc::c_long, arg: u32, mask: u32) -> Vec<libc::sock_filter> {
    vec![
        jump(JEQ_K, nr as u32, 0, 4),
        stmt(LD_W_ABS, off_arg_lo(arg)),
        jump(JSET_K, mask, 0, 1),
        stmt(RET_K, RET_EPERM),
        stmt(RET_K, RET_ALLOW),
    ]
}

/// The allowlist as a classic BPF program, `socket` limited to `families`.
pub fn filter(families: &[libc::c_int]) -> Vec<libc::sock_filter> {
    let families: Vec<u32> = families.iter().map(|&f| f as u32).collect();
    let mut f = vec![
        stmt(LD_W_ABS, OFF_ARCH),
        jump(JEQ_K, AUDIT_ARCH_X86_64, 1, 0),
        stmt(RET_K, RET_KILL),
        stmt(LD_W_ABS, OFF_NR),
        // The x32 ABI's numbers alias the allowed ones; none are used.
        jump(
            libc::BPF_JMP | libc::BPF_JGE | libc::BPF_K,
            X32_SYSCALL_BIT,
            0,
            1,
        ),
        stmt(RET_K, RET_EPERM),
    ];
    for &nr in ALLOWED {
        f.push(jump(JEQ_K, nr as u32, 0, 1));
        f.push(stmt(RET_K, RET_ALLOW));
    }
    // Every block below starts with nr in the accumulator and ends in a
    // return; a call it is not about jumps over it with nr still loaded.
    let blocks: Vec<Vec<libc::sock_filter>> = vec![
        allow_if_in(libc::SYS_socket, 0, &families),
        allow_if_in(
            libc::SYS_ioctl,
            1,
            &[
                libc::FIONBIO as u32,
                libc::FIOCLEX as u32,
                libc::FIONCLEX as u32,
            ],
        ),
        allow_if_in(
            libc::SYS_prctl,
            0,
            &[libc::PR_SET_NAME as u32, libc::PR_GET_NAME as u32],
        ),
        // Reading only: no write access, no creating, no truncating, no
        // O_TMPFILE.
        allow_if_none(
            libc::SYS_open,
            1,
            (libc::O_ACCMODE | libc::O_CREAT | libc::O_TRUNC) as u32 | 0x40_0000,
        ),
        allow_if_none(
            libc::SYS_openat,
            2,
            (libc::O_ACCMODE | libc::O_CREAT | libc::O_TRUNC) as u32 | 0x40_0000,
        ),
        // Memory that is never executable.
        allow_if_none(libc::SYS_mmap, 2, libc::PROT_EXEC as u32),
        allow_if_none(libc::SYS_mprotect, 2, libc::PROT_EXEC as u32),
    ];
    for b in blocks {
        f.extend(b);
    }
    // clone: threads only, into no new namespace.
    let namespaces = (libc::CLONE_NEWNS
        | libc::CLONE_NEWCGROUP
        | libc::CLONE_NEWUTS
        | libc::CLONE_NEWIPC
        | libc::CLONE_NEWUSER
        | libc::CLONE_NEWPID
        | libc::CLONE_NEWNET) as u32;
    f.extend([
        jump(JEQ_K, libc::SYS_clone as u32, 0, 6),
        stmt(LD_W_ABS, off_arg_lo(0)),
        jump(JSET_K, libc::CLONE_THREAD as u32, 0, 2),
        jump(JSET_K, namespaces, 1, 0),
        stmt(RET_K, RET_ALLOW),
        stmt(RET_K, RET_EPERM),
        stmt(RET_K, RET_EPERM),
    ]);
    // clone3's flags are behind a pointer: ENOSYS makes the libc use clone.
    f.extend([
        jump(JEQ_K, libc::SYS_clone3 as u32, 0, 1),
        stmt(RET_K, RET_ENOSYS),
    ]);
    // prlimit64 reads only: no new limit (arg 2 NULL).
    f.extend([
        jump(JEQ_K, libc::SYS_prlimit64 as u32, 0, 6),
        stmt(LD_W_ABS, off_arg_lo(2)),
        jump(JEQ_K, 0, 0, 3),
        stmt(LD_W_ABS, off_arg_hi(2)),
        jump(JEQ_K, 0, 0, 1),
        stmt(RET_K, RET_ALLOW),
        stmt(RET_K, RET_EPERM),
    ]);
    f.push(stmt(RET_K, RET_EPERM));
    f
}

/// Installs the allowlist, `socket` limited to `families`, on every thread
/// of the process (TSYNC). It needs `no_new_privs`, or CAP_SYS_ADMIN.
pub fn seccomp(families: &[libc::c_int]) -> io::Result<()> {
    let prog = filter(families);
    let fprog = libc::sock_fprog {
        len: prog.len() as u16,
        filter: prog.as_ptr() as *mut libc::sock_filter,
    };
    // SAFETY: seccomp(2) with a program that outlives the call; the kernel
    // copies it.
    let r = unsafe {
        libc::syscall(
            libc::SYS_seccomp,
            libc::SECCOMP_SET_MODE_FILTER,
            libc::SECCOMP_FILTER_FLAG_TSYNC,
            &fprog as *const libc::sock_fprog,
        )
    };
    if r != 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Runs the filter on a call, as the kernel would: a tiny interpreter
    /// for the instructions the filter uses.
    fn run(prog: &[libc::sock_filter], nr: i64, args: [u64; 6]) -> u32 {
        let mut data = [0u32; 16];
        data[0] = nr as u32;
        data[1] = AUDIT_ARCH_X86_64;
        for (i, a) in args.iter().enumerate() {
            data[4 + 2 * i] = *a as u32;
            data[5 + 2 * i] = (*a >> 32) as u32;
        }
        let (mut acc, mut pc) = (0u32, 0usize);
        loop {
            let ins = prog[pc];
            let code = u32::from(ins.code);
            pc += 1;
            match code {
                LD_W_ABS => acc = data[(ins.k / 4) as usize],
                RET_K => return ins.k,
                c if c == JEQ_K
                    || c == JSET_K
                    || c == libc::BPF_JMP | libc::BPF_JGE | libc::BPF_K =>
                {
                    let t = if c == JEQ_K {
                        acc == ins.k
                    } else if c == JSET_K {
                        acc & ins.k != 0
                    } else {
                        acc >= ins.k
                    };
                    pc += usize::from(if t { ins.jt } else { ins.jf });
                }
                other => panic!("instruction {other:#x}"),
            }
        }
    }

    #[test]
    fn the_filter_allows_and_refuses() {
        let p = filter(INET);
        assert!(p.len() < 4096);
        let z = [0u64; 6];
        assert_eq!(run(&p, libc::SYS_read, z), RET_ALLOW);
        assert_eq!(run(&p, libc::SYS_execve, z), RET_EPERM);
        assert_eq!(run(&p, libc::SYS_ptrace, z), RET_EPERM);
        assert_eq!(run(&p, libc::SYS_unshare, z), RET_EPERM);
        assert_eq!(run(&p, libc::SYS_listen, z), RET_EPERM);
        assert_eq!(run(&p, libc::SYS_socketpair, z), RET_EPERM);
        assert_eq!(
            run(&p, i64::from(X32_SYSCALL_BIT) | libc::SYS_read, z),
            RET_EPERM
        );
        let a = |x: u64, i: usize| {
            let mut v = [0u64; 6];
            v[i] = x;
            v
        };
        assert_eq!(
            run(&p, libc::SYS_socket, a(libc::AF_INET as u64, 0)),
            RET_ALLOW
        );
        assert_eq!(
            run(&p, libc::SYS_socket, a(libc::AF_INET6 as u64, 0)),
            RET_ALLOW
        );
        assert_eq!(
            run(&p, libc::SYS_socket, a(libc::AF_UNIX as u64, 0)),
            RET_EPERM
        );
        assert_eq!(
            run(&p, libc::SYS_socket, a(libc::AF_NETLINK as u64, 0)),
            RET_EPERM
        );
        assert_eq!(run(&p, libc::SYS_ioctl, a(libc::FIONBIO, 1)), RET_ALLOW);
        assert_eq!(run(&p, libc::SYS_ioctl, a(libc::TIOCSTI, 1)), RET_EPERM);
        assert_eq!(
            run(&p, libc::SYS_prctl, a(libc::PR_SET_NAME as u64, 0)),
            RET_ALLOW
        );
        assert_eq!(
            run(&p, libc::SYS_prctl, a(libc::PR_SET_DUMPABLE as u64, 0)),
            RET_EPERM
        );
        let ro = (libc::O_RDONLY | libc::O_CLOEXEC) as u64;
        assert_eq!(run(&p, libc::SYS_openat, a(ro, 2)), RET_ALLOW);
        assert_eq!(run(&p, libc::SYS_open, a(ro, 1)), RET_ALLOW);
        for flags in [
            libc::O_WRONLY,
            libc::O_RDWR,
            libc::O_CREAT,
            libc::O_TRUNC,
            libc::O_TMPFILE,
        ] {
            assert_eq!(
                run(&p, libc::SYS_openat, a(flags as u64, 2)),
                RET_EPERM,
                "{flags:#x}"
            );
        }
        let rw = (libc::PROT_READ | libc::PROT_WRITE) as u64;
        assert_eq!(run(&p, libc::SYS_mmap, a(rw, 2)), RET_ALLOW);
        assert_eq!(
            run(&p, libc::SYS_mprotect, a(libc::PROT_EXEC as u64, 2)),
            RET_EPERM
        );
        let thread = (libc::CLONE_VM
            | libc::CLONE_FS
            | libc::CLONE_FILES
            | libc::CLONE_SIGHAND
            | libc::CLONE_THREAD
            | libc::CLONE_SYSVSEM
            | libc::CLONE_SETTLS
            | libc::CLONE_PARENT_SETTID
            | libc::CLONE_CHILD_CLEARTID) as u64;
        assert_eq!(run(&p, libc::SYS_clone, a(thread, 0)), RET_ALLOW);
        assert_eq!(
            run(&p, libc::SYS_clone, a(libc::SIGCHLD as u64, 0)),
            RET_EPERM
        );
        assert_eq!(
            run(
                &p,
                libc::SYS_clone,
                a(thread | libc::CLONE_NEWUSER as u64, 0)
            ),
            RET_EPERM
        );
        assert_eq!(run(&p, libc::SYS_clone3, z), RET_ENOSYS);
        assert_eq!(run(&p, libc::SYS_prlimit64, z), RET_ALLOW);
        assert_eq!(
            run(&p, libc::SYS_prlimit64, a(0x7fff_0000_1000, 2)),
            RET_EPERM
        );
        assert_eq!(run(&p, libc::SYS_prlimit64, a(0x1000, 2)), RET_EPERM);
    }

    // The TLS bindings' filter is feos-authn's, with Unix sockets too:
    // FeID's socket. The rest stays refused.
    #[test]
    fn unix_sockets_are_allowed_only_when_named() {
        let p = filter(INET_AND_UNIX);
        let family = |f: libc::c_int| [f as u64, 0, 0, 0, 0, 0];
        for f in [libc::AF_INET, libc::AF_INET6, libc::AF_UNIX] {
            assert_eq!(run(&p, libc::SYS_socket, family(f)), RET_ALLOW, "{f}");
        }
        for f in [libc::AF_NETLINK, libc::AF_PACKET, libc::AF_VSOCK] {
            assert_eq!(run(&p, libc::SYS_socket, family(f)), RET_EPERM, "{f}");
        }
        let z = [0u64; 6];
        for nr in [
            libc::SYS_execve,
            libc::SYS_ptrace,
            libc::SYS_kill,
            libc::SYS_mount,
            libc::SYS_setuid,
            libc::SYS_listen,
            libc::SYS_socketpair,
        ] {
            assert_eq!(run(&p, nr, z), RET_EPERM, "{nr}");
        }
        // Everything else is the same program: one more value in the
        // socket block shifts nothing before it.
        let inet = filter(INET);
        assert_eq!(p.len(), inet.len() + 1);
    }

    #[test]
    fn another_architecture_is_killed() {
        let p = filter(INET);
        let ins = p[1];
        assert_eq!(ins.k, AUDIT_ARCH_X86_64);
        assert_eq!(p[2].k, RET_KILL);
    }
}
