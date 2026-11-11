// Copyright (c) 2026 Neomantra Corp. All rights reserved.
// SPDX-License-Identifier: AGPL-3.0-only WITH LicenseRef-NimbleTerminal-Linking-Exception OR LicenseRef-Neomantra-Commercial

package ntmediabox

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"

	"nimbleterminal.dev/ntmediabox/pkg/mediabox"
)

// SECURITY.md states wire bounds that hosts and third-party workers rely on.
// Each table of bounds is a generated region rendered here from the constants
// the code enforces. TestSecurityDocInSync fails when a region is stale;
// `task security-doc` rewrites them.

var updateSecurityDoc = flag.Bool("update-security-doc", false, "rewrite the generated regions of SECURITY.md")

var securityDocRegions = map[string]func() string{
	"media-bounds":  renderMediaBounds,
	"render-bounds": renderRenderBounds,
}

func regionMarkers(name string) (begin, end string) {
	return "<!-- BEGIN GENERATED:" + name + " (edit the code, then run `task security-doc`) -->",
		"<!-- END GENERATED:" + name + " -->"
}

func renderMediaBounds() string {
	rows := [][3]string{
		{"Control message size", fmt.Sprintf("`MaxControlBytes = %d`", mediabox.MaxControlBytes),
			"`1 <= n <= MaxControlBytes` on every non-frame message"},
		{"Video frame dimensions", fmt.Sprintf("`MaxFrameSide = %d`", mediabox.MaxFrameSide),
			"`1 <= w <= maxW <= MaxFrameSide` and `1 <= h <= maxH <= MaxFrameSide`, where `maxW`/`maxH` are the largest sizes the host has requested"},
		{"Video frame byte length", fmt.Sprintf("`FrameHeaderBytes = %d`", mediabox.FrameHeaderBytes),
			fmt.Sprintf("`uint64(n) == 1 + %d + uint64(w)*uint64(h)*4`, checked before `image.NewRGBA`", mediabox.FrameHeaderBytes)},
		{"Filename / kind length", fmt.Sprintf("`MaxNameBytes = %d`", mediabox.MaxNameBytes),
			"Bare filename only: no `/`, `\\`, `:`, `.`, `..`, leading `-`, or control characters"},
		{"Sidecar captions per open", fmt.Sprintf("`MaxCaptions = %d`", mediabox.MaxCaptions),
			"Larger counts and trailing unparsed bytes are rejected"},
		{"Caption tracks per status", fmt.Sprintf("`MaxTracks = %d`", mediabox.MaxTracks),
			"Larger counts and trailing unparsed bytes are rejected"},
		{"Track / caption field length", fmt.Sprintf("`MaxTrackFieldBytes = %d`", mediabox.MaxTrackFieldBytes),
			"Longer fields are rejected; worker-supplied fields are sanitized"},
		{"Caption text", fmt.Sprintf("`MaxCaptionTextBytes = %d`", mediabox.MaxCaptionTextBytes),
			"Must be valid UTF-8; sanitized"},
		{"Error text", fmt.Sprintf("`MaxErrBytes = %d`", mediabox.MaxErrBytes),
			"Must be valid UTF-8; sanitized"},
	}
	return renderBoundsTable(rows)
}

func renderRenderBounds() string {
	rows := [][3]string{
		{"Raster dimensions", fmt.Sprintf("`MaxRenderSide = %d`", mediabox.MaxRenderSide),
			"`1 <= imgW <= maxW <= MaxRenderSide` and `1 <= imgH <= maxH <= MaxRenderSide`"},
		{"Raster pixel budget", fmt.Sprintf("`MaxRenderPixels = %d` (%d MiB RGBA)", mediabox.MaxRenderPixels, mediabox.MaxRenderPixels*4>>20),
			"`uint64(imgW)*uint64(imgH) <= maxPixels <= MaxRenderPixels`"},
		{"Request input bytes", fmt.Sprintf("`MaxRenderDataBytes = %d` (%d MiB)", mediabox.MaxRenderDataBytes, mediabox.MaxRenderDataBytes>>20),
			"`dataLen <= MaxRenderDataBytes`"},
		{"Structured metadata bytes", fmt.Sprintf("`MaxRenderMetaBytes = %d` (%d MiB)", mediabox.MaxRenderMetaBytes, mediabox.MaxRenderMetaBytes>>20),
			"`metaLen <= MaxRenderMetaBytes`"},
		{"Error text", fmt.Sprintf("`MaxErrBytes = %d`", mediabox.MaxErrBytes),
			"`errLen <= MaxErrBytes`; must be valid UTF-8; sanitized"},
		{"Exact response size", fmt.Sprintf("`RenderRespHeaderBytes = %d`", mediabox.RenderRespHeaderBytes),
			fmt.Sprintf("`uint64(n) == 1 + %d + errLen + metaLen + uint64(imgW)*uint64(imgH)*4`, checked before `Meta` or `Image` is allocated", mediabox.RenderRespHeaderBytes)},
		{"Intrinsic dimensions", "`Width`, `Height` (`float64`)",
			"Must be finite and `>= 0`"},
	}
	return renderBoundsTable(rows)
}

func renderBoundsTable(rows [][3]string) string {
	var b strings.Builder
	b.WriteString("| Bound | Constant | Invariant enforced before allocation |\n| --- | --- | --- |\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", r[0], r[1], r[2])
	}
	return b.String()
}

func TestSecurityDocInSync(t *testing.T) {
	const path = "SECURITY.md"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	updated := doc
	for name, render := range securityDocRegions {
		begin, end := regionMarkers(name)
		if strings.Count(updated, begin) != 1 || strings.Count(updated, end) != 1 {
			t.Fatalf("%s must contain exactly one %q ... %q pair", path, begin, end)
		}
		i := strings.Index(updated, begin) + len(begin)
		j := strings.Index(updated, end)
		if j < i {
			t.Fatalf("%s: region %q ends before it begins", path, name)
		}
		updated = updated[:i] + "\n" + render() + updated[j:]
	}
	if updated == doc {
		return
	}
	if *updateSecurityDoc {
		if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("rewrote the generated regions of %s", path)
		return
	}
	t.Fatalf("%s is out of date with the wire bounds the code enforces; run `task security-doc`", path)
}

// USAGE.md is the wire specification for workers written in other languages.
func TestUsageDocStatesEnforcedLimits(t *testing.T) {
	raw, err := os.ReadFile("USAGE.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	for _, want := range []string{
		fmt.Sprintf("UTF-8 error string (`<= %d`)", mediabox.MaxErrBytes),
		fmt.Sprintf("MAX_ERR_BYTES   = %d", mediabox.MaxErrBytes),
		fmt.Sprintf("(`<= %d MiB`)", mediabox.MaxRenderMetaBytes>>20),
		fmt.Sprintf("(`<= %d MiB`)", mediabox.MaxRenderDataBytes>>20),
		fmt.Sprintf("is exactly `%d` bytes", mediabox.RenderReqFixedBytes),
		fmt.Sprintf("header is `%d` bytes", mediabox.RenderRespHeaderBytes),
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("USAGE.md does not state %q", want)
		}
	}
}
