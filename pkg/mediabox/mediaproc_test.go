// Copyright (c) 2026 Neomantra Corp. All rights reserved.
// SPDX-License-Identifier: MIT

package mediabox

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"io"
	"net"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

type fakeDecoder struct {
	mu      sync.Mutex
	ops     []string
	lastReq OpenRequest
	state   State
	pos     time.Duration
	errStr  string
	caption string
	tracks  []Track
	overlay bool
	frameFn func(w, h int) ([]byte, int, int, error)
	closed  bool
}

func (d *fakeDecoder) op(s string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ops = append(d.ops, s)
	return nil
}

func (d *fakeDecoder) Load(req OpenRequest) error {
	d.mu.Lock()
	d.lastReq = req
	d.overlay = req.CaptionOverlay
	d.state = Paused
	d.mu.Unlock()
	return d.op("load " + req.Name)
}

func (d *fakeDecoder) Play() error {
	d.mu.Lock()
	d.state = Playing
	d.mu.Unlock()
	return d.op("play")
}

func (d *fakeDecoder) Pause() error {
	d.mu.Lock()
	d.state = Paused
	d.mu.Unlock()
	return d.op("pause")
}

func (d *fakeDecoder) Seek(t time.Duration) error {
	d.mu.Lock()
	d.pos = t
	d.mu.Unlock()
	return d.op("seek")
}

func (d *fakeDecoder) SelectTrack(kind, id string) error {
	d.mu.Lock()
	for i := range d.tracks {
		if d.tracks[i].Kind == kind {
			d.tracks[i].Selected = d.tracks[i].ID == id
		}
	}
	d.mu.Unlock()
	return d.op("track " + kind + ":" + id)
}

func (d *fakeDecoder) SetCaptionOverlay(enabled bool) error {
	d.mu.Lock()
	d.overlay = enabled
	d.mu.Unlock()
	if enabled {
		return d.op("overlay on")
	}
	return d.op("overlay off")
}

func (d *fakeDecoder) Status() (Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return Status{
		State:         d.state,
		Position:      d.pos,
		Duration:      time.Minute,
		Err:           d.errStr,
		CaptionText:   d.caption,
		CaptionTracks: slices.Clone(d.tracks),
	}, nil
}

func (d *fakeDecoder) Frame(w, h int) ([]byte, int, int, error) {
	d.mu.Lock()
	fn := d.frameFn
	d.mu.Unlock()
	if fn == nil {
		return nil, 0, 0, nil
	}
	return fn(w, h)
}

func (d *fakeDecoder) Unload() error {
	d.mu.Lock()
	d.state = Idle
	d.pos = 0
	d.caption = ""
	d.mu.Unlock()
	return d.op("unload")
}

func (d *fakeDecoder) Close() error {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	return d.op("close")
}

func (d *fakeDecoder) has(s string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Contains(d.ops, s)
}

func pair(t *testing.T, d *fakeDecoder) (*Client, chan Status, chan *image.RGBA) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	go func() {
		_ = Serve(b, func(kind string) (Decoder, error) {
			if kind != "video" {
				return nil, errors.New("unknown kind")
			}
			return d, nil
		})
	}()
	statuses := make(chan Status, 64)
	frames := make(chan *image.RGBA, 64)
	c := NewClient(a, func(s Status) {
		select {
		case statuses <- s:
		default:
		}
	}, func(img *image.RGBA) {
		select {
		case frames <- img:
		default:
		}
	})
	if err := c.Handshake("video"); err != nil {
		t.Fatal(err)
	}
	return c, statuses, frames
}

func waitDecoder(t *testing.T, d *fakeDecoder, op string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !d.has(op) {
		if time.Now().After(deadline) {
			t.Fatalf("decoder never saw %q: %v", op, d.ops)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPlaybackRoundTrip(t *testing.T) {
	d := &fakeDecoder{frameFn: func(w, h int) ([]byte, int, int, error) {
		return bytes.Repeat([]byte{1, 2, 3, 4}, 2*2), 2, 2, nil
	}}
	c, _, frames := pair(t, d)
	for _, step := range []func() error{
		func() error { return c.Resize(4, 4) },
		func() error { return c.Open("abc") },
		c.Play, c.Pause,
		func() error { return c.Seek(3 * time.Second) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	for _, op := range []string{"load abc", "play", "pause", "seek"} {
		waitDecoder(t, d, op)
	}
	select {
	case img := <-frames:
		if img.Bounds() != image.Rect(0, 0, 2, 2) || img.Pix[0] != 1 || img.Pix[15] != 4 {
			t.Fatalf("%v %v", img.Bounds(), img.Pix)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no frame")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenMediaCaptionsTracksAndGenerations(t *testing.T) {
	d := &fakeDecoder{
		caption: "hello world",
		tracks: []Track{
			{ID: "1", Label: "English", Language: "en", Kind: "sub", Selected: true},
			{ID: "2", Label: "Español", Language: "es", Kind: "sub", Selected: false},
		},
		frameFn: func(w, h int) ([]byte, int, int, error) {
			return bytes.Repeat([]byte{9, 8, 7, 255}, 2*2), 2, 2, nil
		},
	}
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	go func() {
		_ = Serve(b, func(kind string) (Decoder, error) { return d, nil })
	}()

	statuses := make(chan Status, 64)
	frames := make(chan Frame, 64)
	c := NewClientWithGen(a, func(s Status) {
		select {
		case statuses <- s:
		default:
		}
	}, func(f Frame) {
		select {
		case frames <- f:
		default:
		}
	})
	if err := c.Handshake("video"); err != nil {
		t.Fatal(err)
	}
	if err := c.Resize(4, 4); err != nil {
		t.Fatal(err)
	}
	req := OpenRequest{
		Name: "clip.mp4",
		Captions: []Caption{
			{Name: "sub-0.srt", Label: "English", Language: "en", Default: true},
		},
		Limits: Limits{
			MaxFileBytes: 100 << 20,
			MaxWidth:     1920,
			MaxHeight:    1080,
			MaxPixels:    1920 * 1080,
		},
		Autoplay:       false,
		Loop:           true,
		CaptionOverlay: true,
	}
	if err := c.OpenMedia(req); err != nil {
		t.Fatal(err)
	}
	openGen := c.Gen()
	if openGen != 1 {
		t.Fatalf("openGen = %d, want 1", openGen)
	}
	waitDecoder(t, d, "load clip.mp4")

	d.mu.Lock()
	gotReq := d.lastReq
	d.mu.Unlock()
	if gotReq.Name != "clip.mp4" || !gotReq.Loop || !gotReq.CaptionOverlay || len(gotReq.Captions) != 1 || gotReq.Captions[0].Name != "sub-0.srt" || gotReq.Limits.MaxWidth != 1920 {
		t.Fatalf("unexpected OpenRequest: %+v", gotReq)
	}

	if err := c.SelectTrack("sub", "2"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetCaptionOverlay(false); err != nil {
		t.Fatal(err)
	}
	targetGen := c.Gen()
	waitDecoder(t, d, "track sub:2")
	waitDecoder(t, d, "overlay off")

	deadline := time.After(2 * time.Second)
	sawStatus := false
	for !sawStatus {
		select {
		case s := <-statuses:
			if s.Gen == targetGen && s.CaptionText == "hello world" && len(s.CaptionTracks) == 2 && s.CaptionTracks[1].Selected {
				sawStatus = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for status at target generation")
		}
	}

	sawFrame := false
	for !sawFrame {
		select {
		case f := <-frames:
			if f.Gen == targetGen && f.Image.Bounds() == image.Rect(0, 0, 2, 2) {
				sawFrame = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for frame at target generation")
		}
	}
}

func TestStatusFlows(t *testing.T) {
	d := &fakeDecoder{}
	c, statuses, _ := pair(t, d)
	_ = c.Resize(4, 4)
	_ = c.Open("abc")
	_ = c.Play()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case s := <-statuses:
			if s.State == Playing && s.Duration == time.Minute {
				return
			}
		case <-deadline:
			t.Fatal("no playing status")
		}
	}
}

func TestUnknownKindFails(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close() }()
	done := make(chan error, 1)
	go func() {
		done <- Serve(b, func(string) (Decoder, error) { return nil, errors.New("no") })
	}()
	c := NewClient(a, nil, nil)
	_ = c.Handshake("audio")
	if err := <-done; err == nil {
		t.Fatal("served an unknown kind")
	}
}

func rawClient(t *testing.T, w, h int) (*Client, net.Conn, chan *image.RGBA) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	frames := make(chan *image.RGBA, 4)
	c := NewClient(a, nil, func(img *image.RGBA) { frames <- img })
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := b.Read(buf); err != nil {
				return
			}
		}
	}()
	if w > 0 {
		if err := c.Resize(w, h); err != nil {
			t.Fatal(err)
		}
	}
	return c, b, frames
}

func frameMsg(gen, seq uint64, w, h uint32, pix []byte) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.BigEndian, uint32(1+frameHeader+len(pix)))
	b.WriteByte(msgFrame)
	_ = binary.Write(&b, binary.BigEndian, gen)
	_ = binary.Write(&b, binary.BigEndian, seq)
	_ = binary.Write(&b, binary.BigEndian, w)
	_ = binary.Write(&b, binary.BigEndian, h)
	b.Write(pix)
	return b.Bytes()
}

func waitDead(t *testing.T, c *Client) {
	t.Helper()
	select {
	case <-c.Dead():
	case <-time.After(2 * time.Second):
		t.Fatal("hostile message did not kill the client")
	}
}

func TestFrameSizeAcrossResize(t *testing.T) {
	c, b, frames := rawClient(t, 4, 4)
	if err := c.Resize(2, 2); err != nil { // smaller now; a 4×4 frame is still in flight
		t.Fatal(err)
	}
	go func() { _, _ = b.Write(frameMsg(1, 1, 4, 4, make([]byte, 4*4*4))) }()
	select {
	case img := <-frames:
		if img.Bounds().Dx() != 4 {
			t.Fatal(img.Bounds())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight frame at the earlier size refused")
	}
	go func() { _, _ = b.Write(frameMsg(1, 2, 5, 4, make([]byte, 5*4*4))) }() // larger than ever requested
	waitDead(t, c)
}

func TestOversizeFrameKills(t *testing.T) {
	c, b, _ := rawClient(t, 4, 4)
	go func() { _, _ = b.Write(frameMsg(1, 1, MaxFrameSide+1, 1, nil)) }()
	waitDead(t, c)
}

func TestLengthLieKills(t *testing.T) {
	c, b, _ := rawClient(t, 4, 4)
	go func() { _, _ = b.Write(frameMsg(1, 1, 2, 2, make([]byte, 3))) }() // 2×2 needs 16 bytes
	waitDead(t, c)
}

func TestHugeLengthKillsWithoutAllocating(t *testing.T) {
	c, b, _ := rawClient(t, 4, 4)
	go func() { _, _ = b.Write([]byte{0xff, 0xff, 0xff, 0xff, msgFrame}) }()
	waitDead(t, c)
}

func TestBadMessageKills(t *testing.T) {
	c, b, _ := rawClient(t, 4, 4)
	go func() { _, _ = b.Write([]byte{0, 0, 0, 1, 99}) }()
	waitDead(t, c)
}

func TestHostileStatusTrackCountKills(t *testing.T) {
	c, b, _ := rawClient(t, 4, 4)
	var body bytes.Buffer
	_ = binary.Write(&body, binary.BigEndian, uint64(1)) // gen
	body.WriteByte(byte(Playing))                        // state
	_ = binary.Write(&body, binary.BigEndian, uint64(0)) // pos
	_ = binary.Write(&body, binary.BigEndian, uint64(0)) // dur
	_ = binary.Write(&body, binary.BigEndian, uint16(0)) // err len
	_ = binary.Write(&body, binary.BigEndian, uint16(0)) // captionText len
	body.WriteByte(maxTracks + 1)                        // illegal track count
	var msg bytes.Buffer
	_ = binary.Write(&msg, binary.BigEndian, uint32(1+body.Len()))
	msg.WriteByte(msgStatus)
	msg.Write(body.Bytes())
	go func() { _, _ = b.Write(msg.Bytes()) }()
	waitDead(t, c)
}

func TestDeadClosesOnEOF(t *testing.T) {
	c, b, _ := rawClient(t, 0, 0)
	_ = b.Close()
	waitDead(t, c)
	if err := c.Play(); err == nil {
		t.Fatal("write after death succeeded")
	}
}

func TestOpenRejectsNonBareFilenames(t *testing.T) {
	d := &fakeDecoder{}
	c, _, _ := pair(t, d)
	for _, bad := range []string{
		"", ".", "..", "../clip.mp4", "sub/clip.mp4", `sub\clip.mp4`, "/abs/clip.mp4",
		"- dash.mp4", "--option", "bad\x00.mp4", "bad\n.mp4", "bad\x1b.mp4", "bad\x7f.mp4", "bad\xff.mp4",
	} {
		if err := c.OpenMedia(OpenRequest{Name: bad}); err == nil {
			t.Fatalf("Client.OpenMedia accepted non-bare media name %q", bad)
		}
		if err := c.OpenMedia(OpenRequest{Name: "clip.mp4", Captions: []Caption{{Name: bad}}}); err == nil {
			t.Fatalf("Client.OpenMedia accepted non-bare caption name %q", bad)
		}
	}

	// Raw wire check: Serve itself must reject path traversal in msgOpen even if the peer is hostile.
	for _, req := range []OpenRequest{
		{Name: "../secret.mp4"},
		{Name: "sub/clip.mp4"},
		{Name: "-flag.mp4"},
		{Name: "clip.mp4", Captions: []Caption{{Name: "../secret.srt"}}},
	} {
		a, b := net.Pipe()
		done := make(chan error, 1)
		go func() {
			done <- Serve(b, func(string) (Decoder, error) { return d, nil })
		}()
		if err := writeMsg(a, msgHello, []byte("video")); err != nil {
			t.Fatal(err)
		}
		if err := writeMsg(a, msgOpen, encodeOpen(1, req)); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("Serve(%+v) returned %v, want ErrProtocol", req, err)
			}
		case <-time.After(2 * time.Second):
			_ = a.Close()
			_ = b.Close()
			t.Fatalf("Serve(%+v) accepted non-bare filename without failing", req)
		}
		_ = a.Close()
		_ = b.Close()
	}
}

func TestSanitizeWorkerStringsAndUTF8Truncation(t *testing.T) {
	// Ensure truncateUTF8 never splits a multi-byte UTF-8 rune.
	prefix := strings.Repeat("a", maxCaptionTextLen-1)
	truncated := truncateUTF8(prefix+"é", maxCaptionTextLen)
	if !utf8.ValidString(truncated) || len(truncated) != maxCaptionTextLen-1 {
		t.Fatalf("truncateUTF8 split multi-byte rune: len=%d valid=%v", len(truncated), utf8.ValidString(truncated))
	}

	// Ensure ANSI CSI, OSC, C0/C1 controls, DEL, and bidi overrides are stripped from Status fields.
	dirtyStatus := Status{
		Gen:         7,
		State:       Failed,
		Err:         "dec\x1b[31merror\x1b[0m\x1b]52;c;YWJj\x07\u202e!\nnext",
		CaptionText: "line1\x1b[2J\nline2\t\x07ok\x7f",
		CaptionTracks: []Track{
			{
				ID:       "1\x1b[32m",
				Label:    "Eng\x1b]0;title\x1b\\lish\u202e",
				Language: "en\x00",
				Kind:     "sub\r\n",
				Selected: true,
			},
		},
	}
	parsed, ok := parseStatus(encodeStatus(dirtyStatus))
	if !ok {
		t.Fatal("parseStatus failed")
	}
	if parsed.Err != "decerror!next" {
		t.Fatalf("parsed.Err = %q, want %q", parsed.Err, "decerror!next")
	}
	if parsed.CaptionText != "line1\nline2\tok" {
		t.Fatalf("parsed.CaptionText = %q, want %q", parsed.CaptionText, "line1\nline2\tok")
	}
	if len(parsed.CaptionTracks) != 1 {
		t.Fatalf("tracks = %d, want 1", len(parsed.CaptionTracks))
	}
	tr := parsed.CaptionTracks[0]
	if tr.ID != "1" || tr.Label != "English" || tr.Language != "en" || tr.Kind != "sub" {
		t.Fatalf("unsanitized track: %+v", tr)
	}

	// Invalid UTF-8 in Status must fail parseStatus.
	invalidStatus := encodeStatus(Status{State: Failed, Err: "bad\xffutf8"})
	if _, ok := parseStatus(invalidStatus); ok {
		t.Fatal("expected parseStatus to fail for invalid UTF-8 status")
	}
}

func TestNewClientWithKillAndErrOnProtocolViolation(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	killed := make(chan struct{}, 1)
	c := NewClientWithKill(a, nil, nil, func() {
		select {
		case killed <- struct{}{}:
		default:
		}
	})
	go func() { _, _ = b.Write([]byte{0, 0, 0, 1, 99}) }() // invalid msg type -> ErrProtocol
	waitDead(t, c)
	if !errors.Is(c.Err(), ErrProtocol) {
		t.Fatalf("c.Err() = %v, want ErrProtocol", c.Err())
	}
	select {
	case <-killed:
	case <-time.After(time.Second):
		t.Fatal("onKill was not called when worker violated protocol")
	}
}

func TestStopCallsUnloadAndWriteDeadlineUnblocksHungPeer(t *testing.T) {
	d := &fakeDecoder{}
	c, statuses, _ := pair(t, d)
	if err := c.Open("clip.mp4"); err != nil {
		t.Fatal(err)
	}
	waitDecoder(t, d, "load clip.mp4")
	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
	waitDecoder(t, d, "unload")
	stopGen := c.Gen()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case s := <-statuses:
			if s.Gen == stopGen && s.State == Idle {
				goto writeDeadlineCheck
			}
		case <-deadline:
			t.Fatal("timed out waiting for Idle status after Stop")
		}
	}

writeDeadlineCheck:
	// Verify write deadline kills and unblocks a peer that stops reading.
	pa, pb := net.Pipe()
	t.Cleanup(func() { _ = pa.Close(); _ = pb.Close() })
	hc := NewClient(pa, nil, nil)
	hc.writeTimeout = 30 * time.Millisecond
	if err := hc.Play(); err == nil {
		t.Fatal("expected write deadline error when peer does not read")
	}
	waitDead(t, hc)
}

type writeFailConn struct {
	io.Reader
}

func (writeFailConn) Write([]byte) (int, error) {
	return 0, errors.New("broken pipe")
}

func TestServeExitsCleanlyWithoutLeakingReaderGoroutine(t *testing.T) {
	before := runtime.NumGoroutine()
	var stream bytes.Buffer
	_ = writeMsg(&stream, msgHello, []byte("video"))
	_ = writeMsg(&stream, msgOpen, []byte("short")) // causes Serve to return ErrProtocol immediately
	for range 32 {
		_ = writeMsg(&stream, msgPlay, nil)
	}
	d := &fakeDecoder{}
	err := Serve(writeFailConn{Reader: &stream}, func(string) (Decoder, error) { return d, nil })
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("Serve error = %v, want ErrProtocol", err)
	}
	time.Sleep(20 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("Serve leaked reader goroutine: before=%d after=%d", before, after)
	}
}

func FuzzClientRead(f *testing.F) {
	f.Add(frameMsg(1, 1, 2, 2, make([]byte, 16)))
	f.Add([]byte{0, 0, 0, 1, 99})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{0, 0, 0, 3, msgStatus, 1, 2})
	statusBody := encodeStatus(Status{
		Gen:         2,
		State:       Playing,
		Position:    time.Second,
		Duration:    time.Minute,
		CaptionText: "sub",
		CaptionTracks: []Track{
			{ID: "1", Label: "English", Language: "en", Kind: "sub", Selected: true},
		},
	})
	statusMsg := append([]byte{0, 0, 0, byte(1 + len(statusBody)), msgStatus}, statusBody...)
	f.Add(statusMsg)
	f.Fuzz(func(t *testing.T, data []byte) {
		a, b := net.Pipe()
		defer func() { _ = a.Close() }()
		defer func() { _ = b.Close() }()
		c := NewClient(a, func(Status) {}, func(*image.RGBA) {})
		go func() {
			buf := make([]byte, 64)
			for {
				if _, err := b.Read(buf); err != nil {
					return
				}
			}
		}()
		_ = c.Resize(8, 8)
		go func() {
			_, _ = b.Write(data)
			_ = b.Close()
		}()
		select {
		case <-c.Dead():
		case <-time.After(50 * time.Millisecond):
		}
	})
}

func assertCleanWorkerStr(t *testing.T, s string, allowNewline bool) {
	t.Helper()
	if !utf8.ValidString(s) {
		t.Fatalf("invalid UTF-8 after sanitization: %q", s)
	}
	for _, r := range s {
		if r < 0x20 {
			if allowNewline && (r == '\n' || r == '\t') {
				continue
			}
			t.Fatalf("unexpected C0 control %U in %q", r, s)
		}
		if r == 0x7f || (r >= 0x80 && r <= 0x9f) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			t.Fatalf("unexpected control/bidi rune %U in %q", r, s)
		}
	}
}

func FuzzParseStatusAndOpen(f *testing.F) {
	f.Add(encodeStatus(Status{
		Gen:         1,
		State:       Playing,
		Position:    time.Second,
		Duration:    time.Minute,
		Err:         "err\x1b[31m",
		CaptionText: "hello\nworld",
		CaptionTracks: []Track{
			{ID: "1", Label: "English", Language: "en", Kind: "sub", Selected: true},
		},
	}))
	f.Add(encodeOpen(1, OpenRequest{
		Name: "clip.mp4",
		Captions: []Caption{
			{Name: "sub.srt", Label: "English", Language: "en", Default: true},
		},
	}))
	f.Fuzz(func(t *testing.T, data []byte) {
		if s, ok := parseStatus(data); ok {
			assertCleanWorkerStr(t, s.Err, false)
			assertCleanWorkerStr(t, s.CaptionText, true)
			for _, tr := range s.CaptionTracks {
				assertCleanWorkerStr(t, tr.ID, false)
				assertCleanWorkerStr(t, tr.Label, false)
				assertCleanWorkerStr(t, tr.Language, false)
				assertCleanWorkerStr(t, tr.Kind, false)
			}
		}
		if _, req, ok := parseOpen(data); ok {
			if !isBareFilename(req.Name) {
				t.Fatalf("parseOpen accepted non-bare Name %q", req.Name)
			}
			for _, c := range req.Captions {
				if !isBareFilename(c.Name) {
					t.Fatalf("parseOpen accepted non-bare Caption.Name %q", c.Name)
				}
			}
		}
	})
}

func FuzzSanitizeAndTruncateUTF8(f *testing.F) {
	f.Add("hello\x1b[31mworld\x1b]52;c;YWJj\x07\u202e!\n\t", 12, true)
	f.Add("éééé", 5, false)
	f.Add("bad\xffutf8", 4, false)
	f.Fuzz(func(t *testing.T, s string, maxLen int, allowNewline bool) {
		if maxLen > 8192 {
			maxLen = 8192
		}
		trunc := truncateUTF8(s, maxLen)
		if maxLen <= 0 && trunc != "" {
			t.Fatalf("truncateUTF8(%q, %d) = %q, want empty", s, maxLen, trunc)
		}
		if maxLen > 0 && len(trunc) > maxLen {
			t.Fatalf("len(truncateUTF8) = %d > %d", len(trunc), maxLen)
		}
		if utf8.ValidString(s) && !utf8.ValidString(trunc) {
			t.Fatalf("truncateUTF8 produced invalid UTF-8 from valid input: %q -> %q", s, trunc)
		}

		clean, ok := sanitizeWorkerString(s, allowNewline)
		if ok != utf8.ValidString(s) {
			t.Fatalf("sanitizeWorkerString ok=%v, ValidString=%v", ok, utf8.ValidString(s))
		}
		if ok {
			assertCleanWorkerStr(t, clean, allowNewline)
		}
	})
}

type rwBuffer struct {
	io.Reader
	io.Writer
}

func FuzzServeRead(f *testing.F) {
	var stream bytes.Buffer
	_ = writeMsg(&stream, msgHello, []byte("video"))
	_ = writeMsg(&stream, msgResize, []byte{0, 0, 0, 4, 0, 0, 0, 4})
	_ = writeMsg(&stream, msgOpen, encodeOpen(1, OpenRequest{Name: "clip.mp4"}))
	_ = writeMsg(&stream, msgPlay, nil)
	_ = writeMsg(&stream, msgPause, nil)
	_ = writeMsg(&stream, msgClose, u64(2))
	f.Add(stream.Bytes())
	f.Fuzz(func(t *testing.T, data []byte) {
		d := &fakeDecoder{}
		conn := rwBuffer{Reader: bytes.NewReader(data), Writer: io.Discard}
		_ = Serve(conn, func(kind string) (Decoder, error) {
			if kind != "video" {
				return nil, errors.New("unknown kind")
			}
			return d, nil
		})
	})
}

func TestMaxSizeStatusAndOpenRoundTrip(t *testing.T) {
	caps := make([]Caption, maxCaptions)
	for i := range caps {
		caps[i] = Caption{
			Name:     strings.Repeat("c", maxNameLen),
			Label:    strings.Repeat("L", maxTrackFieldLen),
			Language: strings.Repeat("e", maxTrackFieldLen),
			Default:  i == 0,
		}
	}
	tracks := make([]Track, maxTracks)
	for i := range tracks {
		tracks[i] = Track{
			ID:       strings.Repeat("i", maxTrackFieldLen),
			Label:    strings.Repeat("T", maxTrackFieldLen),
			Language: strings.Repeat("n", maxTrackFieldLen),
			Kind:     strings.Repeat("k", maxTrackFieldLen),
			Selected: i == 1,
		}
	}
	dec := &fakeDecoder{
		errStr:  strings.Repeat("E", maxErrLen),
		caption: strings.Repeat("C", maxCaptionTextLen),
		tracks:  tracks,
	}
	client, statuses, _ := pair(t, dec)

	req := OpenRequest{
		Name:     strings.Repeat("v", maxNameLen),
		Captions: caps,
		Limits: Limits{
			MaxFileBytes: 1 << 30,
			MaxWidth:     1920,
			MaxHeight:    1080,
			MaxPixels:    1920 * 1080,
		},
		Autoplay:       false,
		Loop:           true,
		CaptionOverlay: true,
	}
	if err := client.OpenMedia(req); err != nil {
		t.Fatalf("OpenMedia with max captions failed: %v", err)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case gotStatus := <-statuses:
			if len(gotStatus.CaptionTracks) != maxTracks || gotStatus.State != Failed {
				continue
			}
			if len(gotStatus.Err) != maxErrLen || len(gotStatus.CaptionText) != maxCaptionTextLen || len(gotStatus.CaptionTracks) != maxTracks {
				t.Fatalf("host received truncated Status: err=%d cap=%d tracks=%d", len(gotStatus.Err), len(gotStatus.CaptionText), len(gotStatus.CaptionTracks))
			}
			dec.mu.Lock()
			gotReq := dec.lastReq
			dec.mu.Unlock()
			if len(gotReq.Captions) != maxCaptions || len(gotReq.Name) != maxNameLen {
				t.Fatalf("worker received truncated OpenRequest: name=%d caps=%d", len(gotReq.Name), len(gotReq.Captions))
			}
			return
		case <-deadline:
			t.Fatalf("timed out waiting for max-size Status (client.Err=%v)", client.Err())
		}
	}
}

func TestRenderClientConcurrentCloseAndDo(t *testing.T) {
	for range 32 {
		hostConn, workerConn := net.Pipe()
		go func() {
			_ = ServeRender(workerConn, RenderFunc(func(req RenderRequest) (RenderResponse, error) {
				time.Sleep(2 * time.Millisecond)
				return RenderResponse{Pages: 1}, nil
			}))
			_ = workerConn.Close()
		}()

		rc := NewRenderClient(hostConn)
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = rc.Do(context.Background(), RenderRequest{Kind: "ping"})
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Millisecond)
			_ = rc.Close()
		}()
		wg.Wait()
		_ = rc.Close()
		if _, err := rc.Do(context.Background(), RenderRequest{Kind: "ping"}); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("Do after Close = %v, want io.ErrClosedPipe", err)
		}
	}
}
