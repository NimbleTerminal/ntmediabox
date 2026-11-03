// Copyright (c) 2026 Neomantra Corp. All rights reserved.
// SPDX-License-Identifier: MIT

// Package mediabox defines the pure-Go wire protocols, hostile-peer bounds
// checks, and client/server types for communicating with sandboxed media and
// document workers.
//
// The host process treats the worker as hostile: every control message length
// and frame dimension is validated before memory is allocated, and any protocol
// violation terminates the connection.
package mediabox

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// MaxFrameSide bounds a video frame's width and height in pixels.
const MaxFrameSide = 4096

const (
	maxControlLen     = 32 << 10 // maximum length (type + payload) for any non-frame message (covers max Status = 21311B)
	frameHeader       = 8 + 8 + 4 + 4
	serveInterval     = time.Second / 30
	maxCaptions       = 16
	maxTracks         = 32
	maxErrLen         = 512
	maxCaptionTextLen = 4096
	maxTrackFieldLen  = 128
	maxNameLen        = 255
)

// Message types on the wire.
const (
	msgHello = iota + 1
	msgOpen
	msgPlay
	msgPause
	msgSeek
	msgResize
	msgClose
	msgStatus
	msgFrame
	msgSelectTrack
	msgCaptionOverlay
)

// OpenRequest bit flags on the wire.
const (
	openFlagAutoplay       = 1 << 0
	openFlagLoop           = 1 << 1
	openFlagCaptionOverlay = 1 << 2
)

// State is what a media decoder is doing.
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

// Caption describes a sidecar subtitle file inside the worker's media directory.
type Caption struct {
	Name     string // bare filename inside the worker's media directory
	Label    string
	Language string
	Default  bool
}

// Limits bounds media container metadata and file size inside the worker.
type Limits struct {
	MaxFileBytes        int64
	MaxWidth, MaxHeight int
	MaxPixels           int64
}

// OpenRequest configures a media load inside the worker.
type OpenRequest struct {
	Name           string // bare filename inside the worker's media directory
	Captions       []Caption
	Limits         Limits
	Autoplay       bool
	Loop           bool
	CaptionOverlay bool // true suppresses burned-in subtitles in favor of Status.CaptionText
}

// Track describes an embedded or external subtitle/caption track.
type Track struct {
	ID       string
	Label    string
	Language string
	Kind     string
	Selected bool
}

// Status is a worker's playback report.
type Status struct {
	Gen                uint64
	State              State
	Position, Duration time.Duration
	Err                string
	CaptionText        string
	CaptionTracks      []Track
}

// Frame is a decoded RGBA video frame tagged with the worker's active command generation.
type Frame struct {
	Gen   uint64
	Seq   uint64
	Image *image.RGBA
}

// Decoder is one kind of media decoder running inside the confined worker process.
// Frame returns a nil slice when there is no new frame to emit.
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

// Unloader is an optional interface a Decoder may implement to release media
// resources when Client.Stop (msgClose) is called while keeping the worker
// alive for a subsequent Load. If a Decoder does not implement Unloader, Serve
// falls back to Pause().
type Unloader interface {
	Unload() error
}

// ErrProtocol indicates a wire protocol violation.
var ErrProtocol = errors.New("ntmediabox: protocol violation")

func writeMsg(w io.Writer, typ byte, payload ...[]byte) error {
	n := 1
	for _, p := range payload {
		n += len(p)
	}
	buf := make([]byte, 5, 5+n)
	binary.BigEndian.PutUint32(buf, uint32(n))
	buf[4] = typ
	for _, p := range payload {
		buf = append(buf, p...)
	}
	_, err := w.Write(buf)
	return err
}

func u64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }

func truncateUTF8(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	if len(s) <= maxLen {
		return s
	}
	i := maxLen
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}

func appendStr(buf []byte, s string, maxLen int) []byte {
	s = truncateUTF8(s, maxLen)
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(s)))
	return append(buf, s...)
}

func readStr(b []byte, maxLen int) (string, []byte, bool) {
	if len(b) < 2 {
		return "", nil, false
	}
	n := int(binary.BigEndian.Uint16(b[:2]))
	b = b[2:]
	if n > maxLen || len(b) < n {
		return "", nil, false
	}
	return string(b[:n]), b[n:], true
}

// sanitizeWorkerString validates that s is valid UTF-8 and strips terminal
// escape sequences (CSI/OSC), C0/C1 control characters, DEL, and Unicode
// bidirectional override codepoints. When allowNewline is true, '\n' and '\t'
// are preserved.
func sanitizeWorkerString(s string, allowNewline bool) (string, bool) {
	if !utf8.ValidString(s) {
		return "", false
	}
	if s == "" {
		return "", true
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i++
			if i >= len(s) {
				break
			}
			switch s[i] {
			case '[': // CSI: ESC [ params intermediates final
				i++
				for i < len(s) && s[i] >= 0x30 && s[i] <= 0x3f {
					i++
				}
				for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f {
					i++
				}
				if i < len(s) && s[i] >= 0x40 && s[i] <= 0x7e {
					i++
				}
			case ']': // OSC: ESC ] ... (BEL or ST)
				i++
				for i < len(s) {
					if s[i] == 0x07 {
						i++
						break
					}
					if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
						i += 2
						break
					}
					i++
				}
			default:
				r, w := utf8.DecodeRuneInString(s[i:])
				_ = r
				i += w
			}
			continue
		}
		r, w := utf8.DecodeRuneInString(s[i:])
		i += w
		if r < 0x20 {
			if allowNewline && (r == '\n' || r == '\t') {
				b.WriteRune(r)
			}
			continue
		}
		if r == 0x7f || (r >= 0x80 && r <= 0x9f) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String(), true
}

func readCleanStr(b []byte, maxLen int, allowNewline bool) (string, []byte, bool) {
	raw, rest, ok := readStr(b, maxLen)
	if !ok {
		return "", nil, false
	}
	clean, ok := sanitizeWorkerString(raw, allowNewline)
	if !ok {
		return "", nil, false
	}
	return clean, rest, true
}

// Client is the host process's end of a media worker connection.
type Client struct {
	conn       io.ReadWriteCloser
	onStatus   func(Status)
	onFrame    func(*image.RGBA)
	onFrameGen func(Frame)
	onKill     func()
	dead       chan struct{}
	once       sync.Once

	errMu sync.Mutex
	err   error

	wmu          sync.Mutex
	writeTimeout time.Duration
	gen          atomic.Uint64
	smu          sync.Mutex
	maxW         int // largest width requested since start; in-flight frames may be smaller, never larger
	maxH         int // largest height requested since start
}

// NewClient starts reading from conn. The callbacks run on the reader goroutine
// and must not block.
func NewClient(conn io.ReadWriteCloser, onStatus func(Status), onFrame func(*image.RGBA)) *Client {
	c := &Client{
		conn:     conn,
		onStatus: onStatus,
		onFrame:  onFrame,
		dead:     make(chan struct{}),
	}
	go c.read()
	return c
}

// NewClientWithGen starts reading from conn and delivers each decoded frame
// together with the worker's active command generation.
func NewClientWithGen(conn io.ReadWriteCloser, onStatus func(Status), onFrame func(Frame)) *Client {
	return NewClientWithKill(conn, onStatus, onFrame, nil)
}

// NewClientWithKill starts reading from conn, delivers each decoded frame with
// the worker's active command generation, and invokes onKill when Close is
// called or when the connection terminates due to EOF, I/O error, or protocol
// violation.
func NewClientWithKill(conn io.ReadWriteCloser, onStatus func(Status), onFrame func(Frame), onKill func()) *Client {
	c := &Client{
		conn:       conn,
		onStatus:   onStatus,
		onFrameGen: onFrame,
		onKill:     onKill,
		dead:       make(chan struct{}),
	}
	go c.read()
	return c
}

// Dead is closed when the connection ends for any reason.
func (c *Client) Dead() <-chan struct{} { return c.dead }

// Err returns the error that caused the client connection to terminate, or nil
// if the client is still open or was closed cleanly via Close.
func (c *Client) Err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.err
}

// Gen returns the most recent command generation sent by this client.
func (c *Client) Gen() uint64 { return c.gen.Load() }

func (c *Client) die(err error) {
	c.once.Do(func() {
		c.errMu.Lock()
		c.err = err
		c.errMu.Unlock()
		if c.conn != nil {
			_ = c.conn.Close()
		}
		if c.onKill != nil {
			c.onKill()
		}
		close(c.dead)
	})
}

const defaultWriteTimeout = 5 * time.Second

type writeDeadliner interface {
	SetWriteDeadline(time.Time) error
}

func (c *Client) sendLocked(typ byte, payload ...[]byte) error {
	timeout := c.writeTimeout
	if timeout <= 0 {
		timeout = defaultWriteTimeout
	}
	if wd, ok := c.conn.(writeDeadliner); ok {
		_ = wd.SetWriteDeadline(time.Now().Add(timeout))
		defer func() { _ = wd.SetWriteDeadline(time.Time{}) }()
	}
	if err := writeMsg(c.conn, typ, payload...); err != nil {
		c.die(err)
		return err
	}
	return nil
}

func (c *Client) send(typ byte, payload ...[]byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	select {
	case <-c.dead:
		return io.ErrClosedPipe
	default:
	}
	return c.sendLocked(typ, payload...)
}

func (c *Client) sendGen(typ byte, build func(gen uint64) []byte) (uint64, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	select {
	case <-c.dead:
		return 0, io.ErrClosedPipe
	default:
	}
	gen := c.gen.Add(1)
	if err := c.sendLocked(typ, build(gen)); err != nil {
		return 0, err
	}
	return gen, nil
}

// Handshake names the kind of media this connection is for (e.g. "video").
func (c *Client) Handshake(kind string) error {
	if len(kind) == 0 || len(kind) > maxNameLen {
		return fmt.Errorf("ntmediabox: invalid kind %q", kind)
	}
	return c.send(msgHello, []byte(kind))
}

// Open loads a file by its bare name inside the worker's media directory.
func (c *Client) Open(name string) error {
	return c.OpenMedia(OpenRequest{Name: name})
}

func isBareFilename(name string) bool {
	if name == "" || len(name) > maxNameLen || name == "." || name == ".." || strings.HasPrefix(name, "-") || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == '/' || r == '\\' || r == ':' {
			return false
		}
	}
	return true
}

// OpenMedia loads media with optional sidecar captions, limits, and playback flags.
func (c *Client) OpenMedia(req OpenRequest) error {
	if !isBareFilename(req.Name) {
		return fmt.Errorf("ntmediabox: invalid media name %q", req.Name)
	}
	if len(req.Captions) > maxCaptions {
		return fmt.Errorf("ntmediabox: too many captions (%d > %d)", len(req.Captions), maxCaptions)
	}
	for _, cap := range req.Captions {
		if !isBareFilename(cap.Name) {
			return fmt.Errorf("ntmediabox: invalid caption name %q", cap.Name)
		}
	}
	_, err := c.sendGen(msgOpen, func(gen uint64) []byte {
		return encodeOpen(gen, req)
	})
	return err
}

func (c *Client) Play() error  { return c.send(msgPlay) }
func (c *Client) Pause() error { return c.send(msgPause) }

// Seek seeks to t and advances the client's command generation.
func (c *Client) Seek(t time.Duration) error {
	if t < 0 {
		t = 0
	}
	_, err := c.sendGen(msgSeek, func(gen uint64) []byte {
		buf := make([]byte, 16)
		binary.BigEndian.PutUint64(buf[:8], gen)
		binary.BigEndian.PutUint64(buf[8:16], uint64(t))
		return buf
	})
	return err
}

// SelectTrack selects a track by kind (e.g. "sub") and ID ("off" to disable)
// and advances the client's command generation.
func (c *Client) SelectTrack(kind, id string) error {
	if len(kind) > maxTrackFieldLen || len(id) > maxTrackFieldLen {
		return fmt.Errorf("ntmediabox: track field too long")
	}
	_, err := c.sendGen(msgSelectTrack, func(gen uint64) []byte {
		buf := u64(gen)
		buf = appendStr(buf, kind, maxTrackFieldLen)
		buf = appendStr(buf, id, maxTrackFieldLen)
		return buf
	})
	return err
}

// SetCaptionOverlay toggles whether captions are suppressed from burned-in video
// rendering (true) or rendered into the video frames by the worker (false).
func (c *Client) SetCaptionOverlay(enabled bool) error {
	_, err := c.sendGen(msgCaptionOverlay, func(gen uint64) []byte {
		var b byte
		if enabled {
			b = 1
		}
		return append(u64(gen), b)
	})
	return err
}

// Resize sets the bounding box (in pixels) that decoded frames should fit within.
func (c *Client) Resize(w, h int) error {
	if w < 1 || h < 1 || w > MaxFrameSide || h > MaxFrameSide {
		return fmt.Errorf("ntmediabox: bad size %dx%d", w, h)
	}
	c.smu.Lock()
	c.maxW, c.maxH = max(c.maxW, w), max(c.maxH, h)
	c.smu.Unlock()
	var buf [8]byte
	binary.BigEndian.PutUint32(buf[:4], uint32(w))
	binary.BigEndian.PutUint32(buf[4:8], uint32(h))
	return c.send(msgResize, buf[:])
}

// Stop releases the active media but keeps the worker alive for the next Open.
func (c *Client) Stop() error {
	_, err := c.sendGen(msgClose, func(gen uint64) []byte {
		return u64(gen)
	})
	return err
}

// Close ends the connection, and with it the worker.
func (c *Client) Close() error {
	c.die(nil)
	return nil
}

func (c *Client) read() {
	var readErr error
	defer func() { c.die(readErr) }()
	var head [5]byte
	for {
		if _, err := io.ReadFull(c.conn, head[:]); err != nil {
			readErr = err
			return
		}
		n := binary.BigEndian.Uint32(head[:4])
		switch typ := head[4]; {
		case n < 1:
			readErr = ErrProtocol
			return
		case typ == msgStatus:
			if n > maxControlLen {
				readErr = ErrProtocol
				return
			}
			body := make([]byte, n-1)
			if _, err := io.ReadFull(c.conn, body); err != nil {
				readErr = err
				return
			}
			s, ok := parseStatus(body)
			if !ok {
				readErr = ErrProtocol
				return
			}
			if c.onStatus != nil {
				c.onStatus(s)
			}
		case typ == msgFrame:
			if n < 1+frameHeader || n > 1+frameHeader+MaxFrameSide*MaxFrameSide*4 {
				readErr = ErrProtocol
				return
			}
			var fh [frameHeader]byte
			if _, err := io.ReadFull(c.conn, fh[:]); err != nil {
				readErr = err
				return
			}
			gen := binary.BigEndian.Uint64(fh[0:8])
			seq := binary.BigEndian.Uint64(fh[8:16])
			w := binary.BigEndian.Uint32(fh[16:20])
			h := binary.BigEndian.Uint32(fh[20:24])
			c.smu.Lock()
			maxW, maxH := c.maxW, c.maxH
			c.smu.Unlock()
			// Checked before the pixels are allocated.
			if w < 1 || h < 1 || w > uint32(maxW) || h > uint32(maxH) || uint64(n) != 1+frameHeader+uint64(w)*uint64(h)*4 {
				readErr = ErrProtocol
				return
			}
			img := image.NewRGBA(image.Rect(0, 0, int(w), int(h)))
			if _, err := io.ReadFull(c.conn, img.Pix); err != nil {
				readErr = err
				return
			}
			if c.onFrame != nil {
				c.onFrame(img)
			}
			if c.onFrameGen != nil {
				c.onFrameGen(Frame{Gen: gen, Seq: seq, Image: img})
			}
		default:
			readErr = ErrProtocol
			return
		}
	}
}

func encodeOpen(gen uint64, req OpenRequest) []byte {
	var flags byte
	if req.Autoplay {
		flags |= openFlagAutoplay
	}
	if req.Loop {
		flags |= openFlagLoop
	}
	if req.CaptionOverlay {
		flags |= openFlagCaptionOverlay
	}
	maxW := max(req.Limits.MaxWidth, 0)
	maxH := max(req.Limits.MaxHeight, 0)
	buf := make([]byte, 0, 64)
	buf = binary.BigEndian.AppendUint64(buf, gen)
	buf = append(buf, flags)
	buf = binary.BigEndian.AppendUint64(buf, uint64(max(req.Limits.MaxFileBytes, 0)))
	buf = binary.BigEndian.AppendUint32(buf, uint32(maxW))
	buf = binary.BigEndian.AppendUint32(buf, uint32(maxH))
	buf = binary.BigEndian.AppendUint64(buf, uint64(max(req.Limits.MaxPixels, 0)))
	buf = appendStr(buf, req.Name, maxNameLen)
	caps := req.Captions
	if len(caps) > maxCaptions {
		caps = caps[:maxCaptions]
	}
	buf = append(buf, byte(len(caps)))
	for _, cap := range caps {
		buf = appendStr(buf, cap.Name, maxNameLen)
		buf = appendStr(buf, cap.Label, maxTrackFieldLen)
		buf = appendStr(buf, cap.Language, maxTrackFieldLen)
		var def byte
		if cap.Default {
			def = 1
		}
		buf = append(buf, def)
	}
	return buf
}

func parseOpen(b []byte) (uint64, OpenRequest, bool) {
	const fixedHeader = 8 + 1 + 8 + 4 + 4 + 8
	if len(b) < fixedHeader {
		return 0, OpenRequest{}, false
	}
	gen := binary.BigEndian.Uint64(b[0:8])
	flags := b[8]
	maxFileBytes := int64(binary.BigEndian.Uint64(b[9:17]))
	maxW := int(binary.BigEndian.Uint32(b[17:21]))
	maxH := int(binary.BigEndian.Uint32(b[21:25]))
	maxPixels := int64(binary.BigEndian.Uint64(b[25:33]))
	if maxFileBytes < 0 || maxPixels < 0 || maxW < 0 || maxH < 0 {
		return 0, OpenRequest{}, false
	}
	rest := b[fixedHeader:]
	name, rest, ok := readStr(rest, maxNameLen)
	if !ok || !isBareFilename(name) || len(rest) < 1 {
		return 0, OpenRequest{}, false
	}
	numCaps := int(rest[0])
	rest = rest[1:]
	if numCaps > maxCaptions {
		return 0, OpenRequest{}, false
	}
	var caps []Caption
	if numCaps > 0 {
		caps = make([]Caption, 0, numCaps)
		for range numCaps {
			var c Caption
			c.Name, rest, ok = readStr(rest, maxNameLen)
			if !ok || !isBareFilename(c.Name) {
				return 0, OpenRequest{}, false
			}
			c.Label, rest, ok = readStr(rest, maxTrackFieldLen)
			if !ok {
				return 0, OpenRequest{}, false
			}
			c.Language, rest, ok = readStr(rest, maxTrackFieldLen)
			if !ok || len(rest) < 1 {
				return 0, OpenRequest{}, false
			}
			c.Default = rest[0] != 0
			rest = rest[1:]
			caps = append(caps, c)
		}
	}
	if len(rest) != 0 {
		return 0, OpenRequest{}, false
	}
	return gen, OpenRequest{
		Name:     name,
		Captions: caps,
		Limits: Limits{
			MaxFileBytes: maxFileBytes,
			MaxWidth:     maxW,
			MaxHeight:    maxH,
			MaxPixels:    maxPixels,
		},
		Autoplay:       flags&openFlagAutoplay != 0,
		Loop:           flags&openFlagLoop != 0,
		CaptionOverlay: flags&openFlagCaptionOverlay != 0,
	}, true
}

func encodeStatus(s Status) []byte {
	buf := make([]byte, 0, 64)
	buf = binary.BigEndian.AppendUint64(buf, s.Gen)
	buf = append(buf, byte(s.State))
	buf = binary.BigEndian.AppendUint64(buf, uint64(max(s.Position, 0)))
	buf = binary.BigEndian.AppendUint64(buf, uint64(max(s.Duration, 0)))
	buf = appendStr(buf, s.Err, maxErrLen)
	buf = appendStr(buf, s.CaptionText, maxCaptionTextLen)
	tracks := s.CaptionTracks
	if len(tracks) > maxTracks {
		tracks = tracks[:maxTracks]
	}
	buf = append(buf, byte(len(tracks)))
	for _, tr := range tracks {
		buf = appendStr(buf, tr.ID, maxTrackFieldLen)
		buf = appendStr(buf, tr.Label, maxTrackFieldLen)
		buf = appendStr(buf, tr.Language, maxTrackFieldLen)
		buf = appendStr(buf, tr.Kind, maxTrackFieldLen)
		var sel byte
		if tr.Selected {
			sel = 1
		}
		buf = append(buf, sel)
	}
	return buf
}

func parseStatus(b []byte) (Status, bool) {
	const fixedHeader = 8 + 1 + 8 + 8
	if len(b) < fixedHeader {
		return Status{}, false
	}
	s := Status{
		Gen:      binary.BigEndian.Uint64(b[0:8]),
		State:    State(b[8]),
		Position: time.Duration(binary.BigEndian.Uint64(b[9:17])),
		Duration: time.Duration(binary.BigEndian.Uint64(b[17:25])),
	}
	if s.State < Idle || s.State > Failed || s.Position < 0 || s.Duration < 0 {
		return Status{}, false
	}
	rest := b[fixedHeader:]
	var ok bool
	s.Err, rest, ok = readCleanStr(rest, maxErrLen, false)
	if !ok {
		return Status{}, false
	}
	s.CaptionText, rest, ok = readCleanStr(rest, maxCaptionTextLen, true)
	if !ok || len(rest) < 1 {
		return Status{}, false
	}
	numTracks := int(rest[0])
	rest = rest[1:]
	if numTracks > maxTracks {
		return Status{}, false
	}
	if numTracks > 0 {
		s.CaptionTracks = make([]Track, 0, numTracks)
		for range numTracks {
			var tr Track
			tr.ID, rest, ok = readCleanStr(rest, maxTrackFieldLen, false)
			if !ok {
				return Status{}, false
			}
			tr.Label, rest, ok = readCleanStr(rest, maxTrackFieldLen, false)
			if !ok {
				return Status{}, false
			}
			tr.Language, rest, ok = readCleanStr(rest, maxTrackFieldLen, false)
			if !ok {
				return Status{}, false
			}
			tr.Kind, rest, ok = readCleanStr(rest, maxTrackFieldLen, false)
			if !ok || len(rest) < 1 {
				return Status{}, false
			}
			tr.Selected = rest[0] != 0
			rest = rest[1:]
			s.CaptionTracks = append(s.CaptionTracks, tr)
		}
	}
	if len(rest) != 0 {
		return Status{}, false
	}
	return s, true
}

func statusEqual(a, b Status) bool {
	return a.Gen == b.Gen &&
		a.State == b.State &&
		a.Position == b.Position &&
		a.Duration == b.Duration &&
		a.Err == b.Err &&
		a.CaptionText == b.CaptionText &&
		slices.Equal(a.CaptionTracks, b.CaptionTracks)
}

// Serve runs the worker's end of a connection until the connection closes or
// a protocol error occurs. open returns the Decoder for the kind named in the
// client's handshake.
func Serve(conn io.ReadWriter, open func(kind string) (Decoder, error)) error {
	var head [5]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(head[:4])
	if head[4] != msgHello || n < 2 || n > maxControlLen {
		return ErrProtocol
	}
	kind := make([]byte, n-1)
	if _, err := io.ReadFull(conn, kind); err != nil {
		return err
	}
	d, err := open(string(kind))
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()

	type command struct {
		typ  byte
		body []byte
	}
	commands := make(chan command, 16)
	readErr := make(chan error, 1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		var rhead [5]byte
		for {
			if _, err := io.ReadFull(conn, rhead[:]); err != nil {
				readErr <- err
				return
			}
			n := binary.BigEndian.Uint32(rhead[:4])
			if n < 1 || n > maxControlLen {
				readErr <- ErrProtocol
				return
			}
			body := make([]byte, n-1)
			if _, err := io.ReadFull(conn, body); err != nil {
				readErr <- err
				return
			}
			select {
			case <-done:
				return
			case commands <- command{rhead[4], body}:
			}
		}
	}()

	w, h := 0, 0
	loaded := false
	var gen uint64
	var seq uint64
	var last Status
	hasLast := false
	lastErr := ""
	fail := func(err error) {
		if err != nil {
			lastErr = err.Error()
		}
	}
	tick := time.NewTicker(serveInterval)
	defer tick.Stop()
	for {
		select {
		case err := <-readErr:
			return err
		case c := <-commands:
			switch c.typ {
			case msgOpen:
				g, req, ok := parseOpen(c.body)
				if !ok {
					return ErrProtocol
				}
				gen = g
				lastErr = ""
				hasLast = false
				fail(d.Load(req))
				loaded = true
			case msgPlay:
				if len(c.body) != 0 {
					return ErrProtocol
				}
				fail(d.Play())
			case msgPause:
				if len(c.body) != 0 {
					return ErrProtocol
				}
				fail(d.Pause())
			case msgSeek:
				if len(c.body) != 16 {
					return ErrProtocol
				}
				gen = binary.BigEndian.Uint64(c.body[:8])
				fail(d.Seek(time.Duration(binary.BigEndian.Uint64(c.body[8:16]))))
			case msgSelectTrack:
				if len(c.body) < 8 {
					return ErrProtocol
				}
				g := binary.BigEndian.Uint64(c.body[:8])
				tkind, rest, ok := readStr(c.body[8:], maxTrackFieldLen)
				if !ok {
					return ErrProtocol
				}
				tid, rest, ok := readStr(rest, maxTrackFieldLen)
				if !ok || len(rest) != 0 {
					return ErrProtocol
				}
				gen = g
				fail(d.SelectTrack(tkind, tid))
			case msgCaptionOverlay:
				if len(c.body) != 9 {
					return ErrProtocol
				}
				gen = binary.BigEndian.Uint64(c.body[:8])
				fail(d.SetCaptionOverlay(c.body[8] != 0))
			case msgResize:
				if len(c.body) != 8 {
					return ErrProtocol
				}
				rw := int(binary.BigEndian.Uint32(c.body[:4]))
				rh := int(binary.BigEndian.Uint32(c.body[4:8]))
				if rw < 1 || rh < 1 || rw > MaxFrameSide || rh > MaxFrameSide {
					return ErrProtocol
				}
				w, h = rw, rh
			case msgClose:
				if len(c.body) != 8 {
					return ErrProtocol
				}
				gen = binary.BigEndian.Uint64(c.body[:8])
				lastErr = ""
				hasLast = false
				if u, ok := d.(Unloader); ok {
					fail(u.Unload())
				} else {
					fail(d.Pause())
				}
				loaded = false
			default:
				return ErrProtocol
			}
		case <-tick.C:
			var s Status
			if loaded {
				var err error
				s, err = d.Status()
				fail(err)
			} else {
				s = Status{State: Idle}
			}
			s.Gen = gen
			if lastErr != "" {
				s.State = Failed
				s.Err = lastErr
			} else if s.Err != "" && s.State != Failed {
				s.State = Failed
			}
			if !hasLast || !statusEqual(s, last) {
				last = s
				hasLast = true
				if err := writeMsg(conn, msgStatus, encodeStatus(s)); err != nil {
					return err
				}
			}
			if !loaded || w == 0 {
				continue
			}
			rgba, fw, fh, err := d.Frame(w, h)
			fail(err)
			if rgba == nil {
				continue
			}
			seq++
			var fhead [frameHeader]byte
			binary.BigEndian.PutUint64(fhead[0:8], gen)
			binary.BigEndian.PutUint64(fhead[8:16], seq)
			binary.BigEndian.PutUint32(fhead[16:20], uint32(fw))
			binary.BigEndian.PutUint32(fhead[20:24], uint32(fh))
			if err := writeMsg(conn, msgFrame, fhead[:], rgba); err != nil {
				return err
			}
		}
	}
}
