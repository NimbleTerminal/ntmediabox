# Usage & Multi-Language Worker Guide (`ntmediabox`)

Most Bubble Tea applications use the higher-level Nimble Terminal widgets
([`ntvideo`](https://github.com/NimbleTerminal/ntvideo),
[`ntpdf`](https://github.com/NimbleTerminal/ntpdf),
[`ntsvg`](https://github.com/NimbleTerminal/ntsvg),
[`ntpicture`](https://github.com/NimbleTerminal/ntpicture),
[`nttypeset`](https://github.com/NimbleTerminal/nttypeset),
[`ntbrowser`](https://github.com/NimbleTerminal/ntbrowser), and
[`gloss`](https://github.com/NimbleMarkets/gloss)).

Reach for `ntmediabox` directly when you are wiring a **host process to a
sandboxed media or document worker** — whether that worker is written in **Go**,
**C++**, **Rust**, or **Python**.

---

## 1. Go Host & Worker (`StartRenderHelper` & `ServeRender`)

Use `StartRenderHelper` and `ServeRender` for request/response document, vector,
diagram, font, or still-image rendering.

### Worker side (Go)

```go
package main

import (
	"fmt"
	"os"

	"nimbleterminal.dev/ntmediabox"
)

func runRenderWorker() {
	err := ntmediabox.ServeRender(ntmediabox.WorkerConn(), ntmediabox.RenderFunc(
		func(req ntmediabox.RenderRequest) (ntmediabox.RenderResponse, error) {
			switch req.Kind {
			case "pdf-render":
				img, totalPages, ptW, ptH, err := rasterizePDFPage(req.Data, req.Page, req.DPI)
				if err != nil {
					// Application-level errors are returned in RenderResponse.Err
					// so the worker session stays alive for subsequent requests.
					return ntmediabox.RenderResponse{Err: err.Error()}, nil
				}
				return ntmediabox.RenderResponse{
					Pages:  totalPages,
					Page:   req.Page,
					Width:  ptW,
					Height: ptH,
					Image:  ntmediabox.ToRGBA(img),
				}, nil
			default:
				return ntmediabox.RenderResponse{
					Err: fmt.Sprintf("unsupported kind %q", req.Kind),
				}, nil
			}
		},
	))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
```

### Host side (Go)

```go
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
	Name:      "manual.pdf",
	Page:      1,
	Width:     1600,
	Height:    1200,
	DPI:       150,
	MaxPixels: 1600 * 1200,
	Data:      pdfBytes,
})
if err != nil {
	return fmt.Errorf("render failed (%w): %s", err, helper.Stderr())
}
// resp.Image is a bounds-checked *image.RGBA ready for ntpicture.
```

---

## 2. Go Streaming Media (`StartMediaHelper` & `Serve`)

Use `StartMediaHelper` and `Serve` for continuous video or animation playback
where the worker pushes decoded RGBA frames and status updates at its own cadence.

### Worker side (Go)

Implement `ntmediabox.Decoder` (`Load`, `Play`, `Pause`, `Seek`, `SelectTrack`,
`SetCaptionOverlay`, `Status`, `Frame`, `Close`) and optionally
`ntmediabox.Unloader` (`Unload`), then call `Serve`:

```go
err := ntmediabox.Serve(ntmediabox.WorkerConn(), func(kind string) (ntmediabox.Decoder, error) {
	if kind != "video" {
		return nil, fmt.Errorf("unsupported kind %q", kind)
	}
	return newMPVDecoder(), nil
})
```

### Host side (Go)

```go
client, helper, err := ntmediabox.StartMediaHelper(
	ntmediabox.HelperConfig{
		Policy:     ntmediabox.DefaultPolicy(workerBin, stagedMediaDir),
		WorkerPath: workerBin,
	},
	func(st ntmediabox.Status) {
		// Called on the reader goroutine; do not block.
		// Compare st.Gen against client.Gen() to ignore stale statuses.
	},
	func(fr ntmediabox.Frame) {
		// fr.RGBA is a bounds-checked *image.RGBA; fr.Gen is the active generation.
	},
)
if err != nil {
	return err
}
defer helper.Close()
defer client.Close()

if err := client.Handshake("video"); err != nil {
	return err
}
if err := client.Resize(640, 360); err != nil {
	return err
}
if err := client.OpenMedia(ntmediabox.OpenRequest{
	Name: "clip.mp4", // bare filename inside stagedMediaDir
	Captions: []ntmediabox.Caption{
		{Name: "sub-0.srt", Label: "English", Language: "en", Default: true},
	},
	Limits: ntmediabox.Limits{
		MaxFileBytes: 100 << 20,
		MaxWidth:     1920,
		MaxHeight:    1080,
		MaxPixels:    1920 * 1080,
	},
	Autoplay:       false,
	Loop:           true,
	CaptionOverlay: true,
}); err != nil {
	return err
}
```

---

## 3. Render Wire Protocol Specification (`0x20` / `0x21`)

Because `StartRenderHelper` speaks a simple big-endian binary protocol over
`fd 3`, you can write your render worker in **C++**, **Python**, or **Rust**
(for example, to call native MuPDF, Poppler, Skia, libvips, Pillow, or
Matplotlib) while keeping `StartRenderHelper` in your Go host.

Every message starts with a 5-byte frame header:

```
[u32 payload_len_plus_1 (big-endian)][u8 msg_type][payload (payload_len_plus_1 - 1 bytes)]
```

### `msgRenderReq` (`0x20`, Host $\to$ Worker)

| Field | Type | Description |
| --- | --- | --- |
| `kindLen` | `u16` | Byte length of `Kind` (`1..255`) |
| `kind` | `[kindLen]byte` | Operation kind (e.g. `"pdf-render"`, `"svg"`, `"image"`) |
| `nameLen` | `u16` | Byte length of `Name` (`0..255`) |
| `name` | `[nameLen]byte` | Diagnostic filename/label |
| `page` | `u32` | 1-indexed page or frame number |
| `width` | `u32` | Target/max width in pixels (`0` = `MaxRenderSide`) |
| `height` | `u32` | Target/max height in pixels (`0` = `MaxRenderSide`) |
| `dpi` | `u32` | Target DPI |
| `maxPixels` | `u64` | Maximum allowed `imgW * imgH` (`0` = `MaxRenderPixels`) |
| `region[0..3]` | `4 x f64` (IEEE-754 bits as `u64`) | Normalized `[nx, ny, nw, nh]` sub-rectangle (`32` bytes) |
| `flags` | `u32` | Bitflags (`1<<0` text, `1<<1` dark, `1<<2` animate) |
| `metaLen` | `u32` | Byte length of `Meta` (`<= 16 MiB`) |
| `dataLen` | `u32` | Byte length of `Data` (`<= 128 MiB`) |
| `meta` | `[metaLen]byte` | Optional structured parameters |
| `data` | `[dataLen]byte` | Optional raw document/image bytes |

*(The fixed block from `page` through `dataLen` is exactly `68` bytes.)*

### `msgRenderResp` (`0x21`, Worker $\to$ Host)

| Offset | Field | Type | Description |
| --- | --- | --- | --- |
| `0` | `flags` | `u8` | `1 << 0` (`respFlagEnded`) if the worker is closing the session |
| `1..5` | `pages` | `u32` | Total page/frame count |
| `5..9` | `page` | `u32` | 1-indexed resolved page number |
| `9..17` | `width` | `f64` (IEEE-754 `u64`) | Intrinsic width (points, pixels, or viewBox units) |
| `17..25` | `height` | `f64` (IEEE-754 `u64`) | Intrinsic height |
| `25..29` | `imgW` | `u32` | Raster width in pixels (`0` if no image) |
| `29..33` | `imgH` | `u32` | Raster height in pixels (`0` if no image) |
| `33..35` | `errLen` | `u16` | Byte length of UTF-8 error string (`<= 1024`) |
| `35..39` | `metaLen` | `u32` | Byte length of `Meta` payload (`<= 16 MiB`) |
| `39..` | `err` | `[errLen]byte` | UTF-8 application error string |
| `...` | `meta` | `[metaLen]byte` | Optional metadata/text/JSON bytes |
| `...` | `pix` | `[imgW * imgH * 4]byte` | Tightly packed non-premultiplied/RGBA pixels (`Stride == imgW * 4`) |

*(The fixed response header is `39` bytes.)*

---

## 4. C++ Render Worker Example (`fd 3`)

This self-contained C++20 worker loops over `msgRenderReq` messages on `fd 3`
and replies with `msgRenderResp` frames (ideal for wrapping C/C++ libraries like
MuPDF, Poppler, Skia, or libvips under `ntmediabox.StartRenderHelper`):

```cpp
// render_worker.cpp — compile with: c++ -O2 -std=c++20 render_worker.cpp -o render-worker
#include <cstdint>
#include <cstring>
#include <string>
#include <vector>
#include <unistd.h>

static constexpr int kWorkerFD = 3;
static constexpr uint8_t kMsgRenderReq  = 0x20;
static constexpr uint8_t kMsgRenderResp = 0x21;

static bool read_full(int fd, void* buf, size_t n) {
    uint8_t* p = static_cast<uint8_t*>(buf);
    while (n > 0) {
        ssize_t r = ::read(fd, p, n);
        if (r <= 0) return false;
        p += r;
        n -= static_cast<size_t>(r);
    }
    return true;
}

static bool write_full(int fd, const void* buf, size_t n) {
    const uint8_t* p = static_cast<const uint8_t*>(buf);
    while (n > 0) {
        ssize_t w = ::write(fd, p, n);
        if (w <= 0) return false;
        p += w;
        n -= static_cast<size_t>(w);
    }
    return true;
}

static uint16_t be16(const uint8_t* p) {
    return (uint16_t(p[0]) << 8) | uint16_t(p[1]);
}
static uint32_t be32(const uint8_t* p) {
    return (uint32_t(p[0]) << 24) | (uint32_t(p[1]) << 16) |
           (uint32_t(p[2]) << 8)  | uint32_t(p[3]);
}
static void put_be16(uint8_t* p, uint16_t v) {
    p[0] = uint8_t(v >> 8); p[1] = uint8_t(v);
}
static void put_be32(uint8_t* p, uint32_t v) {
    p[0] = uint8_t(v >> 24); p[1] = uint8_t(v >> 16);
    p[2] = uint8_t(v >> 8);  p[3] = uint8_t(v);
}
static void put_be64(uint8_t* p, uint64_t v) {
    put_be32(p, uint32_t(v >> 32));
    put_be32(p + 4, uint32_t(v));
}
static void put_f64(uint8_t* p, double d) {
    uint64_t bits = 0;
    std::memcpy(&bits, &d, sizeof(bits));
    put_be64(p, bits);
}

int main() {
    uint8_t fhead[5];
    while (read_full(kWorkerFD, fhead, sizeof(fhead))) {
        uint32_t frame_len = be32(fhead);
        if (fhead[4] != kMsgRenderReq || frame_len < 1 + 2 + 1 + 2 + 68) return 1;

        uint8_t len_buf[2];
        if (!read_full(kWorkerFD, len_buf, 2)) return 1;
        uint16_t kind_len = be16(len_buf);
        std::string kind(kind_len, '\0');
        if (!read_full(kWorkerFD, kind.data(), kind_len)) return 1;

        if (!read_full(kWorkerFD, len_buf, 2)) return 1;
        uint16_t name_len = be16(len_buf);
        std::string name(name_len, '\0');
        if (name_len > 0 && !read_full(kWorkerFD, name.data(), name_len)) return 1;

        uint8_t fixed[68];
        if (!read_full(kWorkerFD, fixed, sizeof(fixed))) return 1;
        uint32_t page     = be32(fixed + 0);
        uint32_t width    = be32(fixed + 4);
        uint32_t height   = be32(fixed + 8);
        uint32_t meta_len = be32(fixed + 60);
        uint32_t data_len = be32(fixed + 64);

        std::vector<uint8_t> meta(meta_len), data(data_len);
        if (meta_len > 0 && !read_full(kWorkerFD, meta.data(), meta_len)) return 1;
        if (data_len > 0 && !read_full(kWorkerFD, data.data(), data_len)) return 1;

        // Example: synthesize a 2x2 RGBA image (replace with MuPDF/Skia/etc.).
        uint32_t img_w = width > 0 ? width : 2;
        uint32_t img_h = height > 0 ? height : 2;
        std::vector<uint8_t> rgba(size_t(img_w) * size_t(img_h) * 4, 0xff);

        uint8_t resp_hdr[5 + 39]{};
        uint32_t total_payload = 1 + 39 + uint32_t(rgba.size());
        put_be32(resp_hdr, total_payload);
        resp_hdr[4] = kMsgRenderResp;

        uint8_t* h = resp_hdr + 5;
        h[0] = 0; // flags
        put_be32(h + 1, 1);                  // pages
        put_be32(h + 5, page > 0 ? page : 1);// page
        put_f64(h + 9, double(img_w));       // intrinsic width
        put_f64(h + 17, double(img_h));      // intrinsic height
        put_be32(h + 25, img_w);             // imgW
        put_be32(h + 29, img_h);             // imgH
        put_be16(h + 33, 0);                 // errLen
        put_be32(h + 35, 0);                 // metaLen

        if (!write_full(kWorkerFD, resp_hdr, sizeof(resp_hdr)) ||
            !write_full(kWorkerFD, rgba.data(), rgba.size())) {
            return 1;
        }
    }
    return 0;
}
```

---

## 5. Python Render Worker Example (`fd 3`)

A Python worker using only the standard library (`socket`, `struct`) can serve
`ntmediabox.StartRenderHelper` requests to render plots or images with Pillow,
Matplotlib, or CairoSVG:

```python
#!/usr/bin/env python3
"""Confined ntmediabox RenderRequest/RenderResponse worker on fd 3."""
import socket
import struct

MSG_RENDER_REQ  = 0x20
MSG_RENDER_RESP = 0x21
FIXED_REQ_FMT   = ">IIIIQddddIII"  # 68 bytes
FIXED_RESP_FMT  = ">BIIddIIHI"      # 39 bytes


def recv_exact(sock: socket.socket, n: int) -> bytes:
    buf = bytearray()
    while len(buf) < n:
        chunk = sock.recv(n - len(buf))
        if not chunk:
            raise EOFError
        buf.extend(chunk)
    return bytes(buf)


def send_resp(
    sock: socket.socket,
    *,
    pages: int = 1,
    page: int = 1,
    width: float = 0.0,
    height: float = 0.0,
    img_w: int = 0,
    img_h: int = 0,
    rgba: bytes = b"",
    err: str = "",
    meta: bytes = b"",
    ended: bool = False,
) -> None:
    err_bytes = err.encode("utf-8")[:1024]
    flags = 1 if ended else 0
    hdr = struct.pack(
        FIXED_RESP_FMT,
        flags, pages, page, width, height, img_w, img_h, len(err_bytes), len(meta),
    )
    frame_len = 1 + len(hdr) + len(err_bytes) + len(meta) + len(rgba)
    sock.sendall(struct.pack(">IB", frame_len, MSG_RENDER_RESP))
    sock.sendall(hdr)
    if err_bytes:
        sock.sendall(err_bytes)
    if meta:
        sock.sendall(meta)
    if rgba:
        sock.sendall(rgba)


def serve() -> None:
    with socket.fromfd(3, socket.AF_UNIX, socket.SOCK_STREAM) as sock:
        while True:
            try:
                head = recv_exact(sock, 5)
            except EOFError:
                return
            frame_len, msg_type = struct.unpack(">IB", head)
            if msg_type != MSG_RENDER_REQ:
                return

            (kind_len,) = struct.unpack(">H", recv_exact(sock, 2))
            kind = recv_exact(sock, kind_len).decode("utf-8")
            (name_len,) = struct.unpack(">H", recv_exact(sock, 2))
            name = recv_exact(sock, name_len).decode("utf-8") if name_len else ""

            (
                page, width, height, dpi, max_pixels,
                rx, ry, rw, rh, flags, meta_len, data_len,
            ) = struct.unpack(FIXED_REQ_FMT, recv_exact(sock, 68))

            meta = recv_exact(sock, meta_len) if meta_len else b""
            data = recv_exact(sock, data_len) if data_len else b""

            # Replace with Pillow / Matplotlib / CairoSVG rendering:
            w = width or 64
            h = height or 64
            rgba = b"\x20\x60\xc0\xff" * (w * h)
            send_resp(
                sock,
                pages=1, page=max(page, 1),
                width=float(w), height=float(h),
                img_w=w, img_h=h, rgba=rgba,
            )


if __name__ == "__main__":
    serve()
```

### Launching a Python worker from Go (`StartRenderHelper`)

When launching `python3` under `StartRenderHelper`, include the resolved Python
prefix and worker script in `ReadPaths` and pass `-I -B` so Python runs in
isolated mode without attempting to write `.pyc` files:

```go
pol := ntmediabox.DefaultPolicy(pythonBin, pythonPrefix, workerScriptPath)
rc, helper, err := ntmediabox.StartRenderHelper(ntmediabox.HelperConfig{
	Policy:     pol,
	WorkerPath: pythonBin,
	WorkerArgs: []string{"-I", "-B", workerScriptPath},
})
```
