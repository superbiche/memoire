// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import tailwindcss from '@tailwindcss/vite'

// Base is /ui/ — the SPA is embedded into the memoire-api binary and served
// at /ui/ (see services/api/internal/ui). API calls stay relative (/v1/...).
export default defineConfig({
  base: '/ui/',
  plugins: [vue(), tailwindcss()],
  build: {
    // Emit straight into the Go embed directory so `go build` picks it up.
    outDir: '../api/internal/ui/dist',
    emptyOutDir: true,
  },
  server: {
    proxy: {
      '/v1': {
        target: process.env.MEMOIRE_DEV_PROXY || 'http://127.0.0.1:37778',
        // Dev convenience: export MEMOIRE_DEV_TOKEN to have the proxy inject
        // the bearer server-side (any placeholder unlocks the token gate then).
        ...(process.env.MEMOIRE_DEV_TOKEN
          ? { headers: { authorization: `Bearer ${process.env.MEMOIRE_DEV_TOKEN}` } }
          : {}),
      },
    },
  },
})
