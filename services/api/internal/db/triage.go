// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Triage lane v1 (R-A through R-D). Suggestions are METADATA about a row,
// never state on it: nothing in this file writes review_state, provenance,
// or can_instruct, and the only mutation it performs is an append to
// observation_triage_suggestions. Every verdict still travels the human
// routes under their D208 compare-and-set guards.

// TriagePromptVersion values carry their stage, which is what keeps the
// ruled idempotency key literally (observation_id, prompt_version) while
// both stages write into one table.
// Stage 1 is at v2: the judge now emits a CONFIDENCE on its verdict, which
// the de-noise pool reads to pin its doubt block. The bump re-opens every
// previously judged row to re-suggestion, because idempotency is per
// (observation, prompt_version) — that is the intended cost of a prompt
// change, and the override-rate table keys on the version so the two
// generations never mix inside one cell.
const (
	TriagePromptStage1 = "triage-stage1-v2"
	TriagePromptStage2 = "triage-stage2-v1"
)

// TriageActorPrefix is the actor namespace every suggestion is written
// under. It is also the prefix the feed actor-gate (R-D) excludes accepts
// on, so a machine that ever gained verdict authority would be excluded
// from the feed by construction rather than by remembering to.
const TriageActorPrefix = "agent:triage-"

// TriageActor renders the actor for a judge model.
func TriageActor(model string) string { return TriageActorPrefix + model }

// Stage-1 verdict set (R-C). Closed: a verdict outside it is refused at
// insert, because a suggestion the surface cannot render is a row nobody
// will ever act on.
const (
	TriageVerdictAccept     = "accept"
	TriageVerdictNoiseLapse = "noise-lapse"
	TriageVerdictLapse      = "lapse"
	TriageVerdictReject     = "reject"
	TriageVerdictConfirm    = "confirm"
	TriageVerdictElevate    = "elevate"
	// TriageVerdictNone is a COVERAGE row, not a suggestion: "the judge
	// evaluated this row and nominated nothing." It exists because
	// "evaluated, no candidate" and "never evaluated" are otherwise the
	// same absence — which makes a re-sweep re-judge the same rows forever
	// and stops paging from ever exhausting the scope. Coverage rows are
	// not stakeable, never render as a suggestion, and are excluded from
	// every override-rate class: nothing was suggested, so nothing can be
	// applied or flipped.
	TriageVerdictNone = "none"
)

// IsCoverageVerdict reports whether a stored verdict is a coverage marker
// rather than a suggestion.
func IsCoverageVerdict(v string) bool { return v == TriageVerdictNone }

// TriageRejectReason is the CLOSED reject taxonomy (R-C). A suggested
// reject without a member of this set is refused: reject 404s the row off
// the read surface, so it never rides a fast gesture and never lands
// without a stated cause.
const (
	TriageRejectFabrication   = "fabrication"
	TriageRejectWrongProject  = "wrong-project"
	TriageRejectDuplicateOf   = "duplicate-of-id"
	TriageRejectOtherExplicit = "other-explicit"
)

// TriageArm names the stage-2 input arms of the measured A/B (R-C):
// arm A adds the raw event window to the observations, arm B is
// observations-only.
const (
	TriageArmA = "a"
	TriageArmB = "b"
)

func stage1Verdicts() map[string]bool {
	return map[string]bool{
		TriageVerdictAccept:     true,
		TriageVerdictNoiseLapse: true,
		TriageVerdictLapse:      true,
		TriageVerdictReject:     true,
		TriageVerdictNone:       true,
	}
}

// Stage 2 suggests only POSITIVES. A "no-elevation" verdict could never
// resolve — the operator agreeing with it does nothing, so it would sit
// pending forever and the metric would carry a class it can never read.
// A row the judge says nothing about is simply not suggested.
func stage2Verdicts() map[string]bool {
	return map[string]bool{
		TriageVerdictConfirm: true,
		TriageVerdictElevate: true,
		TriageVerdictNone:    true,
	}
}

// TriageGenreSlug is the CLOSED grammar for a genre slug. Genre text is
// MODEL OUTPUT — untrusted by construction — and it travels into a rendered
// shell command the operator is invited to run and into a records/ filename.
// Enforcing the grammar at the boundary is what keeps `$(...)`, quotes,
// spaces and path separators out of both.
var TriageGenreSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// ValidTriageGenre reports whether a slug conforms.
func ValidTriageGenre(slug string) bool { return TriageGenreSlug.MatchString(slug) }

func rejectReasons() map[string]bool {
	return map[string]bool{
		TriageRejectFabrication:   true,
		TriageRejectWrongProject:  true,
		TriageRejectDuplicateOf:   true,
		TriageRejectOtherExplicit: true,
	}
}

// TriageSuggestionInput is one judged row.
type TriageSuggestionInput struct {
	ObservationID    int64  `json:"observation_id"`
	Stage            int    `json:"stage"`
	PromptVersion    string `json:"prompt_version"`
	Model            string `json:"model"`
	SuggestedVerdict string `json:"suggested_verdict"`
	// ReasonGenre is the structured half of the reason: a genre slug. On a
	// reject it MUST be a member of the closed taxonomy; elsewhere it is
	// the free noise/lapse genre slug that feeds R-B's graduation
	// suggestions.
	ReasonGenre string `json:"reason_genre,omitempty"`
	ReasonText  string `json:"reason_text,omitempty"`
	Arm         string `json:"arm,omitempty"`
	// Confidence is the judge's confidence in its own verdict, in [0,1].
	// A POINTER because absent and 0.0 are opposite claims: a v1-era
	// suggestion said nothing about confidence, while an explicit 0 is the
	// judge disclaiming its own call. The de-noise doubt block reads only
	// PRESENT-and-low values for exactly that reason.
	Confidence *float64 `json:"confidence,omitempty"`
}

// Validate enforces the closed vocabularies. `other-explicit` REQUIRES
// non-empty free text — the escape hatch has to cost a sentence, or it
// becomes the whole taxonomy.
func (in TriageSuggestionInput) Validate() error {
	if in.ObservationID <= 0 {
		return errors.New("observation_id is required")
	}
	if strings.TrimSpace(in.PromptVersion) == "" || strings.TrimSpace(in.Model) == "" {
		return errors.New("prompt_version and model are required")
	}
	switch in.Stage {
	case 1:
		if !stage1Verdicts()[in.SuggestedVerdict] {
			return fmt.Errorf("stage-1 suggested_verdict must be one of accept, noise-lapse, lapse, reject, none (got %q)", in.SuggestedVerdict)
		}
	case 2:
		if !stage2Verdicts()[in.SuggestedVerdict] {
			return fmt.Errorf("stage-2 suggested_verdict must be one of confirm, elevate, none (got %q)", in.SuggestedVerdict)
		}
		if !IsCoverageVerdict(in.SuggestedVerdict) && in.Arm != TriageArmA && in.Arm != TriageArmB {
			return fmt.Errorf("stage-2 arm must be %q or %q (got %q)", TriageArmA, TriageArmB, in.Arm)
		}
	default:
		return fmt.Errorf("stage must be 1 or 2 (got %d)", in.Stage)
	}
	// A non-conforming genre never reaches storage. The parser degrades it
	// to free text before it gets here (the reason is not lost, only its
	// promotion to a structured slug); this is the last line of defense for
	// any other caller.
	if g := strings.TrimSpace(in.ReasonGenre); g != "" && in.SuggestedVerdict != TriageVerdictReject {
		if !ValidTriageGenre(g) {
			return fmt.Errorf("reason_genre %q is not a valid slug (%s)", g, TriageGenreSlug.String())
		}
	}
	if in.Confidence != nil && (*in.Confidence < 0 || *in.Confidence > 1) {
		return fmt.Errorf("confidence must be in [0,1] (got %v)", *in.Confidence)
	}
	if in.SuggestedVerdict == TriageVerdictReject {
		if !rejectReasons()[in.ReasonGenre] {
			return fmt.Errorf("a reject suggestion requires reason_genre from the closed taxonomy fabrication, wrong-project, duplicate-of-id, other-explicit (got %q)", in.ReasonGenre)
		}
		if in.ReasonGenre == TriageRejectOtherExplicit && strings.TrimSpace(in.ReasonText) == "" {
			return errors.New("reason_genre other-explicit requires non-empty reason_text")
		}
		if in.ReasonGenre == TriageRejectDuplicateOf && strings.TrimSpace(in.ReasonText) == "" {
			return errors.New("reason_genre duplicate-of-id requires reason_text carrying the duplicated observation id")
		}
	}
	return nil
}

// TriageSuggestion is a stored suggestion as read back.
type TriageSuggestion struct {
	ID               int64  `json:"id"`
	ObservationID    int64  `json:"observation_id"`
	Stage            int    `json:"stage"`
	PromptVersion    string `json:"prompt_version"`
	Model            string `json:"model"`
	Actor            string `json:"actor"`
	SuggestedVerdict string `json:"suggested_verdict"`
	ReasonGenre      string   `json:"reason_genre,omitempty"`
	ReasonText       string   `json:"reason_text,omitempty"`
	Arm              string   `json:"arm,omitempty"`
	Confidence       *float64 `json:"confidence,omitempty"`
	CreatedAtEpoch   int64    `json:"created_at_epoch"`
}

// InsertTriageSuggestion appends a suggestion, idempotent on
// (observation_id, prompt_version). A second pass at the same prompt
// version is a silent no-op, which is what makes re-suggestion cheap and
// safe to re-run after a partial sweep.
//
// It reports whether a row was actually INSERTED. A sweep that counted
// ignored duplicates as fresh work would report a full day's suggestions
// every night while writing nothing — the lane would look productive
// precisely when it had stopped producing.
func (s *Store) InsertTriageSuggestion(ctx context.Context, in TriageSuggestionInput) (bool, error) {
	if err := in.Validate(); err != nil {
		return false, err
	}
	prefix, suffix := insertIgnore(s.engine)
	var confidence any
	if in.Confidence != nil {
		confidence = *in.Confidence
	}
	res, err := s.db.ExecContext(ctx, `INSERT `+prefix+`INTO observation_triage_suggestions(
		observation_id, stage, prompt_version, model, actor, suggested_verdict,
		reason_genre, reason_text, arm, confidence, created_at_epoch
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`+suffix,
		in.ObservationID, in.Stage, in.PromptVersion, in.Model, TriageActor(in.Model),
		in.SuggestedVerdict, nullString(strings.TrimSpace(in.ReasonGenre)),
		nullString(strings.TrimSpace(in.ReasonText)), nullString(in.Arm), confidence, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// TriageScopeSession is one session carrying reviewable rows.
type TriageScopeSession struct {
	Project   string `json:"project"`
	SessionID string `json:"session_id"`
	Rows      int64  `json:"rows"`
}

// ListTriageScopeSessions enumerates the sessions holding rows in the scope,
// oldest work first. The judging unit is "session, chunked" (R-C), so the
// sweep walks THIS list and pages inside each entry — a single global
// ordered read would both mix sessions in one prompt and, under any limit,
// return the same capped prefix forever while the rest of the backlog
// starved behind it.
func (s *Store) ListTriageScopeSessions(ctx context.Context, f TriageScopeFilter) ([]TriageScopeSession, error) {
	f.AfterID = 0
	f.Limit = 0
	where, args := f.where("o")
	rows, err := s.rdb.QueryContext(ctx, `SELECT o.project, COALESCE(o.session_id, ''), COUNT(*)
		FROM observations o
		WHERE `+where+`
		GROUP BY o.project, COALESCE(o.session_id, '')
		ORDER BY MIN(o.id) ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TriageScopeSession
	for rows.Next() {
		var t TriageScopeSession
		if err := rows.Scan(&t.Project, &t.SessionID, &t.Rows); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// triageScopePredicate is the R-A scope, stated once. Reviewable rows are
// read IN PLACE: `proposed`, plus `lapsed` rows whose LAST lapse transition
// was rendered by the TTL system actor. A row a human deliberately lapsed
// is EXCLUDED — the human verdict is respected, and the triage pass is an
// organized encounter, not a de-lapse.
//
// "Last" is written even though the state machine currently admits one
// lapse per row: the predicate should keep meaning what it says if the
// machine ever admits a second.
func triageScopePredicate(alias string) string {
	return `(` + alias + `.review_state = 'proposed' OR (` + alias + `.review_state = 'lapsed' AND (
		SELECT rl.actor FROM observation_review_log rl
		WHERE rl.observation_id = ` + alias + `.id AND rl.to_state = 'lapsed'
		ORDER BY rl.occurred_at_epoch DESC, rl.id DESC LIMIT 1
	) = '` + LapseTTLActor + `'))`
}

// The triage POOLS. De-noise is a fourth pool beside all/proposed/lapsed,
// and like the TTL-lapsed pool it is a FILTER over the one scope, never a
// state of its own (memoire:D0254 — a pool rendered as a state invites a
// verb to move rows into and out of it).
//
// The three values are not a UI vocabulary, they are three different scope
// reads and the difference is load-bearing:
//
//   - TriagePoolWhole is the scope as it has always been. The SWEEP and the
//     counters read it. The judge must keep seeing noise-flagged rows — they
//     are precisely the rows a confidence-bearing verdict is wanted on — so
//     a main-pool exclusion leaking into the sweep's filter would starve the
//     de-noise pool of the judgments that resolve it.
//   - TriagePoolMain is the scope MINUS the de-noise members: what
//     all/proposed/lapsed now show.
//   - TriagePoolDeNoise is the de-noise members only.
const (
	TriagePoolWhole   = ""
	TriagePoolMain    = "main"
	TriagePoolDeNoise = "de-noise"
)

// deNoisePredicate is de-noise POOL MEMBERSHIP: a row carrying an UNHANDLED
// stage-1 noise signal.
//
// The signal is either half of the pair — a judge `noise-lapse` suggestion,
// or derivation's own `noise_candidate` flag. Either alone puts the row in
// the pool; their DISAGREEMENT is what the surface pins into its doubt block.
//
// HANDLED is read from the OBSERVATION, not from the suggestion, and that is
// the whole reason this predicate exists separately from the stage-aware
// handled-row walker below. Flag-only rows carry no suggestion to mark
// handled, so a suggestion-keyed exclusion would re-admit every kept
// flag-only row on every load, forever. Two actions handle the signal, one
// per direction: `not-noise` (kept — the row returns to the ordinary queue)
// and `mark-noise` (confirmed noise — the row is done here).
//
// State exclusions are already carried by the scope predicate this ANDs
// with: the pool spans `proposed` plus TTL-`lapsed`, so an accepted row —
// including one accepted through the surface's own escape hatch mid-session —
// and a rejected row both leave the pool BY STATE, with nothing to write.
func deNoisePredicate(alias string) string {
	return `((EXISTS (SELECT 1 FROM observation_triage_suggestions dts
			WHERE dts.observation_id = ` + alias + `.id
			  AND dts.stage = 1
			  AND dts.suggested_verdict = '` + TriageVerdictNoiseLapse + `')
		  OR ` + alias + `.noise_candidate = 1)
		AND NOT EXISTS (SELECT 1 FROM observation_review_log drl
			WHERE drl.observation_id = ` + alias + `.id
			  AND drl.action IN ('` + ReviewActionNotNoise + `', '` + ReviewActionMarkNoise + `')
			  AND drl.actor NOT LIKE 'agent:%'
			  AND drl.actor NOT LIKE 'system:%'))`
}

// triageEncounterExclusion drops rows the operator has already encountered
// through a triage ACTION, independently of whether a suggestion stands on
// them. It rides ALONGSIDE the stage-aware handled-row walker rather than
// inside it, because the two answer different questions: the walker asks
// "was this SUGGESTION ruled on", and a flag-only row has no suggestion to
// ask about.
//
// Without it a flag-only TTL-lapsed row the operator marks noise comes back
// on every load — the mark-noise action leaves it lapsed under the TTL actor,
// so the scope predicate still admits it and no suggestion exists for the
// walker to catch. That row would simply move from the de-noise pool to the
// main lapsed pool and re-pin the page there instead.
//
// `not-noise` is deliberately NOT in this set. A kept row has been rescued
// from the condemned batch, not ruled on: the accept-or-lapse question is
// still open, and the row belongs back in the ordinary queue.
func triageEncounterExclusion(alias string) string {
	return `NOT EXISTS (SELECT 1 FROM observation_review_log erl
		WHERE erl.observation_id = ` + alias + `.id
		  AND erl.action IN ('` + ReviewActionMarkNoise + `', '` + ReviewActionAffirmLapse + `')
		  AND erl.actor NOT LIKE 'agent:%'
		  AND erl.actor NOT LIKE 'system:%')`
}

// TriageScopeFilter narrows a triage read. Session is the triage unit
// (R-C: "session, chunked").
type TriageScopeFilter struct {
	// Stage selects WHICH scope. 1 (or 0) is the R-A reviewable scope:
	// proposed rows plus TTL-lapsed ones. 2 is the human-validated scope
	// the confirm/elevation pass reads. Both share this one filter, and
	// therefore one paging, budget and exclusion mechanism — a second set
	// of machinery for stage 2 is how stage 2 drifted out of correctness
	// the first time.
	Stage       int
	Project     string
	SessionID   string
	ReviewState string // "" = the whole scope; "proposed" / "lapsed" filter within it
	// Pool selects which side of the de-noise split to read. Empty is the
	// WHOLE scope — the sweep's and the counters' read — and is deliberately
	// not a synonym for "main": see the pool constants.
	Pool    string
	Limit   int
	AfterID int64
	// ExcludeSuggestedAtVersion drops rows already carrying a suggestion at
	// this prompt version. The SWEEP sets it so a re-run does not spend the
	// judge's seat re-judging rows it already judged; the UI leaves it empty,
	// because a suggested row is exactly what the operator came to rule on.
	ExcludeSuggestedAtVersion string
	// IncludeRuled keeps rows a human has already ruled on since their
	// suggestion. Default false: a handled row leaves the queue, or it
	// re-pins the same first page forever and the operator can never reach
	// the rest of the backlog.
	IncludeRuled bool
}

// humanValidatedPredicate is the stage-2 base scope: accepted rows a HUMAN
// validated, read from the review log. Rules-born accepted rows carry no
// accept entry at all and machine accepts carry an `agent:*` one, so
// neither qualifies — the trust apex is not built on rows nobody validated.
func humanValidatedPredicate(alias string) string {
	return alias + `.review_state = 'accepted' AND ` + alias + `.kind != 'raw'
		AND EXISTS (SELECT 1 FROM observation_review_log rl
			WHERE rl.observation_id = ` + alias + `.id AND rl.to_state = 'accepted'
			  AND rl.action IS NULL
			  AND rl.actor NOT LIKE 'agent:%' AND rl.actor NOT LIKE 'system:%')`
}

func (f TriageScopeFilter) stage() int {
	if f.Stage == 2 {
		return 2
	}
	return 1
}

func (f TriageScopeFilter) where(alias string) (string, []any) {
	base := triageScopePredicate(alias)
	if f.stage() == 2 {
		base = humanValidatedPredicate(alias)
	}
	clauses := []string{alias + `.deleted_at_epoch IS NULL`, base}
	var args []any
	if f.Project != "" {
		clauses = append(clauses, alias+`.project = ?`)
		args = append(args, f.Project)
	}
	if f.SessionID != "" {
		clauses = append(clauses, alias+`.session_id = ?`)
		args = append(args, f.SessionID)
	}
	// The TTL-lapsed pool is a FILTER within the scope, never a state of
	// its own (R-A): both branches read the same rows through the same
	// predicate, one narrowed. Stage 2 is accepted-only and has no pool.
	if f.stage() == 1 {
		switch f.ReviewState {
		case "proposed", "lapsed":
			clauses = append(clauses, alias+`.review_state = '`+f.ReviewState+`'`)
		}
		// The de-noise split. Stage 2 is accepted-only and has no pool.
		switch f.Pool {
		case TriagePoolDeNoise:
			clauses = append(clauses, deNoisePredicate(alias))
		case TriagePoolMain:
			clauses = append(clauses, `NOT `+deNoisePredicate(alias))
		}
	}
	if f.AfterID > 0 {
		clauses = append(clauses, alias+`.id > ?`)
		args = append(args, f.AfterID)
	}
	if f.ExcludeSuggestedAtVersion != "" {
		clauses = append(clauses, `NOT EXISTS (SELECT 1 FROM observation_triage_suggestions ts
			WHERE ts.observation_id = `+alias+`.id AND ts.prompt_version = ?)`)
		args = append(args, f.ExcludeSuggestedAtVersion)
	}
	if !f.IncludeRuled {
		clauses = append(clauses, ruledSinceSuggestionExclusion(alias, f.stage()))
		if f.stage() == 1 {
			clauses = append(clauses, triageEncounterExclusion(alias))
		}
	}
	return strings.Join(clauses, " AND "), args
}

// ruledSinceSuggestionExclusion drops rows the operator has already handled:
// a row carrying a HUMAN ruling — a transition or a triage encounter action —
// at or after its suggestion FOR THIS STAGE. Without it, the headline
// TTL-lapsed pool never drains: every row the operator affirms comes straight
// back in the same capped first page, and the rows behind it are unreachable.
//
// The STAGE filter is load-bearing, and its absence made stage 2 unreachable
// on the primary operator flow: a stage-1 suggestion followed by the
// operator's accept is exactly what SHOULD arrive at stage 2, and a
// stage-blind predicate read that accept as "already handled" and excluded
// the row from the confirm/elevation pass entirely. A ruling handles the
// stage it answered, not the next question about the same row.
//
// A row with NO suggestion at this stage is never excluded — nothing has been
// proposed about it, so there is nothing it could have been ruled on.
func ruledSinceSuggestionExclusion(alias string, stage int) string {
	return `NOT EXISTS (
		SELECT 1 FROM observation_triage_suggestions ts
		JOIN observation_review_log rl ON rl.observation_id = ts.observation_id
		WHERE ts.observation_id = ` + alias + `.id
		  AND ts.stage = ` + strconv.Itoa(stage) + `
		  AND rl.occurred_at_epoch >= ts.created_at_epoch
		  AND ` + humanRulingPredicate + `)`
}

// humanRulingPredicate is "a HUMAN ruled on this row", stated once and
// reused by every reader. Two shapes count:
//
//   - a real state transition (action IS NULL), and
//   - a TRIAGE encounter action (mark-noise / triage-affirm-lapse /
//     triage-decline-elevation), which is how the operator rules on a row
//     whose state already says what they want it to say.
//
// The older governance flag edits (evidence-only, restrict-scope,
// mark-stale) are EXCLUDED: they write from_state == to_state like the
// triage actions do, so counting them would score an unrelated flag edit as
// agreement with — or a flip of — a suggested verdict.
//
// `agent:*` and `system:*` are both excluded: a machine verdict is not a
// ruling, and a timer lapsing a judge-flagged row is the timer agreeing
// with the judge, not the operator.
const humanRulingPredicate = `rl.actor NOT LIKE 'agent:%'
	  AND rl.actor NOT LIKE 'system:%'
	  AND (rl.action IS NULL OR rl.action IN ('` + ReviewActionMarkNoise + `', '` +
	ReviewActionAffirmLapse + `', '` + ReviewActionDeclineElevation + `', '` +
	ReviewActionAffirmCurrent + `'))`

// humanRulingWithKeepPredicate is the MEASUREMENT side of the same question,
// and it differs from the queue side by exactly one conditional clause: a
// `not-noise` keep counts as a ruling ONLY against a suggestion that actually
// claimed noise.
//
// The condition is the metric hygiene. A keep on a flag-only row overrides no
// judge verdict — there was none — and a keep on a row the judge wanted
// ACCEPTED is not a ruling on that accept either; counting either would
// inflate the judge's error rate with encounters that said nothing about what
// it suggested. Correlating on `ts.suggested_verdict` is what keeps the join
// honest, and it is why this predicate is only ever used where `ts` is in
// scope.
//
// The queue side must NOT carry the clause: a kept row returns to the
// ordinary triage queue with its accept-or-lapse question still open, and
// scoring it as handled would make it unreachable.
const humanRulingWithKeepPredicate = humanRulingPredicate + `
	  OR (rl.actor NOT LIKE 'agent:%' AND rl.actor NOT LIKE 'system:%'
	      AND rl.action = '` + ReviewActionNotNoise + `'
	      AND ts.suggested_verdict = '` + TriageVerdictNoiseLapse + `')`

// firstHumanRuling renders the correlated subquery reading the FIRST human
// ruling to land on a suggestion's row, as `<to_state>|<action>`.
//
// ONE subquery, not two, because the two halves must describe the SAME log
// row: independently-ordered subqueries could resolve a tie differently and
// report a state from one row with an action from another. Both vocabularies
// are closed identifier sets, so `|` cannot appear inside either half.
func firstHumanRuling(table string) string {
	return `(SELECT rl.to_state || '|' || COALESCE(rl.action, '')
		FROM observation_review_log rl
		WHERE rl.observation_id = ` + table + `.observation_id
		  AND rl.occurred_at_epoch >= ` + table + `.created_at_epoch
		  AND (` + humanRulingWithKeepPredicate + `)
		ORDER BY rl.occurred_at_epoch ASC, rl.id ASC LIMIT 1)`
}

// splitRuling unpacks what firstHumanRuling returns. An empty action means a
// real state transition.
func splitRuling(landed string) (toState, action string) {
	toState, action, _ = strings.Cut(landed, "|")
	return toState, action
}

// TriageRow is one row of the triage surface: the observation plus the
// suggestions standing on it.
type TriageRow struct {
	Observation ObservationRecord  `json:"observation"`
	Suggestions []TriageSuggestion `json:"suggestions"`
	// RawEvents carries the source-event window for the stage-2 A/B's arm
	// A only. It is populated on demand (LoadRawEvents), never by the
	// ordinary scope read: arm B is observations-only, and the measurement
	// is worthless if both arms silently see the same input.
	RawEvents []string `json:"raw_events,omitempty"`
}

// ListTriageScope reads rows in the R-A scope with their suggestions
// attached, oldest id first so a chunked sweep can page deterministically.
func (s *Store) ListTriageScope(ctx context.Context, f TriageScopeFilter) ([]TriageRow, error) {
	where, args := f.where("o")
	limit := normalizeLimit(f.Limit, 50, 500)
	rows, err := s.rdb.QueryContext(ctx, `SELECT o.id, o.event_id, o.project, o.session_id, o.kind, o.text,
		o.source_adapter, o.event_type, o.occurred_at_epoch, o.created_at_epoch, o.deleted_at_epoch,
		o.principal_id, o.confidence, o.review_state, o.derivation_run_id, o.content_hash,
		o.provenance, o.can_instruct, o.can_evidence, o.needs_confirmation, o.noise_candidate, o.supersedes_id
		FROM observations o
		WHERE `+where+`
		ORDER BY o.id ASC
		LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TriageRow
	for rows.Next() {
		rec, err := scanObservationRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, TriageRow{Observation: rec})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		sugg, err := s.listSuggestionsFor(ctx, out[i].Observation.ID)
		if err != nil {
			return nil, err
		}
		out[i].Suggestions = sugg
	}
	return out, nil
}

func (s *Store) listSuggestionsFor(ctx context.Context, observationID int64) ([]TriageSuggestion, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, observation_id, stage, prompt_version, model, actor,
		suggested_verdict, COALESCE(reason_genre, ''), COALESCE(reason_text, ''), COALESCE(arm, ''),
		confidence, created_at_epoch
		FROM observation_triage_suggestions
		WHERE observation_id = ?
		ORDER BY id ASC`, observationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TriageSuggestion
	for rows.Next() {
		var t TriageSuggestion
		// NULL confidence stays NULL through to the client: a v1-era
		// suggestion made no confidence claim, and coercing that absence to
		// 0 would read as the judge disclaiming a verdict it never doubted.
		var confidence sql.NullFloat64
		if err := rows.Scan(&t.ID, &t.ObservationID, &t.Stage, &t.PromptVersion, &t.Model, &t.Actor,
			&t.SuggestedVerdict, &t.ReasonGenre, &t.ReasonText, &t.Arm, &confidence, &t.CreatedAtEpoch); err != nil {
			return nil, err
		}
		if confidence.Valid {
			v := confidence.Float64
			t.Confidence = &v
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// LoadRawEvents fills RawEvents for the given rows from the existing
// observation_source_events provenance edges — the same join D92 surfaces
// on listings, read as text. Arm A of the stage-2 A/B is the only caller.
func (s *Store) LoadRawEvents(ctx context.Context, rows []TriageRow, perRow int) error {
	if perRow <= 0 {
		perRow = 10
	}
	for i := range rows {
		r, err := s.rdb.QueryContext(ctx, `SELECT COALESCE(e.text, '')
			FROM observation_source_events ose
			JOIN events e ON e.id = ose.event_id
			WHERE ose.observation_id = ? AND e.deleted_at_epoch IS NULL
			ORDER BY e.occurred_at_epoch ASC, e.id ASC
			LIMIT ?`, rows[i].Observation.ID, perRow)
		if err != nil {
			return err
		}
		var texts []string
		for r.Next() {
			var t string
			if err := r.Scan(&t); err != nil {
				r.Close()
				return err
			}
			if strings.TrimSpace(t) != "" {
				texts = append(texts, t)
			}
		}
		if err := r.Err(); err != nil {
			r.Close()
			return err
		}
		r.Close()
		rows[i].RawEvents = texts
	}
	return nil
}

// TriageStage2Arm assigns the stage-2 input arm DETERMINISTICALLY, by
// observation id parity. Random assignment would break the lane's own
// idempotency promise: a re-suggestion pass would land a row in the other
// arm and the measurement would compare a moving target. Parity over a
// monotonic id is balanced in expectation and reproducible from the row
// alone, so the arm can be recomputed rather than remembered.
func TriageStage2Arm(observationID int64) string {
	if observationID%2 == 0 {
		return TriageArmA
	}
	return TriageArmB
}

// ListHumanValidatedObservations is the stage-2 scope (R-C): rows a HUMAN
// validated. It is the shared scope read with Stage 2 — one mechanism, so
// paging, budget and the handled-row exclusion behave identically in both
// stages rather than drifting apart.
func (s *Store) ListHumanValidatedObservations(ctx context.Context, project, sessionID string, limit int) ([]ObservationRecord, error) {
	rows, err := s.ListTriageScope(ctx, TriageScopeFilter{
		Stage: 2, Project: project, SessionID: sessionID, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]ObservationRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Observation)
	}
	return out, nil
}

// ListTriageValidatedRows is ListHumanValidatedObservations with the
// stage-2 suggestions attached — the shape the stage-2 surface renders.
func (s *Store) ListTriageValidatedRows(ctx context.Context, project, sessionID string, limit int) ([]TriageRow, error) {
	return s.ListTriageScope(ctx, TriageScopeFilter{
		Stage: 2, Project: project, SessionID: sessionID, Limit: limit,
	})
}

// TriageScopeCounts are the triage surface's OWN counters. Lapsed rows are
// excluded from the queue counters by law (memoire:D146 R-6), so the
// surface computes its scope here rather than reusing them.
type TriageScopeCounts struct {
	// Proposed, TTLLapsed and Total are MAIN-pool counts: they exclude the
	// de-noise members, because those rows are no longer work the main pools
	// present. A backlog number that silently included a separately-worked
	// pool would misstate both.
	Proposed  int64 `json:"proposed"`
	TTLLapsed int64 `json:"ttl_lapsed"`
	Total     int64 `json:"total"`
	// DeNoise is the fourth pool's size — the split the counts strip shows.
	DeNoise int64 `json:"de_noise"`
	// NoiseFlagged and Suggested are WHOLE-scope diagnostics, deliberately
	// not split: they answer "how much of the scope has derivation flagged /
	// has the judge opined on", which is a question about the scope, not
	// about a pool's workload.
	NoiseFlagged int64 `json:"noise_flagged"`
	Suggested    int64 `json:"suggested"`
}

// TriageScopeCountsFor computes the scope counters for a project/session
// narrowing (both optional). It reads the WHOLE scope once and splits it with
// CASE expressions, so the pool counts cannot drift from each other the way
// four separately-filtered queries could.
func (s *Store) TriageScopeCountsFor(ctx context.Context, project, sessionID string) (TriageScopeCounts, error) {
	f := TriageScopeFilter{Project: project, SessionID: sessionID}
	where, args := f.where("o")
	denoise := deNoisePredicate("o")
	var c TriageScopeCounts
	err := s.rdb.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN o.review_state = 'proposed' AND NOT `+denoise+` THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN o.review_state = 'lapsed' AND NOT `+denoise+` THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN NOT `+denoise+` THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN `+denoise+` THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN o.noise_candidate = 1 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN EXISTS (
			SELECT 1 FROM observation_triage_suggestions ts WHERE ts.observation_id = o.id
			  AND ts.suggested_verdict != '`+TriageVerdictNone+`'
		) THEN 1 ELSE 0 END), 0)
		FROM observations o WHERE `+where, args...).
		Scan(&c.Proposed, &c.TTLLapsed, &c.Total, &c.DeNoise, &c.NoiseFlagged, &c.Suggested)
	return c, err
}

// ActorClass folds a review-log actor into the CLOSED three-value
// vocabulary the metrics split uses (R-D). Compiled-in enum values only:
// this is what may cross the unauthenticated /metrics boundary under
// memoire:D224, never the actor string itself.
func ActorClass(actor string) string {
	switch {
	case strings.HasPrefix(actor, TriageActorPrefix), strings.HasPrefix(actor, "agent:"):
		return "agent"
	case strings.HasPrefix(actor, "system:"):
		return "system"
	default:
		return "human"
	}
}

// ActorClasses is the closed vocabulary, in render order.
func ActorClasses() []string { return []string{"human", "system", "agent"} }

// ReviewTransitionCount is one cell of the actor-split review-log census.
type ReviewTransitionCount struct {
	ToState    string `json:"to_state"`
	ActorClass string `json:"actor_class"`
	Count      int64  `json:"count"`
}

// CountReviewTransitionsByActorClass is the R-D metrics actor split: EVERY
// review-log-derived metric reads through this shape, so a machine verdict
// can never be counted as a human one. The fold to the closed actor class
// happens in Go, not in SQL, so no stored actor string reaches a caller
// that might render it on the unauthenticated surface.
//
// It counts TRANSITIONS — rows with no `action` — and excludes every
// flag-verb entry. A flag-verb row writes from_state == to_state and is not
// a transition at all, so counting one would report a state change that
// never happened; and because the action route carries no idempotency key,
// a lost-response retry would then publish one encounter as three human
// lapses on the unauthenticated /metrics surface. Real transitions cannot
// duplicate — the engine refuses from == to and the D208 compare-and-set
// admits one winner — so excluding actions is the whole guard, with no
// DISTINCT needed.
func (s *Store) CountReviewTransitionsByActorClass(ctx context.Context) ([]ReviewTransitionCount, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT to_state, actor, COUNT(*)
		FROM observation_review_log WHERE action IS NULL GROUP BY to_state, actor`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	folded := map[[2]string]int64{}
	for rows.Next() {
		var toState, actor string
		var n int64
		if err := rows.Scan(&toState, &actor, &n); err != nil {
			return nil, err
		}
		folded[[2]string{toState, ActorClass(actor)}] += n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []ReviewTransitionCount
	for _, state := range []string{"accepted", "rejected", "lapsed", "disputed", "superseded", "soft_deleted", "proposed"} {
		for _, class := range ActorClasses() {
			if n, ok := folded[[2]string{state, class}]; ok {
				out = append(out, ReviewTransitionCount{ToState: state, ActorClass: class, Count: n})
			}
		}
	}
	return out, nil
}

// TriageOverrideRate is one cell of the override-rate measurement (R-D),
// split by verdict class, prompt version AND stage-2 input arm.
type TriageOverrideRate struct {
	PromptVersion string `json:"prompt_version"`
	Stage         int    `json:"stage"`
	Verdict       string `json:"verdict"`
	// Arm is the stage-2 input treatment, read from the STORED column on the
	// suggestion row rather than re-derived: the arm a suggestion was judged
	// under is a historical fact about that pass, and re-deriving it would
	// silently re-label old rows if the assignment rule ever changed. Empty
	// for stage 1, which has no arms.
	Arm       string `json:"arm,omitempty"`
	Suggested int64  `json:"suggested"`
	Applied   int64  `json:"applied"`
	Flipped   int64  `json:"flipped"`
	Pending   int64  `json:"pending"`
}

// stage1Outcome maps a stage-1 suggested verdict onto the review-log
// to_state a human APPLYING it would write.
func stage1Outcome(verdict string) (toState string, ok bool) {
	switch verdict {
	case TriageVerdictAccept:
		return "accepted", true
	case TriageVerdictLapse, TriageVerdictNoiseLapse:
		return "lapsed", true
	case TriageVerdictReject:
		return "rejected", true
	}
	return "", false
}

// TriageOverrideRates computes suggested->applied vs suggested->flipped
// from source: the suggestion, and the FIRST HUMAN ruling that followed it.
// Nothing is stored back (memoire:D245 — a stored outcome would be a cache
// that can lie the moment a verdict lands from another surface).
//
// `system:*` is excluded as carefully as `agent:*`. A re-armed review TTL
// (memoire:D0253's own un-pause condition) lapsing a judge-flagged row is
// not the operator agreeing with the judge — it is the timer. Counting it
// as agreement would let the mechanism D0253 paused manufacture the
// override rate, and worse, walk a genre across the graduation threshold
// with a zero flip set that no human ever looked at. A falsifier a timer
// can feed is not a falsifier.
//
// Stage 2 resolves against the row's CURRENT governance triple rather than
// a to_state, because confirm and elevate are a provenance promotion and a
// can_instruct flip, not review-state transitions — but only once a human
// ruling exists on the row, so an untouched row stays honestly pending.
func (s *Store) TriageOverrideRates(ctx context.Context) ([]TriageOverrideRate, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT ts.prompt_version, ts.stage, ts.suggested_verdict,
		COALESCE(ts.arm, ''), o.provenance, o.can_instruct, `+firstHumanRuling("ts")+`
		FROM observation_triage_suggestions ts
		JOIN observations o ON o.id = ts.observation_id
		WHERE ts.suggested_verdict != '`+TriageVerdictNone+`'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type key struct {
		pv      string
		stage   int
		verdict string
		arm     string
	}
	agg := map[key]*TriageOverrideRate{}
	var order []key
	for rows.Next() {
		var pv, verdict, arm string
		var stage int
		var provenance sql.NullString
		var canInstruct int
		var landed *string
		if err := rows.Scan(&pv, &stage, &verdict, &arm, &provenance, &canInstruct, &landed); err != nil {
			return nil, err
		}
		k := key{pv, stage, verdict, arm}
		cell, ok := agg[k]
		if !ok {
			cell = &TriageOverrideRate{PromptVersion: pv, Stage: stage, Verdict: verdict, Arm: arm}
			agg[k] = cell
			order = append(order, k)
		}
		cell.Suggested++
		if landed == nil {
			cell.Pending++
			continue
		}
		toState, action := splitRuling(*landed)
		applied := false
		switch {
		case action == ReviewActionNotNoise:
			// A keep is a FLIP of the noise call by definition, and the
			// row's state cannot say so: on a TTL-lapsed row the action
			// logs to_state == from_state == `lapsed`, which is exactly
			// the outcome a noise-lapse suggestion wanted. Reading the
			// state alone would score every rescued row as agreement.
		case stage == 2:
			switch verdict {
			case TriageVerdictConfirm:
				applied = provenance.String == "user_confirmed"
			case TriageVerdictElevate:
				applied = canInstruct == 1
			}
		default:
			if want, ok := stage1Outcome(verdict); ok {
				applied = toState == want
			}
		}
		if applied {
			cell.Applied++
		} else {
			cell.Flipped++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]TriageOverrideRate, 0, len(order))
	for _, k := range order {
		out = append(out, *agg[k])
	}
	return out, nil
}

// TriageGenreCandidate is one graduation candidate (R-B): a recurring
// noise/lapse genre slug, with the evidence that decides it.
type TriageGenreCandidate struct {
	Genre string `json:"genre"`
	// Occurrences is how many suggestions named this genre.
	Occurrences int64 `json:"occurrences"`
	// FlipSet is the observation ids where the operator did NOT lapse the
	// row the genre was claimed on. A non-empty flip set is the falsifier:
	// graduation REFUSES on it. Graduating a genre kills it at derivation,
	// so a single row the operator kept is a row a graduation would have
	// destroyed unseen.
	FlipSet []int64 `json:"flip_set"`
	// Judged is how many of the occurrences the operator has ruled on at
	// all. A genre nobody has judged has no evidence, only volume.
	Judged int64 `json:"judged"`
}

// TriageGraduationThreshold is the evidence floor a genre must cross to be
// SUGGESTED for graduation (R-B: "YOU propose the threshold; justify it").
//
// Twenty judged occurrences with a zero flip set. The justification is a
// binomial one: if the genre's true keep-rate were as high as 10% — one
// row in ten that graduation would silently destroy — the probability of
// seeing twenty consecutive lapses is 0.9^20 ≈ 12%, so the floor already
// makes a 10%-harmful genre unlikely to reach the operator, while staying
// reachable inside a few days of a lane that judges thousands of rows.
// Anything much lower buys speed with exactly the error this gate exists to
// prevent; anything much higher makes graduation unreachable and the
// mechanism decorative. The number is JUDGMENT, not measurement, and is
// meant to be re-derived from the first weeks of override data.
const TriageGraduationThreshold = 20

// TriageGraduationCandidates lists genres at or over the evidence
// threshold, each carrying its occurrence count and its flip set so the
// operator rules on evidence rather than on a slug.
func (s *Store) TriageGraduationCandidates(ctx context.Context) ([]TriageGenreCandidate, error) {
	all, err := s.triageAllGenres(ctx)
	if err != nil {
		return nil, err
	}
	var out []TriageGenreCandidate
	for _, c := range all {
		if c.Judged >= TriageGraduationThreshold {
			out = append(out, c)
		}
	}
	return out, nil
}

// TriageAllGenres lists every genre the judge has ever claimed, with its
// evidence, regardless of threshold. It feeds the manual noise input's
// autocomplete: a fragmented slug vocabulary weakens the graduation
// evidence count, and converging at input time needs no reconciliation
// machinery over the append-only logs.
func (s *Store) TriageAllGenres(ctx context.Context) ([]TriageGenreCandidate, error) {
	return s.triageAllGenres(ctx)
}

// TriageNoiseReasonPrefix is how the operator names a genre the judge
// MISSED. The triage surface's manual noise-flag input writes it into the
// lapse verdict's reason_text — an existing, audited field — rather than
// widening the closed review-action vocabulary or minting a machine-actor
// suggestion on the operator's behalf.
const TriageNoiseReasonPrefix = "noise:"

// TriageOperatorGenre is one genre the OPERATOR named while lapsing.
type TriageOperatorGenre struct {
	Genre string `json:"genre"`
	Count int64  `json:"count"`
	// JudgeKnows reports whether the judge has ever claimed this slug. A
	// genre the operator keeps naming and the judge never does is PROMPT
	// FEEDBACK — the classifier has a blind spot with a name.
	JudgeKnows bool `json:"judge_knows"`
}

// TriageOperatorGenres lists the genres the operator named on lapses.
//
// These deliberately do NOT count toward graduation evidence. Graduation
// rests on a falsifiable claim — the judge asserts a genre, and the
// operator either lapses the row or keeps it, which is what makes a flip
// set possible. An operator-named genre only ever appears on rows the
// operator was already lapsing, so folding it in would inflate the
// evidence with rows that can never flip and quietly gut the falsifier.
func (s *Store) TriageOperatorGenres(ctx context.Context) ([]TriageOperatorGenre, error) {
	// Two shapes carry the operator's genre: a human LAPSE whose reason
	// names it (a proposed row), and a mark-noise ACTION (an already-lapsed
	// row, where no transition is available to carry it).
	// COUNT(DISTINCT observation_id), not COUNT(*): the action route carries
	// no idempotency key (D208 rejected that machinery for the verdict
	// seam), so a lost-response retry can append the same encounter twice.
	// Counting LOG ROWS would let one duplicate delivery inflate a genre's
	// standing; counting the ROWS THE GENRE WAS NAMED ON cannot.
	rows, err := s.rdb.QueryContext(ctx, `SELECT COALESCE(rl.reason_text, ''), COUNT(DISTINCT rl.observation_id)
		FROM observation_review_log rl
		WHERE (
		    (rl.to_state = 'lapsed' AND rl.action IS NULL)
		    OR rl.action = '`+ReviewActionMarkNoise+`'
		  )
		  AND rl.actor NOT LIKE 'agent:%'
		  AND rl.actor NOT LIKE 'system:%'
		  AND rl.reason_text LIKE '`+TriageNoiseReasonPrefix+`%'
		GROUP BY 1 ORDER BY 2 DESC, 1 ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int64{}
	var order []string
	for rows.Next() {
		var reason string
		var n int64
		if err := rows.Scan(&reason, &n); err != nil {
			return nil, err
		}
		slug := strings.TrimSpace(strings.TrimPrefix(reason, TriageNoiseReasonPrefix))
		if slug == "" {
			continue
		}
		if _, seen := counts[slug]; !seen {
			order = append(order, slug)
		}
		counts[slug] += n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	judged, err := s.triageAllGenres(ctx)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, g := range judged {
		known[g.Genre] = true
	}
	out := make([]TriageOperatorGenre, 0, len(order))
	for _, slug := range order {
		out = append(out, TriageOperatorGenre{Genre: slug, Count: counts[slug], JudgeKnows: known[slug]})
	}
	return out, nil
}

// TriageGenreEvidence reads one genre's evidence regardless of threshold —
// the read a graduation attempt makes before it refuses or proceeds.
func (s *Store) TriageGenreEvidence(ctx context.Context, genre string) (TriageGenreCandidate, error) {
	all, err := s.triageAllGenres(ctx)
	if err != nil {
		return TriageGenreCandidate{}, err
	}
	for _, c := range all {
		if c.Genre == genre {
			return c, nil
		}
	}
	return TriageGenreCandidate{Genre: genre}, nil
}

func (s *Store) triageAllGenres(ctx context.Context) ([]TriageGenreCandidate, error) {
	// Ordered oldest-claim-first so the dedup below keeps a genre's FIRST
	// claim on each row: a ruling that landed after that claim is a ruling
	// on the genre, and re-reading it from a later re-suggestion would
	// silently narrow the window in which the operator could have answered.
	rows, err := s.rdb.QueryContext(ctx, `SELECT ts.reason_genre, ts.observation_id, `+
		firstHumanRuling("ts")+`
		FROM observation_triage_suggestions ts
		WHERE ts.reason_genre IS NOT NULL AND ts.reason_genre != ''
		  AND ts.suggested_verdict IN ('`+TriageVerdictNoiseLapse+`', '`+TriageVerdictLapse+`')
		ORDER BY ts.reason_genre ASC, ts.observation_id ASC, ts.created_at_epoch ASC, ts.id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byGenre := map[string]*TriageGenreCandidate{}
	var order []string
	// Evidence is counted in ROWS, not in suggestion rows. A prompt-version
	// bump re-opens every judged row to re-suggestion, so the same genre can
	// be claimed on one observation once per version; counting the claims
	// would inflate a genre past the graduation threshold on a prompt change
	// alone, and would push duplicate ids into the flip set. The falsifier
	// asks about rows the operator kept, so rows are what it counts.
	seen := map[[2]any]bool{}
	for rows.Next() {
		var genre string
		var obsID int64
		var landed *string
		if err := rows.Scan(&genre, &obsID, &landed); err != nil {
			return nil, err
		}
		if seen[[2]any{genre, obsID}] {
			continue
		}
		seen[[2]any{genre, obsID}] = true
		c, ok := byGenre[genre]
		if !ok {
			c = &TriageGenreCandidate{Genre: genre}
			byGenre[genre] = c
			order = append(order, genre)
		}
		c.Occurrences++
		if landed == nil {
			continue
		}
		c.Judged++
		// A `not-noise` keep is a flip whatever the row's state says. On a
		// TTL-lapsed row the keep logs to_state `lapsed`, so a state-only
		// read would drop it from the flip set — and a graduation would then
		// destroy, unseen, the very genre the operator rescued a row from.
		toState, action := splitRuling(*landed)
		if action == ReviewActionNotNoise || toState != "lapsed" {
			c.FlipSet = append(c.FlipSet, obsID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]TriageGenreCandidate, 0, len(order))
	for _, g := range order {
		out = append(out, *byGenre[g])
	}
	return out, nil
}
