// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// Package buildinfo carries the build-time identity of the binary.
package buildinfo

import "regexp"

// Version is stamped at build time via
//
//	-ldflags "-X memoire/services/api/internal/buildinfo.Version=<version>"
//
// from the repo's VERSION file (MISSING #7: the binary reports what it was
// built from, never a literal in the source). An unstamped build — plain
// `go run` / `go test` — reports "dev": honest about not knowing rather
// than confidently wrong.
var Version = "dev"

// publicVersion bounds what a stamp may put on the UNAUTHENTICATED health
// surface (D224: no unvalidated operator string reaches the wire — a stamp
// is a build input, not a trusted value; review 2026-08-03 F1). The
// grammar is a version SHAPE, not just a charset: "dev", or a dotted
// numeric core with an optional bounded pre-release/build suffix
// (0.6.0, 0.6.0-dev.gb3398a1, 1.0.0+build.5). A bare alphanumeric token —
// the secret-shaped class the charset-only round-1 guard still passed
// (round-2 F1: an AWS-key-shaped stamp reached the wire) — does not parse
// as a version.
var publicVersion = regexp.MustCompile(`^dev$|^[0-9]{1,5}\.[0-9]{1,5}\.[0-9]{1,5}([-+][0-9A-Za-z.-]{1,48})?$`)

// Public returns the stamped version when it fits the closed public shape,
// and collapses anything else to "dev" — a malformed or hostile stamp must
// fail safe, never reach the unauthenticated body verbatim.
func Public() string {
	if publicVersion.MatchString(Version) {
		return Version
	}
	return "dev"
}
