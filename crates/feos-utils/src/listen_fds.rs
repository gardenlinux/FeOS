// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! Sockets that one process hands to the next, by systemd's protocol
//! (sd_listen_fds(3)): `LISTEN_FDS` of them from fd 3 on, named by
//! `LISTEN_FDNAMES`, for the process whose pid `LISTEN_PID` is.
//!
//! The pods' resolver outlives `feosd` so (ADR 0010): systemd's
//! `feosd.socket` or `feos-host-init` holds its sockets and hands them to
//! every `feosd` it starts, and a self-upgrade hands them to the new image.
//! A query that comes while `feosd` restarts waits in them. So do the
//! listeners of the pods' HTTP bindings (ADR 0028), [`HTTP`], from
//! `feosd-http.socket`: a connection waits in their backlog.

use nix::fcntl::{fcntl, open, FcntlArg, FdFlag, OFlag};
use nix::sys::socket::{
    bind, listen, setsockopt, socket, sockopt, AddressFamily, Backlog, SockFlag, SockType,
    SockaddrIn,
};
use nix::sys::stat::Mode;
use nix::unistd::{fork, ForkResult, Pid};
use std::ffi::{CStr, CString};
use std::io;
use std::net::{SocketAddr, SocketAddrV4, TcpListener, UdpSocket};
use std::os::fd::{AsFd, AsRawFd, BorrowedFd, FromRawFd, OwnedFd, RawFd};
use std::os::unix::ffi::OsStrExt;
use std::sync::{Mutex, PoisonError};

/// The name of the resolver's sockets.
pub const DNS: &str = "dns";
/// The name of the HTTP bindings' listeners (ADR 0028).
pub const HTTP: &str = "http";
/// The first fd handed over.
const FIRST: RawFd = 3;
/// More are not taken: a wrong `LISTEN_FDS` must not take the process's own
/// fds.
const MAX: usize = 16;
const VARIABLES: [&str; 3] = ["LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES"];
/// `LISTEN_PID=`, room for the digits of a pid, and the NUL.
const PID_ENTRY: usize = 11 + 10 + 1;

/// The sockets this process hands to the image it execs next.
static HANDED_ON: Mutex<Vec<(OwnedFd, String)>> = Mutex::new(Vec::new());

/// The sockets handed to this process, with their names, closed on exec.
/// The variables leave the environment, so that no child takes them for
/// its own.
///
/// # Safety
///
/// It changes the environment: the process must have no other threads.
pub unsafe fn take() -> Vec<(OwnedFd, String)> {
    let get = |name| std::env::var(name).ok();
    let names = handed(
        get("LISTEN_PID").as_deref(),
        get("LISTEN_FDS").as_deref(),
        get("LISTEN_FDNAMES").as_deref(),
        std::process::id(),
    );
    for variable in VARIABLES {
        // SAFETY: the caller has no other threads.
        unsafe { std::env::remove_var(variable) };
    }
    let mut taken = Vec::new();
    for (fd, name) in (FIRST..).zip(names) {
        // SAFETY: an fd that is not open fails the fcntl and is not taken.
        let borrowed = unsafe { BorrowedFd::borrow_raw(fd) };
        if fcntl(borrowed, FcntlArg::F_SETFD(FdFlag::FD_CLOEXEC)).is_ok() {
            // SAFETY: the fd is open, and handed to this process alone.
            taken.push((unsafe { OwnedFd::from_raw_fd(fd) }, name));
        }
    }
    taken
}

/// The names of the fds handed to the process `own` by the variables: none
/// unless `LISTEN_PID` is its pid. A name that `LISTEN_FDNAMES` lacks is
/// `unknown`, as systemd has it.
fn handed(pid: Option<&str>, fds: Option<&str>, names: Option<&str>, own: u32) -> Vec<String> {
    if pid.and_then(|pid| pid.trim().parse::<u32>().ok()) != Some(own) {
        return Vec::new();
    }
    let Some(count) = fds.and_then(|fds| fds.trim().parse::<usize>().ok()) else {
        return Vec::new();
    };
    let mut names = names.unwrap_or_default().split(':');
    (0..count.min(MAX))
        .map(|_| {
            names
                .next()
                .filter(|name| !name.is_empty())
                .unwrap_or("unknown")
                .to_string()
        })
        .collect()
}

/// Of the handed sockets, the UDP one and the listening TCP one bound to
/// `address`. The others are closed, and the reasons returned.
pub fn bound_to(
    sockets: Vec<(OwnedFd, String)>,
    address: SocketAddr,
) -> (Option<UdpSocket>, Option<TcpListener>, Vec<String>) {
    let (mut udp, mut tcp, mut refused) = (None, None, Vec::new());
    for (fd, name) in sockets {
        let what = format!("socket {name} (fd {})", fd.as_raw_fd());
        let kind = nix::sys::socket::getsockopt(&fd, sockopt::SockType);
        let listening = nix::sys::socket::getsockopt(&fd, sockopt::AcceptConn).unwrap_or(false);
        let at = |local: io::Result<SocketAddr>| match local {
            Ok(local) if local == address => Ok(()),
            Ok(local) => Err(format!("{what} is bound to {local}, not {address}")),
            Err(e) => Err(format!("{what}: {e}")),
        };
        match kind {
            Ok(SockType::Datagram) if udp.is_none() => {
                let socket = UdpSocket::from(fd);
                match at(socket.local_addr()) {
                    Ok(()) => udp = Some(socket),
                    Err(e) => refused.push(e),
                }
            }
            Ok(SockType::Stream) if listening && tcp.is_none() => {
                let listener = TcpListener::from(fd);
                match at(listener.local_addr()) {
                    Ok(()) => tcp = Some(listener),
                    Err(e) => refused.push(e),
                }
            }
            Ok(kind) => refused.push(format!("{what} is not wanted: {kind:?}")),
            Err(e) => refused.push(format!("{what}: {e}")),
        }
    }
    (udp, tcp, refused)
}

/// A UDP and a listening TCP socket bound to `address`, which the machine
/// need not hold yet (`IP_FREEBIND`), closed on exec.
pub fn bind_freely(address: SocketAddrV4) -> io::Result<(OwnedFd, OwnedFd)> {
    let address = SockaddrIn::from(address);
    let bound = |kind| -> nix::Result<OwnedFd> {
        let fd = socket(AddressFamily::Inet, kind, SockFlag::SOCK_CLOEXEC, None)?;
        setsockopt(&fd, sockopt::IpFreebind, &true)?;
        if kind == SockType::Stream {
            setsockopt(&fd, sockopt::ReuseAddr, &true)?;
        }
        bind(fd.as_raw_fd(), &address)?;
        Ok(fd)
    };
    let udp = bound(SockType::Datagram)?;
    let tcp = bound(SockType::Stream)?;
    listen(&tcp, Backlog::MAXCONN)?;
    Ok((udp, tcp))
}

/// A listening TCP socket bound to `address`, which the machine need not
/// hold yet (`IP_FREEBIND`), closed on exec.
pub fn listen_freely(address: SocketAddrV4) -> io::Result<OwnedFd> {
    let fd = socket(
        AddressFamily::Inet,
        SockType::Stream,
        SockFlag::SOCK_CLOEXEC,
        None,
    )?;
    setsockopt(&fd, sockopt::IpFreebind, &true)?;
    setsockopt(&fd, sockopt::ReuseAddr, &true)?;
    bind(fd.as_raw_fd(), &SockaddrIn::from(address))?;
    listen(&fd, Backlog::MAXCONN)?;
    Ok(fd)
}

/// Of the handed sockets, the listening TCP ones bound to `address` at
/// one of `ports`, by port; the others are closed, and the reasons
/// returned.
pub fn listeners_at(
    sockets: Vec<(OwnedFd, String)>,
    address: std::net::IpAddr,
    ports: &[u16],
) -> (Vec<(u16, TcpListener)>, Vec<String>) {
    let (mut taken, mut refused): (Vec<(u16, TcpListener)>, _) = (Vec::new(), Vec::new());
    for (fd, name) in sockets {
        let what = format!("socket {name} (fd {})", fd.as_raw_fd());
        let listening = nix::sys::socket::getsockopt(&fd, sockopt::AcceptConn).unwrap_or(false);
        let stream = matches!(
            nix::sys::socket::getsockopt(&fd, sockopt::SockType),
            Ok(SockType::Stream)
        );
        if !listening || !stream {
            refused.push(format!("{what} is not a listening TCP socket"));
            continue;
        }
        let listener = TcpListener::from(fd);
        match listener.local_addr() {
            Ok(local)
                if local.ip() == address
                    && ports.contains(&local.port())
                    && !taken.iter().any(|(port, _)| *port == local.port()) =>
            {
                taken.push((local.port(), listener));
            }
            Ok(local) => refused.push(format!("{what} is bound to {local}, not wanted")),
            Err(e) => refused.push(format!("{what}: {e}")),
        }
    }
    (taken, refused)
}

/// Keeps a copy of the socket to hand to the image this process execs
/// next, by [`exec`].
pub fn hand_on(fd: BorrowedFd<'_>, name: &str) -> io::Result<()> {
    let copy = fd.try_clone_to_owned()?;
    HANDED_ON
        .lock()
        .unwrap_or_else(PoisonError::into_inner)
        .push((copy, name.to_string()));
    Ok(())
}

/// Replaces this process's image with `path`, with `args` (`argv[0]`
/// first) and its environment, and hands it the sockets of [`hand_on`].
/// It returns only when the exec fails, and fds from 3 on may have been
/// replaced then.
pub fn exec(path: &CStr, args: &[CString]) -> io::Error {
    let handed = HANDED_ON.lock().unwrap_or_else(PoisonError::into_inner);
    let sockets: Vec<_> = handed
        .iter()
        .map(|(fd, name)| (fd.as_fd(), name.as_str()))
        .collect();
    match Exec::new(path, args, &sockets) {
        // SAFETY: the process's image is replaced, or the call fails.
        Ok(mut exec) => unsafe { exec.run() },
        Err(e) => e,
    }
}

/// Starts `path` with `args` (`argv[0]` first) as a child, its stdin on
/// `/dev/null`, its signal mask empty and `SIGPIPE` at its default, and
/// hands it `sockets`. Between the fork and the exec the child makes
/// async-signal-safe calls alone, as a child of a process with threads
/// must.
pub fn spawn(path: &CStr, args: &[CString], sockets: &[(BorrowedFd<'_>, &str)]) -> io::Result<Pid> {
    let mut exec = Exec::new(path, args, sockets)?;
    let null = open("/dev/null", OFlag::O_RDWR | OFlag::O_CLOEXEC, Mode::empty())?;
    // SAFETY: the child execs or exits, with async-signal-safe calls alone.
    match unsafe { fork() }? {
        ForkResult::Child => unsafe {
            let mut signals = std::mem::MaybeUninit::<libc::sigset_t>::uninit();
            libc::sigemptyset(signals.as_mut_ptr());
            libc::sigprocmask(libc::SIG_SETMASK, signals.as_ptr(), std::ptr::null_mut());
            libc::signal(libc::SIGPIPE, libc::SIG_DFL);
            if libc::dup2(null.as_raw_fd(), 0) == 0 {
                let _ = exec.run();
            }
            libc::_exit(127)
        },
        ForkResult::Parent { child } => Ok(child),
    }
}

/// An exec prepared with every allocation it needs, so that it can run in
/// the child of a fork.
struct Exec {
    path: CString,
    /// What `argv` and `envp` point to.
    _strings: Vec<CString>,
    argv: Vec<*const libc::c_char>,
    envp: Vec<*const libc::c_char>,
    /// `LISTEN_PID=`, whose digits the exec writes, and where it is in
    /// `envp`.
    pid: Option<([u8; PID_ENTRY], usize)>,
    sockets: Vec<RawFd>,
    /// Where the sockets are copied to on their way to fd 3 and on.
    copies: Vec<RawFd>,
}

impl Exec {
    fn new(path: &CStr, args: &[CString], sockets: &[(BorrowedFd<'_>, &str)]) -> io::Result<Self> {
        let mut environment: Vec<CString> = std::env::vars_os()
            .filter(|(name, _)| !VARIABLES.iter().any(|variable| name == variable))
            .filter_map(|(name, value)| {
                let mut entry = name.as_bytes().to_vec();
                entry.push(b'=');
                entry.extend_from_slice(value.as_bytes());
                CString::new(entry).ok()
            })
            .collect();
        let names: Vec<&str> = sockets.iter().map(|(_, name)| *name).collect();
        if names.iter().any(|name| name.contains(':')) {
            return Err(io::Error::other("a socket's name has a colon"));
        }
        let mut pid = None;
        if !sockets.is_empty() {
            environment.push(CString::new(format!("LISTEN_FDS={}", sockets.len()))?);
            environment.push(CString::new(format!("LISTEN_FDNAMES={}", names.join(":")))?);
            let mut entry = [0; PID_ENTRY];
            entry[..11].copy_from_slice(b"LISTEN_PID=");
            pid = Some((entry, environment.len()));
        }
        let strings: Vec<CString> = args.iter().cloned().chain(environment).collect();
        let (args, environment) = strings.split_at(args.len());
        let pointers = |strings: &[CString]| -> Vec<*const libc::c_char> {
            strings.iter().map(|string| string.as_ptr()).collect()
        };
        let mut argv = pointers(args);
        argv.push(std::ptr::null());
        let mut envp = pointers(environment);
        if pid.is_some() {
            // The entry's place; the exec points it at the digits.
            envp.push(std::ptr::null());
        }
        envp.push(std::ptr::null());
        Ok(Self {
            path: path.to_owned(),
            _strings: strings,
            argv,
            envp,
            pid,
            sockets: sockets.iter().map(|(fd, _)| fd.as_raw_fd()).collect(),
            copies: vec![-1; sockets.len()],
        })
    }

    /// Moves the sockets to fd 3 and on, open across the exec, writes this
    /// process's pid into `LISTEN_PID`, and execs. Async-signal-safe.
    ///
    /// # Safety
    ///
    /// The process's image is replaced; on failure, the fds from 3 on may
    /// have been replaced.
    unsafe fn run(&mut self) -> io::Error {
        let count = self.sockets.len() as RawFd;
        // Above the fds they go to, so that moving one closes none of the
        // others.
        for (socket, copy) in self.sockets.iter().zip(self.copies.iter_mut()) {
            // SAFETY: fcntl and dup2 on fds, async-signal-safe.
            *copy = unsafe { libc::fcntl(*socket, libc::F_DUPFD_CLOEXEC, FIRST + count) };
            if *copy < 0 {
                return io::Error::last_os_error();
            }
        }
        for (fd, copy) in (FIRST..).zip(self.copies.iter()) {
            // The new fd stays open across the exec.
            if unsafe { libc::dup2(*copy, fd) } < 0 {
                return io::Error::last_os_error();
            }
        }
        if let Some((entry, index)) = self.pid.as_mut() {
            // SAFETY: getpid is async-signal-safe.
            write_pid(&mut entry[11..], unsafe { libc::getpid() } as u32);
            self.envp[*index] = entry.as_ptr().cast();
        }
        // SAFETY: argv and envp are NUL-terminated arrays of C strings.
        unsafe { libc::execve(self.path.as_ptr(), self.argv.as_ptr(), self.envp.as_ptr()) };
        io::Error::last_os_error()
    }
}

/// Writes `pid` in decimal and a NUL into `buffer`, which has room for 10
/// digits and the NUL. Allocates nothing.
fn write_pid(buffer: &mut [u8], mut pid: u32) {
    let mut digits = [0u8; 10];
    let mut count = 0;
    loop {
        digits[count] = b'0' + (pid % 10) as u8;
        count += 1;
        pid /= 10;
        if pid == 0 {
            break;
        }
    }
    for (place, digit) in buffer.iter_mut().zip(digits[..count].iter().rev()) {
        *place = *digit;
    }
    buffer[count] = 0;
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{BufRead, Read};
    use std::net::Ipv4Addr;
    use std::os::unix::net::UnixStream;

    #[test]
    fn only_the_fds_of_this_process_are_taken() {
        let names = |pid, fds, names| handed(pid, fds, names, 42);
        assert_eq!(
            names(Some("42"), Some("2"), Some("dns:dns")),
            ["dns", "dns"]
        );
        assert_eq!(names(Some("42"), Some("2"), None), ["unknown", "unknown"]);
        assert_eq!(
            names(Some("42"), Some("3"), Some("a::c")),
            ["a", "unknown", "c"]
        );
        assert_eq!(names(Some("42"), Some("1"), Some("a:b")), ["a"]);
        assert_eq!(names(Some("42"), Some("1000"), None).len(), MAX);
        for (pid, fds) in [
            (Some("43"), Some("2")),
            (None, Some("2")),
            (Some("42"), None),
            (Some("42"), Some("-1")),
            (Some("x"), Some("2")),
        ] {
            assert!(names(pid, fds, None).is_empty(), "{pid:?} {fds:?}");
        }
    }

    #[test]
    fn a_pid_is_written_without_allocating() {
        for (pid, written) in [
            (0, "0"),
            (7, "7"),
            (4194304, "4194304"),
            (u32::MAX, "4294967295"),
        ] {
            let mut buffer = [b'x'; 11];
            write_pid(&mut buffer, pid);
            let end = buffer.iter().position(|byte| *byte == 0).unwrap();
            assert_eq!(&buffer[..end], written.as_bytes());
        }
    }

    #[test]
    fn the_sockets_at_an_address_are_told_apart() {
        // A port whose UDP and TCP are both free.
        let (udp, tcp, port) = loop {
            let udp = UdpSocket::bind((Ipv4Addr::LOCALHOST, 0)).unwrap();
            let port = udp.local_addr().unwrap().port();
            if let Ok(tcp) = TcpListener::bind((Ipv4Addr::LOCALHOST, port)) {
                break (udp, tcp, port);
            }
        };
        let address = SocketAddr::from((Ipv4Addr::LOCALHOST, port));
        let elsewhere = UdpSocket::bind((Ipv4Addr::LOCALHOST, 0)).unwrap();
        let (unix, _) = UnixStream::pair().unwrap();
        let sockets = vec![
            (OwnedFd::from(elsewhere), "dns".to_string()),
            (OwnedFd::from(unix), "dns".to_string()),
            (OwnedFd::from(tcp), "dns".to_string()),
            (OwnedFd::from(udp), "dns".to_string()),
        ];
        let (udp, tcp, refused) = bound_to(sockets, address);
        assert_eq!(udp.unwrap().local_addr().unwrap(), address);
        assert_eq!(tcp.unwrap().local_addr().unwrap(), address);
        assert_eq!(refused.len(), 2, "{refused:?}");
    }

    #[test]
    fn sockets_bind_to_an_address_the_machine_does_not_hold() {
        // TEST-NET-1, on no machine; a port without root.
        let ip = Ipv4Addr::new(192, 0, 2, 53);
        let (udp, tcp) = bind_freely(SocketAddrV4::new(ip, 0)).unwrap();
        let kind = |fd: &OwnedFd| nix::sys::socket::getsockopt(fd, sockopt::SockType).unwrap();
        assert_eq!(kind(&udp), SockType::Datagram);
        assert_eq!(kind(&tcp), SockType::Stream);
        assert!(nix::sys::socket::getsockopt(&tcp, sockopt::AcceptConn).unwrap());
        assert_eq!(UdpSocket::from(udp).local_addr().unwrap().ip(), ip);
        assert_eq!(TcpListener::from(tcp).local_addr().unwrap().ip(), ip);
    }

    #[test]
    fn listeners_are_taken_by_their_ports() {
        let ip = Ipv4Addr::new(192, 0, 2, 80);
        let first = listen_freely(SocketAddrV4::new(ip, 0)).unwrap();
        let port = TcpListener::from(first.try_clone().unwrap())
            .local_addr()
            .unwrap()
            .port();
        let elsewhere = listen_freely(SocketAddrV4::new(Ipv4Addr::new(192, 0, 2, 81), 0)).unwrap();
        let (udp, _) = bind_freely(SocketAddrV4::new(ip, 0)).unwrap();
        let sockets = vec![
            (udp, HTTP.to_string()),
            (elsewhere, HTTP.to_string()),
            (first, HTTP.to_string()),
        ];
        let (taken, refused) = listeners_at(sockets, ip.into(), &[port, 1]);
        assert_eq!(taken.len(), 1);
        assert_eq!(taken[0].0, port);
        assert_eq!(refused.len(), 2, "{refused:?}");
    }

    #[test]
    fn a_child_is_handed_its_sockets_from_fd_3() {
        let (ours, theirs) = UnixStream::pair().unwrap();
        let (decoy, _other) = UnixStream::pair().unwrap();
        let sh = CString::new("/bin/sh").unwrap();
        let args = [
            sh.clone(),
            CString::new("-c").unwrap(),
            CString::new(r#"echo "$LISTEN_PID $$ $LISTEN_FDS $LISTEN_FDNAMES" >&3; exec 3>&-"#)
                .unwrap(),
        ];
        let pid = spawn(
            &sh,
            &args,
            &[(theirs.as_fd(), "first"), (decoy.as_fd(), "second")],
        )
        .unwrap();
        drop(theirs);
        let mut said = String::new();
        (&ours).read_to_string(&mut said).unwrap();
        nix::sys::wait::waitpid(pid, None).unwrap();
        let pid = pid.as_raw();
        assert_eq!(said.trim(), format!("{pid} {pid} 2 first:second"));
    }

    #[test]
    fn an_exec_hands_on_the_sockets_kept_for_it() {
        let (ours, theirs) = UnixStream::pair().unwrap();
        hand_on(theirs.as_fd(), "next").unwrap();
        drop(theirs);
        let sh = CString::new("/bin/sh").unwrap();
        let args = [
            sh.clone(),
            CString::new("-c").unwrap(),
            CString::new(r#"echo "$LISTEN_PID $$ $LISTEN_FDS $LISTEN_FDNAMES" >&3"#).unwrap(),
        ];
        // SAFETY: the child execs or exits.
        let pid = match unsafe { fork() }.unwrap() {
            ForkResult::Child => {
                let _ = exec(&sh, &args);
                unsafe { libc::_exit(127) }
            }
            ForkResult::Parent { child } => child,
        };
        // This process keeps its copy: a line, not the end.
        let mut said = String::new();
        std::io::BufReader::new(&ours).read_line(&mut said).unwrap();
        nix::sys::wait::waitpid(pid, None).unwrap();
        let pid = pid.as_raw();
        assert_eq!(said.trim(), format!("{pid} {pid} 1 next"));
    }
}
