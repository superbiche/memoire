// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package triage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"memoire/services/api/internal/db"
	"memoire/services/api/internal/modelderive"
)

// Graduation is the R-B ceremony: a recurring noise genre stops being
// emitted at DERIVATION instead of being lapsed at triage, once the triage
// record says the operator kept none of its rows.
//
// It costs seconds and produces BOTH artifacts in one act — the prompt-genre
// addition and the dated record with its evidence attached — because a
// genre killed without a written reason is a silent capability loss nobody
// can audit later.
//
// It cannot run against a deployed server: the artifacts are repo files, and
// the server's write authority is the store. So graduation is an operator
// gesture at the CLI, and the triage surface renders the exact command.

// ErrFlipSetNonEmpty is the falsifier. A graduation attempt over a genre
// the operator did NOT unanimously lapse REFUSES: graduation destroys
// future rows unseen, so one kept row is one row a graduation would have
// silently thrown away.
type ErrFlipSetNonEmpty struct {
	Genre   string
	FlipSet []int64
}

func (e *ErrFlipSetNonEmpty) Error() string {
	ids := make([]string, 0, len(e.FlipSet))
	for _, id := range e.FlipSet {
		ids = append(ids, strconv.FormatInt(id, 10))
	}
	return fmt.Sprintf("refusing to graduate %q: the operator kept %d row(s) this genre claimed (observations %s). "+
		"Graduation kills the genre at derivation, so those rows would never have been written — "+
		"the genre is not noise, or the slug covers two things",
		e.Genre, len(e.FlipSet), strings.Join(ids, ", "))
}

// ErrBelowThreshold refuses a genre with too little evidence.
type ErrBelowThreshold struct {
	Genre     string
	Judged    int
	Threshold int
}

func (e *ErrBelowThreshold) Error() string {
	return fmt.Sprintf("refusing to graduate %q: %d judged occurrence(s), threshold is %d — "+
		"a genre nobody has ruled on has volume, not evidence",
		e.Genre, e.Judged, e.Threshold)
}

// GraduateOptions configures the gesture.
type GraduateOptions struct {
	Genre string
	// Instruction is the never-emit line rendered into the derivation
	// prompt. Empty falls back to a line naming the slug.
	Instruction string
	// RepoRoot is the code checkout holding the embedded registry source.
	RepoRoot string
	// RecordsDir is where the dated graduation records live. Empty means
	// RepoRoot/records (a single-tree layout); with the code and the
	// decision records in separate repositories it is the ledger's records/.
	RecordsDir string
	// Now is injectable for tests.
	Now time.Time
	// DryRun reports what would land and writes nothing.
	DryRun bool
}

// GraduateResult names the two artifacts.
type GraduateResult struct {
	Genre        string                  `json:"genre"`
	RegistryPath string                  `json:"registry_path"`
	RecordPath   string                  `json:"record_path"`
	Evidence     db.TriageGenreCandidate `json:"evidence"`
	DryRun       bool                    `json:"dry_run"`
}

// GraduateStore is the narrow read surface the gesture needs: evidence,
// and nothing else. Graduation reads and writes repo files; it has no
// business touching a row.
type GraduateStore interface {
	TriageGenreEvidence(ctx context.Context, genre string) (db.TriageGenreCandidate, error)
}

// Graduate runs the falsifiers and, when they pass, writes the registry
// entry and the dated record.
func Graduate(ctx context.Context, store GraduateStore, opts GraduateOptions) (GraduateResult, error) {
	genre := strings.TrimSpace(opts.Genre)
	if genre == "" {
		return GraduateResult{}, fmt.Errorf("triage: graduate requires a genre slug")
	}
	if opts.RepoRoot == "" {
		return GraduateResult{}, fmt.Errorf("triage: graduate requires a repo root (the prompt registry is a repo file)")
	}
	recordsDir := opts.RecordsDir
	if recordsDir == "" {
		recordsDir = filepath.Join(opts.RepoRoot, "records")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	ev, err := store.TriageGenreEvidence(ctx, genre)
	if err != nil {
		return GraduateResult{}, err
	}
	// The falsifier runs BEFORE the threshold: a genre with a flip is
	// refused no matter how much evidence it has, because volume never
	// outvotes a row the operator kept.
	if len(ev.FlipSet) > 0 {
		return GraduateResult{}, &ErrFlipSetNonEmpty{Genre: genre, FlipSet: ev.FlipSet}
	}
	if ev.Judged < db.TriageGraduationThreshold {
		return GraduateResult{}, &ErrBelowThreshold{Genre: genre, Judged: int(ev.Judged), Threshold: db.TriageGraduationThreshold}
	}

	registryPath := filepath.Join(opts.RepoRoot, "services", "api", "internal", "modelderive", "graduated-genres.json")
	raw, err := os.ReadFile(registryPath)
	if err != nil {
		return GraduateResult{}, fmt.Errorf("triage: read graduated-genre registry: %w", err)
	}
	var reg modelderive.GraduatedRegistry
	if err := json.Unmarshal(raw, &reg); err != nil {
		return GraduateResult{}, fmt.Errorf("triage: graduated-genre registry is malformed: %w", err)
	}
	for _, g := range reg.Genres {
		if g.Slug == genre {
			return GraduateResult{}, fmt.Errorf("triage: %q already graduated (%s, record %s)", genre, g.GraduatedOn, g.Record)
		}
	}

	// The slug is validated BEFORE it is used to build a path. The store
	// gate should already have refused a non-conforming genre; a graduation
	// is an operator-run gesture that writes files, so it re-checks rather
	// than trusting that nothing ever wrote around the gate.
	if !db.ValidTriageGenre(genre) {
		return GraduateResult{}, fmt.Errorf("triage: genre %q is not a valid slug (%s) — refusing to build a path from it",
			genre, db.TriageGenreSlug.String())
	}

	recordNum, err := nextRecordNumber(recordsDir)
	if err != nil {
		return GraduateResult{}, err
	}
	recordID := fmt.Sprintf("D%04d", recordNum)
	recordPath := filepath.Join(recordsDir,
		fmt.Sprintf("%s-graduate-noise-genre-%s.md", recordID, genre))

	instruction := strings.TrimSpace(opts.Instruction)
	if instruction == "" {
		instruction = fmt.Sprintf("observations of the %s genre", genre)
	}
	entry := modelderive.GraduatedGenre{
		Slug:        genre,
		Instruction: instruction,
		Record:      "memoire:" + recordID,
		GraduatedOn: now.Format("2006-01-02"),
	}
	entry.Evidence.Occurrences = int(ev.Occurrences)
	entry.Evidence.Judged = int(ev.Judged)
	reg.Genres = append(reg.Genres, entry)
	sort.Slice(reg.Genres, func(i, j int) bool { return reg.Genres[i].Slug < reg.Genres[j].Slug })

	res := GraduateResult{Genre: genre, RegistryPath: registryPath, RecordPath: recordPath, Evidence: ev, DryRun: opts.DryRun}
	if opts.DryRun {
		return res, nil
	}

	out, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return GraduateResult{}, err
	}
	// RECORD FIRST, registry second, and roll the record back if the
	// registry write fails. The two writes cannot be one atomic act, so the
	// ORDER chooses which half-landed state is survivable:
	//
	//   record without registry = evidence with no policy change. Harmless:
	//     a proposed record nobody ratified, and derivation still emits.
	//   registry without record = a genre killed at derivation with no
	//     evidence beside it — policy changed by an act that left no trace
	//     of why, which is exactly what R-B's ceremony exists to prevent.
	//
	// So the safe failure direction is evidence-without-policy, and the
	// rollback closes even that: the gesture leaves the complete artifact
	// set or no change at all.
	if err := os.WriteFile(recordPath, []byte(graduationRecord(recordID, genre, instruction, now, ev)), 0o644); err != nil {
		return GraduateResult{}, fmt.Errorf("triage: write graduation record: %w", err)
	}
	if err := writeFileAtomic(registryPath, append(out, '\n')); err != nil {
		if rmErr := os.Remove(recordPath); rmErr != nil {
			return GraduateResult{}, fmt.Errorf("triage: registry write failed (%w) AND the record could not be rolled back (%v) — remove %s by hand before retrying", err, rmErr, recordPath)
		}
		return GraduateResult{}, fmt.Errorf("triage: registry write failed, record rolled back, no change landed: %w", err)
	}
	return res, nil
}

// writeFileAtomic replaces a file by writing a temp beside it and renaming
// over the target. os.WriteFile truncates IN PLACE, so a failure after the
// truncation and before the last byte leaves a half-written — or empty —
// registry, and the rollback above would then remove the record while
// derivation policy stayed corrupted. rename(2) within one directory is
// atomic: the reader sees the old bytes or the new ones, never a prefix.
//
// The temp lives in the SAME directory because rename cannot cross
// filesystems, and is removed on every failure path so a botched graduation
// leaves no residue for the next one to trip over.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".graduated-genres-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	// fsync before the rename: the rename is atomic with respect to other
	// readers, but a crash could otherwise expose a renamed-but-empty file.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

var recordFileRE = regexp.MustCompile(`^D(\d{4})-`)

// nextRecordNumber continues the D-numbering. Records are append-only and
// numbers are never reused, so the next one is simply one past the highest.
func nextRecordNumber(recordsDir string) (int, error) {
	entries, err := os.ReadDir(recordsDir)
	if err != nil {
		return 0, fmt.Errorf("triage: read records home: %w", err)
	}
	highest := 0
	for _, e := range entries {
		m := recordFileRE.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		if n > highest {
			highest = n
		}
	}
	if highest == 0 {
		return 0, fmt.Errorf("triage: found no numbered records under %s", recordsDir)
	}
	return highest + 1, nil
}

// tickSentinel stands in for a literal backtick inside the record template
// below: the template is a raw string, which cannot contain one. A NUL is
// used rather than a word so no genre slug or instruction can collide with
// it when the template is filled.
const tickSentinel = "\x00"

func graduationRecord(recordID, genre, instruction string, now time.Time, ev db.TriageGenreCandidate) string {
	tmpl := `---
id: %s
genre: graduation
status: proposed
date: %s
authority: operator
origin: session
supersedes: []
composes: []
sunset:
domains: [derive-observations, review-and-elevate]
fork: core
detected_by:
source:
declared_literals: []
links: []
---
# The %s noise genre graduates to derivation death

This file is birth-truth, frozen.

## Rule

Observations of the \x00%s\x00 genre are NEVER EMITTED. The derivation prompt
carries the genre as a never-emit instruction — %s — so the rows stop being
written rather than being written and then lapsed.

This is the narrow second noise death point (triage lane v1, R-B). Default
death stays AT TRIAGE, where derivation keeps emitting and a human sees
every row; a genre reaches death AT DERIVATION only per genre, on triage
evidence, ratified by the operator.

## Context

Evidence at graduation, computed from the triage lane's own suggestions and
the verdicts that followed them:

- occurrences (suggestions naming this genre): %d
- judged (occurrences the operator ruled on): %d
- flip set (rows the operator did NOT lapse): EMPTY

The empty flip set is the falsifier, not a formality: a graduation attempt
over a genre carrying any kept row REFUSES, because graduation destroys
future rows of that genre unseen, and one kept row is one row that would
have been thrown away without ever reaching a human.

The discriminator this genre was judged against, of record: an observation
earns storage by adding STRUCTURE THE RAW EVENTS DO NOT CARRY — a pure
single-event echo with no synthesis is noise, a synthesized outcome across
events is not. The rejected discriminator is also of record: "mirrors
another system of record" OVER-FILTERS, because a synthesis like
review -> merge -> deploy -> three clean nights mirrors git and deploy logs
and is exactly what Memoire holds.

## Consequences

The genre stops consuming derivation output, review attention, and store
rows. The prompt version gains its graduation suffix, so runs before and
after this record are distinguishable in the ledger rather than sharing one
name across two materially different prompts. Reversing the graduation is a
new record, never an edit to this one.
`
	body := fmt.Sprintf(tmpl, recordID, now.Format("2006-01-02"), genre, genre, instruction,
		ev.Occurrences, ev.Judged)
	return strings.ReplaceAll(body, tickSentinel, "`")
}
