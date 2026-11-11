// Copyright (c) 2026 Neomantra Corp. All rights reserved.
// SPDX-License-Identifier: MIT

package mediabox

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"io"
	"math"
	"sync"
	"sync/atomic"
)

// Hard upper bounds for sandboxed document, vector, and still-image rendering.
const (
	MaxRenderSide      = 8192
	MaxRenderPixels    = 32 << 20  // 32 megapixels (128 MiB RGBA)
	MaxRenderDataBytes = 128 << 20 // 128 MiB document/image input bytes
	MaxRenderMetaBytes = 16 << 20  // 16 MiB structured metadata/text payload
)

// Fixed-size portions of the render protocol's two messages.
const (
	RenderReqFixedBytes   = renderReqFixed   // fields from page through dataLen in a request
	RenderRespHeaderBytes = renderRespHeader // fields from flags through metaLen in a response
)

const (
	msgRenderReq  = 0x20
	msgRenderResp = 0x21

	// Fixed fields after kind+name in msgRenderReq:
	// page(4) + width(4) + height(4) + dpi(4) + maxPixels(8) + region(32) + flags(4) + metaLen(4) + dataLen(4) = 68 bytes.
	renderReqFixed = 4 + 4 + 4 + 4 + 8 + 32 + 4 + 4 + 4

	// Fixed header in msgRenderResp:
	// flags(1) + pages(4) + page(4) + width(8) + height(8) + imgW(4) + imgH(4) + errLen(2) + metaLen(4) = 39 bytes.
	renderRespHeader = 1 + 4 + 4 + 8 + 8 + 4 + 4 + 2 + 4

	respFlagEnded = 1 << 0
)

// Common RenderRequest.Flags bit positions.
const (
	RenderFlagText    uint32 = 1 << 0
	RenderFlagDark    uint32 = 1 << 1
	RenderFlagAnimate uint32 = 1 << 2
)

// RenderRequest is a single document/image parse or rasterize request sent from
// the host to a confined worker.
type RenderRequest struct {
	Kind      string     // operation kind, e.g. "pdf", "page", "pdf-render", "svg", "svg-region", "mermaid", "image", "typeset"
	Name      string     // diagnostic filename or label (at most 255 bytes)
	Page      int        // 1-indexed page or frame number
	Width     int        // target or maximum width in pixels (0 uses MaxRenderSide)
	Height    int        // target or maximum height in pixels (0 uses MaxRenderSide)
	DPI       int        // target DPI for page rasterization
	MaxPixels int        // maximum allowed w*h for returned Image (0 uses MaxRenderPixels)
	Region    [4]float64 // normalized [nx, ny, nw, nh] sub-rectangle when applicable
	Flags     uint32     // bitflags (e.g. RenderFlagText, RenderFlagDark)
	Meta      []byte     // optional bounded structured parameters (<= MaxRenderMetaBytes)
	Data      []byte     // optional bounded raw document/image bytes (<= MaxRenderDataBytes)
}

// RenderResponse is a confined worker's reply to a RenderRequest.
// The host validates all lengths and image dimensions before allocating Meta or Image.
// Err is validated as UTF-8 and stripped of C0/C1 controls, DEL, ANSI escapes, and
// bidi overrides; Meta is raw untrusted worker bytes (<= MaxRenderMetaBytes) and
// must not be written directly to a terminal without sanitization.
type RenderResponse struct {
	Err    string      // non-empty if the operation returned an application error
	Pages  int         // total page/frame count when applicable
	Page   int         // 1-indexed resolved page number
	Width  float64     // intrinsic width (in points, pixels, or viewBox units)
	Height float64     // intrinsic height (in points, pixels, or viewBox units)
	Ended  bool        // true when the worker session is finished and will answer no more requests
	Meta   []byte      // optional bounded metadata/text/JSON payload (<= MaxRenderMetaBytes)
	Image  *image.RGBA // optional bounds-checked RGBA raster
}

// RenderError is an error a worker reported for one request, as opposed to a
// failure of the connection or of the worker process. Unless Ended is set the
// session is still usable for further requests.
type RenderError struct {
	Msg   string // the worker's sanitized error text
	Ended bool   // the worker ended its session with this response
}

func (e *RenderError) Error() string { return e.Msg }

// RenderHandler handles document/image requests inside a confined worker process.
type RenderHandler interface {
	Handle(req RenderRequest) (RenderResponse, error)
	Close() error
}

// RenderFunc adapts a function into a stateless RenderHandler.
type RenderFunc func(req RenderRequest) (RenderResponse, error)

// Handle calls f(req).
func (f RenderFunc) Handle(req RenderRequest) (RenderResponse, error) { return f(req) }

// Close is a no-op for a stateless RenderFunc.
func (f RenderFunc) Close() error { return nil }

// ToRGBA converts src into a tightly packed *image.RGBA with origin (0, 0) and
// Stride == w*4. If src is already a tightly packed *image.RGBA with origin (0, 0),
// it is returned without copying.
func ToRGBA(src image.Image) *image.RGBA {
	if src == nil {
		return nil
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil
	}
	if rgba, ok := src.(*image.RGBA); ok && b.Min == (image.Point{}) && rgba.Stride == w*4 && len(rgba.Pix) == w*h*4 {
		return rgba
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
	return dst
}

// RenderClient is the host process's end of a request/response render worker connection.
type RenderClient struct {
	conn   io.ReadWriteCloser
	onKill func()
	turn   chan struct{} // holds one token; a Do owns the connection while it holds it
	once   sync.Once
	closed atomic.Bool
	errMu  sync.Mutex
	err    error
}

// NewRenderClient wraps conn as a host-side render client.
func NewRenderClient(conn io.ReadWriteCloser) *RenderClient {
	return NewRenderClientWithKill(conn, nil)
}

// NewRenderClientWithKill wraps conn and invokes onKill when Close is called or
// when a request is cancelled/violates the wire protocol.
func NewRenderClientWithKill(conn io.ReadWriteCloser, onKill func()) *RenderClient {
	c := &RenderClient{conn: conn, onKill: onKill, turn: make(chan struct{}, 1)}
	c.turn <- struct{}{}
	return c
}

// Err returns the terminal error that closed this client, or nil if it is open
// or was closed cleanly (by Close, or by a worker that ended its session without
// an error). A worker that ends its session with an error leaves a *RenderError.
func (c *RenderClient) Err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.err
}

func (c *RenderClient) closeWithErr(err error) {
	c.once.Do(func() {
		c.closed.Store(true)
		c.errMu.Lock()
		c.err = err
		c.errMu.Unlock()
		if c.conn != nil {
			_ = c.conn.Close()
		}
		if c.onKill != nil {
			c.onKill()
		}
	})
}

// Close closes the underlying connection and invokes the kill hook if set.
func (c *RenderClient) Close() error {
	c.closeWithErr(nil)
	return nil
}

// Do sends req to the worker and reads its bounds-checked RenderResponse.
// Requests are served one at a time; a Do whose ctx ends while it is still
// waiting for an earlier request returns ctx.Err() and leaves the client open.
// If ctx ends after the request has been sent, the connection is closed (and
// the worker killed) because the reply can no longer be matched to a request.
// An error the worker reports for the request is returned as a *RenderError
// alongside the response; any other error means the connection is gone.
// Do never times out on its own: pass a ctx with a deadline, or a silent worker
// blocks the call indefinitely.
func (c *RenderClient) Do(ctx context.Context, req RenderRequest) (RenderResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return RenderResponse{}, err
	}
	if len(req.Kind) == 0 || len(req.Kind) > maxNameLen {
		return RenderResponse{}, fmt.Errorf("ntmediabox: invalid render kind %q", req.Kind)
	}
	req.Name = truncateUTF8(req.Name, maxNameLen)
	if len(req.Meta) > MaxRenderMetaBytes {
		return RenderResponse{}, fmt.Errorf("ntmediabox: render metadata exceeds %d bytes", MaxRenderMetaBytes)
	}
	if len(req.Data) > MaxRenderDataBytes {
		return RenderResponse{}, fmt.Errorf("ntmediabox: render data exceeds %d bytes", MaxRenderDataBytes)
	}

	maxW, maxH, maxPixels := renderImageLimits(req)

	// Wait for the connection without outliving ctx: a request that never
	// reached the worker must not close the client when its context expires.
	select {
	case <-c.turn:
	case <-ctx.Done():
		return RenderResponse{}, ctx.Err()
	}
	defer func() { c.turn <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return RenderResponse{}, err
	}
	if c.closed.Load() {
		return RenderResponse{}, io.ErrClosedPipe
	}

	type result struct {
		resp RenderResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		if err := writeRenderReq(c.conn, req); err != nil {
			done <- result{err: err}
			return
		}
		resp, err := readRenderResp(c.conn, maxW, maxH, maxPixels)
		done <- result{resp: resp, err: err}
	}()

	var res result
	select {
	case res = <-done:
	case <-ctx.Done():
		c.closeWithErr(ctx.Err())
		<-done
		return RenderResponse{}, ctx.Err()
	}
	if res.err != nil {
		c.closeWithErr(res.err)
		return RenderResponse{}, res.err
	}
	var appErr error
	if res.resp.Err != "" {
		appErr = &RenderError{Msg: res.resp.Err, Ended: res.resp.Ended}
	}
	if res.resp.Ended {
		c.closeWithErr(appErr)
	}
	return res.resp, appErr
}

// renderImageLimits returns the largest image a worker may return for req.
func renderImageLimits(req RenderRequest) (maxW, maxH, maxPixels int) {
	maxW = req.Width
	if maxW <= 0 || maxW > MaxRenderSide {
		maxW = MaxRenderSide
	}
	maxH = req.Height
	if maxH <= 0 || maxH > MaxRenderSide {
		maxH = MaxRenderSide
	}
	maxPixels = req.MaxPixels
	if maxPixels <= 0 || maxPixels > MaxRenderPixels {
		maxPixels = MaxRenderPixels
	}
	return maxW, maxH, maxPixels
}

// validIntrinsic reports whether v is a finite, non-negative intrinsic dimension.
func validIntrinsic(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0
}

// checkRenderResp reports why the host would reject resp as a reply to req, so
// a worker can answer with a readable error and keep its session.
func checkRenderResp(req RenderRequest, resp RenderResponse) error {
	if !validIntrinsic(resp.Width) || !validIntrinsic(resp.Height) {
		return fmt.Errorf("ntmediabox: worker intrinsic size %vx%v is not finite and non-negative", resp.Width, resp.Height)
	}
	if len(resp.Meta) > MaxRenderMetaBytes {
		return fmt.Errorf("ntmediabox: worker response meta is %d bytes (limit %d)", len(resp.Meta), MaxRenderMetaBytes)
	}
	if resp.Image == nil {
		return nil
	}
	b := resp.Image.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil // ToRGBA drops an empty image
	}
	maxW, maxH, maxPixels := renderImageLimits(req)
	if w > maxW || h > maxH || uint64(w)*uint64(h) > uint64(maxPixels) {
		return fmt.Errorf("ntmediabox: worker image %dx%d exceeds the requested %dx%d (%d pixels)", w, h, maxW, maxH, maxPixels)
	}
	return nil
}

func writeRenderReq(w io.Writer, req RenderRequest) error {
	hdr := make([]byte, 0, 2+len(req.Kind)+2+len(req.Name)+renderReqFixed)
	hdr = appendStr(hdr, req.Kind, maxNameLen)
	hdr = appendStr(hdr, req.Name, maxNameLen)
	hdr = binary.BigEndian.AppendUint32(hdr, uint32(max(req.Page, 0)))
	hdr = binary.BigEndian.AppendUint32(hdr, uint32(max(req.Width, 0)))
	hdr = binary.BigEndian.AppendUint32(hdr, uint32(max(req.Height, 0)))
	hdr = binary.BigEndian.AppendUint32(hdr, uint32(max(req.DPI, 0)))
	hdr = binary.BigEndian.AppendUint64(hdr, uint64(max(req.MaxPixels, 0)))
	for _, v := range req.Region {
		hdr = binary.BigEndian.AppendUint64(hdr, math.Float64bits(v))
	}
	hdr = binary.BigEndian.AppendUint32(hdr, req.Flags)
	hdr = binary.BigEndian.AppendUint32(hdr, uint32(len(req.Meta)))
	hdr = binary.BigEndian.AppendUint32(hdr, uint32(len(req.Data)))
	return writeMsg(w, msgRenderReq, hdr, req.Meta, req.Data)
}

func readRenderReq(r io.Reader) (RenderRequest, error) {
	var head [5]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return RenderRequest{}, err
	}
	n := binary.BigEndian.Uint32(head[:4])
	maxTotal := uint64(1 + 2 + maxNameLen + 2 + maxNameLen + renderReqFixed + MaxRenderMetaBytes + MaxRenderDataBytes)
	if head[4] != msgRenderReq || n < 1+2+1+2+renderReqFixed || uint64(n) > maxTotal {
		return RenderRequest{}, ErrProtocol
	}
	// uint64(n) <= maxTotal is verified above before reading any fields.
	rem := int(n - 1)
	var lenBuf [2]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return RenderRequest{}, err
	}
	rem -= 2
	kindLen := int(binary.BigEndian.Uint16(lenBuf[:]))
	if kindLen < 1 || kindLen > maxNameLen || rem < kindLen+2+renderReqFixed {
		return RenderRequest{}, ErrProtocol
	}
	kindBytes := make([]byte, kindLen)
	if _, err := io.ReadFull(r, kindBytes); err != nil {
		return RenderRequest{}, err
	}
	rem -= kindLen

	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return RenderRequest{}, err
	}
	rem -= 2
	nameLen := int(binary.BigEndian.Uint16(lenBuf[:]))
	if nameLen > maxNameLen || rem < nameLen+renderReqFixed {
		return RenderRequest{}, ErrProtocol
	}
	var nameBytes []byte
	if nameLen > 0 {
		nameBytes = make([]byte, nameLen)
		if _, err := io.ReadFull(r, nameBytes); err != nil {
			return RenderRequest{}, err
		}
		rem -= nameLen
	}

	var fixed [renderReqFixed]byte
	if _, err := io.ReadFull(r, fixed[:]); err != nil {
		return RenderRequest{}, err
	}
	rem -= renderReqFixed

	page := int(int32(binary.BigEndian.Uint32(fixed[0:4])))
	width := int(int32(binary.BigEndian.Uint32(fixed[4:8])))
	height := int(int32(binary.BigEndian.Uint32(fixed[8:12])))
	dpi := int(int32(binary.BigEndian.Uint32(fixed[12:16])))
	maxPixels := int(int64(binary.BigEndian.Uint64(fixed[16:24])))
	if page < 0 || width < 0 || height < 0 || dpi < 0 || maxPixels < 0 {
		return RenderRequest{}, ErrProtocol
	}
	var region [4]float64
	for i := range 4 {
		region[i] = math.Float64frombits(binary.BigEndian.Uint64(fixed[24+i*8 : 32+i*8]))
	}
	flags := binary.BigEndian.Uint32(fixed[56:60])
	metaLen := binary.BigEndian.Uint32(fixed[60:64])
	dataLen := binary.BigEndian.Uint32(fixed[64:68])
	if metaLen > MaxRenderMetaBytes || dataLen > MaxRenderDataBytes || uint64(rem) != uint64(metaLen)+uint64(dataLen) {
		return RenderRequest{}, ErrProtocol
	}

	req := RenderRequest{
		Kind:      string(kindBytes),
		Name:      string(nameBytes),
		Page:      page,
		Width:     width,
		Height:    height,
		DPI:       dpi,
		MaxPixels: maxPixels,
		Region:    region,
		Flags:     flags,
	}
	if metaLen > 0 {
		req.Meta = make([]byte, metaLen)
		if _, err := io.ReadFull(r, req.Meta); err != nil {
			return RenderRequest{}, err
		}
	}
	if dataLen > 0 {
		req.Data = make([]byte, dataLen)
		if _, err := io.ReadFull(r, req.Data); err != nil {
			return RenderRequest{}, err
		}
	}
	return req, nil
}

func writeRenderResp(w io.Writer, resp RenderResponse) error {
	errStr := truncateUTF8(resp.Err, maxErrLen)
	meta := resp.Meta
	if len(meta) > MaxRenderMetaBytes {
		return fmt.Errorf("ntmediabox: worker response meta exceeds %d bytes", MaxRenderMetaBytes)
	}
	var rgba *image.RGBA
	var imgW, imgH int
	var pix []byte
	if resp.Image != nil {
		rgba = ToRGBA(resp.Image)
		if rgba != nil {
			b := rgba.Bounds()
			imgW, imgH = b.Dx(), b.Dy()
			if imgW < 1 || imgH < 1 || imgW > MaxRenderSide || imgH > MaxRenderSide || uint64(imgW)*uint64(imgH) > MaxRenderPixels {
				return fmt.Errorf("ntmediabox: worker image %dx%d exceeds bounds", imgW, imgH)
			}
			pix = rgba.Pix
		}
	}

	var hdr [renderRespHeader]byte
	if resp.Ended {
		hdr[0] |= respFlagEnded
	}
	binary.BigEndian.PutUint32(hdr[1:5], uint32(max(resp.Pages, 0)))
	binary.BigEndian.PutUint32(hdr[5:9], uint32(max(resp.Page, 0)))
	binary.BigEndian.PutUint64(hdr[9:17], math.Float64bits(resp.Width))
	binary.BigEndian.PutUint64(hdr[17:25], math.Float64bits(resp.Height))
	binary.BigEndian.PutUint32(hdr[25:29], uint32(imgW))
	binary.BigEndian.PutUint32(hdr[29:33], uint32(imgH))
	binary.BigEndian.PutUint16(hdr[33:35], uint16(len(errStr)))
	binary.BigEndian.PutUint32(hdr[35:39], uint32(len(meta)))

	return writeMsg(w, msgRenderResp, hdr[:], []byte(errStr), meta, pix)
}

func readRenderResp(r io.Reader, maxW, maxH, maxPixels int) (RenderResponse, error) {
	var head [5]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return RenderResponse{}, err
	}
	n := binary.BigEndian.Uint32(head[:4])
	maxPossible := uint64(1 + renderRespHeader + maxErrLen + MaxRenderMetaBytes + uint64(MaxRenderPixels)*4)
	if head[4] != msgRenderResp || n < 1+renderRespHeader || uint64(n) > maxPossible {
		return RenderResponse{}, ErrProtocol
	}

	var hdr [renderRespHeader]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return RenderResponse{}, err
	}
	flags := hdr[0]
	pages := int(int32(binary.BigEndian.Uint32(hdr[1:5])))
	page := int(int32(binary.BigEndian.Uint32(hdr[5:9])))
	width := math.Float64frombits(binary.BigEndian.Uint64(hdr[9:17]))
	height := math.Float64frombits(binary.BigEndian.Uint64(hdr[17:25]))
	imgW := binary.BigEndian.Uint32(hdr[25:29])
	imgH := binary.BigEndian.Uint32(hdr[29:33])
	errLen := binary.BigEndian.Uint16(hdr[33:35])
	metaLen := binary.BigEndian.Uint32(hdr[35:39])

	if pages < 0 || page < 0 || uint32(errLen) > maxErrLen || metaLen > MaxRenderMetaBytes {
		return RenderResponse{}, ErrProtocol
	}
	if !validIntrinsic(width) || !validIntrinsic(height) {
		return RenderResponse{}, ErrProtocol
	}

	var pixBytes uint64
	if imgW != 0 || imgH != 0 {
		// Checked before the pixels are allocated.
		if imgW < 1 || imgH < 1 ||
			imgW > uint32(maxW) || imgH > uint32(maxH) ||
			imgW > MaxRenderSide || imgH > MaxRenderSide ||
			uint64(imgW)*uint64(imgH) > uint64(maxPixels) ||
			uint64(imgW)*uint64(imgH) > MaxRenderPixels {
			return RenderResponse{}, ErrProtocol
		}
		pixBytes = uint64(imgW) * uint64(imgH) * 4
	}

	wantN := uint64(1+renderRespHeader) + uint64(errLen) + uint64(metaLen) + pixBytes
	if uint64(n) != wantN {
		return RenderResponse{}, ErrProtocol
	}

	resp := RenderResponse{
		Pages:  pages,
		Page:   page,
		Width:  width,
		Height: height,
		Ended:  flags&respFlagEnded != 0,
	}
	if errLen > 0 {
		eb := make([]byte, errLen)
		if _, err := io.ReadFull(r, eb); err != nil {
			return RenderResponse{}, err
		}
		cleanErr, ok := sanitizeWorkerString(string(eb), false)
		if !ok {
			return RenderResponse{}, ErrProtocol
		}
		if cleanErr == "" {
			cleanErr = unprintableWorkerErr
		}
		resp.Err = cleanErr
	}
	if metaLen > 0 {
		resp.Meta = make([]byte, metaLen)
		if _, err := io.ReadFull(r, resp.Meta); err != nil {
			return RenderResponse{}, err
		}
	}
	if pixBytes > 0 {
		img := image.NewRGBA(image.Rect(0, 0, int(imgW), int(imgH)))
		if _, err := io.ReadFull(r, img.Pix); err != nil {
			return RenderResponse{}, err
		}
		resp.Image = img
	}
	return resp, nil
}

// ServeRender runs the worker's end of a RenderClient connection until the
// handler marks a response as Ended, the connection closes, or a protocol
// violation occurs. Panics inside h.Handle are recovered and returned to the
// host as an error response with Ended=true.
func ServeRender(conn io.ReadWriter, h RenderHandler) error {
	defer func() { _ = h.Close() }()
	for {
		req, err := readRenderReq(conn)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		resp := invokeRenderHandler(h, req)
		if err := checkRenderResp(req, resp); err != nil {
			resp = RenderResponse{Err: err.Error(), Ended: resp.Ended}
		}
		if err := writeRenderResp(conn, resp); err != nil {
			return err
		}
		if resp.Ended {
			return nil
		}
	}
}

func invokeRenderHandler(h RenderHandler, req RenderRequest) (resp RenderResponse) {
	defer func() {
		if r := recover(); r != nil {
			resp = RenderResponse{
				Err:   fmt.Sprintf("worker panic: %v", r),
				Ended: true,
			}
		}
	}()
	out, err := h.Handle(req)
	if err != nil && out.Err == "" {
		out.Err = err.Error()
	}
	return out
}
