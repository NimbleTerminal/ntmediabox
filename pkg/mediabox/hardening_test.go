// Copyright (c) 2026 Neomantra Corp. All rights reserved.
// SPDX-License-Identifier: MIT

package mediabox

import (
	"context"
	"encoding/binary"
	"errors"
	"image"
	"math"
	"net"
	"strings"
	"testing"
	"time"
)

// rawRenderResp builds a msgRenderResp frame without going through
// writeRenderResp, the way a hostile worker would.
func rawRenderResp(width, height float64, errStr string) []byte {
	var hdr [renderRespHeader]byte
	binary.BigEndian.PutUint64(hdr[9:17], math.Float64bits(width))
	binary.BigEndian.PutUint64(hdr[17:25], math.Float64bits(height))
	binary.BigEndian.PutUint16(hdr[33:35], uint16(len(errStr)))
	var buf strings.Builder
	_ = writeMsg(&buf, msgRenderResp, hdr[:], []byte(errStr))
	return []byte(buf.String())
}

func TestRenderRejectsNonFiniteOrNegativeIntrinsicSize(t *testing.T) {
	cases := map[string][2]float64{
		"nan_width":       {math.NaN(), 1},
		"nan_height":      {1, math.NaN()},
		"pos_inf_width":   {math.Inf(1), 1},
		"neg_inf_height":  {1, math.Inf(-1)},
		"negative_width":  {-1, 1},
		"negative_height": {1, -0.5},
	}
	for name, dims := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := readRenderResp(strings.NewReader(string(rawRenderResp(dims[0], dims[1], ""))), 8, 8, 64)
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("readRenderResp(width=%v, height=%v) err = %v, want ErrProtocol", dims[0], dims[1], err)
			}
		})
	}
	if _, err := readRenderResp(strings.NewReader(string(rawRenderResp(612, 792, ""))), 8, 8, 64); err != nil {
		t.Fatalf("finite non-negative size rejected: %v", err)
	}
}

func TestSanitizeStripsDocumentedBidiAndSeparators(t *testing.T) {
	for _, r := range []rune{0x200e, 0x200f, 0x061c, 0x2028, 0x2029, 0x202e, 0x2066} {
		got, ok := sanitizeWorkerString("a"+string(r)+"b", true)
		if !ok || got != "ab" {
			t.Errorf("sanitizeWorkerString with %U = %q, want %q", r, got, "ab")
		}
	}
}

func TestSanitizeConvertsTabToSpace(t *testing.T) {
	for _, allowNewline := range []bool{true, false} {
		if got, _ := sanitizeWorkerString("a\tb", allowNewline); got != "a b" {
			t.Errorf("sanitizeWorkerString(tab, allowNewline=%v) = %q, want %q", allowNewline, got, "a b")
		}
	}
}

func TestSanitizeConsumesWholeEscapeSequences(t *testing.T) {
	cases := map[string]string{
		"kitty_apc":        "a\x1b_Ga=T,f=100;AAAA\x1b\\b",
		"dcs_sixel":        "a\x1bPq#0;2;0;0;0\x1b\\b",
		"pm":               "a\x1b^private\x1b\\b",
		"sos":              "a\x1bXstring\x1b\\b",
		"nf_charset":       "a\x1b(Bb",
		"c1_csi":           "a\u009b31mb",
		"c1_osc":           "a\u009d0;title\u009cb",
		"c1_dcs":           "a\u0090payload\u009cb",
		"two_byte_escape":  "a\x1bcb",
		"csi_with_private": "a\x1b[?25lb",
	}
	for name, in := range cases {
		if got, ok := sanitizeWorkerString(in, true); !ok || got != "ab" {
			t.Errorf("%s: sanitizeWorkerString(%q) = %q, want %q", name, in, got, "ab")
		}
	}
}

func TestSanitizeUnterminatedStringSequenceStopsAtNewline(t *testing.T) {
	got, ok := sanitizeWorkerString("a\x1b]0;never terminated\nnext line", true)
	if !ok || got != "a\nnext line" {
		t.Fatalf("sanitizeWorkerString = %q, want %q", got, "a\nnext line")
	}
}

func TestRenderControlOnlyErrStaysAnError(t *testing.T) {
	for _, raw := range []string{"\n", "\x1b[31m", "\u202e"} {
		a, b := net.Pipe()
		go func() {
			_, _ = readRenderReq(b)
			_, _ = b.Write(rawRenderResp(1, 1, raw))
		}()
		client := NewRenderClient(a)
		_, err := client.Do(context.Background(), RenderRequest{Kind: "pdf"})
		if err == nil || strings.TrimSpace(err.Error()) == "" {
			t.Errorf("worker Err %q surfaced as err=%v, want a non-empty error", raw, err)
		}
		_ = a.Close()
		_ = b.Close()
	}
}

func TestStatusFailedWithControlOnlyErrKeepsMessage(t *testing.T) {
	// Built by hand: encodeStatus is the honest worker's path.
	body := make([]byte, 25)
	body[8] = byte(Failed)
	body = appendStr(body, "\x1b[31m", maxErrLen)
	body = appendStr(body, "", maxCaptionTextLen)
	body = append(body, 0)
	s, ok := parseStatus(body)
	if !ok {
		t.Fatal("parseStatus failed")
	}
	if s.State != Failed || s.Err == "" {
		t.Fatalf("status = %+v, want Failed with a non-empty Err", s)
	}
}

// A Do that is still waiting its turn when its context expires must return on
// time and must not close a client whose worker did nothing wrong.
func TestRenderQueuedDoExpiringDoesNotKillWorker(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	go func() {
		_ = ServeRender(b, RenderFunc(func(req RenderRequest) (RenderResponse, error) {
			if req.Kind == "slow" {
				time.Sleep(300 * time.Millisecond)
			}
			return RenderResponse{Pages: 1}, nil
		}))
	}()
	killed := make(chan struct{})
	client := NewRenderClientWithKill(a, func() { close(killed) })

	first := make(chan error, 1)
	go func() {
		_, err := client.Do(context.Background(), RenderRequest{Kind: "slow"})
		first <- err
	}()
	time.Sleep(50 * time.Millisecond) // let the slow request take the connection

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := client.Do(ctx, RenderRequest{Kind: "queued"})
	waited := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued Do err = %v, want DeadlineExceeded", err)
	}
	if waited > 150*time.Millisecond {
		t.Errorf("queued Do with a 50ms deadline returned after %v", waited)
	}
	if err := <-first; err != nil {
		t.Fatalf("in-flight Do failed: %v", err)
	}
	select {
	case <-killed:
		t.Fatal("a queued Do timing out killed a healthy worker")
	default:
	}
	if _, err := client.Do(context.Background(), RenderRequest{Kind: "after"}); err != nil {
		t.Fatalf("client unusable after a queued Do timed out: %v", err)
	}
}

func TestServeReportsInvalidDecoderFrameInsteadOfSendingIt(t *testing.T) {
	cases := map[string]func(w, h int) ([]byte, int, int, error){
		"wider_than_box":  func(w, h int) ([]byte, int, int, error) { return make([]byte, (w+1)*h*4), w + 1, h, nil },
		"taller_than_box": func(w, h int) ([]byte, int, int, error) { return make([]byte, w*(h+1)*4), w, h + 1, nil },
		"short_buffer":    func(w, h int) ([]byte, int, int, error) { return make([]byte, w*h*4-4), w, h, nil },
		"zero_width":      func(w, h int) ([]byte, int, int, error) { return make([]byte, 4), 0, 1, nil },
		"error_with_data": func(w, h int) ([]byte, int, int, error) {
			return make([]byte, w*h*4), w, h, errors.New("decode failed")
		},
	}
	for name, frameFn := range cases {
		t.Run(name, func(t *testing.T) {
			dec := &fakeDecoder{frameFn: frameFn}
			client, statuses, frames := pair(t, dec)
			if err := client.Resize(4, 4); err != nil {
				t.Fatal(err)
			}
			if err := client.Open("clip.mp4"); err != nil {
				t.Fatal(err)
			}
			deadline := time.After(2 * time.Second)
			for {
				select {
				case s := <-statuses:
					if s.State == Failed && s.Err != "" {
						select {
						case img := <-frames:
							t.Fatalf("invalid frame %v reached the host", img.Bounds())
						default:
						}
						if client.Err() != nil {
							t.Fatalf("client died: %v", client.Err())
						}
						return
					}
				case <-client.Dead():
					t.Fatalf("host killed the worker (%v) instead of receiving a Failed status", client.Err())
				case <-deadline:
					t.Fatal("no Failed status for an invalid decoder frame")
				}
			}
		})
	}
}

func TestServeRenderTurnsOutOfBoundsResultsIntoErrors(t *testing.T) {
	cases := map[string]struct {
		req  RenderRequest
		resp RenderResponse
	}{
		"image_wider_than_requested": {
			req:  RenderRequest{Kind: "image", Width: 16, Height: 16},
			resp: RenderResponse{Image: image.NewRGBA(image.Rect(0, 0, 17, 4))},
		},
		"image_over_pixel_budget": {
			req:  RenderRequest{Kind: "image", Width: 100, Height: 100, MaxPixels: 64},
			resp: RenderResponse{Image: image.NewRGBA(image.Rect(0, 0, 9, 9))},
		},
		"nan_intrinsic_width": {
			req:  RenderRequest{Kind: "pdf"},
			resp: RenderResponse{Width: math.NaN(), Height: 1},
		},
		"negative_intrinsic_height": {
			req:  RenderRequest{Kind: "pdf"},
			resp: RenderResponse{Width: 1, Height: -1},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a, b := net.Pipe()
			t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
			go func() {
				_ = ServeRender(b, RenderFunc(func(req RenderRequest) (RenderResponse, error) {
					if req.Kind == "ping" {
						return RenderResponse{Pages: 1}, nil
					}
					return tc.resp, nil
				}))
			}()
			client := NewRenderClient(a)
			_, err := client.Do(context.Background(), tc.req)
			if err == nil || errors.Is(err, ErrProtocol) {
				t.Fatalf("err = %v, want a descriptive worker error (not ErrProtocol)", err)
			}
			if _, err := client.Do(context.Background(), RenderRequest{Kind: "ping"}); err != nil {
				t.Fatalf("worker session did not survive an out-of-bounds result: %v", err)
			}
		})
	}
}

func TestRenderApplicationErrorIsTypedAndKeepsSession(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	go func() {
		_ = ServeRender(b, RenderFunc(func(req RenderRequest) (RenderResponse, error) {
			if req.Kind == "bad" {
				return RenderResponse{Err: "corrupt xref table"}, nil
			}
			return RenderResponse{Pages: 1}, nil
		}))
	}()
	client := NewRenderClient(a)
	_, err := client.Do(context.Background(), RenderRequest{Kind: "bad"})
	var re *RenderError
	if !errors.As(err, &re) {
		t.Fatalf("err = %T %v, want *RenderError", err, err)
	}
	if re.Msg != "corrupt xref table" || re.Ended || err.Error() != "corrupt xref table" {
		t.Fatalf("RenderError = %+v (Error() = %q)", re, err.Error())
	}
	if client.Err() != nil {
		t.Fatalf("client.Err() = %v after a recoverable application error", client.Err())
	}
	if _, err := client.Do(context.Background(), RenderRequest{Kind: "ok"}); err != nil {
		t.Fatalf("session did not survive an application error: %v", err)
	}
}

func TestRenderWorkerPanicIsRecordedAsTerminalError(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	go func() {
		_ = ServeRender(b, RenderFunc(func(RenderRequest) (RenderResponse, error) { panic("corrupt stream") }))
	}()
	client := NewRenderClient(a)
	_, err := client.Do(context.Background(), RenderRequest{Kind: "svg"})
	var re *RenderError
	if !errors.As(err, &re) || !re.Ended {
		t.Fatalf("err = %T %v, want *RenderError with Ended", err, err)
	}
	if !errors.As(client.Err(), &re) || !strings.Contains(client.Err().Error(), "worker panic: corrupt stream") {
		t.Fatalf("client.Err() = %v, want the panic that ended the session", client.Err())
	}
}

func TestRenderTransportFailureIsNotARenderError(t *testing.T) {
	a, b := net.Pipe()
	go func() {
		_, _ = readRenderReq(b)
		_ = b.Close()
	}()
	client := NewRenderClient(a)
	_, err := client.Do(context.Background(), RenderRequest{Kind: "pdf"})
	var re *RenderError
	if err == nil || errors.As(err, &re) {
		t.Fatalf("err = %T %v, want a transport error that is not a *RenderError", err, err)
	}
}

func TestErrorStringOfMaxErrBytesRoundTrips(t *testing.T) {
	if MaxErrBytes != 1024 {
		t.Fatalf("MaxErrBytes = %d, want 1024 (the published wire limit)", MaxErrBytes)
	}
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	go func() {
		_, _ = readRenderReq(b)
		_, _ = b.Write(rawRenderResp(1, 1, strings.Repeat("e", MaxErrBytes)))
	}()
	_, err := NewRenderClient(a).Do(context.Background(), RenderRequest{Kind: "pdf"})
	if err == nil || errors.Is(err, ErrProtocol) || len(err.Error()) != MaxErrBytes {
		t.Fatalf("a %d-byte worker error was not delivered intact: %.60v", MaxErrBytes, err)
	}
}
