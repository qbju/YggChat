// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// Twemoji images, bundled so emoji render the same everywhere with no CDN.
// Graphics: Copyright Twitter, Inc. and other contributors (jdecked/twemoji
// v17.0.3), CC-BY 4.0. Served from the embedded zip at /twemoji/<code>.svg.
package main

import (
	"archive/zip"
	"bytes"
	_ "embed"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

//go:embed twemoji/svg.zip
var twemojiZip []byte

var (
	twOnce  sync.Once
	twFiles map[string]*zip.File
	twName  = regexp.MustCompile(`^[0-9a-f]{1,6}(-[0-9a-f]{1,6}){0,9}\.svg$`)
)

func twemojiHandler(w http.ResponseWriter, r *http.Request) {
	twOnce.Do(func() {
		twFiles = map[string]*zip.File{}
		if zr, err := zip.NewReader(bytes.NewReader(twemojiZip), int64(len(twemojiZip))); err == nil {
			for _, f := range zr.File {
				twFiles[f.Name] = f
			}
		}
	})
	name := strings.TrimPrefix(r.URL.Path, "/twemoji/")
	f := twFiles[name]
	if !twName.MatchString(name) || f == nil {
		http.NotFound(w, r)
		return
	}
	rc, err := f.Open()
	if err != nil {
		http.Error(w, "read error", 500)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	io.Copy(w, rc)
}
