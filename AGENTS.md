# AGENTS.md — ntmediabox

## Build and test

The checked-in [`go.work`](go.work) joins only the two modules in this repository (`use ( . ./pkg/mediabox )`); `ntconfine` comes from the version pinned in [`go.mod`](go.mod), so tests exercise what consumers get. To develop against a local `ntconfine` checkout, copy `go.work` to `go.work.local` (git-ignored), add `../ntconfine` and `../ntconfine/pkg/confine`, and run with `GOWORK=$PWD/go.work.local`. Always build and test with `CGO_ENABLED=0`:

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
- **Documented bounds:** The wire-bound tables in `SECURITY.md` are generated from the exported constants in `pkg/mediabox`. After changing a bound, run `task security-doc`; `TestSecurityDocInSync` and `TestUsageDocStatesEnforcedLimits` fail when the docs drift.
- **Worker self-checks:** `Serve` and `ServeRender` validate decoder and handler output against the request and report a readable error, so a worker bug is not mistaken for a hostile peer.
- **Release order:** `pkg/mediabox` is its own module. Tag `pkg/mediabox/vX.Y.Z` first, bump the root [`go.mod`](go.mod) to it (and to the intended `ntconfine` tag), run `task check-published`, then tag `vX.Y.Z`. Never tag the root while `task check-published` fails.
