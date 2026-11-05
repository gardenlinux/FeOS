// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! The feos-authn binary, jailed (seccomp, no_new_privs, no capabilities),
//! as feosd starts it: the audit file as fd 3, the listening control socket
//! as fd 4, the pod's connections passed over it. Names resolve with the
//! system's resolver.

#[path = "../src/testkit.rs"]
mod testkit;

use std::fs::OpenOptions;
use std::os::fd::AsRawFd;
use std::os::unix::process::CommandExt;
use std::path::Path;
use std::process::{Child, Command};

use bytes::Bytes;
use feos_proto::fleet_service::{
    http_binding, HeaderCredential, HttpBinding, HttpBindingMode, InterceptCa, PodBindings,
};
use http::StatusCode;
use http_body_util::Full;
use testkit::*;

struct Process(Child);

impl Drop for Process {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}

fn spawn(dir: &Path) -> (Process, Feosd) {
    let sock = dir.join("authn.sock");
    let listener = seqpacket_listener(&sock);
    let audit = OpenOptions::new()
        .create(true)
        .append(true)
        .open(dir.join("authn-audit.ndjson"))
        .unwrap();
    let (afd, lfd) = (audit.as_raw_fd(), listener.as_raw_fd());
    let mut cmd = Command::new(env!("CARGO_BIN_EXE_feos-authn"));
    // The test's upstreams are on the loopback address.
    cmd.args(["--pod-id", POD_ID, "--audit-fd", "3", "--control-fd", "4"])
        .arg("--allow-loopback")
        .env("RUST_LOG", "debug");
    // SAFETY: only async-signal-safe calls between fork and exec.
    unsafe {
        cmd.pre_exec(move || {
            // Out of the way first, so that neither lands on the other.
            let a = libc::fcntl(afd, libc::F_DUPFD, 100);
            let l = libc::fcntl(lfd, libc::F_DUPFD, 100);
            if a < 0 || l < 0 || libc::dup2(a, 3) < 0 || libc::dup2(l, 4) < 0 {
                return Err(std::io::Error::last_os_error());
            }
            Ok(())
        });
    }
    let child = cmd.spawn().unwrap();
    drop((audit, listener));
    (Process(child), Feosd::connect(&sock))
}

fn status_field(pid: u32, field: &str) -> String {
    let s = std::fs::read_to_string(format!("/proc/{pid}/status")).unwrap();
    s.lines()
        .find_map(|l| l.strip_prefix(&format!("{field}:")))
        .unwrap_or_default()
        .trim()
        .to_string()
}

fn binding(name: &str, upstream: &str, ca: &str) -> HttpBinding {
    HttpBinding {
        name: name.into(),
        upstream: upstream.into(),
        ca_pem: ca.into(),
        credential: Some(http_binding::Credential::Headers(HeaderCredential {
            headers: [("X-API-Key".to_string(), "s3cret".to_string())].into(),
        })),
        ..Default::default()
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn the_jailed_binary_serves_both_modes() {
    let dir = tempfile::tempdir().unwrap();
    let pki = Pki::new();
    let up = Upstream::start(pki.server_config(&["127.0.0.1", "localhost"])).await;
    let port = up.addr.port();
    let (ca, ca_key) = intercept_ca(&["localhost"]);
    let (proc, feosd) = spawn(dir.path());
    let pid = proc.0.id();
    let mut partner = binding("partner", "https://localhost", &pki.ca_pem);
    partner.mode = HttpBindingMode::Intercept as i32;
    // A key long enough to be redacted.
    let mut echo = binding("echo", &format!("https://127.0.0.1:{port}"), &pki.ca_pem);
    echo.credential = Some(http_binding::Credential::Headers(HeaderCredential {
        headers: [("X-API-Key".to_string(), "s3cret-0123456789".to_string())].into(),
    }));
    let set = PodBindings {
        http: vec![
            binding("erp", &format!("https://127.0.0.1:{port}"), &pki.ca_pem),
            // getaddrinfo in the jail.
            binding("local", &format!("https://localhost:{port}"), &pki.ca_pem),
            binding("internal", "https://10.1.2.3", &pki.ca_pem),
            partner,
            echo,
        ],
        intercept_cas: vec![InterceptCa {
            cert_pem: ca.clone(),
            key_pem: ca_key,
        }],
        ..Default::default()
    };
    feosd.hello(&["10.0.0.0/8"], Some("127.0.0.1"));
    feosd.answer(&set, "v1");
    feosd.ready("v1");

    // The jail holds.
    assert_eq!(status_field(pid, "Seccomp"), "2");
    assert_eq!(status_field(pid, "NoNewPrivs"), "1");
    assert_eq!(status_field(pid, "CapEff"), "0000000000000000");
    assert_eq!(status_field(pid, "CapPrm"), "0000000000000000");
    let fds: Vec<String> = std::fs::read_dir(format!("/proc/{pid}/fd"))
        .map(|d| {
            d.map(|e| e.unwrap().file_name().into_string().unwrap())
                .collect()
        })
        .unwrap_or_default();
    // Readable only while the process is dumpable, which it is not.
    assert!(fds.is_empty(), "{fds:?}");

    let node = Node::new().await;
    let mut s = h1(node.connect(&feosd, 80).await).await;
    for name in ["erp", "local"] {
        let (status, _, body) = read(send1(&mut s, get(name, "/x")).await).await;
        assert_eq!(status, StatusCode::OK, "{name}");
        assert_eq!(&body[..], b"ok");
        let seen = up.last();
        assert_eq!(seen.headers["x-api-key"], "s3cret");
    }
    let (status, hdrs, _) = read(send1(&mut s, get("internal", "/")).await).await;
    assert_eq!(status, StatusCode::FORBIDDEN);
    assert_eq!(hdrs["feos-binding-error"], "denied_destination");
    let post = http::Request::post("/upload")
        .header("host", "erp")
        .body(Full::new(Bytes::from_static(b"body")))
        .unwrap();
    let (status, _, _) = read(send1(&mut s, post).await).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(&up.last().body[..], b"body");

    // An answer that repeats the key, cut in its middle: masked in the
    // jail as well.
    let (status, hdrs, body) = read(send1(&mut s, get("echo", "/fault")).await).await;
    assert_eq!(status, StatusCode::INTERNAL_SERVER_ERROR);
    let body = String::from_utf8(body.to_vec()).unwrap();
    assert!(body.contains("Identifier : redacted*********\""), "{body}");
    assert_eq!(hdrs["x-quota-key"], "redacted*********");

    // The intercept mode: the leaf for exactly the name, then the request
    // goes on to localhost:443, where nothing of the test answers.
    let tls = tls_connect(
        node.connect(&feosd, 443).await,
        tls_client(&ca, &[b"http/1.1"]),
        "localhost",
    )
    .await
    .unwrap();
    let (mut send, conn) = hyper::client::conn::http1::handshake(hyper_util::rt::TokioIo::new(tls))
        .await
        .unwrap();
    tokio::spawn(conn);
    let (status, hdrs, _) = read(send1(&mut send, get("localhost", "/")).await).await;
    assert_eq!(status, StatusCode::BAD_GATEWAY);
    let code = hdrs["feos-binding-error"].to_str().unwrap();
    assert!(code == "unreachable" || code == "tls", "{code}");
    assert!(tls_connect(
        node.connect(&feosd, 443).await,
        tls_client(&ca, &[b"http/1.1"]),
        "other.test",
    )
    .await
    .is_err());

    // Every request has its record; none holds the credential.
    let audit = dir.path().join("authn-audit.ndjson");
    let rec = await_record(&audit, |r| r["type"] == "http" && r["binding"] == "local").await;
    assert_eq!(rec["status"], 200);
    await_record(&audit, |r| r["type"] == "http" && r["binding"] == "partner").await;
    await_record(&audit, |r| r["type"] == "http_end" && r["binding"] == "erp").await;
    assert!(!std::fs::read_to_string(&audit).unwrap().contains("s3cret"));
    assert!(proc.0.id() == pid);
}
