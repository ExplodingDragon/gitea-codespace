// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package web serves the compiled administration application.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// all: keeps the package buildable from a clean checkout that only contains dist/.gitkeep.
//
//go:embed all:dist
var assets embed.FS

// Handler serves assets and application routes from the same origin as the API.
func Handler() http.Handler {
	root, _ := fs.Sub(assets, "dist")
	files := http.FileServerFS(root)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			files.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		data, err := fs.ReadFile(root, "index.html")
		if err != nil {
			http.Error(w, "administration assets are unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.Method != http.MethodHead {
			_, _ = w.Write(data)
		}
	})
}
