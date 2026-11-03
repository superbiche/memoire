// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package modelderive

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"memoire/services/api/internal/db"
)

// Near-dup gate thresholds, calibrated offline against 7,710 labeled pairs
// from the live hub corpus (tmp/neardup-calibration-2026-08-02.md). The
// two-tier shape is load-bearing: every observed distinct-but-similar pair
// ("PR #67" vs "PR #69", verdict "92%" vs "95%") differs in exactly a
// digit-bearing token, so the moderate tier only fires when digit tokens
// match; the unconditional tier catches round-counter bookkeeping whose
// only delta is an iteration number. Known accepted blindness
// (operator-ruled 2026-08-02): digit-free negation/update pairs ("X
// broken" vs "X fixed") can clear the moderate tier and be rejected.
const (
	// DefaultNearDupLoThreshold rejects at >= this trigram-Jaccard when the
	// two texts' digit-bearing token sets are equal. 0.45 measured strictly
	// better than 0.50 on the corpus: the digit guard holds negative
	// rejections flat (9/3710) from 0.60 all the way down to 0.40, while
	// each step down catches ~75 more true dups per 4000.
	DefaultNearDupLoThreshold = 0.45
	// DefaultNearDupHiThreshold rejects unconditionally at >= this
	// trigram-Jaccard.
	DefaultNearDupHiThreshold = 0.90
)

// Supersede thresholds (Fibery #344). A candidate that HITS the gate above is
// rejected today; when it also RESTATES and GROWS ON its target it supersedes
// it instead — the mid-arc "so far" observation a later cut restates with more
// information keeps its content and its filiation instead of being lost as a
// paraphrase or landing as a second triage row.
//
// These three numbers are UNCALIBRATED first values, in the memoire:D72 /
// memoire:D261 sense: chosen by judgment, not measured. The run metadata logs
// the target id and all three scores for every supersede
// (meta_json.superseded_candidates), and the near-dup rejections keep logging
// theirs, so a week of sweeps yields the distribution both sides of the line
// and the numbers get re-derived from data rather than calcifying.
const (
	// DefaultSupersedeContainment is the minimum share of the TARGET's
	// trigrams the candidate must carry — "the new text says what the old
	// one said". Directional on purpose: Jaccard cannot express containment,
	// because the candidate's own growth depresses it.
	DefaultSupersedeContainment = 0.80
	// DefaultSupersedeGrowthRatio is the minimum share of the CANDIDATE's
	// trigrams that are novel — "and it says more".
	DefaultSupersedeGrowthRatio = 0.15
	// DefaultSupersedeGrowthChars is the minimum RUNE-count growth of the
	// normalized text. The ratio alone admits a long reword; an absolute
	// floor demands real added content. Runes, not bytes, so a non-ASCII
	// observation is not held to a stricter bar than an ASCII one.
	DefaultSupersedeGrowthChars = 40
)

// nearDupProfile is the precomputed comparison form of one text.
type nearDupProfile struct {
	trigrams map[string]bool
	digits   map[string]bool
	// runeLen is the normalized text's rune count, the supersede growth
	// rule's length unit.
	runeLen int
}

// nearDupRef ties a profile to the stored observation row it came from —
// either a pre-existing session observation or a candidate this run just
// inserted (round-2: intra-response refs carry their inserted row id so
// EVERY rejection goes through the same in-transaction liveness check; a
// separate intra-run path bypassed it).
type nearDupRef struct {
	profile       nearDupProfile
	observationID int64
	// reviewState is the row's state as the gate read it (empty for a row
	// this run just inserted, which is `proposed` by construction). It gates
	// supersede only: a machine proposal may replace a machine proposal, but
	// a human-confirmed row is superseded only by a human gesture (rewrite,
	// memoire:D146). Near-dup rejection is state-blind, as before.
	reviewState string
	// intraResponse marks a ref that is an earlier candidate of THIS
	// response. Such a match is never a supersede: the two statements come
	// from one generation over one window, so there is no later cut that
	// learned more — it is the plain duplicate the gate already rejects.
	intraResponse bool
	// sameRun marks a ref written by the run now executing (review round-1
	// F1). "Not from this response" is NOT the same statement as "from an
	// earlier run": a stale-running takeover REUSES the run id, so a retry's
	// session read sees the dead attempt's rows as pre-existing session
	// material. Superseding one of those would put both rows under one run id
	// and call a retry of one derivation a succession of two. Refused on the
	// same terms as intraResponse: an ordinary near-dup rejection.
	sameRun bool
}

// nearDupMatch reports the strongest gate hit for a candidate.
type nearDupMatch struct {
	score float64
	ref   nearDupRef
}

// normalizeForNearDup lowercases and collapses every non-alphanumeric rune
// to a single space — the exact normalization the calibration ran under.
func normalizeForNearDup(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func buildNearDupProfile(text string) nearDupProfile {
	norm := normalizeForNearDup(text)
	return nearDupProfile{
		trigrams: trigramSet(norm),
		digits:   digitTokens(norm),
		runeLen:  utf8.RuneCountInString(norm),
	}
}

func trigramSet(norm string) map[string]bool {
	out := map[string]bool{}
	r := []rune(norm)
	if len(r) < 3 {
		if len(r) > 0 {
			out[string(r)] = true
		}
		return out
	}
	for i := 0; i+3 <= len(r); i++ {
		out[string(r[i:i+3])] = true
	}
	return out
}

// digitTokens returns the tokens of the normalized text that carry at least
// one digit ("67", "5x", "0ec85d6").
func digitTokens(norm string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(norm) {
		for _, r := range w {
			if unicode.IsDigit(r) {
				out[w] = true
				break
			}
		}
	}
	return out
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func stringSetsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// nearDupHit applies the calibrated two-tier policy to one candidate/ref
// pair.
func nearDupHit(cand, ref nearDupProfile) (float64, bool) {
	score := jaccard(cand.trigrams, ref.trigrams)
	if score >= DefaultNearDupHiThreshold {
		return score, true
	}
	if score >= DefaultNearDupLoThreshold && stringSetsEqual(cand.digits, ref.digits) {
		return score, true
	}
	return score, false
}

// findNearDup scans refs for the strongest policy hit against text. Kind is
// deliberately not compared: a paraphrase re-emitted under a different kind
// is still a duplicate.
func findNearDup(text string, refs []nearDupRef) (nearDupMatch, bool) {
	hits := nearDupHits(text, refs)
	if len(hits) == 0 {
		return nearDupMatch{}, false
	}
	return hits[0], true
}

// nearDupHits returns EVERY policy hit against text, strongest first. The
// gate needs all of them, not just the best: a stored match must be
// liveness-revalidated at insert time, and when the best match died
// mid-generation the next one may still be live.
func nearDupHits(text string, refs []nearDupRef) []nearDupMatch {
	cand := buildNearDupProfile(text)
	var hits []nearDupMatch
	for _, ref := range refs {
		if score, hit := nearDupHit(cand, ref.profile); hit {
			hits = append(hits, nearDupMatch{score: score, ref: ref})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	return hits
}

// hitIDs extracts the matched observation ids in score order plus a score
// lookup for rejection reasons.
func hitIDs(hits []nearDupMatch) ([]int64, map[int64]float64) {
	var ids []int64
	scores := map[int64]float64{}
	for _, h := range hits {
		if h.ref.observationID > 0 {
			ids = append(ids, h.ref.observationID)
			scores[h.ref.observationID] = h.score
		}
	}
	return ids, scores
}

// nearDupRefsFromSession precomputes gate references for a session's
// existing observations. currentRunID is the run now executing, so a row it
// already wrote — a dead attempt's, after a stale-running takeover reused the
// id — is marked sameRun and cannot be superseded. An EMPTY run id on the row
// (a rewrite successor has none) never matches: those rows stay eligible under
// the ordinary conditions, which is what they were before F1.
func nearDupRefsFromSession(existing []db.SessionObservationSummary, currentRunID string) []nearDupRef {
	refs := make([]nearDupRef, 0, len(existing))
	for _, obs := range existing {
		refs = append(refs, nearDupRef{
			profile:       buildNearDupProfile(obs.Text),
			observationID: obs.ID,
			reviewState:   obs.ReviewState,
			sameRun:       obs.DerivationRunID != "" && obs.DerivationRunID == currentRunID,
		})
	}
	return refs
}

// supersedeScores measures the two directional quantities the supersede
// verdict needs, over the SAME normalized trigram sets the near-dup policy
// compares:
//
//   - containment: |tri(old) ∩ tri(new)| / |tri(old)| — how much of the older
//     statement the candidate carries ("it restates it");
//   - growth: |tri(new) \ tri(old)| / |tri(new)| — how much of the candidate
//     is new ("and it adds").
//
// An empty target trigram set contains vacuously (containment 1), mirroring
// jaccard's both-empty convention.
func supersedeScores(cand, target nearDupProfile) (containment, growth float64) {
	inter := 0
	for k := range target.trigrams {
		if cand.trigrams[k] {
			inter++
		}
	}
	if len(target.trigrams) == 0 {
		containment = 1
	} else {
		containment = float64(inter) / float64(len(target.trigrams))
	}
	if len(cand.trigrams) == 0 {
		return containment, 0
	}
	novel := 0
	for k := range cand.trigrams {
		if !target.trigrams[k] {
			novel++
		}
	}
	return containment, float64(novel) / float64(len(cand.trigrams))
}

// supersedeVerdict decides whether a candidate that already HIT the near-dup
// policy against ref supersedes it instead of being rejected (Fibery #344).
// The scores are returned whatever the verdict, so a refusal can still be
// explained from run metadata.
//
// All five conditions must hold; the hit itself is condition 1, established by
// the caller. This function never widens what the gate catches — it only
// re-classifies pairs the gate already rejects today.
func supersedeVerdict(cand nearDupProfile, ref nearDupRef) (containment, growth float64, ok bool) {
	containment, growth = supersedeScores(cand, ref.profile)
	switch {
	// (2) a machine proposal may replace a machine proposal; any other
	// state — accepted, disputed, rejected, lapsed, superseded — is a row a
	// human ruled on, and only a human gesture supersedes it.
	case ref.reviewState != "proposed":
		return containment, growth, false
	// (3) the target must come from an EARLIER RUN. Two ways a row fails
	// that: it is an earlier candidate of this response, or it was written by
	// this same run id — which a stale-running takeover makes possible
	// (review round-1 F1). A row with no run id at all (rewrite successor)
	// fails neither.
	case ref.intraResponse, ref.sameRun:
		return containment, growth, false
	// (4) restates.
	case containment < DefaultSupersedeContainment:
		return containment, growth, false
	// (5) grows, in both the relative and the absolute sense. In the hi band
	// (trigram >= 0.90) the ratio cannot be met, so near-identical
	// restatements stay rejected — that is the intended consequence, not a
	// gap.
	case growth < DefaultSupersedeGrowthRatio:
		return containment, growth, false
	case cand.runeLen < ref.profile.runeLen+DefaultSupersedeGrowthChars:
		return containment, growth, false
	}
	return containment, growth, true
}
