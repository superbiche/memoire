// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package modelderive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"memoire/services/api/internal/db"
	"memoire/services/api/internal/derive"
)

// Run status values written to derivation_runs.status by the orchestrator.
const (
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusPartial   = "partial"
	StatusFailed    = "failed"
)

// ConfidenceSourceModel labels model-reported confidence in observation
// provenance (docs/MODEL-PROVIDER.md "Review And Confidence").
const ConfidenceSourceModel = "model_self_report"

// OrchestratorStore is the store surface the orchestrator needs. *db.Store
// satisfies it. Keeping it narrow lets tests substitute a fake store.
type OrchestratorStore interface {
	MaxEventID(context.Context) (int64, error)
	ListEventsForDerivation(context.Context, db.DerivationEventFilter) ([]db.EventRecord, error)
	ListEventsForDerivationCoverage(context.Context, db.DerivationEventFilter) (int, []db.EventRecord, error)
	ListSessionObservationsForDerivation(ctx context.Context, project, sessionID string, limit int) ([]db.SessionObservationSummary, error)
	EnsureDerivationRun(context.Context, db.DerivationRunInput) (db.DerivationRunResult, error)
	RecordRunInputEvents(context.Context, string, []int64) error
	InsertDerivedObservation(context.Context, db.DerivedObservationInput) (db.DerivedObservationResult, error)
	InsertDerivedObservationUnlessMatchedLive(context.Context, db.DerivedObservationInput, []int64) (db.DerivedObservationResult, int64, error)
	UpdateDerivationRunStatus(context.Context, db.DerivationRunStatusUpdate) error
}

// nearDupCompareLimit caps how many of the session's newest observations the
// near-dup gate compares against. Wider than the prompt-context budget on
// purpose: comparison is cheap Go work, prompt bytes are not.
const nearDupCompareLimit = 2000

// Orchestrator drives a single model-backed derivation run end to end:
// validate -> fetch -> prompt -> hash -> run-dedup -> call -> parse ->
// write-proposed-observations -> update-run-status.
type Orchestrator struct {
	Store OrchestratorStore
	// Timeout bounds the provider Generate call. Zero means no orchestrator-set
	// timeout (the caller's context still applies).
	Timeout time.Duration
	// Guard is the min-content window guard. The zero value applies the package
	// default thresholds; set Disabled to bypass it.
	Guard WindowGuard
	// Now returns the current unix epoch; overridable for deterministic tests.
	Now func() int64
}

// RunOutcome summarizes a completed orchestration for callers and CLI output.
type RunOutcome struct {
	RunID         string
	Status        string
	Duplicate     bool
	WrittenCount  int
	RejectedCount int
	UnknownCount  int
	Refused       bool
	Usage         Usage
	// Skipped is a non-empty marker (e.g. SkipReasonDegenerateWindow) when the
	// orchestrator declined the window before creating a run: no run record, no
	// provider call, RunID and Status empty.
	Skipped string
	// Forced/SupersededRunID report a -force-rerun minting: a new run was
	// created for a window that would otherwise dedup, superseding the named
	// prior run (empty when the window had never been derived).
	Forced          bool
	SupersededRunID string
	// FetchedAtEpoch is when THIS attempt's input fetch happened — the
	// intent-eligibility timestamp (memoire:D165 rounds 2-3): an intent is
	// served only by an attempt whose fetch STRICTLY postdates the notify.
	// Deliberately the attempt's own stamp, never the run row's original
	// created_at (an in-place retry reuses the row but fetches fresh).
	FetchedAtEpoch int64
	// Guard carries the min-content measurements when Skipped is the guard
	// marker, so the caller can log a self-explanatory skip line.
	Guard GuardResult
}

func (o *Orchestrator) now() int64 {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now().Unix()
}

// Run executes the full orchestration flow for one provider/request.
//
// It returns an error only for setup/validation failures and unrecoverable
// store errors. A provider failure, refusal, or malformed response is not a Go
// error: it is recorded as a failed/partial run and reported in RunOutcome.
func (o *Orchestrator) Run(ctx context.Context, provider Provider, req Request) (RunOutcome, error) {
	if o.Store == nil {
		return RunOutcome{}, errors.New("modelderive: orchestrator store is nil")
	}
	// 1. Validate provider/model/window/limits.
	if provider == nil {
		return RunOutcome{}, errors.New("modelderive: nil provider")
	}
	if req.Provider == "" {
		return RunOutcome{}, errors.New("modelderive: request provider is required")
	}
	if provider.Name() != req.Provider {
		return RunOutcome{}, fmt.Errorf("modelderive: provider %q does not match request provider %q", provider.Name(), req.Provider)
	}
	if req.Model == "" {
		return RunOutcome{}, errors.New("modelderive: model is required for model-backed derivation")
	}
	if req.Window.Project == "" {
		return RunOutcome{}, errors.New("modelderive: window project is required")
	}
	if req.Window.StartEpoch > 0 && req.Window.EndEpoch > 0 && req.Window.EndEpoch < req.Window.StartEpoch {
		return RunOutcome{}, errors.New("modelderive: window end_epoch must be >= start_epoch")
	}
	if req.Limits.MaxInputEvents < 0 || req.Limits.MaxPromptBytes < 0 || req.Limits.MaxOutputObservations < 0 {
		return RunOutcome{}, errors.New("modelderive: limits must be non-negative")
	}

	// 2. Enforce data-boundary discipline before prompt construction.
	boundary := provider.DataBoundary()
	if err := validateDataBoundary(provider.Name(), boundary); err != nil {
		return RunOutcome{}, err
	}
	if (provider.Name() == "local-qwen" || provider.Name() == ProviderBYOK) && req.EndpointID == "" {
		return RunOutcome{}, errors.New("modelderive: endpoint_id is required for local-qwen and byok")
	}

	// 3. Fetch source events. fetchedAt is THIS attempt's intent-eligibility
	// stamp (rounds 2-3: never the run row's created_at — retries reuse the
	// row but fetch fresh). MaxEventID bounds the fetch and lands on the run
	// as a diagnostic; coverage authority is the input MANIFEST recorded
	// below (a PG identity id is allocated before commit, so a high-water id
	// cannot prove snapshot membership — round-3 HIGH-1).
	fetchedAt := time.Now().Unix()
	snapshotMaxEventID, err := o.Store.MaxEventID(ctx)
	if err != nil {
		return RunOutcome{}, err
	}
	fetchFilter := db.DerivationEventFilter{
		Project:          req.Window.Project,
		SessionID:        req.Window.SessionID,
		WindowStartEpoch: req.Window.StartEpoch,
		WindowEndEpoch:   req.Window.EndEpoch,
		EventTypes:       CanonicalInputEventTypes(),
		MaxEventID:       snapshotMaxEventID,
		// Covered events never re-enter a prompt: a sweep window on a
		// still-active session overlaps prior completed runs, and
		// re-derivation emits paraphrase duplicates that exact-hash
		// dedupe cannot catch. ForceRerun deliberately sees everything —
		// its whole point is re-deriving a window despite completion.
		ExcludeCovered: !req.ForceRerun,
	}
	events, err := o.Store.ListEventsForDerivation(ctx, fetchFilter)
	if err != nil {
		return RunOutcome{}, err
	}
	if len(events) == 0 && fetchFilter.ExcludeCovered {
		// Distinguish "window fully covered by completed runs" (a clean
		// skip: no run row, no provider call, re-derivable via -force-rerun)
		// from a genuinely empty window (still an error). The classification
		// comes from ONE statement (rounds 1-2, F1/R2-F1/R2-F2): serial
		// probe + re-fetch pairs are separate snapshots, so a late commit
		// or a soft-delete between them yields a false skip. Bound and
		// service timestamp are refreshed TOGETHER so the skip's
		// FetchedAtEpoch describes exactly the read that proved coverage —
		// a stale stamp under a fresh clock would wrongly serve an intent
		// whose event lies outside the examined snapshot.
		fetchedAt = time.Now().Unix()
		snapshotMaxEventID, err = o.Store.MaxEventID(ctx)
		if err != nil {
			return RunOutcome{}, err
		}
		fetchFilter.MaxEventID = snapshotMaxEventID
		total, uncovered, cerr := o.Store.ListEventsForDerivationCoverage(ctx, fetchFilter)
		if cerr != nil {
			return RunOutcome{}, cerr
		}
		if total > 0 && len(uncovered) == 0 {
			return RunOutcome{Skipped: SkipReasonFullyCovered, FetchedAtEpoch: fetchedAt}, nil
		}
		// A raced commit inside the refreshed snapshot is real work:
		// derive it instead of skipping.
		events = uncovered
	}
	if len(events) == 0 {
		return RunOutcome{}, errors.New("modelderive: no source events matched derivation window")
	}
	req.Events = events

	// 3b. Min-content guard: a degenerate window is skipped before any run
	// record or provider call. No derivation_runs row is written — the
	// run-status vocabulary is sealed to the contract states — so the skip
	// marker in RunOutcome is the only trace and the guard re-evaluates on
	// every sweep until the window grows past the thresholds.
	if guard := EvaluateWindowGuard(events, o.Guard); guard.Skip {
		return RunOutcome{Skipped: SkipReasonDegenerateWindow, Guard: guard, FetchedAtEpoch: fetchedAt}, nil
	}

	// 3c. Session read-back for the already-captured PROMPT context
	// (2026-08-02, D211 step 3). This read serves the prompt only — it is
	// necessarily pre-provider and therefore stale by generation time, which
	// is fine for advisory context but NOT for the near-dup gate: the gate
	// re-reads after the provider returns (8c) and revalidates matched-row
	// liveness inside the insert transaction (round-1 finding 1). ForceRerun
	// sees virgin conditions — an A/B rerun's session already contains the
	// prior run's output, so both the context ("do not re-emit") and the
	// gate would suppress exactly the output the rerun exists to regenerate.
	// Sessionless windows have no session history to compare against.
	gateSession := req.Window.SessionID != "" && !req.ForceRerun
	var sessionObs []db.SessionObservationSummary
	if gateSession {
		sessionObs, err = o.Store.ListSessionObservationsForDerivation(ctx, req.Window.Project, req.Window.SessionID, nearDupCompareLimit)
		if err != nil {
			return RunOutcome{}, err
		}
		req.AlreadyCaptured = sessionObs
	}

	// Resolve actual window bounds when not pinned, mirroring the rules path so
	// the run key matches the inspected window.
	startEpoch := req.Window.StartEpoch
	endEpoch := req.Window.EndEpoch
	if startEpoch == 0 {
		startEpoch = events[0].OccurredAtEpoch
	}
	if endEpoch == 0 {
		endEpoch = events[len(events)-1].OccurredAtEpoch
	}

	// 4. Build prompt and input hash.
	prompt, promptMeta, err := BuildPrompt(req)
	if err != nil {
		return RunOutcome{}, err
	}
	inputHash, err := derive.CanonicalInputHash(events)
	if err != nil {
		return RunOutcome{}, err
	}

	// 5. Create or detect a derivation run. The stale-running ceiling is 2× the
	// execute timeout when one is set (a run older than that was killed
	// mid-call, not slow); zero lets the store default apply.
	var staleCeiling int64
	if o.Timeout > 0 {
		staleCeiling = int64((2 * o.Timeout).Seconds())
	}
	runRes, err := o.Store.EnsureDerivationRun(ctx, db.DerivationRunInput{
		Project:                  req.Window.Project,
		SessionID:                req.Window.SessionID,
		Provider:                 req.Provider,
		Model:                    req.Model,
		EndpointID:               req.EndpointID,
		DataBoundary:             boundary,
		DerivationVersion:        req.DerivationVersion,
		PromptVersion:            req.PromptVersion,
		InputHash:                inputHash,
		WindowStartEpoch:         startEpoch,
		WindowEndEpoch:           endEpoch,
		Status:                   StatusRunning,
		StaleRunningRetrySeconds: staleCeiling,
		Force:                    req.ForceRerun,
		SnapshotMaxEventID:       snapshotMaxEventID,
	})
	if err != nil {
		return RunOutcome{}, err
	}

	// 5b. Record this attempt's input manifest. Fresh mints and retry
	// claims record fatally (the run row is claimed; a crash converges via
	// the stale-retry re-record). A DEDUP upgrades best-effort: the
	// identical input_hash proves the fetched set equals the existing run's
	// input, so attaching the manifest to a pre-0015 row is exact — this is
	// how manifest-less legacy rows regain coverage (they provide NONE
	// until upgraded; rounds 3-4).
	eventIDs := make([]int64, 0, len(events))
	for _, ev := range events {
		eventIDs = append(eventIDs, ev.ID)
	}
	if runRes.Duplicate {
		_ = o.Store.RecordRunInputEvents(ctx, runRes.ID, eventIDs)
		// The existing run's status travels with the outcome so intent
		// bookkeeping can serve an intent off a completed-run dedup — but
		// only through this attempt's post-notify fetch (round-1 MED-3,
		// rounds 2-3 eligibility).
		return RunOutcome{RunID: runRes.ID, Duplicate: true, Status: runRes.Status, FetchedAtEpoch: fetchedAt}, nil
	}
	if err := o.Store.RecordRunInputEvents(ctx, runRes.ID, eventIDs); err != nil {
		return RunOutcome{}, fmt.Errorf("record run input manifest: %w", err)
	}

	// 7. Call provider with timeout and limits.
	callCtx := ctx
	if o.Timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}
	resp, genErr := provider.Generate(callCtx, req)

	// 8. Parse response (when the provider produced output).
	var parsed ParsedResponse
	var parseErr error
	if genErr == nil && len(resp.RawResponse) > 0 {
		parsed, parseErr = ParseResponse(resp.RawResponse, req)
	}
	refused := resp.Refusal != ""

	// 8b. Enforce the hard output cap before any write (capture-don't-discard).
	// MaxOutputObservations is the contract's "hard max output observations" cost
	// gate; until this it was only recorded in metadata and never enforced.
	// Observations beyond the cap are moved to RejectedCandidates with their Extra
	// preserved, which also pushes the run to partial via decideStatus (rejected >
	// 0). A 0/unset limit means no implicit cap: docs/MODEL-PROVIDER.md documents
	// no numeric default, and inventing one is out of scope for this pass.
	if genErr == nil && parseErr == nil && req.Limits.MaxOutputObservations > 0 &&
		len(parsed.Observations) > req.Limits.MaxOutputObservations {
		overflow := parsed.Observations[req.Limits.MaxOutputObservations:]
		parsed.Observations = parsed.Observations[:req.Limits.MaxOutputObservations]
		for _, obs := range overflow {
			// The downgrade counter is a stored-row count (memoire:D136 R-1);
			// a capped-out downgraded candidate is not stored.
			if obs.ProvenanceDowngraded {
				parsed.ProvenanceDowngrades--
			}
			parsed.RejectedCandidates = append(parsed.RejectedCandidates, RejectedCandidate{
				Index:          -1,
				Kind:           obs.Kind,
				Text:           obs.Text,
				SourceEventIDs: obs.SourceEventIDs,
				Class:          RejectClassOutputCap,
				Reason:         fmt.Sprintf("exceeds MaxOutputObservations %d", req.Limits.MaxOutputObservations),
				Extra:          obs.Extra,
			})
		}
	}

	// 8c/9. Near-dup gate + insert (2026-08-02, D211 step 3; reshaped by
	// review rounds 1-3): each candidate is compared against the session's
	// observations as read AFTER the provider returned — the pre-provider
	// snapshot is minutes stale by now and judging against it both missed
	// concurrent writes and rejected against rows deleted mid-generation.
	// EVERY rejection then stands on the TRANSACTIONAL LIVENESS DECISION:
	// InsertDerivedObservationUnlessMatchedLive checks the matched rows'
	// liveness inside the same transaction as the would-be insert, and this
	// applies to matches against earlier candidates in this response too —
	// they join the ref set under their inserted row ids. Rejections are
	// capture-don't-discard — text, match target, and score land in run
	// metadata. Under ForceRerun the ref set is empty (virgin conditions),
	// so only intra-response comparisons apply. The residual: two lanes
	// interleaving their write phases within the same milliseconds can
	// still cross-admit — the D211 cross-lane residual class, narrowed from
	// provider-call minutes to the write window, journaled not solved.
	written := 0
	nearDupRejected := 0
	if genErr == nil && parseErr == nil && len(parsed.Observations) > 0 {
		var refs []nearDupRef
		if gateSession {
			gateObs, gerr := o.Store.ListSessionObservationsForDerivation(ctx, req.Window.Project, req.Window.SessionID, nearDupCompareLimit)
			if gerr != nil {
				return RunOutcome{}, gerr
			}
			refs = nearDupRefsFromSession(gateObs)
		}
		kept := parsed.Observations[:0]
		for _, obs := range parsed.Observations {
			hits := nearDupHits(obs.Text, refs)
			reject := func(reason string) {
				nearDupRejected++
				// Keep the stored-row downgrade counter honest, mirroring the
				// output-cap path: a rejected candidate is not a stored row.
				if obs.ProvenanceDowngraded {
					parsed.ProvenanceDowngrades--
				}
				parsed.RejectedCandidates = append(parsed.RejectedCandidates, RejectedCandidate{
					Index:          -1,
					Kind:           obs.Kind,
					Text:           obs.Text,
					SourceEventIDs: obs.SourceEventIDs,
					Class:          RejectClassNearDuplicate,
					Reason:         reason,
					Extra:          obs.Extra,
				})
			}
			matchedIDs, scoreByID := hitIDs(hits)
			insertedID, liveMatch, ierr := o.insertProposedGated(ctx, req, runRes.ID, inputHash, boundary, obs, matchedIDs)
			if ierr != nil {
				return RunOutcome{}, ierr
			}
			if liveMatch > 0 {
				reject(fmt.Sprintf("near-duplicate of observation %d (trigram=%.2f)", liveMatch, scoreByID[liveMatch]))
				continue
			}
			written++
			// The just-inserted row joins the ref set under its REAL id
			// (round-2): a later candidate matching it re-checks its liveness
			// in the same in-transaction path as any session row — the
			// index-only intra-run shortcut bypassed liveness entirely.
			refs = append(refs, nearDupRef{profile: buildNearDupProfile(obs.Text), observationID: insertedID})
			kept = append(kept, obs)
		}
		parsed.Observations = kept
	}

	// 10/11. Decide final status and write run metadata. Near-dup rejections
	// deliberately do NOT degrade the run: the model processed the window
	// correctly, the content just already exists. This matters for coverage —
	// only a COMPLETED run's manifest covers its events (D211), so a response
	// consisting entirely of near-dups must still complete, or the same
	// events would re-enter every future sweep fetch and re-generate the
	// same rejected paraphrases tick after tick.
	otherRejected := len(parsed.RejectedCandidates) - nearDupRejected
	status := decideStatus(genErr, parseErr, refused, written, otherRejected)
	if status == StatusPartial && genErr == nil && parseErr == nil && !refused &&
		written == 0 && nearDupRejected > 0 && otherRejected == 0 && len(parsed.UnknownCandidates) == 0 {
		status = StatusCompleted
	}
	meta := o.buildRunMeta(req, inputHash, boundary, status, promptMeta, resp, parsed, written, runRes.RetryCount, genErr, parseErr)
	// Context provenance: how much session history the dupe defenses saw,
	// and what the prompt actually carried after the yielding budget
	// (rendered entries + truncation live in promptMeta; the builder is the
	// authority). A truncated context is visible here, never silent.
	if gateSession {
		meta["already_captured"] = map[string]any{
			"session_observations": len(sessionObs),
			"prompt_entries":       promptMeta["already_captured_count"],
			"truncated":            promptMeta["already_captured_truncated"],
		}
	}
	// A forced run re-records its provenance in the final metadata (the final
	// write replaces the insert-time meta wholesale). meta["input_hash"] stays
	// canonical; the row's input_hash column carries the #forced-<nonce> salt.
	if runRes.Forced {
		meta["forced"] = true
		meta["canonical_input_hash"] = inputHash
		if runRes.SupersededRunID != "" {
			meta["superseded_run_id"] = runRes.SupersededRunID
		}
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return RunOutcome{}, err
	}
	if err := o.Store.UpdateDerivationRunStatus(ctx, db.DerivationRunStatusUpdate{
		ID:               runRes.ID,
		Status:           status,
		CompletedAtEpoch: o.now(),
		Metadata:         metaJSON,
	}); err != nil {
		return RunOutcome{}, err
	}
	_ = prompt // prompt bytes captured in promptMeta; full prompt is not persisted for v1.

	return RunOutcome{
		RunID:           runRes.ID,
		Status:          status,
		WrittenCount:    written,
		RejectedCount:   len(parsed.RejectedCandidates),
		UnknownCount:    len(parsed.UnknownCandidates),
		Refused:         refused,
		Usage:           resp.Usage,
		Forced:          runRes.Forced,
		SupersededRunID: runRes.SupersededRunID,
		FetchedAtEpoch:  fetchedAt,
	}, nil
}

// insertProposedGated writes one proposed observation unless one of the
// gate-matched rows is still live at insert time (checked inside the insert
// transaction — round-1 finding 1). liveMatch > 0 means no write happened
// and names the row the rejection stands on; otherwise insertedID is the
// new row's id (it joins the gate's ref set). An empty matchedIDs is a
// plain ungated insert.
func (o *Orchestrator) insertProposedGated(ctx context.Context, req Request, runID, inputHash, boundary string, obs CandidateObservation, matchedIDs []int64) (insertedID, liveMatch int64, err error) {
	contentHash, err := derive.CanonicalContentHash(req.Window.Project, obs.Kind, obs.Text)
	if err != nil {
		return 0, 0, err
	}
	metaMap := map[string]any{
		"confidence_source": ConfidenceSourceModel,
		"rationale":         obs.Rationale,
		"source_event_ids":  obs.SourceEventIDs,
		"derivation": map[string]any{
			"provider":                req.Provider,
			"model":                   req.Model,
			"endpoint_id":             req.EndpointID,
			"data_boundary":           boundary,
			"prompt_version":          req.PromptVersion,
			"derivation_version":      req.DerivationVersion,
			"response_schema_version": SchemaVersion,
			"input_hash":              inputHash,
		},
	}
	if len(obs.Extra) > 0 {
		metaMap["model_response_extra"] = obs.Extra
	}
	meta, err := json.Marshal(metaMap)
	if err != nil {
		return 0, 0, err
	}
	confidence := obs.Confidence
	res, liveMatch, err := o.Store.InsertDerivedObservationUnlessMatchedLive(ctx, db.DerivedObservationInput{
		Project:         req.Window.Project,
		SessionID:       req.Window.SessionID,
		Kind:            obs.Kind,
		Text:            obs.Text,
		Confidence:      &confidence,
		ReviewState:     "proposed",
		DerivationRunID: runID,
		ContentHash:     contentHash,
		SourceEventIDs:  obs.SourceEventIDs,
		ModelDerived:    true,
		Metadata:        meta,
		Provenance:      obs.Provenance,
		NoiseCandidate:  obs.NoiseCandidate,
	}, matchedIDs)
	return res.ID, liveMatch, err
}

// candidateReasonCounts aggregates one counter per rejection class.
// Unknown-kind audit candidates get their own counter, and a syntactically
// valid response carrying zero candidates gets an explicit empty_observations
// marker — the two cases that used to surface as an unexplainable
// "partial, written=0, rejected=0".
func candidateReasonCounts(parsed ParsedResponse, written int, genErr, parseErr error, refused bool) map[string]int {
	counts := map[string]int{}
	for _, rc := range parsed.RejectedCandidates {
		class := rc.Class
		if class == "" {
			class = RejectClassValidation
		}
		counts[class]++
	}
	if n := len(parsed.UnknownCandidates); n > 0 {
		counts[RejectClassUnknownKind] = n
	}
	if written == 0 && len(counts) == 0 && genErr == nil && parseErr == nil && !refused {
		counts[RejectClassEmptyResponse] = 1
	}
	return counts
}

// decideStatus maps the provider/parse outcome onto the three contract run
// states (docs/MODEL-PROVIDER.md "Failure Modes").
func decideStatus(genErr, parseErr error, refused bool, written, rejected int) string {
	// Provider-level failure or malformed top-level response: no usable output.
	if genErr != nil || parseErr != nil {
		return StatusFailed
	}
	// Refusal with no salvaged observations is a failure; with some, partial.
	if refused {
		if written == 0 {
			return StatusFailed
		}
		return StatusPartial
	}
	if written > 0 && rejected == 0 {
		return StatusCompleted
	}
	// written>0 with rejects, or nothing usable extracted: partial.
	return StatusPartial
}

func (o *Orchestrator) buildRunMeta(
	req Request,
	inputHash, boundary, status string,
	promptMeta map[string]any,
	resp ProviderResponse,
	parsed ParsedResponse,
	written int,
	retryCount int,
	genErr, parseErr error,
) map[string]any {
	total := written + len(parsed.UnknownCandidates) + len(parsed.RejectedCandidates)
	unmappedRatio := 0.0
	if total > 0 {
		unmappedRatio = float64(len(parsed.UnknownCandidates)) / float64(total)
	}

	meta := map[string]any{
		"provider":                req.Provider,
		"model":                   req.Model,
		"endpoint_id":             req.EndpointID,
		"data_boundary":           boundary,
		"prompt_version":          req.PromptVersion,
		"derivation_version":      req.DerivationVersion,
		"response_schema_version": SchemaVersion,
		"input_hash":              inputHash,
		"status":                  status,
		"prompt":                  promptMeta,
		"usage": map[string]any{
			"prompt_tokens":     resp.Usage.PromptTokens,
			"completion_tokens": resp.Usage.CompletionTokens,
			"total_tokens":      resp.Usage.TotalTokens,
		},
		"written_observation_count": written,
		"rejected_candidates":       parsed.RejectedCandidates,
		"unmapped_candidates":       parsed.UnknownCandidates,
		"unmapped_ratio":            unmappedRatio,
	}
	// byok transport identity (docs/BYOK-DERIVE-DESIGN.md §2): dialect is
	// audit metadata like endpoint_id, never part of run idempotency.
	if req.Dialect != "" {
		meta["dialect"] = req.Dialect
	}
	// One counter per rejection class so a "partial, written=0" run is
	// explainable from run metadata alone, without reading every reason string
	// (operator brief 2026-07-10, sweep hygiene).
	if counts := candidateReasonCounts(parsed, written, genErr, parseErr, resp.Refusal != ""); len(counts) > 0 {
		meta["candidate_reason_counts"] = counts
	}
	if retryCount > 0 {
		meta["retry_count"] = retryCount
	}
	// memoire:D136 R-1 gate metric: stored candidates whose model-claimed
	// "observed" provenance was downgraded to "inferred". Nonzero values mean
	// the model is still claiming observation — visible per run, never fatal.
	if parsed.ProvenanceDowngrades > 0 {
		meta["provenance_downgrades"] = parsed.ProvenanceDowngrades
	}

	// Unknown top-level envelope fields captured by the lenient parser are kept
	// as run provenance under response_extra. Per-candidate extras travel inside
	// rejected_candidates / unmapped_candidates via their own Extra fields.
	if len(parsed.Extra) > 0 {
		meta["response_extra"] = parsed.Extra
	}

	if resp.Refusal != "" {
		meta["refusal"] = resp.Refusal
	}
	if genErr != nil {
		meta["failure"] = map[string]any{"error": genErr.Error(), "stage": "provider_call"}
	} else if parseErr != nil {
		meta["failure"] = map[string]any{"error": parseErr.Error(), "stage": "parse"}
	}

	// Artifact retention (docs/MODEL-PROVIDER.md "Model Artifacts"): keep the raw
	// provider response only for failed/partial runs. Stored as a string because a
	// failed run's raw response may not be valid JSON (a malformed-response failure
	// is exactly when retention matters) and must not break run-metadata encoding.
	if (status == StatusFailed || status == StatusPartial) && len(resp.RawResponse) > 0 {
		meta["raw_response"] = string(resp.RawResponse)
	}

	return meta
}
