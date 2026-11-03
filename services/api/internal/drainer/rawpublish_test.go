// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package drainer

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"memoire/services/api/internal/rawstore"
)

// Cold-archive review R4: a closed-day item drained while the packer runs
// must never be visible in raw/ before its copy is complete, so the packer
// cannot pack a prefix and delete the final file.
func TestRawPublishIsAtomicAgainstThePacker(t *testing.T) {
	cfg := testConfig(t)
	d := New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Old queue-write timestamp: the name lands in an already-closed day.
	name := "1790812800777-http-" + strings.Repeat("b", 64) + ".json"
	body := bytes.Repeat([]byte("x"), 256<<10)
	src := filepath.Join(cfg.Queue.Inflight, name)
	if err := os.WriteFile(src, body, 0o600); err != nil {
		t.Fatal(err)
	}
	packNow := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	paused := false
	orig := beforeRawPublish
	t.Cleanup(func() { beforeRawPublish = orig })
	beforeRawPublish = func() {
		paused = true
		if _, err := os.Stat(filepath.Join(cfg.Raw, name)); !os.IsNotExist(err) {
			t.Errorf("raw/%s visible before the copy is published: %v", name, err)
		}
		rep, err := rawstore.PackClosedDays(cfg.Raw, rawstore.PackOptions{Now: packNow, GraceDays: 2, Apply: true})
		if err != nil {
			t.Errorf("packer during the paused copy: %v", err)
		}
		if rep.FlatFilesPacked != 0 || rep.PartsWritten != 0 {
			t.Errorf("packer saw the unpublished copy: %+v", rep)
		}
	}
	if err := d.copyRaw(src, name); err != nil {
		t.Fatal(err)
	}
	if !paused {
		t.Fatal("publish seam never ran")
	}
	got, err := os.ReadFile(filepath.Join(cfg.Raw, name))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("published raw copy incomplete or missing: %d bytes, err=%v", len(got), err)
	}
	if entries, _ := os.ReadDir(rawstore.IncomingDir(cfg.Raw)); len(entries) != 0 {
		t.Fatalf("staging copy left behind: %d entries", len(entries))
	}
	// The next packer run packs the complete file.
	beforeRawPublish = orig
	rep, err := rawstore.PackClosedDays(cfg.Raw, rawstore.PackOptions{Now: packNow, GraceDays: 2, Apply: true})
	if err != nil || rep.PartsWritten != 1 {
		t.Fatalf("packer after publish: %+v %v", rep, err)
	}
	var packed []byte
	if err := rawstore.Walk(cfg.Raw, func(e rawstore.Entry, b []byte) error {
		if e.Name == name {
			packed = b
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(packed, body) {
		t.Fatalf("packed copy differs from the queue item: %d bytes", len(packed))
	}
}

// First copy wins, as with O_EXCL: an existing raw file is never replaced.
func TestRawPublishKeepsExistingCopy(t *testing.T) {
	cfg := testConfig(t)
	d := New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	name := "1790812800778-http-c.json"
	if err := os.WriteFile(filepath.Join(cfg.Raw, name), []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(cfg.Queue.Inflight, name)
	if err := os.WriteFile(src, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.copyRaw(src, name); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(cfg.Raw, name)); string(got) != "first" {
		t.Fatalf("existing raw copy replaced: %q", got)
	}
	if entries, _ := os.ReadDir(rawstore.IncomingDir(cfg.Raw)); len(entries) != 0 {
		t.Fatalf("staging copy left behind: %d entries", len(entries))
	}
}
