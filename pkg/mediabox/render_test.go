// Copyright (c) 2026 Neomantra Corp. All rights reserved.
// SPDX-License-Identifier: MIT

package mediabox

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"net"
	"strings"
	"testing"
	"time"
)

func TestRenderRoundTripSessionAndImage(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })

	calls := 0
	go func() {
		_ = ServeRender(b, RenderFunc(func(req RenderRequest) (RenderResponse, error) {
			calls++
			switch req.Kind {
			case "pdf":
				return RenderResponse{
					Pages:  3,
					Page:   req.Page,
					Width:  612,
					Height: 792,
					Meta:   []byte(`{"title":"report"}`),
				}, nil
			case "pdf-render":
				img := image.NewNRGBA(image.Rect(2, 2, 6, 5)) // non-RGBA, non-zero Min to exercise ToRGBA
				img.Set(2, 2, color.NRGBA{R: 10, G: 20, B: 30, A: 255})
				return RenderResponse{
					Pages: 3,
					Page:  req.Page,
					Image: ToRGBA(img),
					Ended: true,
				}, nil
			default:
				return RenderResponse{Ended: true}, errors.New("unknown kind")
			}
		}))
	}()

	client := NewRenderClient(a)
	defer func() { _ = client.Close() }()

	resp, err := client.Do(context.Background(), RenderRequest{
		Kind: "pdf",
		Name: "report.pdf",
		Page: 1,
		Data: []byte("%PDF-1.4..."),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Pages != 3 || resp.Width != 612 || resp.Height != 792 || string(resp.Meta) != `{"title":"report"}` {
		t.Fatalf("unexpected pdf response: %+v", resp)
	}

	resp2, err := client.Do(context.Background(), RenderRequest{
		Kind:   "pdf-render",
		Page:   2,
		Width:  8,
		Height: 8,
		DPI:    150,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp2.Image == nil || resp2.Image.Bounds() != image.Rect(0, 0, 4, 3) {
		t.Fatalf("unexpected image bounds: %+v", resp2.Image)
	}
	if resp2.Image.Pix[0] != 10 || resp2.Image.Pix[1] != 20 || resp2.Image.Pix[2] != 30 || resp2.Image.Pix[3] != 255 {
		t.Fatalf("unexpected pixel values: %v", resp2.Image.Pix[:4])
	}
}

func TestRenderPanicRecoveryMarksEnded(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })

	go func() {
		_ = ServeRender(b, RenderFunc(func(RenderRequest) (RenderResponse, error) {
			panic("corrupt stream")
		}))
	}()

	client := NewRenderClient(a)
	_, err := client.Do(context.Background(), RenderRequest{Kind: "svg", Data: []byte("<svg/>")})
	if err == nil || !strings.Contains(err.Error(), "worker panic: corrupt stream") {
		t.Fatalf("expected recovered panic error, got %v", err)
	}
}

func TestRenderContextCancelClosesAndInvokesKill(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })

	// Drain request on b and then hang without replying.
	go func() {
		_, _ = readRenderReq(b)
	}()

	killed := make(chan struct{}, 1)
	client := NewRenderClientWithKill(a, func() {
		select {
		case killed <- struct{}{}:
		default:
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := client.Do(ctx, RenderRequest{Kind: "svg", Data: []byte("<svg/>")})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
	select {
	case <-killed:
	case <-time.After(time.Second):
		t.Fatal("onKill was not invoked on context cancellation")
	}
}

func TestRenderHostilePeerChecks(t *testing.T) {
	tests := []struct {
		name  string
		req   RenderRequest
		write func(b net.Conn)
	}{
		{
			name: "oversize_width_beyond_requested",
			req:  RenderRequest{Kind: "image", Width: 16, Height: 16},
			write: func(b net.Conn) {
				_, _ = readRenderReq(b)
				_ = writeRenderResp(b, RenderResponse{
					Image: image.NewRGBA(image.Rect(0, 0, 17, 4)),
				})
			},
		},
		{
			name: "oversize_pixels_beyond_requested",
			req:  RenderRequest{Kind: "image", Width: 100, Height: 100, MaxPixels: 64},
			write: func(b net.Conn) {
				_, _ = readRenderReq(b)
				_ = writeRenderResp(b, RenderResponse{
					Image: image.NewRGBA(image.Rect(0, 0, 9, 9)), // 81 > 64
				})
			},
		},
		{
			name: "length_mismatch_lie",
			req:  RenderRequest{Kind: "image", Width: 16, Height: 16},
			write: func(b net.Conn) {
				_, _ = readRenderReq(b)
				var hdr [renderRespHeader]byte
				binary.BigEndian.PutUint32(hdr[25:29], 4) // imgW = 4
				binary.BigEndian.PutUint32(hdr[29:33], 4) // imgH = 4 (needs 64 bytes)
				_ = writeMsg(b, msgRenderResp, hdr[:], make([]byte, 12))
			},
		},
		{
			name: "huge_length_header",
			req:  RenderRequest{Kind: "image", Width: 16, Height: 16},
			write: func(b net.Conn) {
				_, _ = readRenderReq(b)
				_, _ = b.Write([]byte{0xff, 0xff, 0xff, 0xff, msgRenderResp})
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, b := net.Pipe()
			defer func() { _ = a.Close() }()
			defer func() { _ = b.Close() }()
			go tc.write(b)
			client := NewRenderClient(a)
			_, err := client.Do(context.Background(), tc.req)
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("expected ErrProtocol, got %v", err)
			}
			if !errors.Is(client.Err(), ErrProtocol) {
				t.Fatalf("expected client.Err() == ErrProtocol, got %v", client.Err())
			}
		})
	}
}

func TestRenderSanitizesErrAndRejectsInvalidUTF8(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	go func() {
		_, _ = readRenderReq(b)
		_ = writeRenderResp(b, RenderResponse{
			Err: "bad\x1b[31m-pdf\x1b]52;c;YWJj\x07\u202e\nline2",
		})
		_, _ = readRenderReq(b)
		_ = writeRenderResp(b, RenderResponse{
			Err: "invalid-\xff-utf8",
		})
	}()

	client := NewRenderClient(a)
	_, err := client.Do(context.Background(), RenderRequest{Kind: "pdf"})
	if err == nil || err.Error() != "bad-pdfline2" {
		t.Fatalf("unexpected sanitized error: %v", err)
	}

	_, err = client.Do(context.Background(), RenderRequest{Kind: "pdf"})
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("expected ErrProtocol for invalid UTF-8 error string, got %v", err)
	}
}

func FuzzRenderClientResp(f *testing.F) {
	var hdr [renderRespHeader]byte
	binary.BigEndian.PutUint32(hdr[25:29], 2)
	binary.BigEndian.PutUint32(hdr[29:33], 2)
	var valid bytes.Buffer
	_ = writeMsg(&valid, msgRenderResp, hdr[:], make([]byte, 16))
	f.Add(valid.Bytes())
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, msgRenderResp})
	f.Add([]byte{0, 0, 0, 1, 99})
	f.Fuzz(func(t *testing.T, data []byte) {
		if resp, err := readRenderResp(bytes.NewReader(data), 8, 8, 64); err == nil {
			assertCleanWorkerStr(t, resp.Err, false)
			if resp.Image != nil {
				b := resp.Image.Bounds()
				if b.Dx() < 1 || b.Dy() < 1 || b.Dx() > 8 || b.Dy() > 8 || b.Dx()*b.Dy() > 64 || len(resp.Image.Pix) != b.Dx()*b.Dy()*4 {
					t.Fatalf("invalid decoded image bounds: %v (pix=%d)", b, len(resp.Image.Pix))
				}
			}
		}
		_, _ = readRenderReq(bytes.NewReader(data))
	})
}
