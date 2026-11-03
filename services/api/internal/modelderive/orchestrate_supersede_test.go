// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package modelderive

// Derivation-time supersede end to end (Fibery #344), against the real store
// (memoire:D49): a later cut restating an earlier cut's proposal writes a
// successor and retires the target, the outcome and run metadata say so, the
// kill switch restores today's behavior byte for byte, and -provisional marks
// an open-tail run.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"memoire/services/api/internal/db"
)

// rawConn opens a read connection to the test store's file, the way the
// existing orchestrator tests inspect columns the store surface does not
// expose (meta_json, review_state).
func rawConn(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatalf("open raw conn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func reviewStateOf(t *testing.T, dbPath string, id int64) string {
	t.Helper()
	var state string
	if err := rawConn(t, dbPath).QueryRowContext(context.Background(),
		`SELECT review_state FROM observations WHERE id = ?`, id).Scan(&state); err != nil {
		t.Fatalf("review_state read: %v", err)
	}
	return state
}

func countRows(t *testing.T, dbPath, query string) int {
	t.Helper()
	var n int
	if err := rawConn(t, dbPath).QueryRowContext(context.Background(), query).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// The mid-arc pair, shared with the classifier battery: an extension inside
// the gate's reachable band (trigram 0.595, containment 1.0, growth 0.405).
const (
	cutSoFar    = "The rhythm-cut detector walks the same population the prompt builder admits, so cuts land on event seams."
	cutExtended = cutSoFar + " The min-content guard vetoes any cut that would strand a sub-floor fragment."
)

// runTwoCuts derives cutSoFar, then derives cutExtended over a second event,
// returning both outcomes and the first run's observation id.
func runTwoCuts(t *testing.T, session string, tune func(*Request)) (first, second RunOutcome, targetID int64, store *db.Store, dbPath string) {
	t.Helper()
	dbPath, store = openTestStore(t)
	ctx := context.Background()
	seedPrompt(t, store, session, "e1", 1000, "the detector walks the builder population")

	firstProvider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		return obsResponse(req, "fact", cutSoFar), nil
	})
	var err error
	first, err = newOrchestrator(store).Run(ctx, firstProvider, orchRequest(session))
	if err != nil || first.WrittenCount != 1 {
		t.Fatalf("first cut: err=%v out=%#v", err, first)
	}
	obs, _, err := store.ListObservations(ctx, db.ObservationFilter{
		Project: "memoire-test", SessionID: session, ReviewState: "proposed",
	})
	if err != nil || len(obs) != 1 {
		t.Fatalf("expected the first cut's row: %v %d", err, len(obs))
	}
	targetID = obs[0].ID

	seedPrompt(t, store, session, "e2", 1001, "and the guard vetoes stranded fragments")
	secondProvider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		return obsResponse(req, "fact", cutExtended), nil
	})
	req := orchRequest(session)
	if tune != nil {
		tune(&req)
	}
	second, err = newOrchestrator(store).Run(ctx, secondProvider, req)
	if err != nil {
		t.Fatalf("second cut: %v", err)
	}
	return first, second, targetID, store, dbPath
}

func observationMetaOf(t *testing.T, dbPath string, id int64) map[string]any {
	t.Helper()
	var raw string
	if err := rawConn(t, dbPath).QueryRowContext(context.Background(),
		`SELECT meta_json FROM observations WHERE id = ?`, id).Scan(&raw); err != nil {
		t.Fatalf("observation meta: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatalf("observation meta decode: %v", err)
	}
	return meta
}

func TestOrchestratorSupersedesEarlierCut(t *testing.T) {
	_, second, targetID, store, dbPath := runTwoCuts(t, "sess-sup", nil)
	ctx := context.Background()

	if second.SupersededCount != 1 {
		t.Fatalf("the extension must supersede: %#v", second)
	}
	if second.WrittenCount != 1 {
		t.Fatalf("a superseding write IS a write: written=%d", second.WrittenCount)
	}
	if second.RejectedCount != 0 {
		t.Fatalf("a supersede is not a rejection: rejected=%d", second.RejectedCount)
	}
	// D43 row: written>0 and rejected==0 => completed. The table is untouched.
	if second.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", second.Status)
	}

	// The target retired; the successor carries filiation and is the only
	// proposed row left.
	target, err := store.GetObservationWithFilter(ctx, targetID, db.ObservationFilter{IncludeProposed: true})
	if err == nil && target.ReviewState != "superseded" {
		t.Fatalf("target state = %q, want superseded", target.ReviewState)
	}
	proposedRows, _, err := store.ListObservations(ctx, db.ObservationFilter{
		Project: "memoire-test", SessionID: "sess-sup", ReviewState: "proposed",
	})
	if err != nil || len(proposedRows) != 1 {
		t.Fatalf("exactly one proposed row must remain: %d err=%v", len(proposedRows), err)
	}
	successor := proposedRows[0]
	if successor.Text != cutExtended {
		t.Fatalf("the surviving row must be the extension, got %q", successor.Text)
	}
	if successor.SupersedesID == nil || *successor.SupersedesID != targetID {
		t.Fatalf("successor supersedes_id = %v, want %d", successor.SupersedesID, targetID)
	}

	// The review log names the mechanism, not a human.
	logRows, err := store.ListObservationReviewLog(ctx, db.ObservationReviewLogFilter{ObservationID: targetID})
	if err != nil || len(logRows) != 1 {
		t.Fatalf("target review log: %+v err=%v", logRows, err)
	}
	if logRows[0].Actor != db.DeriveSupersedeActor || logRows[0].ToState != "superseded" {
		t.Fatalf("review-log row = %+v", logRows[0])
	}

	// Run metadata carries the custody record and the counters.
	meta := runMeta(t, store, second.RunID)
	if meta["superseded_count"] != float64(1) {
		t.Fatalf("run meta superseded_count = %v", meta["superseded_count"])
	}
	if meta["written_observation_count"] != float64(1) {
		t.Fatalf("run meta written_observation_count = %v", meta["written_observation_count"])
	}
	cands, ok := meta["superseded_candidates"].([]any)
	if !ok || len(cands) != 1 {
		t.Fatalf("run meta superseded_candidates = %v", meta["superseded_candidates"])
	}
	entry := cands[0].(map[string]any)
	if entry["superseded_id"] != float64(targetID) {
		t.Fatalf("superseded_candidates[0].superseded_id = %v, want %d", entry["superseded_id"], targetID)
	}
	if entry["observation_id"] != float64(successor.ID) {
		t.Fatalf("superseded_candidates[0].observation_id = %v, want %d", entry["observation_id"], successor.ID)
	}
	for _, k := range []string{"trigram", "containment", "growth"} {
		v, isNum := entry[k].(float64)
		if !isNum || v <= 0 {
			t.Fatalf("superseded_candidates[0].%s = %v, want a measured score", k, entry[k])
		}
	}
	if entry["text"] != cutExtended {
		t.Fatalf("superseded_candidates[0].text must carry the candidate text")
	}
	counts, ok := meta["candidate_reason_counts"].(map[string]any)
	if !ok || counts[OutcomeClassSupersede] != float64(1) {
		t.Fatalf("candidate_reason_counts = %v, want supersede:1", meta["candidate_reason_counts"])
	}

	// The successor's own meta_json carries the same three scores.
	obsMeta := observationMetaOf(t, dbPath, successor.ID)
	sup, ok := obsMeta["supersede"].(map[string]any)
	if !ok || sup["of"] != float64(targetID) {
		t.Fatalf("observation meta supersede = %v", obsMeta["supersede"])
	}
	if obsMeta["confidence_source"] != ConfidenceSourceModel {
		t.Fatalf("a superseding row keeps memoire:D47 provenance, got %v", obsMeta["confidence_source"])
	}

	// D5 at the seam: the retired row is gone from the derivation read-back,
	// so neither the next prompt's section nor the next gate sees it.
	list, err := store.ListSessionObservationsForDerivation(ctx, "memoire-test", "sess-sup", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range list {
		if o.ID == targetID {
			t.Fatal("a superseded row must not be offered to the next cut")
		}
	}
}

func TestOrchestratorSupersedeRefusesAcceptedTarget(t *testing.T) {
	// A human-ruled row is superseded only by a human gesture: the candidate
	// rejects with the reason string this gate has always used.
	dbPath, store := openTestStore(t)
	ctx := context.Background()
	seedPrompt(t, store, "sess-sup-acc", "e1", 1000, "the detector walks the builder population")
	firstProvider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		return obsResponse(req, "fact", cutSoFar), nil
	})
	if out, err := newOrchestrator(store).Run(ctx, firstProvider, orchRequest("sess-sup-acc")); err != nil || out.WrittenCount != 1 {
		t.Fatalf("first cut: err=%v out=%#v", err, out)
	}
	rows, _, err := store.ListObservations(ctx, db.ObservationFilter{
		Project: "memoire-test", SessionID: "sess-sup-acc", ReviewState: "proposed",
	})
	if err != nil || len(rows) != 1 {
		t.Fatalf("seed row: %v %d", err, len(rows))
	}
	targetID := rows[0].ID
	if _, err := store.TransitionObservationReview(ctx, targetID, "accepted", "operator agrees"); err != nil {
		t.Fatalf("accept: %v", err)
	}

	seedPrompt(t, store, "sess-sup-acc", "e2", 1001, "and the guard vetoes stranded fragments")
	secondProvider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		return obsResponse(req, "fact", cutExtended), nil
	})
	out, err := newOrchestrator(store).Run(ctx, secondProvider, orchRequest("sess-sup-acc"))
	if err != nil {
		t.Fatalf("second cut: %v", err)
	}
	if out.SupersededCount != 0 || out.WrittenCount != 0 || out.RejectedCount != 1 {
		t.Fatalf("an accepted target must reject as today: %#v", out)
	}
	if out.Status != StatusCompleted {
		t.Fatalf("a near-dup-only response still completes (D212): %q", out.Status)
	}
	meta := runMeta(t, store, out.RunID)
	rejected, ok := meta["rejected_candidates"].([]any)
	if !ok || len(rejected) != 1 {
		t.Fatalf("rejected_candidates = %v", meta["rejected_candidates"])
	}
	reason := rejected[0].(map[string]any)["reason"]
	want := fmt.Sprintf("near-duplicate of observation %d (trigram=0.59)", targetID)
	if reason != want {
		t.Fatalf("reason string must be unchanged:\n got %v\nwant %v", reason, want)
	}
	if _, present := meta["superseded_candidates"]; present {
		t.Fatal("a run that superseded nothing must carry no superseded_candidates")
	}
	if state := reviewStateOf(t, dbPath, targetID); state != "accepted" {
		t.Fatalf("the accepted target must be untouched, got %q", state)
	}
}

// casRacerStore accepts the supersede target between the gate's read and the
// store's CAS — the race the brief names, driven deterministically instead of
// hoped for. It fires only when an offer exists, so the classification has
// already said "supersede" by the time the target moves.
type casRacerStore struct {
	OrchestratorStore
	store *db.Store
	t     *testing.T
	moved int64
}

func (s *casRacerStore) InsertDerivedObservationSuperseding(ctx context.Context, in db.DerivedObservationInput, matchedIDs []int64, offers []db.ObservationSupersedeOffer) (db.DerivedObservationGateResult, error) {
	if len(offers) > 0 && s.moved == 0 {
		s.moved = offers[0].TargetID
		if _, err := s.store.TransitionObservationReview(ctx, s.moved, "accepted", "operator won the race"); err != nil {
			s.t.Errorf("race accept: %v", err)
		}
	}
	return s.OrchestratorStore.InsertDerivedObservationSuperseding(ctx, in, matchedIDs, offers)
}

func TestOrchestratorSupersedeLosesCASToConcurrentAccept(t *testing.T) {
	// The target is `proposed` when the gate reads it — so the candidate IS
	// classified as a supersede and an offer is made — and `accepted` by the
	// time the CAS runs. The outcome, stated exactly: the candidate is
	// REJECTED as a near-duplicate of that row with the unchanged reason
	// string, nothing is written, nothing is superseded, and the run still
	// completes (D212: a near-dup-only response must cover its events).
	dbPath, store := openTestStore(t)
	ctx := context.Background()
	seedPrompt(t, store, "sess-sup-race", "e1", 1000, "the detector walks the builder population")
	if out, err := newOrchestrator(store).Run(ctx,
		localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
			return obsResponse(req, "fact", cutSoFar), nil
		}), orchRequest("sess-sup-race")); err != nil || out.WrittenCount != 1 {
		t.Fatalf("first cut: err=%v out=%#v", err, out)
	}
	rows, _, err := store.ListObservations(ctx, db.ObservationFilter{
		Project: "memoire-test", SessionID: "sess-sup-race", ReviewState: "proposed",
	})
	if err != nil || len(rows) != 1 {
		t.Fatalf("seed row: %v %d", err, len(rows))
	}
	targetID := rows[0].ID

	seedPrompt(t, store, "sess-sup-race", "e2", 1001, "and the guard vetoes stranded fragments")
	racer := &casRacerStore{OrchestratorStore: store, store: store, t: t}
	o := newOrchestrator(store)
	o.Store = racer
	out, err := o.Run(ctx, localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		return obsResponse(req, "fact", cutExtended), nil
	}), orchRequest("sess-sup-race"))
	if err != nil {
		t.Fatalf("racing run: %v", err)
	}
	if racer.moved != targetID {
		t.Fatalf("the fixture must have raced the offered target %d, moved %d", targetID, racer.moved)
	}
	if out.SupersededCount != 0 || out.WrittenCount != 0 || out.RejectedCount != 1 {
		t.Fatalf("a CAS loser must reject and write nothing: %#v", out)
	}
	if out.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", out.Status)
	}
	meta := runMeta(t, store, out.RunID)
	rejected, ok := meta["rejected_candidates"].([]any)
	if !ok || len(rejected) != 1 {
		t.Fatalf("rejected_candidates = %v", meta["rejected_candidates"])
	}
	reason := rejected[0].(map[string]any)["reason"]
	want := fmt.Sprintf("near-duplicate of observation %d (trigram=0.59)", targetID)
	if reason != want {
		t.Fatalf("reason string must be the unchanged near-dup one:\n got %v\nwant %v", reason, want)
	}
	if _, present := meta["superseded_candidates"]; present {
		t.Fatal("a run that lost the CAS must record no supersede")
	}
	if state := reviewStateOf(t, dbPath, targetID); state != "accepted" {
		t.Fatalf("the raced target must keep the winner's state, got %q", state)
	}
	if n := countRows(t, dbPath, `SELECT count(*) FROM observations WHERE supersedes_id IS NOT NULL`); n != 0 {
		t.Fatalf("no successor may exist, got %d", n)
	}
	if n := countRows(t, dbPath, `SELECT count(*) FROM observations WHERE review_state = 'superseded'`); n != 0 {
		t.Fatalf("no row may be retired, got %d", n)
	}
}

func TestOrchestratorSupersedeRefusesIntraResponseMatch(t *testing.T) {
	// Two candidates in ONE response, the second an extension of the first:
	// there is no earlier cut that learned more, so the second is the plain
	// duplicate the gate rejects.
	dbPath, store := openTestStore(t)
	ctx := context.Background()
	seedPrompt(t, store, "sess-sup-intra", "e1", 1000, "the detector walks the builder population")
	provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		a, _ := json.Marshal(cutSoFar)
		b, _ := json.Marshal(cutExtended)
		raw := fmt.Sprintf(`{"schema_version":"1","observations":[`+
			`{"kind":"fact","text":%s,"confidence":0.9,"source_event_ids":[%d]},`+
			`{"kind":"fact","text":%s,"confidence":0.9,"source_event_ids":[%d]}]}`,
			a, req.Events[0].ID, b, req.Events[0].ID)
		return ProviderResponse{RawResponse: json.RawMessage(raw)}, nil
	})
	out, err := newOrchestrator(store).Run(ctx, provider, orchRequest("sess-sup-intra"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.SupersededCount != 0 {
		t.Fatalf("an intra-response match must not supersede: %#v", out)
	}
	if out.WrittenCount != 1 || out.RejectedCount != 1 {
		t.Fatalf("want one write and one near-dup rejection: %#v", out)
	}
	if children := countRows(t, dbPath, `SELECT count(*) FROM observations WHERE supersedes_id IS NOT NULL`); children != 0 {
		t.Fatalf("no filiation may be recorded, got %d", children)
	}
}

func TestOrchestratorSupersedeSecondExtensionInSameResponse(t *testing.T) {
	// Two extensions of the SAME earlier target in one response: exactly one
	// supersede happens, and the second candidate is judged against the
	// SUCCESSOR (an intra-response match => rejection), never against the row
	// the store has already retired.
	dbPath, store := openTestStore(t)
	ctx := context.Background()
	seedPrompt(t, store, "sess-sup-two", "e1", 1000, "the detector walks the builder population")
	if out, err := newOrchestrator(store).Run(ctx,
		localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
			return obsResponse(req, "fact", cutSoFar), nil
		}), orchRequest("sess-sup-two")); err != nil || out.WrittenCount != 1 {
		t.Fatalf("first cut: err=%v out=%#v", err, out)
	}

	seedPrompt(t, store, "sess-sup-two", "e2", 1001, "and the guard vetoes stranded fragments")
	provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		a, _ := json.Marshal(cutExtended)
		b, _ := json.Marshal(cutExtended + " Twice.")
		raw := fmt.Sprintf(`{"schema_version":"1","observations":[`+
			`{"kind":"fact","text":%s,"confidence":0.9,"source_event_ids":[%d]},`+
			`{"kind":"fact","text":%s,"confidence":0.9,"source_event_ids":[%d]}]}`,
			a, req.Events[0].ID, b, req.Events[0].ID)
		return ProviderResponse{RawResponse: json.RawMessage(raw)}, nil
	})
	out, err := newOrchestrator(store).Run(ctx, provider, orchRequest("sess-sup-two"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.SupersededCount != 1 {
		t.Fatalf("exactly one supersede must happen, got %d (%#v)", out.SupersededCount, out)
	}
	if out.RejectedCount != 1 {
		t.Fatalf("the second extension must be rejected once, got %d", out.RejectedCount)
	}
	if children := countRows(t, dbPath, `SELECT count(*) FROM observations WHERE supersedes_id IS NOT NULL`); children != 1 {
		t.Fatalf("exactly one successor may exist, got %d", children)
	}
	if retired := countRows(t, dbPath, `SELECT count(*) FROM observations WHERE review_state = 'superseded'`); retired != 1 {
		t.Fatalf("exactly one row may be retired, got %d", retired)
	}
	// The second candidate's rejection names the SUCCESSOR, not the row this
	// run retired. NOTE, measured rather than assumed: this assertion does
	// NOT isolate the ref-drop — the successor contains the target, so it
	// outscores it and the store's first-live match is the successor either
	// way (verified by removing the drop: this test still passes). The
	// ref-drop's own falsifier is TestDropNearDupRef; the CAS zero-row branch
	// is what actually guards the retired-target case.
	var successorID int64
	if err := rawConn(t, dbPath).QueryRowContext(ctx,
		`SELECT id FROM observations WHERE supersedes_id IS NOT NULL`).Scan(&successorID); err != nil {
		t.Fatalf("successor id: %v", err)
	}
	meta := runMeta(t, store, out.RunID)
	rejected, ok := meta["rejected_candidates"].([]any)
	if !ok || len(rejected) != 1 {
		t.Fatalf("rejected_candidates = %v", meta["rejected_candidates"])
	}
	reason, _ := rejected[0].(map[string]any)["reason"].(string)
	if !strings.Contains(reason, fmt.Sprintf("observation %d ", successorID)) {
		t.Fatalf("the rejection must name the successor %d, got %q", successorID, reason)
	}
}

func TestOrchestratorSupersedeKillSwitchRestoresTodaysGate(t *testing.T) {
	// -no-supersede: the same pair, the same run, the same rejection — reason
	// string, counters, class and target state.
	//
	// Scope: the switch restores the gate OUTCOME. It does not un-apply D5 —
	// superseded rows stay out of the per-session derivation read-back
	// whatever the switch says — so a session holding rows retired by an
	// operator REWRITE compares against a slightly smaller ref set than it did
	// before this slice. That is a read-shape change, not a gate change, and
	// it is not what this switch governs.
	_, second, targetID, store, dbPath := runTwoCuts(t, "sess-sup-off", func(req *Request) {
		req.NoSupersede = true
	})
	if second.SupersededCount != 0 || second.WrittenCount != 0 || second.RejectedCount != 1 {
		t.Fatalf("the kill switch must restore today's rejection: %#v", second)
	}
	if second.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed (D212 near-dup-only)", second.Status)
	}
	meta := runMeta(t, store, second.RunID)
	rejected := meta["rejected_candidates"].([]any)
	reason := rejected[0].(map[string]any)["reason"]
	want := fmt.Sprintf("near-duplicate of observation %d (trigram=0.59)", targetID)
	if reason != want {
		t.Fatalf("reason string must be unchanged:\n got %v\nwant %v", reason, want)
	}
	counts := meta["candidate_reason_counts"].(map[string]any)
	if counts[RejectClassNearDuplicate] != float64(1) {
		t.Fatalf("candidate_reason_counts = %v, want near_duplicate:1", counts)
	}
	if _, present := counts[OutcomeClassSupersede]; present {
		t.Fatal("the kill switch must not emit a supersede counter")
	}
	if state := reviewStateOf(t, dbPath, targetID); state != "proposed" {
		t.Fatalf("the target must stay proposed, got %q", state)
	}
}

func TestOrchestratorSupersedeRefusesSameRunRetryTarget(t *testing.T) {
	// Review round-1 F1, the concrete route, driven through the REAL
	// stale-running takeover rather than a fake: a first attempt writes a
	// proposed observation and its run is left at `running` past the age
	// ceiling (a sweep killed mid-call); the retry over the same window reuses
	// that run id (memoire:D233 CAS takeover), and its session read therefore
	// sees the dead attempt's row as pre-existing session material. A
	// qualifying extension of that row must be REJECTED, not superseded —
	// otherwise one derivation's retry files two rows under one run id and
	// calls it a succession.
	dbPath, store := openTestStore(t)
	ctx := context.Background()
	seedPrompt(t, store, "sess-sup-retry", "e1", 1000, "the detector walks the builder population")

	first, err := newOrchestrator(store).Run(ctx,
		localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
			return obsResponse(req, "fact", cutSoFar), nil
		}), orchRequest("sess-sup-retry"))
	if err != nil || first.WrittenCount != 1 {
		t.Fatalf("first attempt: err=%v out=%#v", err, first)
	}
	rows, _, err := store.ListObservations(ctx, db.ObservationFilter{
		Project: "memoire-test", SessionID: "sess-sup-retry", ReviewState: "proposed",
	})
	if err != nil || len(rows) != 1 {
		t.Fatalf("first attempt's row: %v %d", err, len(rows))
	}
	targetID := rows[0].ID

	// Kill the run mid-call, after the write: status back to `running`, no
	// completion stamp, and an attempt start old enough to clear the
	// stale-running ceiling. Raw SQL because no store verb forges this state —
	// which is the point: it is a crash, not a gesture.
	conn := rawConn(t, dbPath)
	if _, err := conn.ExecContext(ctx, `UPDATE derivation_runs
		SET status = 'running', completed_at_epoch = NULL, created_at_epoch = ?, meta_json = '{}'
		WHERE id = ?`, time.Now().Unix()-7200, first.RunID); err != nil {
		t.Fatalf("forge the killed run: %v", err)
	}

	// The retry: same window, same events, so the same input hash finds that
	// row and takes it over.
	retry, err := newOrchestrator(store).Run(ctx,
		localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
			return obsResponse(req, "fact", cutExtended), nil
		}), orchRequest("sess-sup-retry"))
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if retry.RunID != first.RunID {
		t.Fatalf("the fixture must exercise the takeover: retry run %q != first run %q", retry.RunID, first.RunID)
	}
	if retry.Duplicate {
		t.Fatalf("a taken-over run is not a duplicate short-circuit: %#v", retry)
	}
	if retry.SupersededCount != 0 {
		t.Fatalf("a same-run target must not be superseded: %#v", retry)
	}
	if retry.WrittenCount != 0 || retry.RejectedCount != 1 {
		t.Fatalf("the extension must be rejected as a near-duplicate: %#v", retry)
	}
	meta := runMeta(t, store, retry.RunID)
	rejected, ok := meta["rejected_candidates"].([]any)
	if !ok || len(rejected) != 1 {
		t.Fatalf("rejected_candidates = %v", meta["rejected_candidates"])
	}
	reason := rejected[0].(map[string]any)["reason"]
	want := fmt.Sprintf("near-duplicate of observation %d (trigram=0.59)", targetID)
	if reason != want {
		t.Fatalf("reason string must be the unchanged near-dup one:\n got %v\nwant %v", reason, want)
	}
	if _, present := meta["superseded_candidates"]; present {
		t.Fatal("a same-run rejection must record no supersede")
	}
	if state := reviewStateOf(t, dbPath, targetID); state != "proposed" {
		t.Fatalf("the same-run target must be untouched, got %q", state)
	}
	if n := countRows(t, dbPath, `SELECT count(*) FROM observations WHERE supersedes_id IS NOT NULL`); n != 0 {
		t.Fatalf("no successor may exist, got %d", n)
	}

	// SIBLING ASSERTION: the same target, the same extension, a DIFFERENT run
	// — a new event changes the input hash, so this is a new run — supersedes.
	// Without it the case above would also pass with supersede switched off
	// entirely.
	seedPrompt(t, store, "sess-sup-retry", "e2", 1001, "and the guard vetoes stranded fragments")
	later, err := newOrchestrator(store).Run(ctx,
		localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
			return obsResponse(req, "fact", cutExtended), nil
		}), orchRequest("sess-sup-retry"))
	if err != nil {
		t.Fatalf("later run: %v", err)
	}
	if later.RunID == first.RunID {
		t.Fatalf("the sibling case must run under a different run id, got %q", later.RunID)
	}
	if later.SupersededCount != 1 || later.WrittenCount != 1 {
		t.Fatalf("a target from an earlier run must supersede: %#v", later)
	}
	if state := reviewStateOf(t, dbPath, targetID); state != "superseded" {
		t.Fatalf("the earlier run's target must be retired, got %q", state)
	}
}

func TestDropNearDupRef(t *testing.T) {
	// The ref set must stop offering a row the run just retired, and drop
	// exactly that one, in order.
	refs := []nearDupRef{
		{observationID: 1, reviewState: "proposed"},
		{observationID: 2, reviewState: "proposed"},
		{observationID: 3, reviewState: "proposed"},
	}
	got := dropNearDupRef(refs, 2)
	if len(got) != 2 || got[0].observationID != 1 || got[1].observationID != 3 {
		t.Fatalf("want [1 3] in order, got %+v", got)
	}
	if again := dropNearDupRef(got, 99); len(again) != 2 {
		t.Fatalf("dropping an absent id must change nothing, got %+v", again)
	}
}

func TestOrchestratorProvisionalMarker(t *testing.T) {
	// -provisional rides the run meta AND every written observation's
	// meta_json, and is absent by default.
	dbPath, store := openTestStore(t)
	ctx := context.Background()
	seedPrompt(t, store, "sess-prov", "e1", 1000, "the detector walks the builder population")
	provider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		return obsResponse(req, "fact", cutSoFar), nil
	})

	plain, err := newOrchestrator(store).Run(ctx, provider, orchRequest("sess-prov"))
	if err != nil || plain.WrittenCount != 1 {
		t.Fatalf("plain run: err=%v out=%#v", err, plain)
	}
	if _, present := runMeta(t, store, plain.RunID)["provisional"]; present {
		t.Fatal("an ordinary run must carry no provisional marker")
	}
	rows, _, err := store.ListObservations(ctx, db.ObservationFilter{
		Project: "memoire-test", SessionID: "sess-prov", ReviewState: "proposed",
	})
	if err != nil || len(rows) != 1 {
		t.Fatalf("seed row: %v %d", err, len(rows))
	}
	if _, present := observationMetaOf(t, dbPath, rows[0].ID)["provisional"]; present {
		t.Fatal("an ordinary run's observation must carry no provisional marker")
	}

	// A second session, derived as an open tail.
	seedPrompt(t, store, "sess-prov-open", "e2", 2000, "a fresh window with an open tail")
	openProvider := localFake(func(ctx context.Context, req Request) (ProviderResponse, error) {
		return obsResponse(req, "decision", "chose to mark the open tail provisional for measurement"), nil
	})
	req := orchRequest("sess-prov-open")
	req.Provisional = true
	out, err := newOrchestrator(store).Run(ctx, openProvider, req)
	if err != nil || out.WrittenCount != 1 {
		t.Fatalf("provisional run: err=%v out=%#v", err, out)
	}
	if runMeta(t, store, out.RunID)["provisional"] != true {
		t.Fatal("the run meta must carry provisional:true")
	}
	openRows, _, err := store.ListObservations(ctx, db.ObservationFilter{
		Project: "memoire-test", SessionID: "sess-prov-open", ReviewState: "proposed",
	})
	if err != nil || len(openRows) != 1 {
		t.Fatalf("provisional row: %v %d", err, len(openRows))
	}
	if observationMetaOf(t, dbPath, openRows[0].ID)["provisional"] != true {
		t.Fatal("each written observation must carry provisional:true")
	}
}
