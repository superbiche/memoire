// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package sweep

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"memoire/services/api/internal/rawstore"
)

func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(b)
	}
	return out
}

// Cold-archive oracle: a sweep over packed raw/ custody produces the same
// redacted generation, quarantine and report as over the same files flat.
func TestSweepPackedEquivalentToFlat(t *testing.T) {
	run := func(packed bool) (*sweepFixture, Report) {
		fx := newSweepFixture(t)
		files := map[string][]byte{
			"1790812800001-http-a.json": cleanEnvelope(),
			"1790812800002-http-b.json": hitEnvelope(),
			"1790899200001-http-c.json": secondHitEnvelope(),
			"1790985600001-http-d.json": secondCleanEnvelope(),
		}
		names := make([]string, 0, len(files))
		for name, body := range files {
			writeRawFixture(t, fx, name, body)
			names = append(names, name)
		}
		sort.Strings(names)
		if packed {
			// Two packs and one file left flat between them.
			for pack, members := range map[string][]string{
				"pack-2026-10-01.000.tar.zst": names[:2],
				"pack-2026-10-03.000.tar.zst": names[3:],
			} {
				if _, err := rawstore.WritePart(fx.rawDir, pack, members); err != nil {
					t.Fatal(err)
				}
				for _, n := range members {
					if err := os.Remove(filepath.Join(fx.rawDir, n)); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
		report, err := Run(context.Background(), fx.options())
		if err != nil {
			t.Fatalf("packed=%v: %v", packed, err)
		}
		return fx, report
	}
	flatFx, flatReport := run(false)
	packFx, packReport := run(true)

	flatReport.RawDir, flatReport.OutputDir, flatReport.QuarantineOut = "", "", ""
	packReport.RawDir, packReport.OutputDir, packReport.QuarantineOut = "", "", ""
	if packReport.FilesScanned != 4 || packReport.FilesScanned != flatReport.FilesScanned ||
		packReport.FilesWithHits != flatReport.FilesWithHits ||
		packReport.TotalInputBytes != flatReport.TotalInputBytes ||
		packReport.TotalOutputBytes != flatReport.TotalOutputBytes ||
		packReport.Reconciliation != flatReport.Reconciliation {
		t.Fatalf("report differs:\npacked %+v\n  flat %+v", packReport, flatReport)
	}
	gotOut, wantOut := readTree(t, packFx.outputDir), readTree(t, flatFx.outputDir)
	if len(gotOut) != len(wantOut) {
		t.Fatalf("redacted generation: packed %d files, flat %d", len(gotOut), len(wantOut))
	}
	for name, body := range wantOut {
		if gotOut[name] != body {
			t.Fatalf("redacted %s differs between packed and flat input", name)
		}
	}
	gotQ, wantQ := readTree(t, decryptQuarantine(t, packFx)), readTree(t, decryptQuarantine(t, flatFx))
	if len(gotQ) != 4 || len(gotQ) != len(wantQ) {
		t.Fatalf("quarantine: packed %d members, flat %d", len(gotQ), len(wantQ))
	}
	for name, body := range wantQ {
		if gotQ[name] != body {
			t.Fatalf("quarantined original %s differs", name)
		}
	}
}

// Custody oracle C2 for the retro-sweep: it refuses while the custody lock
// is held, before writing any output.
func TestSweepEntersCustody(t *testing.T) {
	fx := newSweepFixture(t)
	writeRawFixture(t, fx, "1790812800001-http-a.json", cleanEnvelope())
	unlock, err := rawstore.Lock(fx.rawDir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := Run(context.Background(), fx.options()); err == nil || !strings.Contains(err.Error(), "custody lock") {
		t.Fatalf("sweep with the lock held: %v", err)
	}
	if entries, _ := os.ReadDir(fx.outputDir); len(entries) != 0 {
		t.Fatal("a refused sweep wrote output")
	}
}
