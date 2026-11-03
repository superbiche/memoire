// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package sweep

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"memoire/services/api/internal/redact"
)

// TestSweepRefusesDepthError: a raw file the scrub cannot walk (nesting
// beyond the limit) fails the run instead of being copied unredacted
// (review round 1, F7).
func TestSweepRefusesDepthError(t *testing.T) {
	fx := newSweepFixture(t)
	deep := []byte(`{"schema_version":1,"project":"memoire-test","payload":{"d":` + strings.Repeat("[", redact.MaxNesting+1) + strings.Repeat("]", redact.MaxNesting+1) + `}}`)
	writeRawFixture(t, fx, "1700000000002-deep-cccccccc.json", deep)
	_, err := Run(context.Background(), fx.options())
	if !errors.Is(err, redact.ErrDepth) {
		t.Fatalf("want ErrDepth, got %v", err)
	}
	if _, serr := os.Stat(filepath.Join(fx.outputDir, "1700000000002-deep-cccccccc.json")); serr == nil {
		t.Fatalf("the refused file must not reach the output generation")
	}
}
