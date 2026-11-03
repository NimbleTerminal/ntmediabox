# ntmediabox — Sandboxed media & document worker protocols for Nimble Terminal

<p>
    <a href="https://github.com/NimbleTerminal/ntmediabox/tags"><img src="https://img.shields.io/github/tag/NimbleTerminal/ntmediabox.svg" alt="Latest Release"></a>
    <a href="https://pkg.go.dev/nimbleterminal.dev/ntmediabox"><img src="https://pkg.go.dev/badge/nimbleterminal.dev/ntmediabox.svg" alt="Go Reference"></a>
    <a href="https://github.com/NimbleTerminal/ntmediabox/actions/workflows/ci.yml"><img src="https://github.com/NimbleTerminal/ntmediabox/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
    <a href="./CODE_OF_CONDUCT.md"><img src="https://img.shields.io/badge/Contributor%20Covenant-2.1-4baaaa.svg" alt="Code Of Conduct"></a>
</p>

`ntmediabox` (`nimbleterminal.dev/ntmediabox`) is the sandboxed media and document
worker layer behind the Nimble Terminal ecosystem. It pairs OS process
confinement from [`ntconfine`](https://github.com/NimbleTerminal/ntconfine) with
two hostile-peer binary wire protocols:

- **Streaming media (`Client` / `Serve` / `StartMediaHelper`):** Drives a
  confined video or animation decoder (such as `libmpv` via `goffi` in
  `ntvideo` and `ntbrowser`) over a socketpair, streaming bounds-checked RGBA
  frames and sanitized playback status back to the host.
- **Document, vector & still-image rendering (`RenderClient` / `ServeRender` / `StartRenderHelper`):**
  Drives a confined request/response renderer for PDFs (`ntpdf`), SVGs (`ntsvg`),
  Mermaid diagrams, typeset pages (`nttypeset`), and raster images (`ntpicture`).

Most Bubble Tea applications use the higher-level widgets (`ntvideo`, `ntpdf`,
`ntsvg`, `ntpicture`, `nttypeset`, `ntbrowser`, `gloss`) rather than calling
`ntmediabox` directly. Reach for `ntmediabox` when you are building a host or
worker binary (in Go, C++, Rust, or Python) that isolates a decoder in a
separate process.

| Package | License | What it provides |
| --- | --- | --- |
| [`nimbleterminal.dev/ntmediabox`](.) | Dual (`AGPL-3.0-only` w/ Linking Exception or Commercial) | Full sandbox launcher (`StartMediaHelper`, `StartRenderHelper`, `StartHelper`, `DefaultPolicy`) + re-exported wire protocols |
| [`nimbleterminal.dev/ntmediabox/pkg/mediabox`](./pkg/mediabox) | [MIT](./pkg/mediabox/LICENSE) | Standalone, zero-dependency wire protocols (`Client`, `Serve`, `RenderClient`, `ServeRender`) over any `io.ReadWriteCloser` |

- **[Usage & Multi-Language Worker Guide (`USAGE.md`)](./USAGE.md):** Complete host and worker examples for **Go**, **C++**, and **Python**, plus the binary wire layout.
- **[Security Policy & Perimeter (`SECURITY.md`)](./SECURITY.md):** Hostile-peer wire bounds, terminal escape/bidi sanitization, OS confinement integration, and review checklist for humans and coding agents.

## Install

Requires Go 1.26 or later (`CGO_ENABLED=0`).

```sh
# Full sandboxed helper launcher + wire protocols
go get nimbleterminal.dev/ntmediabox@latest

# Or the standalone MIT wire-protocol package only
go get nimbleterminal.dev/ntmediabox/pkg/mediabox@latest
```

## Quickstart

```go
// Worker process: serve RenderRequest calls over WorkerConn() (fd 3 on Unix).
err := ntmediabox.ServeRender(ntmediabox.WorkerConn(), ntmediabox.RenderFunc(
	func(req ntmediabox.RenderRequest) (ntmediabox.RenderResponse, error) {
		img, pages, err := renderPage(req.Kind, req.Data, req.Page, req.Width, req.Height)
		if err != nil {
			return ntmediabox.RenderResponse{Err: err.Error()}, nil
		}
		return ntmediabox.RenderResponse{
			Pages: pages,
			Page:  req.Page,
			Image: ntmediabox.ToRGBA(img),
		}, nil
	},
))
```

```go
// Host process: start the confined worker and issue a context-bounded request.
rc, helper, err := ntmediabox.StartRenderHelper(ntmediabox.HelperConfig{
	Policy:     ntmediabox.DefaultPolicy(workerBin),
	WorkerPath: workerBin,
	WorkerArgs: []string{"--render-worker"},
})
if err != nil {
	return err
}
defer helper.Close()
defer rc.Close()

ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

resp, err := rc.Do(ctx, ntmediabox.RenderRequest{
	Kind:      "pdf-render",
	Name:      "report.pdf",
	Page:      1,
	Width:     1024,
	Height:    1024,
	MaxPixels: 1024 * 1024,
	Data:      pdfBytes,
})
// resp.Image is a bounds-checked *image.RGBA; if ctx times out or the worker
// sends a malformed response, rc closes the socket and kills the worker.
```

See [`USAGE.md`](./USAGE.md) for streaming video (`StartMediaHelper` & `Serve`)
and C++ / Python worker implementations, and [`SECURITY.md`](./SECURITY.md) for
the hostile-peer security model.

## Validation

Install [Task](https://taskfile.dev/), then run the full preflight:

```sh
task ci                 # full preflight: fmt-check, whitespace-check, lint, check, build, fuzz
task check              # vet, race tests (root + pkg/mediabox), wasm/windows/linux cross-builds, notices check
task lint               # golangci-lint across darwin, linux, windows, and pkg/mediabox
task fuzz               # fuzz all wire-protocol and policy parsers
task test-sandbox-linux # run the Linux sandbox suite in Docker from macOS
```

Repository invariants and build rules are documented in [AGENTS.md](./AGENTS.md).

## Open Collaboration

Contributions and feedback are welcome. Please follow our
[Code of Conduct](./CODE_OF_CONDUCT.md), and report security issues privately as
described in [SECURITY.md](./SECURITY.md).

- [Report an issue](https://github.com/NimbleTerminal/ntmediabox/issues)
- [Submit a pull request](https://github.com/NimbleTerminal/ntmediabox/pulls)

## License

Copyright (c) 2026 [Neomantra Corp](https://www.neomantra.com). All rights reserved.

`ntmediabox` is dual-licensed (`AGPL-3.0-only WITH LicenseRef-NimbleTerminal-Linking-Exception OR LicenseRef-Neomantra-Commercial`), with the public client/protocol subpackage [`pkg/mediabox`](./pkg/mediabox) available under the [MIT License](./pkg/mediabox/LICENSE):

1. **Open-Source & Local CLI/Desktop Use:** Licensed under the **GNU Affero General Public License v3.0 only (`AGPL-3.0-only`)** with the **NimbleTerminal Open-Source Linking Exception (AGPLv3 Section 7)** in [`LICENSE`](./LICENSE), allowing MIT-licensed modules from the `NimbleTerminal`, `NimbleMarkets`, and `wethinkt` organizations (including `ntvideo`, `ntpicture`, `ntpdf`, `ntsvg`, `nttypeset`, `gloss`, and `go-thinkt`) to import and link `ntmediabox` directly for local CLI and desktop execution while remaining 100% MIT-licensed.
2. **Cloud, Hosted Sandbox & Closed-Source OEM Use:** Deploying, hosting, streaming, or executing `ntmediabox` (or tools powered by it) inside a commercial hosted service, remote terminal/multiplexer service, cloud AI agent sandbox, multi-tenant server, or closed-source commercial product requires either full **AGPLv3 Section 13** Corresponding Source disclosure of the covered work or a **Commercial OEM & Enterprise License from Neomantra Corp.** (see [`LICENSE-COMMERCIAL.md`](./LICENSE-COMMERCIAL.md) or contact `licensing@neomantra.net`).

Dependency notices are listed in [`THIRD_PARTY_NOTICES.md`](./THIRD_PARTY_NOTICES.md).

----
Made with :heart: and :fire: by the team behind [Nimble Terminal](https://nimbleterminal.dev).
