// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// memoire-triage drives the triage lane (triage lane v1, R-A through R-D):
// the daily judging sweep that appends SUGGESTIONS, the override-rate read,
// the graduation-candidate read, and the evidence-gated graduation gesture.
//
// It rules on nothing. Every verdict still travels the human routes; this
// binary's only store mutation is an append to
// observation_triage_suggestions.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"memoire/services/api/internal/config"
	"memoire/services/api/internal/db"
	"memoire/services/api/internal/modelderive"
	"memoire/services/api/internal/triage"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "memoire-triage:", err)
		os.Exit(1)
	}
}

type options struct {
	sweep           bool
	stage           int
	project         string
	sessionID       string
	endpointID      string
	model           string
	limit           int
	maxPromptBytes  int
	maxRowsPerChunk int
	budget          int
	timeout         time.Duration
	dryRun          bool

	scope         bool
	overrideRates bool
	candidates    bool
	graduate      string
	instruction   string
	repoRoot      string
	recordsDir    string
	jsonOut       bool
}

func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("memoire-triage", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	fs.BoolVar(&o.sweep, "sweep", false, "judge the reviewable scope and append suggestions")
	fs.IntVar(&o.stage, "stage", 1, "1 = verdict suggestions over the reviewable scope; 2 = confirm/elevation candidates over the validated set")
	fs.StringVar(&o.project, "project", "", "narrow to one project")
	fs.StringVar(&o.sessionID, "session-id", "", "narrow to one session (the triage unit)")
	fs.StringVar(&o.endpointID, "endpoint-id", envOr("MEMOIRE_TRIAGE_ENDPOINT", ""), "judge endpoint id from the model-endpoints registry (env MEMOIRE_TRIAGE_ENDPOINT)")
	fs.StringVar(&o.model, "model", envOr("MEMOIRE_TRIAGE_MODEL", ""), "judge model, allowlisted for the endpoint (env MEMOIRE_TRIAGE_MODEL)")
	fs.IntVar(&o.limit, "limit", 200, "maximum rows to read from the scope in one pass")
	fs.IntVar(&o.maxPromptBytes, "max-prompt-bytes", triage.DefaultMaxPromptBytes, "conservative per-chunk prompt ceiling")
	fs.IntVar(&o.maxRowsPerChunk, "max-rows-per-chunk", triage.DefaultMaxRowsPerChunk, "maximum rows in one judging unit")
	fs.IntVar(&o.budget, "budget", 0, "maximum rows one pass may judge across all sessions (0 = walk the scope to exhaustion)")
	fs.DurationVar(&o.timeout, "timeout", 5*time.Minute, "bound on the whole pass")
	fs.BoolVar(&o.dryRun, "dry-run", false, "judge but write nothing")

	fs.BoolVar(&o.scope, "scope", false, "print the triage surface's own scope counters")
	fs.BoolVar(&o.overrideRates, "override-rates", false, "print suggested/applied/flipped by verdict and prompt version")
	fs.BoolVar(&o.candidates, "candidates", false, "print graduation candidates with their evidence")
	fs.StringVar(&o.graduate, "graduate", "", "graduate a noise genre to derivation death (refuses on a non-zero flip set)")
	fs.StringVar(&o.instruction, "instruction", "", "the never-emit line the graduated genre adds to the derivation prompt")
	fs.StringVar(&o.repoRoot, "repo-root", ".", "code checkout for -graduate (holds the prompt registry)")
	fs.StringVar(&o.recordsDir, "records-dir", "", "records directory for -graduate (default <repo-root>/records; the ledger's records/ when code and records are separate repositories)")
	fs.BoolVar(&o.jsonOut, "json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}

	modes := 0
	for _, on := range []bool{o.sweep, o.scope, o.overrideRates, o.candidates, o.graduate != ""} {
		if on {
			modes++
		}
	}
	if modes != 1 {
		return errors.New("exactly one of -sweep, -scope, -override-rates, -candidates, -graduate is required")
	}

	store, closeStore, err := openStore()
	if err != nil {
		return err
	}
	defer closeStore()

	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()

	switch {
	case o.scope:
		return runScope(ctx, stdout, store, o)
	case o.overrideRates:
		return runOverrideRates(ctx, stdout, store, o)
	case o.candidates:
		return runCandidates(ctx, stdout, store, o)
	case o.graduate != "":
		return runGraduate(ctx, stdout, store, o)
	default:
		return runSweep(ctx, stdout, store, o)
	}
}

func runScope(ctx context.Context, stdout io.Writer, store *db.Store, o options) error {
	c, err := store.TriageScopeCountsFor(ctx, o.project, o.sessionID)
	if err != nil {
		return err
	}
	if o.jsonOut {
		return writeJSON(stdout, c)
	}
	fmt.Fprintf(stdout, "proposed=%d ttl_lapsed=%d de_noise=%d total=%d noise_flagged=%d suggested=%d\n",
		c.Proposed, c.TTLLapsed, c.DeNoise, c.Total, c.NoiseFlagged, c.Suggested)
	return nil
}

func runOverrideRates(ctx context.Context, stdout io.Writer, store *db.Store, o options) error {
	rates, err := store.TriageOverrideRates(ctx)
	if err != nil {
		return err
	}
	if o.jsonOut {
		return writeJSON(stdout, rates)
	}
	if len(rates) == 0 {
		fmt.Fprintln(stdout, "no suggestions yet")
		return nil
	}
	for _, r := range rates {
		fmt.Fprintf(stdout, "%s %s suggested=%d applied=%d flipped=%d pending=%d\n",
			r.PromptVersion, r.Verdict, r.Suggested, r.Applied, r.Flipped, r.Pending)
	}
	return nil
}

func runCandidates(ctx context.Context, stdout io.Writer, store *db.Store, o options) error {
	cands, err := store.TriageGraduationCandidates(ctx)
	if err != nil {
		return err
	}
	if o.jsonOut {
		return writeJSON(stdout, cands)
	}
	if len(cands) == 0 {
		fmt.Fprintf(stdout, "no genre has reached the evidence threshold (%d judged occurrences)\n",
			db.TriageGraduationThreshold)
		return nil
	}
	for _, c := range cands {
		fmt.Fprintf(stdout, "%s occurrences=%d judged=%d flip_set=%d\n",
			c.Genre, c.Occurrences, c.Judged, len(c.FlipSet))
		if len(c.FlipSet) > 0 {
			fmt.Fprintf(stdout, "  NOT graduatable: the operator kept %v\n", c.FlipSet)
			continue
		}
		// The genre is model output. It is shell-QUOTED here even though the
		// store gate should have refused a non-conforming slug: this line is
		// an invitation to paste into a shell, and an invitation must not be
		// the one place the defense is missing.
		fmt.Fprintf(stdout, "  memoire-triage -graduate %s -repo-root <code checkout> -records-dir <ledger>/records\n", shellQuote(c.Genre))
	}
	return nil
}

// shellQuote renders a value safe to paste into a POSIX shell: single
// quotes, with any embedded single quote closed, escaped and reopened.
func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

func runGraduate(ctx context.Context, stdout io.Writer, store *db.Store, o options) error {
	res, err := triage.Graduate(ctx, store, triage.GraduateOptions{
		Genre:       o.graduate,
		Instruction: o.instruction,
		RepoRoot:    o.repoRoot,
		RecordsDir:  o.recordsDir,
		DryRun:      o.dryRun,
	})
	if err != nil {
		return err
	}

	// The graduation is exactly these two files. They may sit in two
	// repositories (records in the ledger, the registry in the code
	// checkout), so each is committed in the repository that holds it.
	commitSet := []string{res.RecordPath, res.RegistryPath}

	if o.jsonOut {
		return writeJSON(stdout, map[string]any{
			"genre":         res.Genre,
			"dry_run":       res.DryRun,
			"record_path":   res.RecordPath,
			"registry_path": res.RegistryPath,
			"commit_set":    commitSet,
			"evidence":      res.Evidence,
		})
	}

	verb := "graduated"
	if res.DryRun {
		verb = "would graduate"
	}
	fmt.Fprintf(stdout, "%s %s (occurrences=%d judged=%d flip_set=0)\n  %s\n  %s\n",
		verb, res.Genre, res.Evidence.Occurrences, res.Evidence.Judged, res.RecordPath, res.RegistryPath)
	if res.DryRun {
		return nil
	}
	fmt.Fprintln(stdout, "commit the COMPLETE set, each file in the repository that holds it — a graduation is reviewed, never an exec-edit:")
	for _, p := range commitSet {
		fmt.Fprintf(stdout, "  git -C %s add %s\n", shellQuote(filepath.Dir(p)), shellQuote(filepath.Base(p)))
	}
	return nil
}

func runSweep(ctx context.Context, stdout io.Writer, store *db.Store, o options) error {
	if o.endpointID == "" || o.model == "" {
		return errors.New("-endpoint-id and -model are required (set MEMOIRE_TRIAGE_ENDPOINT / MEMOIRE_TRIAGE_MODEL)")
	}
	judge, err := buildJudge(o)
	if err != nil {
		return err
	}
	opts := triage.SweepOptions{
		Project:         o.project,
		SessionID:       o.sessionID,
		EndpointID:      o.endpointID,
		Model:           o.model,
		Stage:           o.stage,
		Limit:           o.limit,
		MaxPromptBytes:  o.maxPromptBytes,
		MaxRowsPerChunk: o.maxRowsPerChunk,
		Budget:          o.budget,
		DryRun:          o.dryRun,
	}
	// Both stages ride the SAME sweep: one paging, budget and exclusion
	// mechanism. Stage 2's own reader was how it drifted out of
	// correctness — a capped window with no paging and no already-judged
	// exclusion, starving every validated row past the first page.
	outcome, err := triage.Sweep(ctx, store, judge, opts)
	if err != nil {
		return err
	}
	if o.jsonOut {
		return writeJSON(stdout, outcome)
	}
	fmt.Fprintf(stdout, "stage=%d sessions=%d chunks=%d rows_attempted=%d rows_judged=%d suggestions=%d covered=%d skipped=%d rejected=%d chunk_failed=%d budget_reached=%t dry_run=%t\n",
		outcome.Stage, outcome.Sessions, outcome.Chunks, outcome.RowsAttempted, outcome.RowsJudged,
		outcome.Suggestions, outcome.Covered, outcome.SuggestionsSkipped, outcome.Rejected,
		outcome.ChunkFailed, outcome.BudgetReached, outcome.DryRun)
	for _, v := range []string{db.TriageVerdictAccept, db.TriageVerdictNoiseLapse, db.TriageVerdictLapse,
		db.TriageVerdictReject, db.TriageVerdictConfirm, db.TriageVerdictElevate,
		db.TriageVerdictNone} {
		if n := outcome.ByVerdict[v]; n > 0 {
			fmt.Fprintf(stdout, "  %s=%d\n", v, n)
		}
	}
	if outcome.FirstFailure != "" {
		fmt.Fprintf(stdout, "  first_failure=%s\n", outcome.FirstFailure)
	}
	return nil
}

// qwenJudge adapts the local-qwen adapter to the triage.Judge seam. The
// adapter carries the endpoint allowlist, the availability probe, and the
// bearer-by-name secret handling, so the judge lane inherits memoire:D137's
// local-boundary condition and memoire:D55/D58's secret seam instead of
// reimplementing either.
type qwenJudge struct{ a *modelderive.QwenAdapter }

func (j qwenJudge) Chat(ctx context.Context, endpointID, model, prompt string) ([]byte, error) {
	resp, err := j.a.Chat(ctx, endpointID, model, prompt)
	if err != nil {
		return nil, err
	}
	if resp.Refusal != "" {
		return nil, fmt.Errorf("judge refused: %s", resp.Refusal)
	}
	return resp.RawResponse, nil
}

func buildJudge(o options) (triage.Judge, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	endpoints, err := config.LoadModelEndpoints(cfg.ModelEndpointsPath)
	if err != nil {
		return nil, err
	}
	var ep config.ModelEndpoint
	found := false
	for _, e := range endpoints {
		if e.EndpointID == o.endpointID {
			ep, found = e, true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("endpoint-id %q is not present in the model endpoints registry %s", o.endpointID, cfg.ModelEndpointsPath)
	}
	// Secret pre-flight, memoire:D77: a missing configured key is an
	// operator error naming ONLY the variable, never a buried failure.
	if ep.APIKeyEnv != "" {
		if v, ok := os.LookupEnv(ep.APIKeyEnv); !ok || strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("endpoint %q requires environment variable %s but it is unset (its value is never read into config)", ep.EndpointID, ep.APIKeyEnv)
		}
	}
	qwenEndpoints := make([]modelderive.QwenEndpoint, 0, len(endpoints))
	for _, e := range endpoints {
		qwenEndpoints = append(qwenEndpoints, modelderive.QwenEndpoint{
			EndpointID: e.EndpointID, BaseURL: e.BaseURL, APIKeyEnv: e.APIKeyEnv, Models: e.Models,
		})
	}
	adapter, err := modelderive.NewQwenAdapter(modelderive.QwenConfig{
		Endpoints:       qwenEndpoints,
		Timeout:         o.timeout,
		MaxPromptBytes:  o.maxPromptBytes,
		ReasoningEffort: os.Getenv("MEMOIRE_TRIAGE_EFFORT"),
	}, nil, modelderive.EnvLookup(os.LookupEnv))
	if err != nil {
		return nil, err
	}
	// memoire:D137: automatic derivation-family model calls are legal only
	// for a local data boundary. The judge lane is automatic by design, so
	// the condition is asserted rather than assumed.
	if err := adapter.CheckDataBoundary(modelderive.DataBoundaryLocal); err != nil {
		return nil, err
	}
	return qwenJudge{a: adapter}, nil
}

func openStore() (*db.Store, func(), error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	if err := cfg.EnsureDirs(); err != nil {
		return nil, nil, err
	}
	store, err := db.Open(cfg.DBPath, cfg.SchemaDir)
	if err != nil {
		return nil, nil, err
	}
	return store, func() { store.Close() }, nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}
