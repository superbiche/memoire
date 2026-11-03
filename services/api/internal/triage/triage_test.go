// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package triage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"memoire/services/api/internal/db"
	"memoire/services/api/internal/modelderive"
)

func row(id int64, text string) db.TriageRow {
	return db.TriageRow{Observation: db.ObservationRecord{
		ID: id, Kind: "fact", Project: "memoire-test", ReviewState: "proposed", Text: text,
	}}
}

// stubJudge answers with a canned response per call, so the sweep's own
// behavior is what is under test rather than a model's.
type stubJudge struct {
	responses [][]byte
	errs      []error
	prompts   []string
	calls     int
}

func (j *stubJudge) Chat(_ context.Context, _, _, prompt string) ([]byte, error) {
	j.prompts = append(j.prompts, prompt)
	i := j.calls
	j.calls++
	if i < len(j.errs) && j.errs[i] != nil {
		return nil, j.errs[i]
	}
	if i < len(j.responses) {
		return j.responses[i], nil
	}
	return nil, errors.New("stub judge exhausted")
}

// memStore is the narrow sweep store. It records what was written and, by
// having no verdict method at all, makes "the lane cannot rule" a
// compile-time property rather than a promise.
type memStore struct {
	rows     []db.TriageRow
	written  []db.TriageSuggestionInput
	rawCalls int
}

func (m *memStore) ListTriageScope(context.Context, db.TriageScopeFilter) ([]db.TriageRow, error) {
	return m.rows, nil
}

func (m *memStore) LoadRawEvents(_ context.Context, rows []db.TriageRow, _ int) error {
	m.rawCalls++
	for i := range rows {
		rows[i].RawEvents = []string{"raw event for " + fmt.Sprint(rows[i].Observation.ID)}
	}
	return nil
}

func (m *memStore) ListTriageScopeSessions(context.Context, db.TriageScopeFilter) ([]db.TriageScopeSession, error) {
	seen := map[string]bool{}
	var out []db.TriageScopeSession
	for _, r := range m.rows {
		key := r.Observation.Project + "\x1f" + r.Observation.SessionID
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, db.TriageScopeSession{Project: r.Observation.Project, SessionID: r.Observation.SessionID})
	}
	return out, nil
}

func (m *memStore) InsertTriageSuggestion(_ context.Context, in db.TriageSuggestionInput) (bool, error) {
	for _, w := range m.written {
		if w.ObservationID == in.ObservationID && w.PromptVersion == in.PromptVersion {
			return false, nil // the idempotency guard, mirrored
		}
	}
	m.written = append(m.written, in)
	return true, nil
}

func verdictsJSON(t *testing.T, vs ...Verdict) []byte {
	t.Helper()
	b, err := json.Marshal(responseEnvelope{SchemaVersion: ResponseSchemaVersion, Verdicts: vs})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSweepWritesSuggestionsOnly(t *testing.T) {
	store := &memStore{rows: []db.TriageRow{
		row(1, "a synthesized outcome across several events"),
		row(2, "awaiting completion of the other lane"),
	}}
	judge := &stubJudge{responses: [][]byte{verdictsJSON(t,
		Verdict{ObservationID: 1, Verdict: db.TriageVerdictAccept},
		Verdict{ObservationID: 2, Verdict: db.TriageVerdictNoiseLapse, Genre: "orchestration-chatter"},
	)}}

	out, err := Sweep(context.Background(), store, judge, SweepOptions{
		Stage: 1, EndpointID: "hl5", Model: "judge-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.RowsJudged != 2 || out.Suggestions != 2 || out.Chunks != 1 {
		t.Fatalf("unexpected outcome %+v", out)
	}
	if len(store.written) != 2 {
		t.Fatalf("want 2 suggestions written, got %d", len(store.written))
	}
	for _, w := range store.written {
		if w.Stage != 1 || w.PromptVersion != PromptVersionStage1 {
			t.Errorf("suggestion carries the wrong pass identity: %+v", w)
		}
	}
	if out.ByVerdict[db.TriageVerdictNoiseLapse] != 1 {
		t.Errorf("verdict census wrong: %+v", out.ByVerdict)
	}
}

func TestSweepDryRunWritesNothing(t *testing.T) {
	store := &memStore{rows: []db.TriageRow{row(1, "something")}}
	judge := &stubJudge{responses: [][]byte{verdictsJSON(t,
		Verdict{ObservationID: 1, Verdict: db.TriageVerdictLapse, Genre: "thin-restatement"})}}
	out, err := Sweep(context.Background(), store, judge, SweepOptions{
		Stage: 1, EndpointID: "hl5", Model: "judge-model", DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.RowsJudged != 1 {
		t.Fatalf("dry run must still judge, got %+v", out)
	}
	if len(store.written) != 0 || out.Suggestions != 0 {
		t.Fatalf("dry run wrote %d suggestions", len(store.written))
	}
}

// A verdict about a row the judge was never shown is the hallucination
// shape that matters: it would otherwise attach a suggestion to a real row
// nobody judged.
func TestVerdictOutsideThePromptIsRejected(t *testing.T) {
	store := &memStore{rows: []db.TriageRow{row(1, "in scope")}}
	judge := &stubJudge{responses: [][]byte{verdictsJSON(t,
		Verdict{ObservationID: 1, Verdict: db.TriageVerdictAccept},
		Verdict{ObservationID: 999, Verdict: db.TriageVerdictReject, Genre: db.TriageRejectFabrication},
	)}}
	out, err := Sweep(context.Background(), store, judge, SweepOptions{
		Stage: 1, EndpointID: "hl5", Model: "judge-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Suggestions != 1 || out.Rejected != 1 {
		t.Fatalf("want 1 stored, 1 rejected, got %+v", out)
	}
	if store.written[0].ObservationID != 1 {
		t.Fatalf("stored a suggestion for an unjudged row: %+v", store.written[0])
	}
}

func TestMalformedRejectIsCapturedNotStored(t *testing.T) {
	store := &memStore{rows: []db.TriageRow{row(1, "x"), row(2, "y")}}
	judge := &stubJudge{responses: [][]byte{verdictsJSON(t,
		// A reject with no taxonomy reason: captured, never stored.
		Verdict{ObservationID: 1, Verdict: db.TriageVerdictReject},
		Verdict{ObservationID: 2, Verdict: db.TriageVerdictAccept},
	)}}
	out, err := Sweep(context.Background(), store, judge, SweepOptions{
		Stage: 1, EndpointID: "hl5", Model: "judge-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The good verdict is stored; the malformed reject is captured and not
	// stored, and its row — which WAS evaluated — gets a coverage row so a
	// re-sweep does not judge it again.
	if out.Rejected != 1 || out.ByVerdict[db.TriageVerdictAccept] != 1 || out.ByVerdict[db.TriageVerdictNone] != 1 {
		t.Fatalf("want the malformed reject captured, the good one stored, and a coverage row, got %+v", out)
	}
}

func TestChunkFailureIsDataNotError(t *testing.T) {
	rows := make([]db.TriageRow, 0, 4)
	for i := int64(1); i <= 4; i++ {
		rows = append(rows, row(i, strings.Repeat("a longish observation body ", 20)))
	}
	store := &memStore{rows: rows}
	judge := &stubJudge{
		errs:      []error{errors.New("endpoint unavailable"), nil},
		responses: [][]byte{nil, verdictsJSON(t, Verdict{ObservationID: 3, Verdict: db.TriageVerdictAccept})},
	}
	out, err := Sweep(context.Background(), store, judge, SweepOptions{
		Stage: 1, EndpointID: "hl5", Model: "judge-model", MaxRowsPerChunk: 2,
	})
	if err != nil {
		t.Fatalf("one failed chunk must not fail the pass: %v", err)
	}
	if out.Chunks != 2 || out.ChunkFailed != 1 {
		t.Fatalf("unexpected outcome %+v", out)
	}
	if out.FirstFailure == "" {
		t.Error("a failed chunk must be reported, not swallowed")
	}
}

func TestEveryChunkFailingIsAnError(t *testing.T) {
	store := &memStore{rows: []db.TriageRow{row(1, "x")}}
	judge := &stubJudge{errs: []error{errors.New("dead endpoint")}}
	_, err := Sweep(context.Background(), store, judge, SweepOptions{
		Stage: 1, EndpointID: "hl5", Model: "judge-model",
	})
	if err == nil {
		t.Fatal("a pass whose every chunk failed must not report success")
	}
}

func TestChunkingRespectsBothCeilings(t *testing.T) {
	var rows []db.TriageRow
	for i := int64(1); i <= 7; i++ {
		rows = append(rows, row(i, "short"))
	}
	chunks := Chunk(rows, DefaultMaxPromptBytes, 3, func(r []db.TriageRow) (string, error) {
		return BuildStage1Prompt(r)
	})
	if len(chunks) != 3 || len(chunks[0]) != 3 || len(chunks[2]) != 1 {
		t.Fatalf("row ceiling not honored: %d chunks with sizes %d/%d/%d",
			len(chunks), len(chunks[0]), len(chunks[1]), len(chunks[2]))
	}
	// A byte ceiling smaller than one row still emits that row alone: a
	// dropped row would silently vanish from the operator's queue.
	fat := []db.TriageRow{row(1, strings.Repeat("x", 5000)), row(2, strings.Repeat("y", 5000))}
	tight := Chunk(fat, 1000, 10, func(r []db.TriageRow) (string, error) { return BuildStage1Prompt(r) })
	if len(tight) != 2 {
		t.Fatalf("oversized rows must each get their own chunk, got %d", len(tight))
	}
}

// Stage 2's A/B is worthless if both arms see the same input, so the arms
// never share a prompt and only arm A carries raw events.
func TestStage2ArmsAreSeparatedAndDeterministic(t *testing.T) {
	rows := []db.TriageRow{
		{Observation: db.ObservationRecord{ID: 2, Kind: "fact", ReviewState: "accepted", Text: "even id, arm A"}},
		{Observation: db.ObservationRecord{ID: 3, Kind: "fact", ReviewState: "accepted", Text: "odd id, arm B"}},
	}
	store := &memStore{}
	judge := &stubJudge{responses: [][]byte{
		verdictsJSON(t, Verdict{ObservationID: 2, Verdict: db.TriageVerdictConfirm}),
		verdictsJSON(t, Verdict{ObservationID: 3, Verdict: db.TriageVerdictConfirm}),
	}}
	out, err := SweepRows(context.Background(), store, judge, SweepOptions{
		Stage: 2, EndpointID: "hl5", Model: "judge-model",
	}, rows)
	if err != nil {
		t.Fatal(err)
	}
	if out.Chunks != 2 {
		t.Fatalf("arms must not share a prompt, got %d chunks", out.Chunks)
	}
	if len(judge.prompts) != 2 {
		t.Fatalf("want two prompts, got %d", len(judge.prompts))
	}
	if !strings.Contains(judge.prompts[0], "Source events:") {
		t.Error("arm A must carry the raw event window")
	}
	if strings.Contains(judge.prompts[1], "Source events:") {
		t.Error("arm B must be observations-only")
	}
	for _, w := range store.written {
		if w.Arm != db.TriageStage2Arm(w.ObservationID) {
			t.Errorf("arm must be recomputable from the row: %+v", w)
		}
	}
}

func TestParseRejectsMalformedEnvelope(t *testing.T) {
	scope := map[int64]bool{1: true}
	for _, tc := range []struct{ name, body, want string }{
		{"not json", "sorry, I cannot", "not JSON"},
		{"wrong schema", `{"schema_version":"9","verdicts":[]}`, "schema_version"},
		{"trailing data", `{"schema_version":"1","verdicts":[]} {"extra":1}`, "trailing data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseResponse([]byte(tc.body), 1, scope); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error containing %q, got %v", tc.want, err)
			}
		})
	}
	// A single markdown fence is unwrapped, the same conservative
	// allowance the derivation parser makes.
	fenced := "```json\n" + `{"schema_version":"1","verdicts":[{"observation_id":1,"verdict":"accept"}]}` + "\n```"
	res, err := ParseResponse([]byte(fenced), 1, scope)
	if err != nil {
		t.Fatalf("a fenced envelope must parse: %v", err)
	}
	if len(res.Verdicts) != 1 {
		t.Fatalf("want 1 verdict, got %d", len(res.Verdicts))
	}
}

// The R-B falsifier, required by the ruling: a graduation attempt with a
// NON-ZERO flip set must refuse.
func TestGraduationRefusesOnNonZeroFlipSet(t *testing.T) {
	repo := newFakeRepo(t)
	store := evidenceStore{db.TriageGenreCandidate{
		Genre: "orchestration-chatter", Occurrences: 40, Judged: 40, FlipSet: []int64{77},
	}}
	_, err := Graduate(context.Background(), store, GraduateOptions{
		Genre: "orchestration-chatter", RepoRoot: repo, Now: time.Unix(1_800_000_000, 0),
	})
	var flip *ErrFlipSetNonEmpty
	if !errors.As(err, &flip) {
		t.Fatalf("want a flip-set refusal, got %v", err)
	}
	if !strings.Contains(err.Error(), "77") {
		t.Errorf("the refusal must name the kept rows: %v", err)
	}
	assertNoArtifacts(t, repo)
}

func TestGraduationRefusesBelowThreshold(t *testing.T) {
	repo := newFakeRepo(t)
	store := evidenceStore{db.TriageGenreCandidate{Genre: "thin-restatement", Occurrences: 5, Judged: 5}}
	_, err := Graduate(context.Background(), store, GraduateOptions{
		Genre: "thin-restatement", RepoRoot: repo, Now: time.Unix(1_800_000_000, 0),
	})
	var below *ErrBelowThreshold
	if !errors.As(err, &below) {
		t.Fatalf("want a threshold refusal, got %v", err)
	}
	assertNoArtifacts(t, repo)
}

// A clean graduation produces BOTH artifacts in one act, and the record
// carries the evidence and both discriminators verbatim.
func TestGraduationWritesRegistryAndRecord(t *testing.T) {
	repo := newFakeRepo(t)
	ev := db.TriageGenreCandidate{Genre: "status-narration", Occurrences: 31, Judged: 24}
	store := evidenceStore{ev}
	res, err := Graduate(context.Background(), store, GraduateOptions{
		Genre:       "status-narration",
		Instruction: "bare completion notices carrying no durable value",
		RepoRoot:    repo,
		Now:         time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	rawReg, err := os.ReadFile(res.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	var reg modelderive.GraduatedRegistry
	if err := json.Unmarshal(rawReg, &reg); err != nil {
		t.Fatal(err)
	}
	if len(reg.Genres) != 1 || reg.Genres[0].Slug != "status-narration" {
		t.Fatalf("registry entry not written: %+v", reg.Genres)
	}
	if reg.Genres[0].Evidence.Judged != 24 || reg.Genres[0].Record != "memoire:D0253" {
		t.Fatalf("registry entry lost its evidence or record link: %+v", reg.Genres[0])
	}
	rec, err := os.ReadFile(res.RecordPath)
	if err != nil {
		t.Fatal(err)
	}
	body := string(rec)
	for _, want := range []string{
		"id: D0253",
		"genre: graduation",
		"status: proposed",
		"occurrences (suggestions naming this genre): 31",
		"flip set (rows the operator did NOT lapse): EMPTY",
		"STRUCTURE THE RAW EVENTS DO NOT CARRY",
		"OVER-FILTERS",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("record missing %q", want)
		}
	}
	if strings.Contains(body, "\x00") {
		t.Error("the backtick sentinel leaked into the record")
	}

	// A second graduation of the same genre refuses rather than writing a
	// duplicate entry and burning a record number.
	if _, err := Graduate(context.Background(), store, GraduateOptions{
		Genre: "status-narration", RepoRoot: repo, Now: time.Now(),
	}); err == nil || !strings.Contains(err.Error(), "already graduated") {
		t.Fatalf("want an already-graduated refusal, got %v", err)
	}
}

// With no genre graduated the derivation prompt is byte-identical to the
// pre-triage one and the prompt version gains no suffix — a deployment that
// graduates nothing regresses by nothing.
func TestNoGraduationMeansNoPromptChange(t *testing.T) {
	if got := modelderive.GraduatedPromptSuffix(); got != "" {
		t.Fatalf("suffix on an empty registry = %q, want empty", got)
	}
	g, err := modelderive.GraduatedNoiseGenres()
	if err != nil {
		t.Fatal(err)
	}
	if len(g) != 0 {
		t.Fatalf("the shipped registry must start empty, got %+v", g)
	}
}

type evidenceStore struct{ ev db.TriageGenreCandidate }

func (e evidenceStore) TriageGenreEvidence(context.Context, string) (db.TriageGenreCandidate, error) {
	return e.ev, nil
}

// newFakeRepo mirrors just enough of the repo layout for the graduation
// gesture: a records home to continue the numbering from, and the registry
// the derivation prompt is built from.
// Code and records in separate repositories: the record lands in
// RecordsDir, the registry in RepoRoot, and numbering reads RecordsDir.
func TestGraduationSplitsRecordsFromRegistry(t *testing.T) {
	code := newFakeRepo(t)
	if err := os.RemoveAll(filepath.Join(code, "records")); err != nil {
		t.Fatal(err)
	}
	ledger := t.TempDir()
	if err := os.WriteFile(filepath.Join(ledger, "D0300-prior.md"), []byte("# prior\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := evidenceStore{db.TriageGenreCandidate{Genre: "status-narration", Occurrences: 31, Judged: 24}}
	res, err := Graduate(context.Background(), store, GraduateOptions{
		Genre: "status-narration", RepoRoot: code, RecordsDir: ledger,
		Now: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(res.RecordPath) != ledger || !strings.HasPrefix(filepath.Base(res.RecordPath), "D0301-") {
		t.Fatalf("record must land in the records dir with the next number: %s", res.RecordPath)
	}
	if !strings.HasPrefix(res.RegistryPath, code+string(filepath.Separator)) {
		t.Fatalf("registry must stay in the code checkout: %s", res.RegistryPath)
	}
	if _, err := os.Stat(filepath.Join(code, "records")); !os.IsNotExist(err) {
		t.Fatalf("no records/ may appear in the code checkout (stat err %v)", err)
	}
}

func newFakeRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	records := filepath.Join(root, "records")
	if err := os.MkdirAll(records, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(records, "D0252-something.md"), []byte("# prior\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	regDir := filepath.Join(root, "services", "api", "internal", "modelderive")
	if err := os.MkdirAll(regDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(regDir, "graduated-genres.json"),
		[]byte(`{"comment":["test"],"genres":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func assertNoArtifacts(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "records"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("a refused graduation must write no record, found %d files", len(entries))
	}
	raw, err := os.ReadFile(filepath.Join(root, "services", "api", "internal", "modelderive", "graduated-genres.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "slug") {
		t.Error("a refused graduation must not touch the registry")
	}
}

// F6's failure direction: the gesture leaves the COMPLETE artifact set or no
// change at all. The record is written first — evidence without a policy
// change is survivable, a policy change without its evidence is not — and a
// failing registry write rolls the record back.
func TestGraduationIsFailureSafe(t *testing.T) {
	repo := newFakeRepo(t)
	store := evidenceStore{db.TriageGenreCandidate{Genre: "status-narration", Occurrences: 31, Judged: 24}}
	registry := filepath.Join(repo, "services", "api", "internal", "modelderive", "graduated-genres.json")

	// Make the SECOND write fail. The registry is now replaced atomically —
	// temp beside it, then rename — so the failure has to be injected where
	// that sequence starts: a read-only DIRECTORY, which blocks creating
	// the temp. (A read-only registry FILE would no longer do it, and that
	// is the point: rename over it succeeds, which is exactly the
	// in-place-truncation hazard this write pattern removes.)
	regDir := filepath.Dir(registry)
	if err := os.Chmod(regDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(regDir, 0o755) })

	_, err := Graduate(context.Background(), store, GraduateOptions{
		Genre: "status-narration", RepoRoot: repo, Now: time.Unix(1_800_000_000, 0),
	})
	if err == nil {
		t.Fatal("a failing registry write must fail the gesture")
	}
	if !strings.Contains(err.Error(), "rolled back") {
		t.Errorf("the failure must say the record was rolled back, got %v", err)
	}
	entries, rerr := os.ReadDir(filepath.Join(repo, "records"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 1 {
		t.Fatalf("the record must be rolled back, found %d files in records/", len(entries))
	}
	raw, rerr := os.ReadFile(registry)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if strings.Contains(string(raw), "status-narration") {
		t.Error("no policy change may survive a failed graduation")
	}
	// No temp residue for the next graduation to trip over.
	entries2, _ := os.ReadDir(filepath.Dir(registry))
	for _, e := range entries2 {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("a failed graduation left temp residue: %s", e.Name())
		}
	}
}

// The registry is replaced ATOMICALLY: a reader sees the old bytes or the
// new ones, never a truncated prefix. os.WriteFile truncates in place, so a
// failure mid-write would have left a malformed registry that the record
// rollback could not repair.
func TestRegistryWriteIsAtomic(t *testing.T) {
	repo := newFakeRepo(t)
	registry := filepath.Join(repo, "services", "api", "internal", "modelderive", "graduated-genres.json")
	before, err := os.ReadFile(registry)
	if err != nil {
		t.Fatal(err)
	}
	store := evidenceStore{db.TriageGenreCandidate{Genre: "status-narration", Occurrences: 31, Judged: 24}}
	if _, err := Graduate(context.Background(), store, GraduateOptions{
		Genre: "status-narration", RepoRoot: repo, Now: time.Unix(1_800_000_000, 0),
	}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(registry)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) == string(before) {
		t.Fatal("the registry should have changed")
	}
	var reg modelderive.GraduatedRegistry
	if err := json.Unmarshal(after, &reg); err != nil {
		t.Fatalf("the registry must always be parseable JSON, never a truncated prefix: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(registry))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("a successful graduation left temp residue: %s", e.Name())
		}
	}
}

// A genre that never passed the slug grammar must not be able to build a
// filesystem path, even if something wrote around the store gate.
func TestGraduationRefusesANonSlugGenre(t *testing.T) {
	repo := newFakeRepo(t)
	hostile := "../../etc/pwned"
	store := evidenceStore{db.TriageGenreCandidate{Genre: hostile, Occurrences: 40, Judged: 40}}
	_, err := Graduate(context.Background(), store, GraduateOptions{
		Genre: hostile, RepoRoot: repo, Now: time.Unix(1_800_000_000, 0),
	})
	if err == nil || !strings.Contains(err.Error(), "not a valid slug") {
		t.Fatalf("want a slug refusal, got %v", err)
	}
	assertNoArtifacts(t, repo)
}

// The parser DEGRADES a hostile genre to free text rather than promoting it
// to a slug that would later drive a rendered command and a record path.
func TestHostileGenreIsDegradedToFreeText(t *testing.T) {
	scope := map[int64]bool{1: true}
	for _, hostile := range []string{"$(printf PWNED)", "../../etc/pwned", "has spaces", "UPPER"} {
		body := fmt.Sprintf(`{"schema_version":"1","verdicts":[{"observation_id":1,"verdict":"noise-lapse","genre":%q}]}`, hostile)
		res, err := ParseResponse([]byte(body), 1, scope)
		if err != nil {
			t.Fatalf("a hostile genre must not fail the whole envelope: %v", err)
		}
		if len(res.Verdicts) != 1 {
			t.Fatalf("the verdict itself survives, got %d", len(res.Verdicts))
		}
		if res.Verdicts[0].Genre != "" {
			t.Errorf("genre %q must not be promoted to a slug, got %q", hostile, res.Verdicts[0].Genre)
		}
		if !strings.Contains(res.Verdicts[0].Reason, hostile) {
			t.Errorf("the judge's text must be kept as free text, got %q", res.Verdicts[0].Reason)
		}
	}
}
