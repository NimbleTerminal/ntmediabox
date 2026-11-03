# ntmediabox: sandboxed media workers for Nimble Terminal — design

Date: 2026-11-01 · Status: approved 2026-11-01 (Evan) · Author: Antigravity
Extends `ntbrowser/docs/design/2026-10-31-ntbrowser-profiles-content-design.md` (§5, decisions 21–27).

## 1. Intent

`ntbrowser` runs risky native media decoding (`libmpv` via `goffi`) in a separate, OS-confined helper process (`ntbrowser-media`) speaking a small length-prefixed binary protocol (`mediaproc`). Untrusted media bytes stay inside the helper; only bounds-checked RGBA pixels and status cross back to the host.

`gloss` is the second host that needs this boundary. Today `gloss` plays video in-process through `ntvideo.Model` (`libmpv` loaded into the `gloss` process via `goffi`), with a scrubber, sidecar and embedded subtitles, terminal text captions in glyph mode, burned-in subtitles in Kitty mode, and `Keep()`/`Close()` lifecycle management.

`ntmediabox` (`nimbleterminal.dev/ntmediabox`) extracts the helper confinement and wire protocol into a shared, stdlib-only Go module shaped by both hosts' needs, and `ntvideo` gains a remote worker backend and decoder adapter so any `ntvideo.Model` host can isolate native decoding without giving up `Model`'s UI or capabilities.

## 2. Goals, non-goals, and invariants

### Goals
- Extract helper confinement and the frame-worker protocol from `ntbrowser` into `nimbleterminal.dev/ntmediabox` (with the pure-Go wire protocols in a standalone MIT-licensed submodule `nimbleterminal.dev/ntmediabox/pkg/mediabox`) with no dependency on `ntbrowser/profile`, `ntvideo`, `goffi`, or Bubble Tea.
- Provide both a streaming media protocol (`Client`, `Serve`, `Decoder`, `StartMediaHelper`) and a request/response render protocol (`RenderClient`, `ServeRender`, `RenderHandler`, `StartRenderHelper`) for sandboxed PDF (`ntpdf`), SVG/Mermaid (`ntsvg`), raster image (`ntpicture`), and typesetting (`nttypeset`) workers.
- Move `ntbrowser` onto `ntmediabox` with zero behaviour or profile change (`TestMainDoesNotLinkNativeCode` and all profile defaults stand).
- Support `gloss`'s video playback needs (initial paused frame, seeking while paused or playing, sidecar and embedded subtitle tracks, track cycling, burned-in vs terminal-text caption rendering, `Limits`, and `Keep`/`Close`) through a remote backend and worker adapter in `ntvideo`.
- Put `gloss`'s worker-backed video playback behind a CLI flag until proven.
- Keep Windows working when sandbox enforcement is optional (`AllowUnsandboxed: true`), while failing closed (`ErrUnavailable`) when enforcement is required.

### Non-goals
- **Audio** (see §4.1). This project ships silent frame output only (`ao=null`, `aid=no`).
- **GPU access, hardware decoding, or shared memory** between helper and host. Decoding in the helper is software-only (`hwdec=no`). (Note: on the host side, `ntpicture` may still present the validated RGBA frames to a local Kitty terminal using its own transport.)
- **General process/plugin sandboxing.** `ntmediabox` is a media and document/image frame worker, not a general arbitrary-command sandbox framework.
- **Windows OS sandbox enforcement.** Windows reports enforcement as unavailable.

### Rules that must survive the move
1. **Fail closed:** when OS enforcement is required (`AllowUnsandboxed: false`, the zero-value default) and unavailable, starting the helper returns `ErrUnavailable`. There is no silent fallback.
2. **Honest per-OS reporting:** report what was actually applied (`landlock+seccomp` with ABI version on Linux, `sandbox-exec` marked deprecated on macOS, `none` when unsandboxed or on Windows).
3. **Closed by default:** the zero `ntmediabox.Policy` grants no readable, writable, or executable paths, allows no network, and requires OS enforcement.
4. **Hostile helper output:** the host checks every message length and frame dimension (`1 <= w <= maxW <= MaxFrameSide`, `1 <= h <= maxH <= MaxFrameSide`, `n == 1 + frameHeader + w*h*4`) and sanitizes worker strings before allocating pixel memory or returning status. When started via `StartMediaHelper`, `StartRenderHelper`, `NewClientWithKill`, or `NewRenderClientWithKill`, any protocol violation kills and reaps the helper process via `Helper.Close()` and closes the client without taking down the host.
5. **Lifecycle coupling:** the helper dies when the host exits (via `CLOEXEC`-hardened socket/pipe EOF and explicit `SIGKILL`/process termination on close or protocol violation).
6. **Test preservation:** all hostile-peer tests, fuzz targets (`FuzzClientRead`, `FuzzRenderClientResp`), and sandbox escape tests (`TestHelperIsConfined`, `TestLockedCannotEscape`, `io_uring` denial, `seccomp` `TSYNC` failure, BPF simulator) survive and run on real kernels (failing closed in CI when `NTMEDIABOX_REQUIRE_SANDBOX=1`).

## 3. Recon findings

1. **Why `goffi` breaks Linux self-staging (`AllThreadsSyscall`):**
   - On Linux, `landlock_restrict_self` and `prctl(PR_SET_NO_NEW_PRIVS)` are per-OS-thread in the kernel (unlike `seccomp`, Landlock has no `TSYNC` flag). Because the Go runtime spawns multiple OS threads (`sysmon`, GC, template thread) before `main()` runs, restricting a Go process requires `syscall.AllThreadsSyscall`.
   - `github.com/go-webgpu/goffi/ffi` imports `github.com/go-webgpu/goffi/internal/fakecgo` on Linux. `fakecgo` sets `syscall.cgo_libc_setegid` to non-nil and `runtime.iscgo = true` without linking real `libc` thread-sync hooks.
   - In Go's `syscall/syscall_linux.go`, `AllThreadsSyscall` immediately returns `ENOTSUP` whenever `cgo_libc_setegid != nil` (and `runtime.syscall_runtime_doAllThreadsSyscall` panics if `iscgo` is true).
2. **Why `gloss` links `goffi` on Linux:**
   - `gloss` links `goffi` through **two** independent dependency chains:
     1. `gloss/internal/app` → `nimbleterminal.dev/ntvideo` → `github.com/go-webgpu/goffi/ffi`
     2. `gloss/internal/app` & `gloss/internal/document` → `nimbleterminal.dev/nt3d` → `nimbleterminal.dev/ntgpuimage` → `github.com/gogpu/wgpu/hal/allbackends` (`hal/vulkan`, `hal/gles`, `hal/software`) → `github.com/go-webgpu/goffi/ffi`
   - Removing `goffi` from `gloss`'s main binary is not feasible without stripping GPU 3D rendering (`nt3d`/`ntgpuimage`) and in-process `ntvideo` (which must remain while the worker is behind a flag).
3. **Latent `best-effort` bug in `ntbrowser/sandbox/helper_unix.go`:**
   - Today in `ntbrowser`, if `!Available()` and `p.Enforcement == EnforceBestEffort`, `StartHelper` passes `checkRequired(p, false)` and then still calls `helperCommand`, which invokes `sandbox-exec` or `MediaStage` (Landlock) and fails instead of running the helper unconfined. `ntmediabox` fixes this so `AllowUnsandboxed: true` cleanly starts the helper without `sandbox-exec` or Landlock when `Available()` is false, while recording `Enforced() == false`.
4. **`ntvideo` v0.4.0 architecture:**
   - `ntvideo.Model` drives an unexported `backend` interface (`Load`, `Play`, `Pause`, `Seek`, `SetVolume`, `SetMuted`, `SelectTrack`, `Layout`, `SetFit`, `Poll`, `Frame`, `Close`, plus optional `setCaptionOverlay(bool)`).
   - `mpvBackend` (`backend_mpv.go`) already implements sidecar caption loading (`sub-add`), embedded + external subtitle track listing (`Status.CaptionTracks`), track selection (`sid`), burned-in vs plain-text subtitle toggling (`sub-visibility` + `Status.CaptionText`), paused initial-frame rendering, and dimension limit checks (`checkDimensions`).
   - `Player` (`player.go`) is a thin wrapper around `backend`, currently exposing only `Load(path)`, `Play`, `Pause`, `Seek`, `Status`, `Frame`, and `Close`.

## 4. Resolution of the seven hard problems

### 4.1 Audio (deferred)

`ntmediabox` workers run with `DisableAudio: true` (`ao=null`, `aid=no`) and no access to audio devices or sound servers. When `gloss` runs video through the worker, playback is silent.

For a later project, the three options for audio are recorded here so they are not lost:
1. **Stay silent in sandboxed mode:** keep the worker silent and require in-process playback when audio is needed.
2. **Opt-in wider audio policy:** add an explicit audio gate to `Policy` that opens the platform audio IPC (macOS CoreAudio Mach services `com.apple.audio.audiohald` / `coreaudiod`; Linux PipeWire/PulseAudio Unix socket or `/dev/snd`), trading sandbox narrowness for native audio sync.
3. **Stream decoded PCM over the worker socket:** keep the helper completely isolated from OS audio services, configure `libmpv` to decode audio to raw bounded PCM chunks, send `msgAudio` frames over the `ntmediabox` socket, and play PCM in the host process.

No audio protocol messages or policy gates are added in this project.

### 4.2 Linux stage, packaging, and binary trust

#### Evaluation: standalone stage binary vs removing `goffi` from `gloss`
- **Removing `goffi` from `gloss` (rejected):** As established in §3.2, `gloss` links `goffi` via both `ntvideo` and `nt3d` (`ntgpuimage`/`gogpu/wgpu`). Even if `ntvideo` split `mpvBackend` out, `gloss` still links `goffi` for 3D rendering and for default in-process video while the worker is behind a flag. Worse, requiring every host's main binary to be `goffi`-free would prevent any host using `nt3d`, `goffi`, `purego`, or `cgo` from using `ntmediabox` on Linux.
- **Standalone pure-Go stage binary `cmd/ntmediabox-stage` (adopted):**
  - `nimbleterminal.dev/ntmediabox` has zero external dependencies (stdlib only, `CGO_ENABLED=0`).
  - It ships `cmd/ntmediabox-stage`, a ~30-line pure-Go executable in the root `ntmediabox` module that calls `ntmediabox.RunStage()`. Because it links only `os`, `syscall`, `unsafe`, `encoding/json`, and `fmt`, `syscall.AllThreadsSyscall` always succeeds on Linux.
  - `ntmediabox` also exports `RunStage(args []string) error` so a pure-Go host whose main binary links no `goffi` (`ntbrowser`) can still use its own executable (`os.Executable()` with `--media-stage`) if configured, keeping `ntbrowser` at two binaries (`ntbrowser` and `ntbrowser-media`). If `RunStage` ever encounters `ENOTSUP` from `AllThreadsSyscall` (e.g., misconfigured on a binary linking `goffi`), it fails closed with an explicit error explaining that `ntmediabox-stage` is required.

#### How the stage receives its policy on Linux and macOS
In `ntbrowser` previously, `MediaStage` hardcoded `helperLibPaths`, `dynamicLoaders`, `HelperPath()`, and a single `tempDir` argument. In `ntmediabox`:
- On **Linux**, `helperCommand` serializes the validated `stageSpec` (the resolved `Policy`, resolved `WorkerPath`, and `WorkerArgs`) as base64-encoded JSON over an inherited pipe descriptor (`--media-stage fd:<N>`) so policy paths never appear in `/proc/<pid>/cmdline`. The stage process reads the pipe, decodes `stageSpec` in memory, applies resource limits (`RLIMIT_CORE=0`, `RLIMIT_NOFILE`, `RLIMIT_AS`, `RLIMIT_NPROC`), applies the Landlock ruleset from `Policy.ReadPaths`, `Policy.WritePaths`, and `Policy.ExecPaths` across all threads, installs the `seccomp-bpf` filter with `SECCOMP_FILTER_TSYNC`, and calls `syscall.Exec(spec.WorkerPath, spec.WorkerArgs, []string{})`.
- On **macOS**, `helperCommand` writes the rendered SBPL policy to a `0600` file and invokes `/usr/bin/sandbox-exec -f <profilePath>` (removing ephemeral profile files in `Helper.Close()`).

#### How the host finds and trusts binaries
1. **No `$PATH` lookup:** `ResolveSibling(name string)` resolves `os.Executable()` via `filepath.EvalSymlinks(exe)` and looks only in `filepath.Dir(resolvedExe)` (appending `.exe` on Windows). It never searches `$PATH` or the working directory.
2. **Ownership and permission checks (`VerifyBinary`):** Before executing a stage or worker binary, `ntmediabox` resolves symlinks on the target path and checks `os.Stat`:
   - Must be a regular file (`mode.IsRegular()`).
   - On Unix (`darwin`, `linux`):
     - Must be executable (`mode.Perm()&0o111 != 0`).
     - Must not be world-writable (`mode.Perm()&0o002 == 0`).
     - Must be owned by `os.Geteuid()` or `root` (`uid == 0`).
     - Its parent directory must not be world-writable without the sticky bit (`dirMode.Perm()&0o002 == 0 || dirMode&os.ModeSticky != 0`).

#### Worker binary for `gloss`
- `ntbrowser` builds `bin/ntbrowser` and `bin/ntbrowser-media` (unchanged).
- For `gloss`: because `gloss` already links `ntvideo` (and already uses self-reexec `--gloss-parser-worker` for PDF/SVG/Mermaid workers in `internal/document/parser_worker_native.go`), `gloss` can either:
  - **Option A (recommended):** Use `gloss --gloss-media-worker <dir>` as the worker entrypoint (calling `ntvideo.ServeWorker`), paired with `ntmediabox-stage` on Linux. On macOS and Windows `gloss` stays a single binary; on Linux `task build` and release packaging build `ntmediabox-stage` beside `gloss`.
  - **Option B:** Build a standalone `ntvideo-worker` (or `gloss-media`) binary beside `gloss` on all platforms.
  `ntmediabox.HelperConfig` takes `WorkerPath`, `WorkerArgs`, and `StagePath`, so either choice works with the exact same `ntmediabox` and `ntvideo` APIs.

### 4.3 `ntvideo.Model` integration

#### Evaluation: remote `backend` inside `ntvideo` vs rewriting `gloss` around `Player`
- **Rewriting `gloss` around `Player` (rejected):** `ntvideo.Model` owns ~600 lines of presentation and control logic that `gloss` relies on: the interactive mouse scrubber (`scrubber.go`), terminal-text caption overlay in glyph mode (`captions.go`), `ntpicture.Picture` generation pacing (`PresentedMsg` awaiting and stale-frame rejection), `Keep()`/`KeepOnMainScreen()`/`Close()` cleanup, poster display, render mode switching (`auto`/`glyph`/`raster`), and the browser WASM DOM backend (`backend_js.go`). Rewriting `gloss` around `Player` would duplicate that logic in `gloss` and split `gloss/internal/app/video.go` into two divergent code paths.
- **Remote `backend` inside `ntvideo` + worker `Decoder` adapter in `ntvideo` (adopted):**
  - `ntvideo` imports `nimbleterminal.dev/ntmediabox`.
  - On the **host side**, `ntvideo` adds `WithWorker(cfg WorkerConfig) Option`, which swaps `Model`'s unexported `newBackend` factory for `newWorkerBackend(cfg)`. `workerBackend` implements `ntvideo`'s unexported `backend` interface (plus `setCaptionOverlay(bool)`) over `ntmediabox.Client`.
  - On the **worker side**, `ntvideo` exports `NewWorkerDecoder(dir string, opts PlayerOptions) ntmediabox.Decoder` and `ServeWorker(conn io.ReadWriter, dir string, opts PlayerOptions) error`, which wrap `mpvBackend` (`DisableHardwareDecoding: true`, `DisableAudio: true`) to serve the `ntmediabox` protocol.
  - In `gloss`, enabling the worker flag adds a single `ntvideo.WithWorker(...)` option to `ntvideo.New(...)` in `internal/app/video.go`. All of `gloss`'s keys, scrubber, captions, `Keep()`, export, and browser WASM code remain unchanged.

#### Dependency DAG (no cycles, no native code in `ntbrowser`)
```
nimbleterminal.dev/ntmediabox        (stdlib only; builds on darwin, linux, windows, js/wasm)
       ▲                  ▲
       │                  └──────────────────────────┐
       │                                             │
nimbleterminal.dev/ntbrowser                  nimbleterminal.dev/ntvideo
(main module: no ntvideo, no goffi)           (workerBackend + ServeWorker/NewWorkerDecoder)
       ▲                                             ▲
       │                                     ┌───────┴────────┐
cmd/ntbrowser-media                   cmd/ntbrowser-media   nimbleterminal.dev/gloss
```

### 4.4 `ntmediabox.Policy` and OS enforcement reporting

`ntmediabox` replaces `profile.Profile` with its own self-contained `Policy`:

```go
// Policy defines the OS confinement for a media helper process.
// The zero value is closed: no paths are readable, writable, or executable,
// network and terminal access are always denied, and OS enforcement is required.
type Policy struct {
	ReadPaths        []string // files or directories the helper may read
	WritePaths       []string // files or directories the helper may write (normally nil)
	ExecPaths        []string // executables the helper stage may exec
	AllowUnsandboxed bool     // false (default) fails closed with ErrUnavailable when OS enforcement is unavailable
}
```

- **Default helper constructor:** `DefaultPolicy(helperBin string, readPaths ...string) Policy` populates `ReadPaths` with `DefaultLibPaths()` (including narrowed Homebrew library dirs `/opt/homebrew/{lib,Cellar,opt}`, `/usr/local/{lib,Cellar,opt}`, and system font/fontconfig directories) plus `readPaths`, `WritePaths: nil`, `ExecPaths` with `DefaultExecPaths()` plus `helperBin`, and `AllowUnsandboxed: false`.
- **Path validation and resolution:** `Policy.Validate()` checks that `ExecPaths` is non-empty and that every path in `ReadPaths`, `WritePaths`, and `ExecPaths` is an absolute, valid UTF-8 path with no control characters. `VerifyBinary` verifies that the worker and stage binaries exist and have safe ownership and permissions. Every existing path in `ReadPaths`, `WritePaths`, and `ExecPaths` is resolved with `filepath.EvalSymlinks` before building the Landlock ruleset or `sandbox-exec` profile; non-existent paths in `ReadPaths`/`WritePaths` (`ENOENT`) are skipped by Landlock so optional system library/font directories do not prevent startup, while hosts (`ntvideo`, `ntbrowser`) pre-validate media files before launching the worker.
- **Honest per-OS reporting:**
  ```go
  type Mechanism string

  const (
      MechanismNone        Mechanism = "none"
      MechanismLandlock    Mechanism = "landlock+seccomp"
      MechanismSandboxExec Mechanism = "sandbox-exec"
  )

  type Support struct {
      Available   bool
      Mechanism   Mechanism
      LandlockABI int    // 1..6 on Linux when Available; 0 elsewhere
      Deprecated  bool   // true on macOS: Apple deprecated sandbox-exec
      Detail      string // plain-text summary of what is enforced and any OS caveats
  }

  func Probe() Support
  ```
  - On macOS, `Support.Deprecated` is `true` and `Support.Detail` explicitly notes that `/usr/bin/sandbox-exec` is deprecated by Apple.
  - On Linux, `Support.LandlockABI` reports the kernel's Landlock ABI version (1–6), and `Support.Detail` notes whether Landlock network/scope flags (ABI v4/v6) supplemented the mandatory `seccomp-bpf` `socket`/`kill`/`io_uring` filter.
  - On Windows and `js/wasm`, `Support.Available` is `false` and `Support.Mechanism` is `MechanismNone`.

### 4.5 Scope of the main-process lockdown

- `Reexec`, `Apply`, and `ntbrowser`'s main-process browser policy (which allows `/dev/tty`, DNS/mDNS, TLS roots, outbound TCP, and the `file:` start page) stay in `ntbrowser/sandbox` until a second host needs main-process self-lockdown.
- `ntmediabox` owns **helper confinement and the worker protocol** (`Policy`, `StartHelper`, `Helper`, `RunStage`, `Probe`, `Client`, `Serve`, `Decoder`).
- `ntbrowser/sandbox` delegates `StartHelper`, `HelperPath`, `KillHelper`, and `MediaStage` to `ntmediabox`, mapping `profile.Profile` onto `ntmediabox.Policy`:
  ```go
  pol := ntmediabox.DefaultPolicy(helper, tempDir)
  pol.AllowUnsandboxed = p.Enforcement == profile.EnforceBestEffort
  ```

### 4.6 Captions, local files, and frame/render limits

#### How video and sidecar subtitle files reach the helper
To preserve `ntbrowser`'s invariant that **only bare filenames ever cross the wire** (`name == filepath.Base(name)`, no slashes, no `..`, verified by `TestDecoderRefusesPathNames`):
1. **In `ntbrowser`:** `ntbrowser` downloads remote videos into its private `0700` `mediaDir` under a random bare name and calls `client.Open(name)`. `Policy.ReadPaths` has `mediaDir`.
2. **In `ntvideo`'s `workerBackend` (used by `gloss`):**
   - For each `Load(Source{URL: videoPath, Captions: captions})`:
     1. The host validates `videoPath` and each `captions[i].Source` using `validateLocalFile` (must be a regular local file within `Limits.MaxFileBytes`, no URL/protocol prefixes, symlinks resolved).
     2. The host creates a private `0700` staging directory `mediaDir` (`os.MkdirTemp("", "ntvideo-media-")`, resolved with `filepath.EvalSymlinks`).
     3. Rather than copying a potentially 2 GiB video file, the host creates a symlink `mediaDir/video<ext>` → `resolvedVideoPath` (falling back to `os.Link` hardlink on Windows if unprivileged symlinks are unavailable) and adds `resolvedVideoPath` as an individual **file** rule in `Policy.ReadPaths` alongside `mediaDir`. Both Linux Landlock (`O_PATH` on a regular file with `accessFile & accessReadFile`) and macOS `sandbox-exec` (`(allow file-read* (literal "/resolved/path/to/video.mp4"))`) enforce file-granularity read access, so the helper can read that single video file and **cannot** list or read any other file in the video's parent directory.
     4. Sidecar subtitle files (`<= 16` files, `<= 2 MiB` in `gloss` / `<= 4 MiB` in `ntvideo`) are copied into `mediaDir` as regular `0600` files under bare names (`sub-0.srt`, `sub-1.vtt`, etc., preserving only the validated `.srt`/`.vtt`/`.ass`/`.ssa` extension).
     5. The helper is started with `Policy.ReadPaths: append(DefaultLibPaths(), mediaDir, resolvedVideoPath)` and given `mediaDir` as its working directory.
     6. Over the wire, `OpenMedia` sends only bare filenames (`"video.mp4"`, `"sub-0.srt"`, ...). Inside the helper, `NewWorkerDecoder` rejects any video or caption name that is not a bare filename inside `mediaDir`.
     7. When `Load` is called again or `Close` runs, the previous helper is killed and its `mediaDir` removed.
   - If `WithCaptionEvents(true)` is enabled on `ntvideo.Model`, `workerBackend` parses the SRT/WebVTT sidecars in pure Go on the **host** (`readCueFile`) and attaches the `cueSnapshot` during `Poll()` based on the selected track ID reported in `Status.CaptionTracks`.

#### How `gloss`'s frame and render limits map onto `Limits` and `MaxFrameSide`
| Limit | Value | Where enforced |
|---|---|---|
| `ntmediabox.MaxFrameSide` | `4096` px | `Client.Resize` and `Client.read` before pixel allocation; helper `Serve` bounds requested `(w, h)` |
| Requested frame box (`maxW`, `maxH`) | `<= 960×540` in `ntvideo.Model`; `<= MaxFrameSide` in `ntbrowser` | `Client.read` rejects any `msgFrame` with `w > maxW` or `h > maxH` before allocating `image.NewRGBA` |
| `ntvideo.Limits.MaxFileBytes` | `2 GiB` default (`64 MiB` download cap in `ntbrowser`) | Host `validateLocalFile` before starting helper, and helper `validateLocalFile` on `Load` |
| `ntvideo.Limits.MaxWidth`, `MaxHeight` | `4096 × 4096` default | Sent in `OpenRequest`; helper `mpvBackend.Poll` calls `checkDimensions` on container metadata and stops playback on violation |
| `ntvideo.Limits.MaxPixels` | `4096 × 2160` (`8,847,360` px) default | Sent in `OpenRequest`; enforced by `checkDimensions` in helper |
| Sidecar subtitle limits | `<= 16` files, `<= 2 MiB` (`gloss`) / `<= 4 MiB` (`ntvideo`) | Host preflight before copying into `mediaDir` and protocol cap on `OpenRequest.Captions` |
| `Status.CaptionText` | `<= 4096` UTF-8 bytes / runes, control/ANSI stripped | Bounded on wire (`maxControlLen = 8192`) and sanitized by `captionPlainText` on host |

### 4.7 Windows and unsandboxed fallback

- On Windows (`helper_windows.go`):
  - `Available()` returns `false`, and `Probe()` returns `Support{Available: false, Mechanism: MechanismNone, Detail: "OS sandbox enforcement is not implemented on Windows"}`.
  - If `pol.AllowUnsandboxed == false` (the default), `StartHelper` returns `nil, ErrUnavailable` (fails closed).
  - If `pol.AllowUnsandboxed == true`, `StartHelper` launches the worker with `cmd.StdinPipe()` and `cmd.StdoutPipe()` wrapped in an `io.ReadWriteCloser` (`ExtraFiles` fd 3 is not supported on Windows). In the worker, `ntmediabox.WorkerConn()` returns `os.NewFile(3, "socket")` on Unix and a `{os.Stdin, os.Stdout}` `io.ReadWriteCloser` on Windows.
  - `h.Enforced()` returns `false`.
  - When the host closes `h` or exits, closing `stdin` delivers `io.EOF` to the worker, and `h.Close()` calls `cmd.Process.Kill()`.
  - Every `ntmediabox.Client` check (message length caps, `MaxFrameSide`, `maxW`/`maxH`, `w*h*4` exact byte count, track/status bounds, and fuzz-tested parsing) runs identically on Windows.

## 5. Interfaces

### 5.1 Package `nimbleterminal.dev/ntmediabox`

```go
package ntmediabox

const (
	MaxFrameSide = 4096
	StageArg     = "--media-stage"
)

var (
	ErrUnavailable = errors.New("ntmediabox: OS sandbox enforcement is not available here")
	ErrProtocol    = errors.New("ntmediabox: protocol violation")
)

type Policy struct {
	ReadPaths        []string
	WritePaths       []string
	ExecPaths        []string
	AllowUnsandboxed bool
}

func DefaultLibPaths() []string
func DefaultExecPaths() []string
func DefaultPolicy(helperBin string, readPaths ...string) Policy
func (p Policy) Validate() error

type HelperConfig struct {
	Policy     Policy
	WorkerPath string   // path to worker executable
	WorkerArgs []string // arguments passed to worker (e.g. []string{workDir})
	StagePath  string   // Linux only: path to pure-Go stage binary; defaults to ResolveSibling("ntmediabox-stage")
	StageArgs  []string // Linux only: optional prefix args before "--media-stage=<spec>"
}

type Helper struct {
	Conn io.ReadWriteCloser
	// unexported cmd, file, support
}

func StartHelper(cfg HelperConfig) (*Helper, error)
func (h *Helper) PID() int
func (h *Helper) Enforced() bool
func (h *Helper) Support() Support
func (h *Helper) InheritableFD() (int, error) // Unix only: dup for macOS re-exec
func (h *Helper) Close() error

func KillHelper(pid int)
func ResolveSibling(name string) (string, error)
func VerifyBinary(path string) (string, error)
func RunStage(args []string) error // called by cmd/ntmediabox-stage and ntbrowser
func WorkerConn() io.ReadWriteCloser
```

### 5.2 Protocol types in `nimbleterminal.dev/ntmediabox`

```go
type State int

const (
	Idle State = iota
	Loading
	Buffering
	Playing
	Paused
	Ended
	Failed
)

type Caption struct {
	Name, Label, Language string // Name is a bare filename inside the worker's directory
	Default               bool
}

type Limits struct {
	MaxFileBytes        int64
	MaxWidth, MaxHeight int
	MaxPixels           int64
}

type OpenRequest struct {
	Name           string // bare filename inside the worker's directory
	Captions       []Caption
	Limits         Limits
	Autoplay       bool
	Loop           bool
	CaptionOverlay bool // true = suppress burned-in subtitles (emit CaptionText only)
}

type Track struct {
	ID, Label, Language, Kind string
	Selected                  bool
}

type Status struct {
	Gen                uint64 // command generation at which this status was sampled
	State              State
	Position, Duration time.Duration
	Err                string
	CaptionText        string
	CaptionTracks      []Track
}

type Frame struct {
	Gen   uint64
	Image *image.RGBA
}

type Decoder interface {
	Load(req OpenRequest) error
	Play() error
	Pause() error
	Seek(time.Duration) error
	SelectTrack(kind, id string) error
	SetCaptionOverlay(enabled bool) error
	Status() (Status, error)
	Frame(w, h int) (rgba []byte, fw, fh int, err error)
	Close() error
}

func NewClient(conn io.ReadWriteCloser, onStatus func(Status), onFrame func(*image.RGBA)) *Client
func (c *Client) Handshake(kind string) error
func (c *Client) Open(name string) error
func (c *Client) OpenMedia(req OpenRequest) error
func (c *Client) Play() error
func (c *Client) Pause() error
func (c *Client) Seek(t time.Duration) error
func (c *Client) Resize(w, h int) error
func (c *Client) SelectTrack(kind, id string) error
func (c *Client) SetCaptionOverlay(enabled bool) error
func (c *Client) Stop() error
func (c *Client) Close() error
func (c *Client) Dead() <-chan struct{}
func (c *Client) Gen() uint64

func Serve(conn io.ReadWriter, open func(kind string) (Decoder, error)) error
```

### 5.3 Additions to `nimbleterminal.dev/ntvideo`

```go
// WorkerConfig configures sandboxed out-of-process decoding for Model.
type WorkerConfig struct {
	WorkerPath       string   // Worker executable; empty resolves "ntvideo-worker" beside os.Executable()
	WorkerArgs       []string // Optional prefix args before the staging directory (e.g. []string{"--gloss-media-worker"})
	StagePath        string   // Linux stage binary; empty resolves "ntmediabox-stage" beside os.Executable()
	AllowUnsandboxed bool     // If false (default), fail closed when OS sandbox enforcement is unavailable
}

// WithWorker runs native media decoding in a confined ntmediabox helper process.
// It has no effect on the browser DOM backend.
func WithWorker(cfg WorkerConfig) Option

// ServeWorker runs the worker side of an ntmediabox connection for kind "video",
// reading files only by bare name from dir with hardware decoding and audio off.
func ServeWorker(conn io.ReadWriter, dir string, opts PlayerOptions) error

// NewWorkerDecoder returns an ntmediabox.Decoder backed by Player in dir.
func NewWorkerDecoder(dir string, opts PlayerOptions) ntmediabox.Decoder
```

## 6. Wire protocol specification

All messages use a 5-byte header: `uint32` big-endian payload length `n` (`n >= 1`, including the 1-byte type), followed by `typ byte` and `n-1` payload bytes.
- Control messages (`typ != msgFrame`) are capped at `maxControlLen = 8192` bytes. Any message exceeding `maxControlLen` is a protocol violation and closes the connection.
- State-changing control messages (`msgOpen`, `msgSeek`, `msgSelectTrack`, `msgCaptionOverlay`, `msgClose`) carry a monotonically increasing `gen uint64` assigned by `Client`. The worker updates its active `gen` when processing the command and echoes `gen` in every `msgStatus` and `msgFrame`. This allows `ntvideo`'s `workerBackend` to deterministically discard in-flight statuses and frames produced before a `Load`, `Seek`, or track change settled.

| Type | ID | Direction | Payload |
|---|---|---|---|
| `msgHello` | 1 | Host → Worker | `kind` bytes (`"video"`) |
| `msgOpen` | 2 | Host → Worker | `gen uint64`, `flags byte` (`bit 0 = Autoplay`, `bit 1 = Loop`, `bit 2 = CaptionOverlay`), `MaxFileBytes int64`, `MaxWidth uint32`, `MaxHeight uint32`, `MaxPixels int64`, `name` (`uint16` len + bytes), `numCaptions uint8` (`<= 16`), each caption: `name`, `label`, `language` (`uint16` len + bytes each) + `default byte` |
| `msgPlay` | 3 | Host → Worker | empty |
| `msgPause` | 4 | Host → Worker | empty |
| `msgSeek` | 5 | Host → Worker | `gen uint64`, `duration uint64` |
| `msgResize` | 6 | Host → Worker | `w uint32`, `h uint32` |
| `msgClose` | 7 | Host → Worker | `gen uint64` |
| `msgStatus` | 8 | Worker → Host | `gen uint64`, `state byte`, `pos uint64`, `dur uint64`, `err` (`uint16` len `<= 512` + bytes), `captionText` (`uint16` len `<= 4096` + bytes), `numTracks uint8` (`<= 32`), each track: `id`, `label`, `language`, `kind` (`uint16` len `<= 128` + bytes each) + `selected byte` |
| `msgFrame` | 9 | Worker → Host | `gen uint64`, `seq uint64`, `w uint32`, `h uint32`, `rgba` (`w*h*4` bytes) |
| `msgSelectTrack` | 10 | Host → Worker | `gen uint64`, `kind` (`uint16` len + bytes), `id` (`uint16` len + bytes) |
| `msgCaptionOverlay` | 11 | Host → Worker | `gen uint64`, `enabled byte` |

*Note on `msgFrame` header:* `frameHeader` is `8 (gen) + 8 (seq) + 4 (w) + 4 (h) = 24` bytes. `Client` checks `w >= 1 && h >= 1 && w <= maxW && h <= maxH && uint64(n) == 1 + 24 + uint64(w)*uint64(h)*4` before allocating `image.NewRGBA`.

## 7. Verification plan

1. **`ntmediabox` unit, hostile-peer, fuzz, and escape tests:**
   - Zero `Policy{}` is closed and fails `Validate()`; `AllowUnsandboxed: false` fails with `ErrUnavailable` when enforcement is unavailable.
   - `VerifyBinary` rejects symlinks to missing files, directories, world-writable binaries, and world-writable non-sticky parent directories.
   - Protocol round-trip for `Open`, `OpenMedia` (with captions and limits), `Play`, `Pause`, `Seek`, `Resize`, `SelectTrack`, `SetCaptionOverlay`, `Status` (including `CaptionTracks` and `CaptionText`), and `Frame` (including generation tracking across `Seek`/`Open`).
   - Hostile-peer tests: oversize frame (`MaxFrameSide+1`), frame larger than `maxW`/`maxH`, frame across shrink resize, byte-length lie, `0xffffffff` length without allocation, over-cap track count or string length in `msgStatus`, unknown message type, EOF death.
   - `FuzzClientRead` fuzzing the binary protocol parser (including the extended `msgStatus` and `msgFrame` headers).
   - Sandbox escape tests (`TestHelperIsConfined`, `TestSeccompTSyncFailureIsAnError`, `io_uring` denial) confirmed running (not skipped) on macOS and on Linux (`dwarfspark` arm64 and Docker/amd64 if available).
   - `GOOS=js GOARCH=wasm` and `GOOS=windows` builds and tests.
2. **`ntbrowser` migration (Phase 2):**
   - `ntbrowser/sandbox` and `ntbrowser/app` use `ntmediabox`; `ntbrowser/mediaproc` is removed or aliased cleanly.
   - `TestMainDoesNotLinkNativeCode` passes (`ntbrowser` links neither `ntvideo` nor `goffi`).
   - `TestLockedCannotEscape` and `TestHelperIsConfined` pass.
   - Real `mpv` test (`cmd/ntbrowser-media` `TestRealHelperDecodes`) passes on macOS and `dwarfspark`.
   - `task check` (including `notices-check` and `wasm`) passes.
3. **`ntvideo` remote backend & worker adapter (Phase 3):**
   - `workerBackend` unit tests with a fake decoder over pipes: initial paused frame, play/pause, seek while paused and playing, stale-sample rejection across seek/load/track-change, caption track cycling, burned-in vs terminal-text caption overlay, cue events with sidecar SRT/VTT, helper death recovery (`Status{State: Failed}`), and `Close`/`Keep`.
   - Real `libmpv` playback test through `WithWorker` using `ntmediabox-stage` on Linux and `sandbox-exec` on macOS.
   - `task ci` (including WASM and Windows cross-builds) passes.
4. **`gloss` worker integration behind a flag (Phase 4):**
   - Flag wired to `ntvideo.WithWorker(...)` in `internal/app/video.go`.
   - Real video playback (with sidecar subtitles, track cycling `c`, glyph vs Kitty caption mode `g`, scrub bar, and `e` export) tested on macOS and `dwarfspark`.
   - Windows cross-build and unsandboxed worker fallback verified.
   - `task ci` (including docs, completions, notices, i18n coverage) passes.

## 8. Approved decisions (Evan, 2026-11-01)

1. **Worker binary for `gloss` (§4.2) — Option A (**Evan**):**
   `gloss` uses a hidden self-invocation flag (`gloss --gloss-media-worker <dir>`, matching its existing `gloss --gloss-parser-worker`) to run `ntvideo.ServeWorker`, paired with `ntmediabox-stage` on Linux. This keeps `gloss` a single binary on macOS and Windows, and two binaries (`gloss` + `ntmediabox-stage`) on Linux.
2. **`gloss` CLI flag name and enforcement mode (§4.7) — Option A (**Evan**):**
   `--video-sandbox` (boolean, default `false` until proven). When explicitly passed on the CLI, it requires OS sandbox enforcement (`AllowUnsandboxed: false`, failing closed on Windows or kernels without Landlock), while `ntvideo.WorkerConfig` supports `AllowUnsandboxed: true` so that when the worker becomes `gloss`'s default later, Windows falls back to an unsandboxed worker process rather than losing video playback.
3. **Local video file access in `ntvideo`'s worker (§4.6) — Approved (**Evan**):**
   Create a symlink inside the private `0700` `mediaDir` to the validated local video file and add that single resolved file path to `Policy.ReadPaths` (file-level rule in Landlock and `sandbox-exec`, never exposing the parent directory, zero copy of multi-GB videos), while copying sidecar subtitle files (`<= 2–4 MiB`) into `mediaDir`. Only bare filenames cross the wire.
4. **`ntmediabox` package layout (§5.1) — Option A (**Evan**):**
   Single package `nimbleterminal.dev/ntmediabox` (plus `cmd/ntmediabox-stage`), which compiles on `darwin`, `linux`, `windows`, and `js/wasm`.
