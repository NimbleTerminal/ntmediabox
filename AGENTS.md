# AGENTS.md — ntmediabox

## Build and test

Local development uses [`go.work`](go.work) (`use ( . ./pkg/mediabox ../ntconfine ../ntconfine/pkg/confine )`). Always build and test with `CGO_ENABLED=0`:

```sh
task check
```

Or directly:

```sh
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go -C pkg/mediabox vet ./...
CGO_ENABLED=0 go test -race -count=1 ./...   # -race on macOS only
CGO_ENABLED=0 go -C pkg/mediabox test -race -count=1 ./...
CGO_ENABLED=0 GOOS=js GOARCH=wasm go build .
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...
```

## Rules and invariants

- **Zero external third-party dependencies:** `nimbleterminal.dev/ntmediabox` depends only on `nimbleterminal.dev/ntconfine`, `nimbleterminal.dev/ntmediabox/pkg/mediabox`, and the Go standard library (`CGO_ENABLED=0`), and never imports `goffi`, `ntvideo`, `ntbrowser`, or Bubble Tea.
- **Fail closed:** The zero `Policy` is closed (`AllowUnsandboxed: false`, no readable/writable/executable paths). When OS enforcement is unavailable and `AllowUnsandboxed` is `false`, `StartHelper` must return `ErrUnavailable`.
- **Honest reporting:** `Probe()` and `Helper.Support()` must report only what the OS actually enforces (`landlock+seccomp` on Linux, `sandbox-exec` marked `Deprecated: true` on macOS, `none` elsewhere).
- **Hostile helper output:** `Client` treats the worker as hostile. Check all lengths and frame dimensions (`1 <= w <= maxW <= MaxFrameSide`, `1 <= h <= maxH <= MaxFrameSide`, `uint64(n) == 1 + frameHeader + uint64(w)*uint64(h)*4`) before allocating.
- **Generated notices:** Run `task notices` (`task notices-check`) whenever dependencies or build targets change.
