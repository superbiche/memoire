// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// Package ui embeds the built review SPA (services/ui, Vue 3 + Vite) and
// serves it at /ui/. The dist/ directory is produced by `npm run build` in
// services/ui (wired through `make ui`); only a .gitkeep is committed so the
// package always compiles — before a UI build, /ui/ simply returns 404.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// Handler serves the embedded SPA under the /ui/ prefix. Real files (hashed
// assets) are served as-is; anything else falls back to index.html so
// client-side navigation deep-links keep working.
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic(err) // embed guarantees dist exists at compile time
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.StripPrefix("/ui", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path != "" && path != "index.html" {
			if info, statErr := fs.Stat(sub, path); statErr == nil && !info.IsDir() {
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		// The entry document must never be cached: asset names are hashed and
		// change on every rebuild.
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, sub, "index.html")
	}))
}
