// Copyright (c) 2026 Neomantra Corp. All rights reserved.
// SPDX-License-Identifier: AGPL-3.0-only WITH LicenseRef-NimbleTerminal-Linking-Exception OR LicenseRef-Neomantra-Commercial

package ntmediabox

import (
	"io"

	"nimbleterminal.dev/ntmediabox/pkg/mediabox"
)

// Hard upper bounds for sandboxed document, vector, and still-image rendering.
const (
	MaxRenderSide      = mediabox.MaxRenderSide
	MaxRenderPixels    = mediabox.MaxRenderPixels
	MaxRenderDataBytes = mediabox.MaxRenderDataBytes
	MaxRenderMetaBytes = mediabox.MaxRenderMetaBytes

	RenderFlagText    = mediabox.RenderFlagText
	RenderFlagDark    = mediabox.RenderFlagDark
	RenderFlagAnimate = mediabox.RenderFlagAnimate
)

// RenderRequest is a single document/image parse or rasterize request sent from
// the host to a confined worker.
type RenderRequest = mediabox.RenderRequest

// RenderResponse is a confined worker's reply to a RenderRequest.
type RenderResponse = mediabox.RenderResponse

// RenderHandler handles document/image requests inside a confined worker process.
type RenderHandler = mediabox.RenderHandler

// RenderFunc adapts a function into a stateless RenderHandler.
type RenderFunc = mediabox.RenderFunc

// RenderClient is the host process's end of a request/response render worker connection.
type RenderClient = mediabox.RenderClient

// ToRGBA converts src into a tightly packed *image.RGBA with origin (0, 0) and
// Stride == w*4.
var ToRGBA = mediabox.ToRGBA

// NewRenderClient wraps conn as a host-side render client.
func NewRenderClient(conn io.ReadWriteCloser) *RenderClient {
	return mediabox.NewRenderClient(conn)
}

// NewRenderClientWithKill wraps conn and invokes onKill when Close is called or
// when a request is cancelled/violates the wire protocol.
func NewRenderClientWithKill(conn io.ReadWriteCloser, onKill func()) *RenderClient {
	return mediabox.NewRenderClientWithKill(conn, onKill)
}

// ServeRender runs the worker's end of a RenderClient connection.
func ServeRender(conn io.ReadWriter, h RenderHandler) error {
	return mediabox.ServeRender(conn, h)
}

// StartRenderHelper starts a confined worker process via StartHelper and returns
// a RenderClient wired to kill and reap the worker when closed or cancelled.
func StartRenderHelper(cfg HelperConfig) (*RenderClient, *Helper, error) {
	h, err := StartHelper(cfg)
	if err != nil {
		return nil, nil, err
	}
	c := NewRenderClientWithKill(h.Conn, func() {
		_ = h.Close()
	})
	return c, h, nil
}
