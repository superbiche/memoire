// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package purge

// Cold-archive oracle: purge over packed raw/ custody must be
// indistinguishable from purge over the same corpus kept flat — same
// matches, same deletions, same quarantined bytes, same surviving corpus —
// and no purged byte may survive inside a rewritten pack.

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"memoire/services/api/internal/rawstore"
)

// Drainer-shaped names for the fixture's raw files (parts only ever hold
// 13-digit-millisecond names of their own UTC day): e1, e2 on 2026-10-01,
// garbled, keep on 2026-10-02.
var datedRaw = map[string]string{
	"e1.json":      "1790812800001-http-e1.json",
	"e2.json":      "1790812800002-http-e2.json",
	"garbled.json": "1790899200001-http-garbled.json",
	"keep.json":    "1790899200002-http-keep.json",
}

// dateFixtureRaw renames the fixture's raw files to drainer-shaped names, so
// flat and packed runs compare the same corpus.
func dateFixtureRaw(t *testing.T, rawDir string) {
	t.Helper()
	for from, to := range datedRaw {
		if err := os.Rename(filepath.Join(rawDir, from), filepath.Join(rawDir, to)); err != nil {
			t.Fatal(err)
		}
	}
}

// packFixtureRaw dates the fixture's raw files and moves them into two
// day parts, leaving e2 flat between them: {e1} | e2 (flat) | {garbled, keep}.
func packFixtureRaw(t *testing.T, rawDir string) {
	t.Helper()
	dateFixtureRaw(t, rawDir)
	for pack, names := range map[string][]string{
		"pack-2026-10-01.000.tar.zst": {datedRaw["e1.json"]},
		"pack-2026-10-02.000.tar.zst": {datedRaw["garbled.json"], datedRaw["keep.json"]},
	} {
		if _, err := rawstore.WritePart(rawDir, pack, names); err != nil {
			t.Fatal(err)
		}
		for _, n := range names {
			if err := os.Remove(filepath.Join(rawDir, n)); err != nil {
				t.Fatal(err)
			}
		}
	}
}

type corpusEntry struct{ name, body string }

func corpus(t *testing.T, rawDir string) []corpusEntry {
	t.Helper()
	var out []corpusEntry
	if err := rawstore.Walk(rawDir, func(e rawstore.Entry, body []byte) error {
		out = append(out, corpusEntry{e.Name, string(body)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// quarantineSet decrypts the quarantine archive and returns
// "<basename>\x00<bytes>" for each member, sorted: the origin prefix
// (raw/ vs raw-pack/<pack>/) is location, not content.
func quarantineSet(t *testing.T, archive, identity string) []string {
	t.Helper()
	out, err := exec.Command("age", "-d", "-i", identity, archive).Output()
	if err != nil {
		t.Fatalf("age decrypt: %v", err)
	}
	tr := tar.NewReader(bytes.NewReader(out))
	var set []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		set = append(set, filepath.Base(hdr.Name)+"\x00"+string(b))
	}
	sort.Strings(set)
	return set
}

// countsOnly zeroes the fields that legitimately differ between a flat and
// a packed run: archive sizes, paths and DB byte counts.
func countsOnly(r Report) Report {
	r.QuarantineOut = ""
	r.QuarantineBytes = 0
	r.DBBytesBefore, r.DBBytesAfter = 0, 0
	r.RawPacksRewritten = 0
	return r
}

func assertNoPurgedBytesInPacks(t *testing.T, rawDir, needle string) {
	t.Helper()
	entries, err := os.ReadDir(rawDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !rawstore.IsPack(e.Name()) {
			continue
		}
		members, err := rawstore.Members(filepath.Join(rawDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range members {
			if bytes.Contains(m.Body, []byte(needle)) {
				t.Fatalf("pack %s still holds purged bytes in %s", e.Name(), m.Name)
			}
		}
	}
}

func TestPackedPurgeEquivalentToFlat(t *testing.T) {
	requireAge(t)
	for _, tc := range []struct {
		name     string
		selector Selector
		needle   string
		rewrites int
	}{
		{"project", Selector{Project: "purgeme"}, `"purgeme"`, 1},
		{"event-ids", Selector{EventIDs: []int64{1}}, `"k1"`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pub, identity := ageKeypair(t)
			run := func(packed bool) (Report, fixture) {
				fx := newFixture(t)
				if packed {
					packFixtureRaw(t, fx.opts.RawDir)
				} else {
					dateFixtureRaw(t, fx.opts.RawDir)
				}
				fx.opts.Selector = tc.selector
				fx.opts.Apply = true
				fx.opts.AgeRecipient = pub
				fx.opts.QuarantineOut = filepath.Join(fx.home, "q.tar.age")
				report, err := Run(context.Background(), fx.opts)
				if err != nil {
					t.Fatalf("packed=%v: %v", packed, err)
				}
				return report, fx
			}
			flatReport, flatFx := run(false)
			packReport, packFx := run(true)

			if got, want := countsOnly(packReport), countsOnly(flatReport); got != want {
				t.Fatalf("report differs:\npacked %+v\n  flat %+v", got, want)
			}
			if packReport.RawPacksRewritten != tc.rewrites {
				t.Fatalf("packs rewritten=%d, want %d", packReport.RawPacksRewritten, tc.rewrites)
			}
			if got, want := quarantineSet(t, packFx.opts.QuarantineOut, identity), quarantineSet(t, flatFx.opts.QuarantineOut, identity); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("quarantine differs:\npacked %q\n  flat %q", got, want)
			}
			gotCorpus, wantCorpus := corpus(t, packFx.opts.RawDir), corpus(t, flatFx.opts.RawDir)
			if len(gotCorpus) != len(wantCorpus) {
				t.Fatalf("surviving corpus: packed %v, flat %v", gotCorpus, wantCorpus)
			}
			for i := range gotCorpus {
				if gotCorpus[i] != wantCorpus[i] {
					t.Fatalf("surviving corpus[%d]: packed %v, flat %v", i, gotCorpus[i], wantCorpus[i])
				}
			}
			assertNoPurgedBytesInPacks(t, packFx.opts.RawDir, tc.needle)

			// Convergence: a repeat apply over the packed store is zero-work.
			packFx.opts.NoQuarantine = true
			packFx.opts.QuarantineOut = ""
			second, err := Run(context.Background(), packFx.opts)
			if err != nil {
				t.Fatal(err)
			}
			if second.RawFilesMatched != 0 || second.RawPacksRewritten != 0 || second.Generation != packReport.Generation {
				t.Fatalf("repeat apply not convergent: %+v", second)
			}
		})
	}
}

func TestPackedDryRunMatchesFlatAndWritesNothing(t *testing.T) {
	flatFx, packFx := newFixture(t), newFixture(t)
	dateFixtureRaw(t, flatFx.opts.RawDir)
	packFixtureRaw(t, packFx.opts.RawDir)
	before := corpus(t, packFx.opts.RawDir)
	for _, fx := range []*fixture{&flatFx, &packFx} {
		fx.opts.Selector = Selector{Project: "purgeme"}
	}
	flatReport, err := Run(context.Background(), flatFx.opts)
	if err != nil {
		t.Fatal(err)
	}
	packReport, err := Run(context.Background(), packFx.opts)
	if err != nil {
		t.Fatal(err)
	}
	if countsOnly(packReport) != countsOnly(flatReport) {
		t.Fatalf("dry-run differs:\npacked %+v\n  flat %+v", packReport, flatReport)
	}
	after := corpus(t, packFx.opts.RawDir)
	if len(after) != len(before) {
		t.Fatal("dry-run changed the packed corpus")
	}
}

// Crash after the DB commit: a committed intent listing packed members is
// finished by ResumeIntent (pack rewritten without them), and a second
// resume-style rewrite of an already-clean pack is a no-op.
func TestCommittedResumeRewritesPacks(t *testing.T) {
	fx := newFixture(t)
	packFixtureRaw(t, fx.opts.RawDir)
	intent := intentJournal{
		CreatedAtEpoch: 1,
		Phase:          phaseCommitted,
		Selector:       "project",
		Files: FileMatches{
			Raw:       []string{datedRaw["e2.json"]},
			RawPacked: map[string][]string{"pack-2026-10-01.000.tar.zst": {datedRaw["e1.json"]}},
		},
	}
	if err := writeIntent(fx.opts.IntentPath, intent); err != nil {
		t.Fatal(err)
	}
	report, err := ResumeIntent(context.Background(), fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	if report.RawFilesDeleted != 2 || report.RawPacksRewritten != 1 {
		t.Fatalf("resume: raw deleted=%d packs rewritten=%d, want 2/1", report.RawFilesDeleted, report.RawPacksRewritten)
	}
	for _, c := range corpus(t, fx.opts.RawDir) {
		if c.name == datedRaw["e1.json"] || c.name == datedRaw["e2.json"] {
			t.Fatalf("%s survived the resumed purge", c.name)
		}
	}
	if _, err := os.Stat(filepath.Join(fx.opts.RawDir, "pack-2026-10-01.000.tar.zst")); !os.IsNotExist(err) {
		t.Fatalf("a pack emptied by purge should be removed: %v", err)
	}
	if _, err := os.Stat(fx.opts.IntentPath); err == nil {
		t.Fatal("resume must remove the intent journal")
	}
}

// Purge and the packer share the raw lock: a held lock makes purge refuse
// before any destructive step.
func TestApplyRefusesWhileRawLockHeld(t *testing.T) {
	fx := newFixture(t)
	unlock, err := rawstore.Lock(fx.opts.RawDir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	fx.opts.Selector = Selector{Project: "purgeme"}
	fx.opts.Apply = true
	fx.opts.NoQuarantine = true
	if _, err := Run(context.Background(), fx.opts); err == nil {
		t.Fatal("apply must refuse while the raw lock is held")
	}
	if got := countRows(t, fx.opts.DBPath, `SELECT count(*) FROM events`); got != 3 {
		t.Fatalf("refused apply mutated the store: %d events", got)
	}
}

// datedEnvelope writes a drainer-named raw file (closed UTC day 2026-10-01)
// carrying the purge fixture's project and event identity.
func datedEnvelope(t *testing.T, rawDir, project, sourceID string) string {
	t.Helper()
	name := "1790812800123-http-" + strings.Repeat("a", 64) + ".json"
	body := `{"project":"` + project + `","source_adapter":"claude-code","source_event_id":"` + sourceID + `","text":"purgeme dated secret"}`
	if err := os.WriteFile(filepath.Join(rawDir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}

// Review R2: a journaled flat file is relocated into a part between the
// DB commit and the resume (a packer run that did not honour the purge
// interlock). The resume must still destroy it, wherever it now lives.
func TestResumeDestroysJournaledFlatRelocatedIntoPart(t *testing.T) {
	fx := newFixture(t)
	name := datedEnvelope(t, fx.opts.RawDir, "purgeme", "k2")
	intent := intentJournal{
		CreatedAtEpoch: 1,
		Phase:          phaseCommitted,
		Selector:       "project",
		Files:          FileMatches{Raw: []string{name}},
	}
	if err := writeIntent(fx.opts.IntentPath, intent); err != nil {
		t.Fatal(err)
	}
	// The interlock: the packer refuses while the intent exists.
	popts := rawstore.PackOptions{Now: time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC), GraceDays: 2, Apply: true}
	guarded := popts
	guarded.BlockingFiles = []string{fx.opts.IntentPath}
	if _, err := rawstore.PackClosedDays(fx.opts.RawDir, guarded); err == nil {
		t.Fatal("packer must refuse while the purge intent exists")
	}
	// The default (nil BlockingFiles) also honours <home>/purge-intent.json.
	if _, err := rawstore.PackClosedDays(fx.opts.RawDir, popts); err == nil {
		t.Fatal("the default custody interlock must refuse while the purge intent exists")
	}
	// Without any interlock (older binary, manual run), the file moves.
	unguarded := popts
	unguarded.BlockingFiles = []string{}
	if rep, err := rawstore.PackClosedDays(fx.opts.RawDir, unguarded); err != nil || rep.PartsWritten != 1 {
		t.Fatalf("relocation: %+v %v", rep, err)
	}
	if _, err := os.Stat(filepath.Join(fx.opts.RawDir, name)); !os.IsNotExist(err) {
		t.Fatalf("precondition: the journaled flat must now live in a part: %v", err)
	}
	report, err := ResumeIntent(context.Background(), fx.opts)
	if err != nil {
		t.Fatal(err)
	}
	if report.RawPacksRewritten != 1 {
		t.Fatalf("resume rewrote %d parts, want 1", report.RawPacksRewritten)
	}
	for _, c := range corpus(t, fx.opts.RawDir) {
		if c.name == name {
			t.Fatal("the relocated journaled envelope survived the resume")
		}
	}
	assertNoPurgedBytesInPacks(t, fx.opts.RawDir, "purgeme dated secret")
}

// Review R3: an abandoned part temp holding the purged bytes must not
// survive a purge apply.
func TestApplyDiscardsStaleTempsHoldingPurgedBytes(t *testing.T) {
	fx := newFixture(t)
	for _, d := range []string{rawstore.TempDir(fx.opts.RawDir), rawstore.IncomingDir(fx.opts.RawDir)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "pack-2026-10-01.000.tar.zst.tmp"), []byte(`{"project":"purgeme","text":"purgeme secret alpha"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fx.opts.Selector = Selector{Project: "purgeme"}
	fx.opts.Apply = true
	fx.opts.NoQuarantine = true
	if _, err := Run(context.Background(), fx.opts); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{rawstore.TempDir(fx.opts.RawDir), rawstore.IncomingDir(fx.opts.RawDir)} {
		if entries, _ := os.ReadDir(d); len(entries) != 0 {
			t.Fatalf("%s still holds %d stale temp(s) after the purge", d, len(entries))
		}
	}
}
