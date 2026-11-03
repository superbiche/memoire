// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package modelderive

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Graduated noise genres are the SECOND noise death point (triage lane v1,
// R-B). Default death is at TRIAGE, where derivation keeps emitting and a
// human sees every row; a genre reaches DERIVATION death only on triage
// evidence — a recurring slug over the evidence threshold with a ZERO flip
// set — ratified by the operator in one gesture.
//
// The registry is embedded, not read from disk: the standalone lanes ship
// one binary, and a graduation must be a reviewable commit rather than an
// exec-edit against a live deployment (the memoire:D252 custody lesson).

//go:embed graduated-genres.json
var graduatedGenresJSON []byte

// GraduatedGenre is one genre killed at derivation, with the evidence and
// the record that ratified it carried alongside — a killed genre that
// cannot say why it was killed is indistinguishable from a bug.
type GraduatedGenre struct {
	Slug        string `json:"slug"`
	Instruction string `json:"instruction"`
	Record      string `json:"record"`
	GraduatedOn string `json:"graduated_on"`
	Evidence    struct {
		Occurrences int `json:"occurrences"`
		Judged      int `json:"judged"`
	} `json:"evidence"`
}

// GraduatedRegistry is the on-disk shape, exported so the graduation
// gesture can rewrite the file it is embedded from.
type GraduatedRegistry struct {
	Comment []string         `json:"comment"`
	Genres  []GraduatedGenre `json:"genres"`
}

var (
	graduatedOnce sync.Once
	graduated     []GraduatedGenre
	graduatedErr  error
)

func loadGraduated() {
	var reg GraduatedRegistry
	if err := json.Unmarshal(graduatedGenresJSON, &reg); err != nil {
		graduatedErr = fmt.Errorf("modelderive: graduated-genres.json is malformed: %w", err)
		return
	}
	sort.Slice(reg.Genres, func(i, j int) bool { return reg.Genres[i].Slug < reg.Genres[j].Slug })
	graduated = reg.Genres
}

// GraduatedNoiseGenres returns the ratified genres in a stable order.
func GraduatedNoiseGenres() ([]GraduatedGenre, error) {
	graduatedOnce.Do(loadGraduated)
	return graduated, graduatedErr
}

// GraduatedPromptSuffix is the honesty half: a graduation CHANGES the
// prompt, so the prompt version must change with it, or two materially
// different prompts would share a version and the override-rate and
// derivation ledgers would both lie by one name. With no graduated genres
// the suffix is empty and the version is byte-identical to the pre-triage
// one — a graduation-free deployment regresses by nothing.
func GraduatedPromptSuffix() string {
	g, err := GraduatedNoiseGenres()
	if err != nil || len(g) == 0 {
		return ""
	}
	return fmt.Sprintf("+g%d", len(g))
}

// renderGraduatedGenres writes the never-emit section into a derivation
// prompt. It renders nothing at all when no genre has graduated, so the
// built prompt stays byte-identical to the pre-triage one.
func renderGraduatedGenres(b *strings.Builder) {
	g, err := GraduatedNoiseGenres()
	if err != nil || len(g) == 0 {
		return
	}
	b.WriteString("### Never emit (graduated noise genres)\n\n")
	b.WriteString("Each genre below was killed HERE, at derivation, after the triage lane\n")
	b.WriteString("recorded it repeatedly and the operator kept none of its rows. Do not\n")
	b.WriteString("emit an observation of these genres at all — not even flagged:\n")
	for _, e := range g {
		fmt.Fprintf(b, "- %s: %s\n", e.Slug, e.Instruction)
	}
	b.WriteString("\n")
}
