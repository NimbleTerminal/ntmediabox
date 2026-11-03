# Security Policy & Perimeter (`ntmediabox`)

This document describes the security perimeter of `ntmediabox`
(`nimbleterminal.dev/ntmediabox` and `nimbleterminal.dev/ntmediabox/pkg/mediabox`),
how it combines OS subprocess confinement (`ntconfine`) with hostile-peer wire
validation, and the invariants that human reviewers and coding agents must
preserve.

## Reporting a Vulnerability

Please **do not** report security vulnerabilities in public GitHub issues.
Send reports privately to **`security@neomantra.net`** with a description of the
issue, affected platform, and a minimal reproduction.

---

## 1. Threat Model & Two-Layer Defense

`ntmediabox` isolates risky media and document decoders (`libmpv`, `ffmpeg`,
PDFium, SVG/image/font rasterizers) from the host TUI or server process using
two independent boundaries:

1. **OS Process Confinement (via [`ntconfine`](https://github.com/NimbleTerminal/ntconfine)):**
   The worker runs in a separate OS-confined subprocess (`landlock+seccomp` on
   Linux, `sandbox-exec` with `dyld-support.sb` on macOS, fail-closed on
   unsupported platforms unless `AllowUnsandboxed: true`). If a malicious media
   or document file exploits a memory-safety bug in the decoder, the OS blocks
   it from reading `$HOME` or `/etc`, opening network sockets, accessing
   `/dev/tty`, or signaling the host.
2. **Hostile-Peer Wire Validation (`pkg/mediabox`):**
   Even after a worker is compromised inside its OS sandbox, it still holds `fd 3`
   connected to the host. `Client` and `RenderClient` therefore **treat every
   byte arriving from the worker as hostile**:
   - Every length prefix, string length, and image dimension is checked against
     hard upper bounds **before** allocating memory.
   - Every worker string destined for UI/status display is validated as UTF-8
     and stripped of terminal control sequences and Unicode bidi overrides.
   - Any protocol violation (`ErrProtocol`) or request timeout (`ctx.Done()`)
     immediately closes the socket and kills and reaps the worker process group.

---

## 2. Hard Wire Bounds & Pre-Allocation Checks

 All multi-byte integers on the wire are big-endian, with `[u32 length][u8 type][payload]`
framing (`length == 1 + len(payload)`).

### Streaming Media Protocol (`Client` / `Serve`)

| Bound | Constant / Value | Invariant enforced before allocation |
| --- | --- | --- |
| Control frame size | `maxControlLen = 32768` (`32 KiB`) | `1 <= n <= maxControlLen` on all control frames (`msgHello`, `msgOpen`, `msgStatus`, etc.) |
| Video frame dimensions | `MaxFrameSide = 4096` | `1 <= w <= maxW <= MaxFrameSide` and `1 <= h <= maxH <= MaxFrameSide` |
| Video frame byte length | `frameHeader = 24` | `uint64(n) == 1 + 24 + uint64(w)*uint64(h)*4` checked in `uint64` arithmetic before allocating `image.NewRGBA` |
| Filename / kind length | `maxNameLen = 255` | Bare filename only (`isBareFilename`): no `/`, `\`, `..`, NUL, or control chars |
| Subtitle / track caps | `maxCaptions = 16`, `maxTracks = 16`, `maxTrackFieldLen = 255` | Rejects counts `> 16` or trailing unparsed bytes |
| Caption & error text | `maxCaptionTextLen = 4096`, `maxErrLen = 1024` | Must be valid UTF-8; sanitized via `sanitizeWorkerString` |

### Document, Vector & Still-Image Protocol (`RenderClient` / `ServeRender`)

| Bound | Constant / Value | Invariant enforced before allocation |
| --- | --- | --- |
| Raster dimensions | `MaxRenderSide = 8192` | `1 <= imgW <= maxW <= MaxRenderSide` and `1 <= imgH <= maxH <= MaxRenderSide` |
| Raster pixel budget | `MaxRenderPixels = 32 << 20` (`32 Mpx` / `128 MiB` RGBA) | `uint64(imgW)*uint64(imgH) <= uint64(maxPixels) <= MaxRenderPixels` |
| Request input bytes | `MaxRenderDataBytes = 128 << 20` (`128 MiB`) | `dataLen <= MaxRenderDataBytes` |
| Structured metadata bytes | `MaxRenderMetaBytes = 16 << 20` (`16 MiB`) | `metaLen <= MaxRenderMetaBytes` |
| Exact response frame size | `renderRespHeader = 39` | `uint64(n) == 1 + 39 + uint64(errLen) + uint64(metaLen) + uint64(imgW)*uint64(imgH)*4` verified before allocating `Meta` or `Image` |
| Intrinsic dimensions | `Width`, `Height` (`float64`) | Must be finite (`!math.IsNaN`, `!math.IsInf`) and `>= 0` |

---

## 3. Terminal Escape & Bidi Sanitization

A compromised worker must never be able to inject terminal escape sequences or
Trojan-Source bidi overrides into the host terminal:

- **`Status.Err`, `Status.CaptionText`, `Track.{ID,Label,Language,Kind}`, and `RenderResponse.Err`:**
  Validated as UTF-8 (`utf8.ValidString`) and passed through `sanitizeWorkerString`:
  - Strips ANSI CSI (`ESC [ ... final`) and OSC (`ESC ] ... BEL/ST`) sequences
    and bare `ESC` (`0x1b`) prefixes.
  - Strips all C0 controls (`< 0x20`, except `\n` in `CaptionText` and `\t`
    converted to space), `DEL` (`0x7f`), C1 controls (`U+0080..U+009F`), line/paragraph
    separators (`U+2028`, `U+2029`), and Unicode bidi override/isolate codepoints
    (`U+200E`, `U+200F`, `U+061C`, `U+202A..U+202E`, `U+2066..U+2069`).
- **`RenderResponse.Meta`:**
  Returned as raw untrusted bytes (`<= MaxRenderMetaBytes`) for structured
  machine parsing (e.g., JSON layout/hit-test metadata). Hosts **must not**
  write `Meta` bytes directly to a terminal without sanitizing any extracted
  strings first.

---

## 4. Path Traversal & Generation Integrity

- **Bare filenames only (`isBareFilename`):**
  Both `Client.Open`/`OpenMedia` (on the host) and `parseOpen` (in the worker)
  reject any media or caption `Name` that is empty, `.`, `..`, contains `/` or
  `\`, or contains control characters. The worker resolves bare filenames inside
  its pre-staged media directory and checks that the target is a regular file
  with no symlink traversal outside that directory.
- **Monotonic command generations (`Gen`):**
  `Client` increments an atomic `uint64` generation counter on every state-changing
  command (`Open`, `OpenMedia`, `Seek`, `SelectTrack`, `SetCaptionOverlay`,
  `Stop`). The worker echoes its active `gen` in every `Status` and `Frame`
  header so the host can deterministically discard stale frames and statuses
  that were already in flight before a seek or file switch.
- **Immediate kill on timeout or protocol error:**
  `StartMediaHelper` and `StartRenderHelper` wire `onKill` to `Helper.Close()`.
  If the worker violates the wire protocol (`ErrProtocol`) or if a
  `RenderClient.Do(ctx, req)` call is cancelled by `ctx.Done()`, the client
  closes the connection and `Helper.Close()` sends `SIGKILL` to the worker's
  process group and reaps the child.

---

## 5. Invariant Checklist for Humans and Coding Agents

When modifying `ntmediabox`, verify every item below:

- [ ] **Zero external third-party dependencies:** `nimbleterminal.dev/ntmediabox`
      depends only on `nimbleterminal.dev/ntconfine`,
      `nimbleterminal.dev/ntmediabox/pkg/mediabox`, and the Go standard library
      (`CGO_ENABLED=0`), and never imports `goffi`, `ntvideo`, `ntbrowser`, or
      Bubble Tea.
- [ ] **Standalone `pkg/mediabox`:** `nimbleterminal.dev/ntmediabox/pkg/mediabox`
      depends only on the Go standard library (`CGO_ENABLED=0`) under its MIT
      license.
- [ ] **Hostile helper output:** Never allocate a slice or `image.RGBA` from a
      worker-supplied length or `w, h` pair until all bounds and exact
      `uint64` byte-count equations are verified.
- [ ] **Fuzz coverage:** Any change to wire encoding or parsing must keep
      `FuzzParseStatusAndOpen`, `FuzzSanitizeAndTruncateUTF8`, `FuzzServeRead`,
      `FuzzRenderClientResp`, and `FuzzClientRead` passing.
- [ ] **Preflight gate:** Run `task ci` before committing.
