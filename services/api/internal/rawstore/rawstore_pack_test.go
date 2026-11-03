// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package rawstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

var packNow = time.Date(2026, 10, 5, 3, 0, 0, 0, time.UTC)

func packApply(t *testing.T, dir string, mutate ...func(*PackOptions)) PackReport {
	t.Helper()
	opts := PackOptions{Now: packNow, GraceDays: 2, Apply: true}
	for _, m := range mutate {
		m(&opts)
	}
	rep, err := PackClosedDays(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func fileSum(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

// Oracle: packing closed days changes WHERE bytes live, never WHAT a reader
// sees — same names, same bytes, same mtimes, same global name order.
func TestPackClosedDaysIsInvisibleToWalk(t *testing.T) {
	dir := t.TempDir()
	fixtureCorpus(t, dir)
	before := walkAll(t, dir)
	if len(before) != 7 {
		t.Fatalf("flat walk yielded %d envelopes, want 7", len(before))
	}

	dry, err := PackClosedDays(dir, PackOptions{Now: packNow, GraceDays: 2})
	if err != nil {
		t.Fatal(err)
	}
	if dry.PartsWritten != 0 || dry.FlatFilesDeleted != 0 || dry.FlatFilesPacked != 5 {
		t.Fatalf("dry-run report %+v, want 5 packable files and no writes", dry)
	}
	equalWalks(t, walkAll(t, dir), before)

	rep := packApply(t, dir)
	if rep.PartsWritten != 2 || rep.FlatFilesPacked != 5 || rep.FlatFilesDeleted != 5 || rep.UndatedFiles != 1 || rep.OpenDayFiles != 1 {
		t.Fatalf("apply report %+v", rep)
	}
	want := []string{
		rawName(dayMS(t, "2026-10-03")+9, 6),
		"legacy-claude-0001.json",
		"notes.txt",
		"pack-2026-10-01.000.tar.zst",
		"pack-2026-10-02.000.tar.zst",
	}
	sort.Strings(want)
	if got := dirNames(t, dir); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("raw dir after pack:\n got %v\nwant %v", got, want)
	}
	after := walkAll(t, dir)
	equalWalks(t, strip(after), strip(before))
	stillFlat := map[string]bool{rawName(dayMS(t, "2026-10-03")+9, 6): true, "legacy-claude-0001.json": true}
	for _, w := range after {
		if packed := w.pack != ""; packed == stillFlat[w.name] {
			t.Fatalf("%s read from pack %q, want packed=%v", w.name, w.pack, !stillFlat[w.name])
		}
	}
	if again := packApply(t, dir); again.PartsWritten != 0 || again.FlatFilesPacked != 0 {
		t.Fatalf("second run not idempotent: %+v", again)
	}
}

// R1: a late file for an already-packed day goes into a NEW part; the
// published part stays byte-identical, so a by-name custody consumer (the
// snapshot) never holds a stale revision of a name.
func TestLateFileGoesToNewImmutablePart(t *testing.T) {
	dir := t.TempDir()
	fixtureCorpus(t, dir)
	packApply(t, dir)
	part0 := filepath.Join(dir, "pack-2026-10-01.000.tar.zst")
	sum0 := fileSum(t, part0)
	// Sorts BETWEEN existing members of the day: the merge must interleave.
	late := flat{rawName(dayMS(t, "2026-10-01")+6, 99), `{"late":true}`, time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)}
	writeFlat(t, dir, late)
	before := walkAll(t, dir)

	rep := packApply(t, dir)
	if rep.PartsWritten != 1 || rep.FlatFilesPacked != 1 || rep.FlatFilesDeleted != 1 {
		t.Fatalf("late pack report %+v", rep)
	}
	if fileSum(t, part0) != sum0 {
		t.Fatal("an existing part was rewritten: parts must be immutable")
	}
	if _, err := os.Stat(filepath.Join(dir, "pack-2026-10-01.001.tar.zst")); err != nil {
		t.Fatalf("late file must land in part .001: %v", err)
	}
	after := walkAll(t, dir)
	equalWalks(t, strip(after), strip(before))
	for _, w := range after {
		if w.name == late.name && w.pack != "pack-2026-10-01.001.tar.zst" {
			t.Fatalf("late file read from %q", w.pack)
		}
	}
}

// Crash window: a part was published but the flat copies were not deleted.
// Walk yields each name once; the next run only deletes the copies.
func TestCrashAfterPublishBeforeDelete(t *testing.T) {
	dir := t.TempDir()
	fixtureCorpus(t, dir)
	before := walkAll(t, dir)
	d1 := dayMS(t, "2026-10-01")
	names := []string{rawName(d1+5, 1), rawName(d1+7, 3), rawName(d1+86_399_999, 2)}
	if n, err := WritePart(dir, PartName(time.UnixMilli(d1), 0), names); err != nil || n != 3 {
		t.Fatalf("write part: %d %v", n, err)
	}
	equalWalks(t, strip(walkAll(t, dir)), strip(before))

	rep := packApply(t, dir)
	if rep.PartsWritten != 1 || rep.AlreadyPackedFlats != 3 || rep.FlatFilesDeleted != 5 {
		t.Fatalf("recovery report %+v, want day-1 copies deleted only, day 2 packed", rep)
	}
	equalWalks(t, strip(walkAll(t, dir)), strip(before))
}

// A flat file whose name is packed with DIFFERENT bytes is a custody
// conflict: the packer and every reader refuse instead of choosing.
func TestConflictingFlatAndPackedBytesRefuse(t *testing.T) {
	dir := t.TempDir()
	d1 := dayMS(t, "2026-10-01")
	name := rawName(d1+1, 1)
	writeFlat(t, dir, flat{name: name, body: `{"v":1}`})
	if _, err := WritePart(dir, PartName(time.UnixMilli(d1), 0), []string{name}); err != nil {
		t.Fatal(err)
	}
	writeFlat(t, dir, flat{name: name, body: `{"v":2}`})
	if _, err := PackClosedDays(dir, PackOptions{Now: packNow, GraceDays: 2, Apply: true}); err == nil {
		t.Fatal("conflicting bytes must refuse")
	}
	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		t.Fatalf("refused run must keep the flat file: %v", err)
	}
	if err := Walk(dir, func(Entry, []byte) error { return nil }); err == nil {
		t.Fatal("walk must refuse a name present twice with different bytes")
	}
}

// Parts are immutable: WritePart refuses an existing name.
func TestWritePartRefusesExistingPart(t *testing.T) {
	dir := t.TempDir()
	d1 := dayMS(t, "2026-10-01")
	a := rawName(d1+1, 1)
	writeFlat(t, dir, flat{name: a, body: "{}"})
	part := PartName(time.UnixMilli(d1), 0)
	if _, err := WritePart(dir, part, []string{a}); err != nil {
		t.Fatal(err)
	}
	if _, err := WritePart(dir, part, []string{a}); err == nil {
		t.Fatal("WritePart must refuse to replace an existing part")
	}
}

// Purge's lever: dropping members rewrites the part without them; the
// removed bytes are nowhere in the part or the temp dir; a repeat is a
// no-op; an emptied part is removed.
func TestDropMembersRemovesBytes(t *testing.T) {
	dir := t.TempDir()
	fixtureCorpus(t, dir)
	packApply(t, dir)
	d2 := dayMS(t, "2026-10-02")
	part := PartName(time.UnixMilli(d2), 0)
	victim := rawName(d2+1, 4)
	res, err := DropMembers(dir, part, map[string]bool{victim: true, "absent.json": true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Written || res.Dropped != 1 || res.Members != 1 {
		t.Fatalf("drop result %+v", res)
	}
	members, err := Members(filepath.Join(dir, part))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if m.Name == victim || bytes.Contains(m.Body, []byte(`"n":4`)) {
			t.Fatal("dropped bytes survive in the rewritten part")
		}
	}
	if entries, _ := os.ReadDir(TempDir(dir)); len(entries) != 0 {
		t.Fatalf("temp dir must be empty after a rewrite, has %d entries", len(entries))
	}
	if again, err := DropMembers(dir, part, map[string]bool{victim: true}); err != nil || again.Written || again.Dropped != 0 {
		t.Fatalf("repeat drop must be a no-op: %+v %v", again, err)
	}
	if res, err := DropMembers(dir, part, map[string]bool{rawName(d2+2, 5): true}); err != nil || res.Members != 0 {
		t.Fatalf("drop last member: %+v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(dir, part)); !os.IsNotExist(err) {
		t.Fatalf("empty part should be removed: %v", err)
	}
}

// R3: part temps live beside raw/ (never in the raw/ inventory), are
// invisible to readers, and are discarded by the packer and by
// RemoveStaleTemps (purge).
func TestTempsLiveOutsideRawAndAreDiscarded(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "raw")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sep := string(os.PathSeparator)
	if strings.HasPrefix(TempDir(dir), dir+sep) || strings.HasPrefix(IncomingDir(dir), dir+sep) {
		t.Fatal("temp dirs must live outside raw/")
	}
	fixtureCorpus(t, dir)
	before := walkAll(t, dir)
	for _, d := range []string{TempDir(dir), IncomingDir(dir)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "stale.tmp"), []byte(`{"secret":"x"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	equalWalks(t, walkAll(t, dir), before)
	rep := packApply(t, dir)
	if rep.StaleTempRemoved != 1 {
		t.Fatalf("packer must discard its stale temps, removed %d", rep.StaleTempRemoved)
	}
	n, err := RemoveStaleTemps(dir)
	if err != nil || n != 1 {
		t.Fatalf("RemoveStaleTemps removed %d (want the incoming leftover) err=%v", n, err)
	}
	for _, d := range []string{TempDir(dir), IncomingDir(dir)} {
		if entries, _ := os.ReadDir(d); len(entries) != 0 {
			t.Fatalf("%s not empty", d)
		}
	}
}

// R2: an unfinished purge intent blocks the packer before it touches raw/.
func TestPackerRefusesWhilePurgeIntentExists(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "raw")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fixtureCorpus(t, dir)
	intent := filepath.Join(root, "purge-intent.json")
	if err := os.WriteFile(intent, []byte(`{"phase":"committed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PackClosedDays(dir, PackOptions{Now: packNow, GraceDays: 2, Apply: true, BlockingFiles: []string{intent}}); err == nil {
		t.Fatal("packer must refuse while a purge intent exists")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 8 {
		t.Fatalf("refused run touched raw/: %d entries", len(entries))
	}
}

// R8: a day is split into bounded parts; MaxParts stops between parts and a
// later run continues with the next sequence.
func TestDaySplitsIntoBoundedParts(t *testing.T) {
	dir := t.TempDir()
	d1 := dayMS(t, "2026-10-01")
	for i := 0; i < 5; i++ {
		writeFlat(t, dir, flat{name: rawName(d1+int64(i), i), body: fmt.Sprintf(`{"i":%d}`, i)})
	}
	before := walkAll(t, dir)
	rep := packApply(t, dir, func(o *PackOptions) { o.MaxFilesPerPart = 2; o.MaxParts = 2 })
	if rep.PartsWritten != 2 || rep.FlatFilesDeleted != 4 || !rep.Stopped {
		t.Fatalf("bounded run %+v, want 2 parts, 4 files, stopped", rep)
	}
	equalWalks(t, strip(walkAll(t, dir)), strip(before))
	rep = packApply(t, dir, func(o *PackOptions) { o.MaxFilesPerPart = 2 })
	if rep.PartsWritten != 1 || rep.FlatFilesDeleted != 1 {
		t.Fatalf("resumed run %+v", rep)
	}
	if got := dirNames(t, dir); strings.Join(got, ",") != "pack-2026-10-01.000.tar.zst,pack-2026-10-01.001.tar.zst,pack-2026-10-01.002.tar.zst" {
		t.Fatalf("parts %v", got)
	}
	equalWalks(t, strip(walkAll(t, dir)), strip(before))
}

// Order guards: a name held by two parts, or a part whose members fall
// before an earlier day's members, makes Walk fail loudly.
func TestWalkRefusesDuplicatesAndOverlaps(t *testing.T) {
	t.Run("same name in two parts", func(t *testing.T) {
		dir := t.TempDir()
		d1 := dayMS(t, "2026-10-01")
		a := rawName(d1+1, 1)
		writeFlat(t, dir, flat{name: a, body: "{}"})
		for seq := 0; seq < 2; seq++ {
			if _, err := WritePart(dir, PartName(time.UnixMilli(d1), seq), []string{a}); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Remove(filepath.Join(dir, a)); err != nil {
			t.Fatal(err)
		}
		if err := Walk(dir, func(Entry, []byte) error { return nil }); err == nil {
			t.Fatal("walk must refuse a name held by two parts")
		}
	})
	t.Run("part outside its day", func(t *testing.T) {
		dir := t.TempDir()
		d1 := dayMS(t, "2026-10-01")
		a, b, c := rawName(d1+1, 1), rawName(d1+3, 2), rawName(d1+2, 3)
		writeFlat(t, dir, flat{name: a, body: "{}"}, flat{name: b, body: "{}"}, flat{name: c, body: "{}"})
		if _, err := WritePart(dir, "pack-2026-10-01.000.tar.zst", []string{a, b}); err != nil {
			t.Fatal(err)
		}
		if _, err := WritePart(dir, "pack-2026-10-02.000.tar.zst", []string{c}); err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{a, b, c} {
			_ = os.Remove(filepath.Join(dir, n))
		}
		if err := Walk(dir, func(Entry, []byte) error { return nil }); err == nil {
			t.Fatal("walk must refuse a part whose members precede an earlier day's")
		}
	})
}

func TestLocateFindsPackedNames(t *testing.T) {
	dir := t.TempDir()
	fixtureCorpus(t, dir)
	packApply(t, dir)
	d1 := dayMS(t, "2026-10-01")
	got, err := Locate(dir, []string{rawName(d1+5, 1), "legacy-claude-0001.json", rawName(d1+4, 77)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || strings.Join(got[rawName(d1+5, 1)], ",") != "pack-2026-10-01.000.tar.zst" {
		t.Fatalf("locate %v", got)
	}
}

func TestLockIsExclusive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "raw")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(dir); err == nil {
		t.Fatal("second lock must fail while the first is held")
	}
	unlock()
	unlock2, err := Lock(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	unlock2()
	if strings.HasPrefix(LockPath(dir), dir+string(os.PathSeparator)) {
		t.Fatal("the lock file must live outside raw/ (snapshot captures every top-level file)")
	}
}

func readGen(t *testing.T, manifest string) int64 {
	t.Helper()
	b, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Generation int64 `json:"generation"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m.Generation
}

// Review N1: the rollback of a part advances the custody generation BEFORE
// the part disappears (so a reused part name forces a snapshot re-baseline),
// restores the members byte- and mtime-identical, and converges on re-run.
func TestUnpackAdvancesGenerationAndRestoresFlats(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "raw")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(root, "custody", "MANIFEST.json")
	if err := os.MkdirAll(filepath.Dir(manifest), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte(`{"generation": 3, "last_purge": {"selector": "project"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fixtureCorpus(t, dir)
	before := walkAll(t, dir)
	packApply(t, dir)
	part := "pack-2026-10-01.000.tar.zst"

	rep, err := Unpack(dir, part, manifest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Generation != 4 || rep.Restored != 3 || !rep.PartRemoved {
		t.Fatalf("unpack report %+v", rep)
	}
	if readGen(t, manifest) != 4 {
		t.Fatal("custody generation not advanced")
	}
	if b, _ := os.ReadFile(manifest); !bytes.Contains(b, []byte(`"last_purge"`)) {
		t.Fatal("unpack must preserve the purge stamp in the manifest")
	}
	if _, err := os.Stat(filepath.Join(dir, part)); !os.IsNotExist(err) {
		t.Fatalf("part must be removed: %v", err)
	}
	equalWalks(t, strip(walkAll(t, dir)), strip(before)) // names, bytes AND whole-second mtimes

	// Re-run after a crash between restore and removal: re-create the part,
	// leave the flats; the second run skips them and only bumps + removes.
	d1 := dayMS(t, "2026-10-01")
	if _, err := WritePart(dir, part, []string{rawName(d1+5, 1), rawName(d1+7, 3), rawName(d1+86_399_999, 2)}); err != nil {
		t.Fatal(err)
	}
	rep, err = Unpack(dir, part, manifest, nil)
	if err != nil || rep.AlreadyFlat != 3 || rep.Restored != 0 || rep.Generation != 5 {
		t.Fatalf("re-run %+v %v", rep, err)
	}
	equalWalks(t, strip(walkAll(t, dir)), strip(before))
}

func TestUnpackRefusesConflictLockAndIntent(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "raw")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(root, "custody", "MANIFEST.json")
	d1 := dayMS(t, "2026-10-01")
	a := rawName(d1+1, 1)
	writeFlat(t, dir, flat{name: a, body: `{"v":1}`})
	part := PartName(time.UnixMilli(d1), 0)
	if _, err := WritePart(dir, part, []string{a}); err != nil {
		t.Fatal(err)
	}
	// Flat copy with different bytes: conflict, the part stays.
	writeFlat(t, dir, flat{name: a, body: `{"v":2}`})
	if _, err := Unpack(dir, part, manifest, nil); err == nil {
		t.Fatal("conflicting flat bytes must refuse")
	}
	if _, err := os.Stat(filepath.Join(dir, part)); err != nil {
		t.Fatal("a refused unpack must keep the part")
	}
	if err := os.Remove(filepath.Join(dir, a)); err != nil {
		t.Fatal(err)
	}
	intent := filepath.Join(root, "purge-intent.json")
	if err := os.WriteFile(intent, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Unpack(dir, part, manifest, []string{intent}); err == nil {
		t.Fatal("unpack must refuse while a purge intent exists")
	}
	_ = os.Remove(intent)
	unlock, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Unpack(dir, part, manifest, nil); err == nil {
		t.Fatal("unpack must refuse while the custody lock is held")
	}
	unlock()
}

// Review N3: an intent journaled after the early check is still seen,
// because the check repeats under the lock.
func TestPackerRechecksIntentUnderLock(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "raw")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fixtureCorpus(t, dir)
	intent := filepath.Join(root, "purge-intent.json")
	if err := os.WriteFile(intent, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkBlocking([]string{intent}); err == nil {
		t.Fatal("checkBlocking must refuse an existing intent")
	}
	if _, err := PackClosedDays(dir, PackOptions{Now: packNow, GraceDays: 2, Apply: true, BlockingFiles: []string{intent}}); err == nil {
		t.Fatal("apply must refuse")
	}
}

func TestDayOfRequiresThirteenDigitMillis(t *testing.T) {
	for name, want := range map[string]bool{
		"1790812800123-http-x.json":  true,
		"179081280012-http-x.json":   false,
		"17908128001230-http-x.json": false,
		"legacy-claude.json":         false,
		"+999999999999-http-x.json":  false, // review N4: a sign is not a digit
		"-999999999999-http-x.json":  false,
		"0100000000000-http-y.json":  true, // zero-padded 13 digits stay valid
		"17908128001a3-http-x.json":  false,
	} {
		if _, ok := DayOf(name); ok != want {
			t.Fatalf("DayOf(%q) ok=%v, want %v", name, ok, want)
		}
	}
}

// Review N7: an existing custody manifest must be exactly one JSON object
// with a non-negative integer generation; anything else refuses and is left
// untouched. Only an absent manifest starts at generation 0.
func TestBumpGenerationRejectsMalformedManifests(t *testing.T) {
	for name, body := range map[string]string{
		"null":          `null`,
		"array":         `[1]`,
		"no generation": `{"updated_at_epoch": 1}`,
		"negative":      `{"generation": -1}`,
		"fraction":      `{"generation": 1.5}`,
		"string":        `{"generation": "3"}`,
		"trailing":      `{"generation": 3} {"generation": 9}`,
		"max int64":     `{"generation": 9223372036854775807}`,
		"overflow":      `{"generation": 9223372036854775808}`,
		"not json":      `{generation: 3}`,
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "MANIFEST.json")
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := BumpGeneration(p, "t"); err == nil {
				t.Fatalf("manifest %s must refuse", body)
			}
			if b, _ := os.ReadFile(p); string(b) != body {
				t.Fatal("a refused bump must leave the manifest untouched")
			}
		})
	}
	absent := filepath.Join(t.TempDir(), "custody", "MANIFEST.json")
	if g, err := BumpGeneration(absent, "t"); err != nil || g != 1 {
		t.Fatalf("absent manifest: %d %v, want generation 1", g, err)
	}
	if g, err := BumpGeneration(absent, "t"); err != nil || g != 2 {
		t.Fatalf("second bump: %d %v", g, err)
	}
}
