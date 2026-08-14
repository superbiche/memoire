// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package modelderive

import (
	"sort"
	"strings"
	"unicode"

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

// nearDupProfile is the precomputed comparison form of one text.
type nearDupProfile struct {
	trigrams map[string]bool
	digits   map[string]bool
}

// nearDupRef ties a profile to the stored observation row it came from —
// either a pre-existing session observation or a candidate this run just
// inserted (round-2: intra-response refs carry their inserted row id so
// EVERY rejection goes through the same in-transaction liveness check; a
// separate intra-run path bypassed it).
type nearDupRef struct {
	profile       nearDupProfile
	observationID int64
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
// existing observations.
func nearDupRefsFromSession(existing []db.SessionObservationSummary) []nearDupRef {
	refs := make([]nearDupRef, 0, len(existing))
	for _, obs := range existing {
		refs = append(refs, nearDupRef{
			profile:       buildNearDupProfile(obs.Text),
			observationID: obs.ID,
		})
	}
	return refs
}
