// Copyright (c) 2026 Neomantra Corp. All rights reserved.
// SPDX-License-Identifier: AGPL-3.0-only WITH LicenseRef-NimbleTerminal-Linking-Exception OR LicenseRef-Neomantra-Commercial

package ntmediabox

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"net"
	"testing"
	"time"
)

type stubRootDecoder struct{}

func (stubRootDecoder) Load(OpenRequest) error           { return nil }
func (stubRootDecoder) Play() error                      { return nil }
func (stubRootDecoder) Pause() error                     { return nil }
func (stubRootDecoder) Seek(time.Duration) error         { return nil }
func (stubRootDecoder) SelectTrack(string, string) error { return nil }
func (stubRootDecoder) SetCaptionOverlay(bool) error     { return nil }
func (stubRootDecoder) Status() (Status, error)          { return Status{State: Paused}, nil }
func (stubRootDecoder) Frame(int, int) ([]byte, int, int, error) {
	return bytes.Repeat([]byte{1, 2, 3, 255}, 4), 2, 2, nil
}
func (stubRootDecoder) Close() error { return nil }

func TestRootReexportsPlaybackAndRender(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close() }()
	defer func() { _ = b.Close() }()
	go func() {
		_ = Serve(b, func(string) (Decoder, error) { return stubRootDecoder{}, nil })
	}()

	frames := make(chan Frame, 4)
	c := NewClientWithGen(a, nil, func(f Frame) {
		select {
		case frames <- f:
		default:
		}
	})
	defer func() { _ = c.Close() }()
	if err := c.Handshake("video"); err != nil {
		t.Fatal(err)
	}
	if err := c.Resize(4, 4); err != nil {
		t.Fatal(err)
	}
	if err := c.Open("clip.mp4"); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-frames:
		if f.Image.Bounds() != image.Rect(0, 0, 2, 2) {
			t.Fatalf("unexpected frame bounds: %v", f.Image.Bounds())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for frame")
	}

	ra, rb := net.Pipe()
	defer func() { _ = ra.Close() }()
	defer func() { _ = rb.Close() }()
	go func() {
		_ = ServeRender(rb, RenderFunc(func(req RenderRequest) (RenderResponse, error) {
			img := image.NewRGBA(image.Rect(0, 0, 2, 2))
			img.SetRGBA(0, 0, color.RGBA{R: 7, G: 8, B: 9, A: 255})
			return RenderResponse{Pages: 1, Page: 1, Image: img, Ended: true}, nil
		}))
	}()
	rc := NewRenderClient(ra)
	defer func() { _ = rc.Close() }()
	resp, err := rc.Do(context.Background(), RenderRequest{Kind: "svg", Width: 4, Height: 4})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Image == nil || resp.Image.RGBAAt(0, 0) != (color.RGBA{R: 7, G: 8, B: 9, A: 255}) {
		t.Fatalf("unexpected render response: %+v", resp)
	}
}
