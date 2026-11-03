// Copyright (c) 2026 Neomantra Corp. All rights reserved.
// SPDX-License-Identifier: AGPL-3.0-only WITH LicenseRef-NimbleTerminal-Linking-Exception OR LicenseRef-Neomantra-Commercial

// Package ntmediabox runs native media and document decoders in a separate
// OS-confined helper process and speaks a bounds-checked binary frame protocol
// over the helper's socket or pipe.
//
// The host process treats the worker as hostile: every control message length
// and frame dimension is validated before memory is allocated, and any protocol
// violation terminates the connection and kills the worker.
//
// The pure-Go wire protocol and client/server types are also available as a
// standalone MIT-licensed subpackage at nimbleterminal.dev/ntmediabox/pkg/mediabox.
package ntmediabox

import (
	"image"
	"io"

	"nimbleterminal.dev/ntmediabox/pkg/mediabox"
)

// MaxFrameSide bounds a video frame's width and height in pixels.
const MaxFrameSide = mediabox.MaxFrameSide

// State is what a media decoder is doing.
type State = mediabox.State

const (
	Idle      = mediabox.Idle
	Loading   = mediabox.Loading
	Buffering = mediabox.Buffering
	Playing   = mediabox.Playing
	Paused    = mediabox.Paused
	Ended     = mediabox.Ended
	Failed    = mediabox.Failed
)

// Caption describes a sidecar subtitle file inside the worker's media directory.
type Caption = mediabox.Caption

// Limits bounds media container metadata and file size inside the worker.
type Limits = mediabox.Limits

// OpenRequest configures a media load inside the worker.
type OpenRequest = mediabox.OpenRequest

// Track describes an embedded or external subtitle/caption track.
type Track = mediabox.Track

// Status is a worker's playback report.
type Status = mediabox.Status

// Frame is a decoded RGBA video frame tagged with the worker's active command generation.
type Frame = mediabox.Frame

// Decoder is one kind of media decoder running inside the confined worker process.
type Decoder = mediabox.Decoder

// Unloader is an optional interface a Decoder may implement to release media on Stop.
type Unloader = mediabox.Unloader

// Client is the host process's end of a media worker connection.
type Client = mediabox.Client

// ErrProtocol indicates a wire protocol violation.
var ErrProtocol = mediabox.ErrProtocol

// NewClient starts reading from conn. The callbacks run on the reader goroutine
// and must not block.
func NewClient(conn io.ReadWriteCloser, onStatus func(Status), onFrame func(*image.RGBA)) *Client {
	return mediabox.NewClient(conn, onStatus, onFrame)
}

// NewClientWithGen starts reading from conn and delivers each decoded frame
// together with the worker's active command generation.
func NewClientWithGen(conn io.ReadWriteCloser, onStatus func(Status), onFrame func(Frame)) *Client {
	return mediabox.NewClientWithGen(conn, onStatus, onFrame)
}

// NewClientWithKill starts reading from conn, delivers each decoded frame with
// the worker's active command generation, and invokes onKill when Close is
// called or when the connection terminates due to EOF, I/O error, or protocol
// violation.
func NewClientWithKill(conn io.ReadWriteCloser, onStatus func(Status), onFrame func(Frame), onKill func()) *Client {
	return mediabox.NewClientWithKill(conn, onStatus, onFrame, onKill)
}

// StartMediaHelper starts a confined worker process via StartHelper and returns
// a Client wired to kill and reap the worker when closed or when a protocol
// violation or EOF occurs.
func StartMediaHelper(cfg HelperConfig, onStatus func(Status), onFrame func(Frame)) (*Client, *Helper, error) {
	h, err := StartHelper(cfg)
	if err != nil {
		return nil, nil, err
	}
	c := NewClientWithKill(h.Conn, onStatus, onFrame, func() {
		_ = h.Close()
	})
	return c, h, nil
}

// Serve runs the worker's end of a connection until the connection closes or
// a protocol error occurs. open returns the Decoder for the kind named in the
// client's handshake.
func Serve(conn io.ReadWriter, open func(kind string) (Decoder, error)) error {
	return mediabox.Serve(conn, open)
}
