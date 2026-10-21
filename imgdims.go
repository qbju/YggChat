// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

package main

// Decompression bombs: a 600 KB PNG/GIF/WebP can claim 50000x50000 pixels,
// and the UI (browser/WebView) would try to decode gigabytes and crash. The
// UI never sends more than 1600 px, so anything far bigger is refused.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

const (
	imgMaxSide   = 8192
	imgMaxPixels = 24 << 20 // ~25 MP (100 MB as RGBA)
)

var errImgDims = errors.New("image dimensions too large or unreadable")

// imgDimsOK reads only the header of raw and checks its size.
func imgDimsOK(mime string, raw []byte) error {
	var w, h int
	switch mime {
	case textMime:
		return nil
	case "image/webp":
		var ok bool
		if w, h, ok = webpSize(raw); !ok {
			return errImgDims
		}
	default:
		cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
		if err != nil {
			return errImgDims
		}
		w, h = cfg.Width, cfg.Height
	}
	if w <= 0 || h <= 0 || w > imgMaxSide || h > imgMaxSide || w*h > imgMaxPixels {
		return errImgDims
	}
	return nil
}

// webpSize parses the canvas size of a WebP file (VP8, VP8L or VP8X).
func webpSize(b []byte) (int, int, bool) {
	if len(b) < 30 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WEBP" {
		return 0, 0, false
	}
	switch string(b[12:16]) {
	case "VP8X":
		w := int(b[24]) | int(b[25])<<8 | int(b[26])<<16
		h := int(b[27]) | int(b[28])<<8 | int(b[29])<<16
		return w + 1, h + 1, true
	case "VP8 ":
		d := b[20:]
		if d[3] != 0x9d || d[4] != 0x01 || d[5] != 0x2a {
			return 0, 0, false
		}
		return int(binary.LittleEndian.Uint16(d[6:8]) & 0x3fff), int(binary.LittleEndian.Uint16(d[8:10]) & 0x3fff), true
	case "VP8L":
		d := b[20:]
		if d[0] != 0x2f {
			return 0, 0, false
		}
		v := binary.LittleEndian.Uint32(d[1:5])
		return int(v&0x3fff) + 1, int((v>>14)&0x3fff) + 1, true
	}
	return 0, 0, false
}
