// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! ToolService: agent tool primitives implemented natively in FeOS
//! (not inside the sandbox container). All of them operate on the *payload
//! container's* rootfs, confined via openat2(RESOLVE_BENEATH)
//! (feos_utils::container_fs):
//! the tool surface is image-independent — it works against a scratch image.
//!
//!   webfetch  HTTP fetch (reqwest/rustls) + readable extraction (trafilatura-rs)
//!   read/ls   in-process, lossy-utf8 read with offset/limit (a PNG/JPEG/
//!             GIF/WebP comes back whole as bytes instead), sorted listing
//!   write/edit in-process, exact-replace edit semantics (unique oldText)
//!   rg        ripgrep's own crates (grep-regex/grep-searcher) in-process;
//!             the line limit stops the search inside the sink (no head(1))
//!   fd        fd semantics on the `ignore` walk engine (gitignore-aware,
//!             hidden-skipping, no symlink following) + smart-case regex

use std::fs::File;
use std::io::Read;
use std::io::Write;
use std::time::Duration;

use feos_proto::tool_service::tool_service_server::ToolService;
use feos_proto::tool_service::{
    EditRequest, EditResponse, FdRequest, FdResponse, LsRequest, LsResponse, ReadRequest,
    ReadResponse, RgRequest, RgResponse, WebfetchRequest, WebfetchResponse, WriteRequest,
    WriteResponse,
};
use grep_searcher::{BinaryDetection, Searcher, SearcherBuilder, Sink, SinkMatch};
use nix::libc;
use tonic::{Request, Response, Status};

use feos_utils::container_fs as toolfs;

const MAX_BODY: usize = 2 * 1024 * 1024; // webfetch: cap the parsed HTML
const REQ_TIMEOUT: Duration = Duration::from_secs(30);
const UA: &str = "feos-webfetch/1.0 (+readable extraction; trafilatura-rs)";

const READ_MAX_BYTES: u64 = 16 * 1024 * 1024; // refuse bigger files (paged reads)
const READ_DEF_LIMIT: usize = 2000;
// An image is read whole and goes to a model as base64: 3.75 MiB of bytes
// is exactly the 5 MiB of base64 the model APIs accept, and it also stays
// under gRPC's 4 MiB default message size on the manager's side.
const IMAGE_MAX_BYTES: usize = 3840 * 1024;
const IMAGE_MAX_SIDE: u32 = 8000;
const LS_DEF_LIMIT: usize = 500;
const RG_DEF_LIMIT: u32 = 100;
const RG_MAX_BYTES: usize = 1024 * 1024;
const FD_DEF_LIMIT: usize = 1000;

#[derive(Default)]
pub struct ToolApiHandler;

// ------------------------------------------------------------ webfetch (Ws)

async fn fetch(url: &str) -> Result<(u16, String, String), String> {
    let client = reqwest::Client::builder()
        .timeout(REQ_TIMEOUT)
        .user_agent(UA)
        .build()
        .map_err(|e| format!("fetch: {}", chain(&e)))?;
    let mut resp = client
        .get(url)
        .send()
        .await
        .map_err(|e| format!("fetch: {}", chain(&e)))?;
    let status = resp.status().as_u16();
    let final_url = resp.url().to_string();
    let mut body = Vec::new();
    while let Some(chunk) = resp
        .chunk()
        .await
        .map_err(|e| format!("read body: {}", chain(&e)))?
    {
        if body.len() + chunk.len() > MAX_BODY {
            return Err(format!("read body: larger than {MAX_BODY} bytes"));
        }
        body.extend_from_slice(&chunk);
    }
    Ok((
        status,
        final_url,
        String::from_utf8_lossy(&body).into_owned(),
    ))
}

/// An error with its causes: reqwest's own message leaves out what went
/// wrong underneath, such as a failed name lookup.
fn chain(error: &dyn std::error::Error) -> String {
    let mut message = error.to_string();
    let mut source = error.source();
    while let Some(cause) = source {
        message.push_str(": ");
        message.push_str(&cause.to_string());
        source = cause.source();
    }
    message
}

/// "Temporary failure in name resolution" is the resolver telling us to
/// come back, and musl's is a single UDP query to one nameserver at a
/// time — one dropped packet on a fresh guest is enough. Retrying a
/// transient lookup failure is the whole of the fix.
async fn fetch_retrying(url: &str) -> Result<(u16, String, String), String> {
    const ATTEMPTS: u32 = 3;
    let mut last = String::new();
    for attempt in 1..=ATTEMPTS {
        match fetch(url).await {
            Ok(t) => return Ok(t),
            Err(e) => {
                let transient = e.contains("Try again")
                    || e.contains("failed to lookup address")
                    || e.contains("Temporary failure");
                last = e;
                if !transient || attempt == ATTEMPTS {
                    break;
                }
                log::warn!("webfetch {url}: attempt {attempt} ({last}), retrying");
                tokio::time::sleep(Duration::from_millis(300 * attempt as u64)).await;
            }
        }
    }
    Err(last)
}

async fn webfetch(url: &str) -> (WebfetchResponse, i32) {
    let (status, final_url, html) = match fetch_retrying(url).await {
        Ok(t) => t,
        Err(e) => {
            return (
                WebfetchResponse {
                    error: format!("webfetch: {e}"),
                    ..Default::default()
                },
                1,
            )
        }
    };
    if !(200..400).contains(&status) {
        return (
            WebfetchResponse {
                status: status as u32,
                effective_url: final_url,
                error: format!("webfetch: HTTP {status}"),
                ..Default::default()
            },
            1,
        );
    }
    // Extraction is CPU work: keep it off the async workers.
    let extracted = tokio::task::spawn_blocking(move || {
        let opts = trafilatura::Options::default()
            .with_fallback(true)
            .with_links(true);
        trafilatura::extract(&html, &opts)
            .map(|res| (res.metadata.title, res.content_text))
            .map_err(|e| e.to_string())
    })
    .await
    .unwrap_or_else(|e| Err(format!("extraction task: {e}")));
    match extracted {
        Ok((title, text)) => {
            if text.trim().is_empty() {
                (
                    WebfetchResponse {
                        status: status as u32,
                        effective_url: final_url,
                        error: "webfetch: no readable content extracted".into(),
                        ..Default::default()
                    },
                    1,
                )
            } else {
                (
                    WebfetchResponse {
                        status: status as u32,
                        effective_url: final_url,
                        title,
                        text,
                        error: String::new(),
                    },
                    0,
                )
            }
        }
        Err(e) => (
            WebfetchResponse {
                status: status as u32,
                effective_url: final_url,
                error: format!("webfetch: extract: {e}"),
                ..Default::default()
            },
            1,
        ),
    }
}

// ------------------------------------------------------- in-proc primitives

/// An image `read` hands back whole: the formats model APIs accept, told
/// apart by their leading bytes (a screenshot is not always named .png).
#[derive(Debug, PartialEq)]
struct Picture {
    media_type: &'static str,
    width: u32,
    height: u32,
}

/// sniff_picture recognises PNG, JPEG, GIF and WebP and reads their pixel
/// size from the header. None: not one of them, read it as text. Err: it
/// starts like one but the header does not parse — sending that on would
/// fail at the model API instead of here.
fn sniff_picture(b: &[u8]) -> Option<Result<Picture, String>> {
    let be16 = |i: usize| {
        b.get(i..i + 2)
            .map(|s| u16::from_be_bytes([s[0], s[1]]) as u32)
    };
    let le16 = |i: usize| {
        b.get(i..i + 2)
            .map(|s| u16::from_le_bytes([s[0], s[1]]) as u32)
    };
    let le24 = |i: usize| {
        b.get(i..i + 3)
            .map(|s| u32::from_le_bytes([s[0], s[1], s[2], 0]))
    };
    let pic = |media_type, dims: Option<(u32, u32)>| {
        Some(match dims {
            Some((width, height)) if width > 0 && height > 0 => Ok(Picture {
                media_type,
                width,
                height,
            }),
            _ => Err(format!("{media_type}: the header does not parse")),
        })
    };

    if b.starts_with(b"\x89PNG\r\n\x1a\n") {
        // IHDR is always the first chunk: width and height right after it.
        let dims = (b.get(12..16) == Some(b"IHDR")).then(|| {
            let w = b.get(16..20)?;
            let h = b.get(20..24)?;
            Some((
                u32::from_be_bytes([w[0], w[1], w[2], w[3]]),
                u32::from_be_bytes([h[0], h[1], h[2], h[3]]),
            ))
        });
        return pic("image/png", dims.flatten());
    }
    if b.starts_with(b"GIF87a") || b.starts_with(b"GIF89a") {
        return pic("image/gif", le16(6).zip(le16(8)));
    }
    if b.starts_with(b"\xff\xd8\xff") {
        // Walk the marker segments to the first start-of-frame.
        let mut i = 2;
        let dims = loop {
            if b.get(i) != Some(&0xff) {
                break None;
            }
            while b.get(i) == Some(&0xff) {
                i += 1; // fill bytes
            }
            let Some(&m) = b.get(i) else { break None };
            i += 1;
            match m {
                0xd8 | 0x01 | 0xd0..=0xd7 => continue, // no length field
                0xd9 => break None,                    // end of image, no frame
                0xc0..=0xcf if m != 0xc4 && m != 0xc8 && m != 0xcc => {
                    // length(2) precision(1) height(2) width(2)
                    break be16(i + 5).zip(be16(i + 3));
                }
                _ => match be16(i) {
                    Some(len) if len >= 2 => i += len as usize,
                    _ => break None,
                },
            }
        };
        return pic("image/jpeg", dims);
    }
    if b.starts_with(b"RIFF") && b.get(8..12) == Some(b"WEBP") {
        let dims = match b.get(12..16) {
            // lossy: 14-bit sizes after the 9d 01 2a start code
            Some(b"VP8 ") if b.get(23..26) == Some(&[0x9d, 0x01, 0x2a]) => le16(26)
                .zip(le16(28))
                .map(|(w, h)| (w & 0x3fff, h & 0x3fff)),
            // lossless: 14-bit sizes minus one, packed after the 0x2f signature
            Some(b"VP8L") if b.get(20) == Some(&0x2f) => b.get(21..25).map(|s| {
                let v = u32::from_le_bytes([s[0], s[1], s[2], s[3]]);
                ((v & 0x3fff) + 1, ((v >> 14) & 0x3fff) + 1)
            }),
            // extended: 24-bit canvas sizes minus one
            Some(b"VP8X") => le24(24).zip(le24(27)).map(|(w, h)| (w + 1, h + 1)),
            _ => None,
        };
        return pic("image/webp", dims);
    }
    None
}

/// read_sync mirrors the old in-container python script: utf-8 (lossy)
/// whole-file read, 1-based line offset + limit window. A picture is the
/// exception and comes back whole as bytes (see ReadResponse.image).
fn read_sync(req: ReadRequest) -> ReadResponse {
    let fail = |e: String| ReadResponse {
        error: e,
        ..Default::default()
    };
    let root = match toolfs::open_root(&req.container) {
        Ok(r) => r,
        Err(e) => return fail(e),
    };
    let mut f = match toolfs::open_file(&root, &req.path, libc::O_RDONLY, 0) {
        Ok(f) => f,
        Err(e) => return fail(format!("Path not found: {} ({e})", req.path)),
    };
    let mut buf = Vec::new();
    if let Err(e) = std::io::Read::take(&mut f, READ_MAX_BYTES + 1).read_to_end(&mut buf) {
        return fail(format!("read {}: {e}", req.path));
    }
    if buf.len() as u64 > READ_MAX_BYTES {
        return fail(format!(
            "file exceeds {} MiB — use offset/limit is not implemented for >cap; file is too large",
            READ_MAX_BYTES / 1024 / 1024
        ));
    }
    match sniff_picture(&buf) {
        None => {}
        Some(Err(e)) => return fail(format!("{}: {e}", req.path)),
        Some(Ok(p)) => {
            if buf.len() > IMAGE_MAX_BYTES {
                return fail(format!(
                    "{}: {} is {} KiB, images are read up to {} KiB — take a smaller \
                     one (a viewport instead of a full-page screenshot, or JPEG)",
                    req.path,
                    p.media_type,
                    buf.len() / 1024,
                    IMAGE_MAX_BYTES / 1024
                ));
            }
            if p.width > IMAGE_MAX_SIDE || p.height > IMAGE_MAX_SIDE {
                return fail(format!(
                    "{}: {} is {}x{} px, images are read up to {IMAGE_MAX_SIDE} px on a \
                     side — take a smaller one (a viewport instead of a full-page screenshot)",
                    req.path, p.media_type, p.width, p.height
                ));
            }
            return ReadResponse {
                image: buf,
                media_type: p.media_type.to_string(),
                width: p.width,
                height: p.height,
                ..Default::default()
            };
        }
    }
    let text = String::from_utf8_lossy(&buf);
    let lines: Vec<&str> = text.lines().collect();
    let total = lines.len();
    let off = req.offset.max(1) as usize;
    let lim = if req.limit == 0 {
        READ_DEF_LIMIT
    } else {
        req.limit as usize
    };
    if off > total {
        return ReadResponse::default(); // empty selection, like python slice
    }
    let end = (off - 1 + lim).min(total);
    let sel = &lines[off - 1..end];
    let mut out = sel.join("\n");
    // Re-add the trailing newline the selected range had in the file.
    let ends_with_nl = text.ends_with('\n');
    if !out.is_empty() && (end < total || ends_with_nl) {
        out.push('\n');
    }
    ReadResponse {
        text: out,
        truncated: end < total,
        ..Default::default()
    }
}

fn write_sync(req: WriteRequest) -> WriteResponse {
    let fail = |e: String| WriteResponse { error: e };
    let root = match toolfs::open_root(&req.container) {
        Ok(r) => r,
        Err(e) => return fail(e),
    };
    let mut f = match toolfs::create_file(&root, &req.path) {
        Ok(f) => f,
        Err(e) => return fail(e),
    };
    match f.write_all(&req.content) {
        Ok(()) => WriteResponse {
            error: String::new(),
        },
        Err(e) => fail(format!("write {}: {e}", req.path)),
    }
}

/// edit_sync: exact-replace semantics identical to the old python script —
/// every oldText must occur EXACTLY once in the pre-edit file.
fn edit_sync(req: EditRequest) -> EditResponse {
    let fail = |e: String| EditResponse {
        error: e,
        ..Default::default()
    };
    if req.edits.is_empty() {
        return fail("edits must not be empty".to_string());
    }
    let root = match toolfs::open_root(&req.container) {
        Ok(r) => r,
        Err(e) => return fail(e),
    };
    let mut f = match toolfs::open_file(&root, &req.path, libc::O_RDONLY, 0) {
        Ok(f) => f,
        Err(e) => return fail(e),
    };
    let mut raw = Vec::new();
    if let Err(e) = f.read_to_end(&mut raw) {
        return fail(format!("read {}: {e}", req.path));
    }
    // strict utf-8 like the python script (no silent corruption)
    let mut src = match String::from_utf8(raw) {
        Ok(s) => s,
        Err(e) => return fail(format!("{} is not valid utf-8: {e}", req.path)),
    };
    for (i, ed) in req.edits.iter().enumerate() {
        let n = src.matches(&ed.old).count();
        if n != 1 {
            return fail(format!("edit {i}: oldText not unique (occurrences: {n})"));
        }
        src = src.replacen(&ed.old, &ed.new, 1);
    }
    let mut f = match toolfs::open_file(&root, &req.path, libc::O_WRONLY | libc::O_TRUNC, 0) {
        Ok(f) => f,
        Err(e) => return fail(format!("truncate {}: {e}", req.path)),
    };
    match f.write_all(src.as_bytes()) {
        Ok(()) => EditResponse {
            replacements: req.edits.len() as u32,
            error: String::new(),
        },
        Err(e) => fail(format!("write {}: {e}", req.path)),
    }
}

/// ls_sync mirrors the python: entries sorted case-insensitively, "/" suffix
/// for directories, dotfiles included, default limit 500.
fn ls_sync(req: LsRequest) -> LsResponse {
    let fail = |e: String| LsResponse {
        error: e,
        ..Default::default()
    };
    let root = match toolfs::open_root(&req.container) {
        Ok(r) => r,
        Err(e) => return fail(e),
    };
    // Open via openat2 (confinement), enumerate via the pinned fd path.
    let dir = match toolfs::open_file(&root, &req.path, libc::O_PATH | libc::O_DIRECTORY, 0) {
        Ok(d) => d,
        Err(e) if e.contains("ENOTDIR") || e.contains("Not a directory") => {
            return fail(format!("Not a directory: {}", req.path))
        }
        Err(e) => return fail(format!("Path not found: {} ({e})", req.path)),
    };
    let lim = if req.limit == 0 {
        LS_DEF_LIMIT
    } else {
        req.limit as usize
    };
    let rd = match std::fs::read_dir(toolfs::fd_walk_path(&dir)) {
        Ok(r) => r,
        Err(e) => return fail(format!("list {}: {e}", req.path)),
    };
    let mut entries: Vec<String> = Vec::new();
    for ent in rd.flatten() {
        let mut name = ent.file_name().to_string_lossy().into_owned();
        let is_dir = ent.file_type().map(|t| t.is_dir()).unwrap_or(false);
        if is_dir {
            name.push('/');
        }
        entries.push(name);
    }
    entries.sort_by_key(|a| a.to_lowercase());
    let truncated = entries.len() > lim;
    entries.truncate(lim);
    LsResponse {
        entries,
        truncated,
        error: String::new(),
    }
}

// -------------------------------------------------------------------- rg --

/// Display-label prefix matching rgs path echo: with path "." (or "/")
/// results echo relative paths, with "/workspace" they echo "/workspace/…".
fn label_prefix(path: &str) -> String {
    let p = if path.is_empty() { "/" } else { path };
    match p {
        "." | "/" => String::new(),
        other => {
            let t = other.trim_end_matches('/');
            if t.is_empty() {
                String::new()
            } else {
                format!("{t}/")
            }
        }
    }
}

/// Line-collecting search sink: formats "label:line:content" (rg
/// --no-heading), "--" separators between context groups, and stops the
/// search by returning Ok(false) once the line or byte limit is hit.
struct RgSink {
    label: String,
    limit: u32,
    cap: usize,
    context: bool,
    out: String,
    matches: u32,
    lines: u32,
    truncated: bool,
    stop: bool,
    pending_break: bool,
}

impl RgSink {
    fn push(&mut self, line: &str) {
        if self.stop {
            return;
        }
        if self.out.len() + line.len() + 1 > self.cap {
            self.truncated = true;
            self.stop = true;
            return;
        }
        self.out.push_str(line);
        self.out.push('\n');
        self.lines += 1;
        if self.lines >= self.limit {
            self.truncated = true;
            self.stop = true;
        }
    }

    fn maybe_break(&mut self) {
        if self.context && self.pending_break && !self.out.is_empty() {
            self.push("--");
        }
        self.pending_break = false;
    }
}

impl Sink for RgSink {
    type Error = std::io::Error;

    fn matched(
        &mut self,
        _searcher: &Searcher,
        mat: &SinkMatch<'_>,
    ) -> Result<bool, std::io::Error> {
        if self.stop {
            return Ok(false);
        }
        self.maybe_break();
        let n = mat.line_number().unwrap_or(0);
        let text = String::from_utf8_lossy(mat.bytes());
        let line = format!("{}:{}:{}", self.label, n, text.trim_end_matches('\n'));
        self.push(&line);
        self.matches += 1;
        Ok(!self.stop)
    }

    fn context(
        &mut self,
        _searcher: &Searcher,
        ctx: &grep_searcher::SinkContext<'_>,
    ) -> Result<bool, std::io::Error> {
        if self.stop {
            return Ok(false);
        }
        if self.context {
            self.maybe_break();
            let n = ctx.line_number().unwrap_or(0);
            let text = String::from_utf8_lossy(ctx.bytes());
            let line = format!("{}-{}-{}", self.label, n, text.trim_end_matches('\n'));
            self.push(&line);
        }
        Ok(!self.stop)
    }

    fn context_break(&mut self, _searcher: &Searcher) -> Result<bool, std::io::Error> {
        self.pending_break = true;
        Ok(!self.stop)
    }

    fn binary_data(
        &mut self,
        _searcher: &Searcher,
        _byte_offset: u64,
    ) -> Result<bool, std::io::Error> {
        Ok(false)
    }
}

fn rg_sync(req: RgRequest) -> RgResponse {
    let fail = |e: String| RgResponse {
        error: e,
        ..Default::default()
    };
    if req.pattern.is_empty() {
        return fail("pattern is required".to_string());
    }
    let root = match toolfs::open_root(&req.container) {
        Ok(r) => r,
        Err(e) => return fail(e),
    };
    let rel = match toolfs::to_rel(&req.path) {
        Ok(r) => r,
        Err(e) => return fail(e),
    };

    let mpattern = if req.literal {
        regex_syntax::escape(&req.pattern)
    } else {
        req.pattern.clone()
    };
    let matcher = match grep_regex::RegexMatcherBuilder::new()
        .case_insensitive(req.ignore_case)
        .build(&mpattern)
    {
        Ok(m) => m,
        Err(e) => return fail(format!("rg: {e}")),
    };
    let glob = if req.glob.is_empty() {
        None
    } else {
        match globset::GlobBuilder::new(&req.glob)
            .literal_separator(true)
            .build()
        {
            Ok(g) => Some(g.compile_matcher()),
            Err(e) => return fail(format!("rg -g: {e}")),
        }
    };

    let limit = if req.limit == 0 {
        RG_DEF_LIMIT
    } else {
        req.limit
    };
    let context = req.context as usize;
    let mut searcher = SearcherBuilder::new()
        .binary_detection(BinaryDetection::quit(b'\x00'))
        .line_number(true)
        .before_context(context)
        .after_context(context)
        .build();
    let mut sink = RgSink {
        label: String::new(), // set per file
        limit,
        cap: RG_MAX_BYTES,
        context: context > 0,
        out: String::new(),
        matches: 0,
        lines: 0,
        truncated: false,
        stop: false,
        pending_break: false,
    };
    let prefix = label_prefix(&req.path);

    // Search root: open beneath the rootfs (O_PATH) and walk through the
    // pinned fd path; files are re-opened via openat2 for confinement.
    let base = toolfs::open_file(&root, &req.path, libc::O_PATH | libc::O_DIRECTORY, 0)
        .map(|d| (d, true))
        .or_else(|_| toolfs::open_file(&root, &req.path, libc::O_RDONLY, 0).map(|f| (f, false)));

    let search_file = |searcher: &mut Searcher, sink: &mut RgSink, fd: File, label: String| {
        sink.label = label;
        let _ = searcher.search_reader(&matcher, fd, sink);
    };

    match base {
        Ok((dir, true)) => {
            let walk_root = toolfs::fd_walk_path(&dir);
            let wbuilder = ignore::WalkBuilder::new(&walk_root);
            for ent in wbuilder.build().flatten() {
                if sink.stop {
                    break;
                }
                if ent.depth() == 0 {
                    continue;
                }
                let ft = match ent.file_type() {
                    Some(t) => t,
                    None => continue,
                };
                if !ft.is_file() {
                    continue;
                }
                let full = ent.path().to_string_lossy().into_owned();
                let relfile = full[walk_root.len()..].trim_start_matches('/').to_string();
                if let Some(g) = &glob
                    && !g.is_match(&relfile)
                {
                    continue;
                }
                // content access re-anchored via openat2: a symlink swapped
                // in by the payload between walk and open cannot escape.
                let f = match toolfs::open_file(&root, &join_rel(&rel, &relfile), libc::O_RDONLY, 0)
                {
                    Ok(f) => f,
                    Err(_) => continue, // unreadable: skip like rg's other-errors
                };
                search_file(&mut searcher, &mut sink, f, format!("{prefix}{relfile}"));
            }
        }
        Ok((file, false)) => {
            let label = match rel.rsplit('/').next() {
                Some(name) if !name.is_empty() => format!("{prefix}{name}"),
                _ => prefix.trim_end_matches('/').to_string(),
            };
            search_file(&mut searcher, &mut sink, file, label);
        }
        Err(e) => return fail(format!("{}: {e}", req.path)),
    }

    RgResponse {
        text: sink.out,
        matches: sink.matches,
        truncated: sink.truncated,
        error: String::new(),
    }
}

/// The part of a walked path below the walk's root, without a leading "/".
fn walk_rel<'a>(walk_root: &str, full: &'a str) -> &'a str {
    full.strip_prefix(walk_root)
        .unwrap_or(full)
        .trim_start_matches('/')
}

fn join_rel(base: &str, rel: &str) -> String {
    if base.is_empty() {
        rel.to_string()
    } else {
        format!("{base}/{rel}")
    }
}

// -------------------------------------------------------------------- fd --

/// fd_sync: fd semantics on the `ignore` walker — file-NAME matching,
/// regex by default (glob with glob=true), smart case, gitignore respected,
/// hidden files skipped (stock fd defaults).
fn fd_sync(req: FdRequest) -> FdResponse {
    let fail = |e: String| FdResponse {
        error: e,
        ..Default::default()
    };
    if req.pattern.is_empty() {
        return fail("pattern is required".to_string());
    }
    let root = match toolfs::open_root(&req.container) {
        Ok(r) => r,
        Err(e) => return fail(e),
    };
    if let Err(e) = toolfs::to_rel(&req.path) {
        return fail(e);
    }
    let base = match toolfs::open_file(&root, &req.path, libc::O_PATH | libc::O_DIRECTORY, 0) {
        Ok(d) => d,
        Err(e) => return fail(format!("{}: not a directory ({e})", req.path)),
    };

    // smart case: insensitive iff the pattern has no uppercase chars (fd)
    let sensitive = req.pattern.chars().any(|c| c.is_uppercase());
    let name_match: Box<dyn Fn(&str) -> bool> = if req.glob {
        match globset::GlobBuilder::new(&req.pattern)
            .case_insensitive(!sensitive)
            .literal_separator(true)
            .build()
        {
            Ok(g) => {
                let m = g.compile_matcher();
                Box::new(move |name: &str| m.is_match(name))
            }
            Err(e) => return fail(format!("fd --glob: {e}")),
        }
    } else {
        match regex::RegexBuilder::new(&req.pattern)
            .case_insensitive(!sensitive)
            .build()
        {
            Ok(re) => Box::new(move |name: &str| re.is_match(name)),
            Err(e) => return fail(format!("fd: {e}")),
        }
    };

    let lim = if req.limit == 0 {
        FD_DEF_LIMIT
    } else {
        req.limit as usize
    };
    let prefix = label_prefix(&req.path);
    let walk_root = toolfs::fd_walk_path(&base);
    let mut entries: Vec<String> = Vec::new();
    let mut truncated = false;
    for ent in ignore::WalkBuilder::new(&walk_root).build().flatten() {
        if ent.depth() == 0 {
            continue;
        }
        let name = ent.file_name().to_string_lossy();
        if !name_match(&name) {
            continue;
        }
        // The name matches, the answer is the path below the base.
        let full = ent.path().to_string_lossy();
        entries.push(format!("{prefix}{}", walk_rel(&walk_root, &full)));
        if entries.len() >= lim {
            truncated = true;
            break;
        }
    }
    FdResponse {
        entries,
        truncated,
        error: String::new(),
    }
}

// ------------------------------------------------------------ tonic wiring

#[tonic::async_trait]
impl ToolService for ToolApiHandler {
    async fn webfetch(
        &self,
        request: Request<WebfetchRequest>,
    ) -> Result<Response<WebfetchResponse>, Status> {
        let url = request.into_inner().url;
        let parsed = reqwest::Url::parse(&url)
            .map_err(|_| Status::invalid_argument(format!("invalid url {url:?}")))?;
        let scheme = parsed.scheme();
        if (scheme != "http" && scheme != "https") || parsed.host_str().is_none() {
            return Err(Status::invalid_argument(format!(
                "invalid url {url:?} (http/https only)"
            )));
        }
        // The gRPC deadline bounds the whole call; REQ_TIMEOUT the request.
        Ok(Response::new(webfetch(&url).await.0))
    }

    async fn read(&self, request: Request<ReadRequest>) -> Result<Response<ReadResponse>, Status> {
        run_blocking(request.into_inner(), read_sync).await
    }

    async fn write(
        &self,
        request: Request<WriteRequest>,
    ) -> Result<Response<WriteResponse>, Status> {
        run_blocking(request.into_inner(), write_sync).await
    }

    async fn edit(&self, request: Request<EditRequest>) -> Result<Response<EditResponse>, Status> {
        run_blocking(request.into_inner(), edit_sync).await
    }

    async fn ls(&self, request: Request<LsRequest>) -> Result<Response<LsResponse>, Status> {
        run_blocking(request.into_inner(), ls_sync).await
    }

    async fn rg(&self, request: Request<RgRequest>) -> Result<Response<RgResponse>, Status> {
        run_blocking(request.into_inner(), rg_sync).await
    }

    async fn fd(&self, request: Request<FdRequest>) -> Result<Response<FdResponse>, Status> {
        run_blocking(request.into_inner(), fd_sync).await
    }
}

async fn run_blocking<T: Send + 'static, R: Send + 'static>(
    req: T,
    f: fn(T) -> R,
) -> Result<Response<R>, Status> {
    tokio::task::spawn_blocking(move || f(req))
        .await
        .map(Response::new)
        .map_err(|e| Status::internal(format!("tool worker: {e}")))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn png(w: u32, h: u32) -> Vec<u8> {
        let mut b = b"\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR".to_vec();
        b.extend_from_slice(&w.to_be_bytes());
        b.extend_from_slice(&h.to_be_bytes());
        b.extend_from_slice(&[8, 6, 0, 0, 0]);
        b
    }

    fn ok(media_type: &'static str, width: u32, height: u32) -> Option<Result<Picture, String>> {
        Some(Ok(Picture {
            media_type,
            width,
            height,
        }))
    }

    #[test]
    fn walked_paths_keep_their_directories() {
        let root = "/proc/self/fd/7";
        assert_eq!(walk_rel(root, "/proc/self/fd/7/0001-a.md"), "0001-a.md");
        assert_eq!(
            walk_rel(root, "/proc/self/fd/7/ADRs/0001-a.md"),
            "ADRs/0001-a.md"
        );
        assert_eq!(
            format!(
                "{}{}",
                label_prefix("/workspace/docs"),
                walk_rel(root, "/proc/self/fd/7/ADRs/0001-a.md")
            ),
            "/workspace/docs/ADRs/0001-a.md"
        );
    }

    #[test]
    fn png_size_comes_from_ihdr() {
        assert_eq!(sniff_picture(&png(1280, 720)), ok("image/png", 1280, 720));
    }

    #[test]
    fn gif_size_is_little_endian() {
        let b = b"GIF89a\x00\x05\xd0\x02\x00\x00";
        assert_eq!(sniff_picture(b), ok("image/gif", 1280, 720));
    }

    #[test]
    fn jpeg_walks_segments_to_the_frame() {
        let mut b = vec![0xff, 0xd8];
        // APP0 (JFIF), then a DHT that must be skipped, not taken for a frame
        b.extend_from_slice(&[0xff, 0xe0, 0x00, 0x10]);
        b.extend_from_slice(b"JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00");
        b.extend_from_slice(&[0xff, 0xc4, 0x00, 0x04, 0x00, 0x00]);
        // SOF2 (progressive): length, precision, height 720, width 1280
        b.extend_from_slice(&[0xff, 0xc2, 0x00, 0x11, 0x08, 0x02, 0xd0, 0x05, 0x00]);
        assert_eq!(sniff_picture(&b), ok("image/jpeg", 1280, 720));
    }

    #[test]
    fn webp_all_three_headers() {
        let riff = |chunk: &[u8], body: &[u8]| {
            let mut b = b"RIFF\x00\x00\x00\x00WEBP".to_vec();
            b.extend_from_slice(chunk);
            b.extend_from_slice(&[0, 0, 0, 0]);
            b.extend_from_slice(body);
            b
        };
        // lossy: frame tag, start code, 14-bit width/height
        let lossy = riff(
            b"VP8 ",
            &[0, 0, 0, 0x9d, 0x01, 0x2a, 0x00, 0x05, 0xd0, 0x02],
        );
        assert_eq!(sniff_picture(&lossy), ok("image/webp", 1280, 720));
        // lossless: (w-1) | (h-1) << 14
        let v: u32 = 1279 | (719 << 14);
        let mut body = vec![0x2f];
        body.extend_from_slice(&v.to_le_bytes());
        assert_eq!(
            sniff_picture(&riff(b"VP8L", &body)),
            ok("image/webp", 1280, 720)
        );
        // extended: 24-bit (w-1), (h-1) after flags + reserved
        let ext = riff(b"VP8X", &[0, 0, 0, 0, 0xff, 0x04, 0x00, 0xcf, 0x02, 0x00]);
        assert_eq!(sniff_picture(&ext), ok("image/webp", 1280, 720));
    }

    #[test]
    fn text_and_svg_stay_text() {
        assert_eq!(sniff_picture(b"hello\n"), None);
        assert_eq!(
            sniff_picture(b"<svg xmlns=\"http://www.w3.org/2000/svg\"/>"),
            None
        );
        assert_eq!(sniff_picture(b""), None);
    }

    #[test]
    fn a_broken_header_is_an_error_not_text() {
        let b = &png(10, 10)[..18];
        assert!(matches!(sniff_picture(b), Some(Err(_))));
        assert!(matches!(sniff_picture(&png(0, 10)), Some(Err(_))));
        assert!(matches!(sniff_picture(b"\xff\xd8\xff\xd9"), Some(Err(_))));
    }
}
