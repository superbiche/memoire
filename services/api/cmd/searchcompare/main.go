// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// searchcompare — injection-quality evidence harness (D157/D158 ranking
// gate): runs the SAME real-workload queries through the REAL store search
// code on both engines and emits a side-by-side markdown pack for judging.
// One-off tool; lives in tmp/ (gitignored) on purpose.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"memoire/services/api/internal/db"
)

func main() {
	sqlitePath := flag.String("sqlite", "", "frozen sqlite backup path")
	pgURL := flag.String("pg", os.Getenv("MEMOIRE_PG_URL"), "postgres DSN (or MEMOIRE_PG_URL)")
	queriesPath := flag.String("queries", "", "file with one query per line")
	outPath := flag.String("out", "", "markdown output path")
	flag.Parse()
	if *sqlitePath == "" || *pgURL == "" || *queriesPath == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "need -sqlite, -pg/MEMOIRE_PG_URL, -queries, -out")
		os.Exit(2)
	}

	// Observer opens (round-1 finding 3): no migrations, no provisioning,
	// no write pool — this tool must not mutate what it measures. The alias
	// guard canonicalizes (abs + symlink resolution) and, for existing
	// files, compares identity via os.SameFile, so relative spellings,
	// symlinks, and hard links cannot smuggle an input in as -out
	// (round-2 finding N2).
	fatal(guardOutPath(*outPath, *sqlitePath, *queriesPath), "guard")
	lite, err := db.OpenReadOnly(*sqlitePath)
	fatal(err, "open sqlite")
	defer lite.Close()
	pg, err := db.OpenReadOnly(*pgURL)
	fatal(err, "open postgres")
	defer pg.Close()

	body, err := os.ReadFile(*queriesPath)
	fatal(err, "read queries")
	var queries []string
	for _, line := range strings.Split(string(body), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			queries = append(queries, line)
		}
	}

	out, err := os.Create(*outPath)
	fatal(err, "create out")
	defer out.Close()

	ctx := context.Background()
	fmt.Fprintf(out, "# Injection-quality evidence: SQLite %s vs Postgres %s\n\n", lite.SearchEngine(), pg.SearchEngine())
	fmt.Fprintf(out, "%d real queries mined from captured memoire_search/claude-mem tool calls.\n", len(queries))
	fmt.Fprintf(out, "Both sides run the REAL store SearchWithFilter (unscoped project, limit 10).\n")
	fmt.Fprintf(out, "Engine stamp (round-1 finding 2): sqlite=%s postgres=%s.\n\n", lite.SearchEngine(), pg.SearchEngine())

	type bucket struct{ lo, hi int }
	overlapHist := map[string]int{}
	var errsLite, errsPG, bothEmpty int

	for i, q := range queries {
		liteHits, liteErr := lite.Search(ctx, q, "", 10)
		pgHits, pgErr := pg.Search(ctx, q, "", 10)
		fmt.Fprintf(out, "---\n\n## Q%02d: `%s`\n\n", i+1, q)
		if liteErr != nil {
			fmt.Fprintf(out, "SQLITE ERROR: %v\n", liteErr)
			errsLite++
		}
		if pgErr != nil {
			fmt.Fprintf(out, "PG ERROR: %v\n", pgErr)
			errsPG++
		}
		if liteErr != nil || pgErr != nil {
			continue
		}
		if len(liteHits) == 0 && len(pgHits) == 0 {
			bothEmpty++
			fmt.Fprintf(out, "both empty\n\n")
			continue
		}
		ov := overlap(liteHits, pgHits)
		den := len(liteHits)
		if len(pgHits) > den {
			den = len(pgHits)
		}
		fmt.Fprintf(out, "overlap: %d/%d\n\n", ov, den)
		switch {
		case den == 0:
		case ov*10 >= den*7:
			overlapHist["high(>=70%)"]++
		case ov*10 >= den*4:
			overlapHist["mid(40-69%)"]++
		default:
			overlapHist["low(<40%)"]++
		}
		fmt.Fprintf(out, "### SQLite (%s)\n\n", lite.SearchEngine())
		list(out, liteHits)
		fmt.Fprintf(out, "### Postgres (%s)\n\n", pg.SearchEngine())
		list(out, pgHits)
	}

	fmt.Fprintf(out, "---\n\n## Summary\n\n")
	fmt.Fprintf(out, "- queries: %d; sqlite errors: %d; pg errors: %d; both-empty: %d\n", len(queries), errsLite, errsPG, bothEmpty)
	for _, k := range []string{"high(>=70%)", "mid(40-69%)", "low(<40%)"} {
		fmt.Fprintf(out, "- overlap %s: %d\n", k, overlapHist[k])
	}
	_ = bucket{}
	fmt.Println("written", *outPath)
}


// guardOutPath refuses an -out path that aliases any input. Inputs exist;
// out may not yet — its parent directory is resolved instead.
func guardOutPath(out string, inputs ...string) error {
	canon := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		if a, err := filepath.Abs(p); err == nil {
			p = a
		}
		return p
	}
	outDir := canon(filepath.Dir(out))
	outCanon := filepath.Join(outDir, filepath.Base(out))
	outInfo, outErr := os.Stat(out)
	for _, in := range inputs {
		if canon(in) == outCanon {
			return fmt.Errorf("-out aliases input %s", in)
		}
		if outErr == nil {
			if inInfo, err := os.Stat(in); err == nil && os.SameFile(inInfo, outInfo) {
				return fmt.Errorf("-out is the same file as input %s (hard link or alias)", in)
			}
		}
	}
	return nil
}

func list(out *os.File, hits []db.SearchHit) {
	if len(hits) == 0 {
		fmt.Fprintf(out, "(no hits)\n\n")
		return
	}
	for i, h := range hits {
		text := h.Text
		if text == "" {
			text = h.Snippet
		}
		text = strings.ReplaceAll(text, "\n", " ")
		if len(text) > 220 {
			text = text[:220] + "…"
		}
		fmt.Fprintf(out, "%d. [%d %s/%s %s] %s\n", i+1, h.ID, h.Kind, h.ReviewState, h.Project, text)
	}
	fmt.Fprintf(out, "\n")
}

func overlap(a, b []db.SearchHit) int {
	seen := map[int64]struct{}{}
	for _, h := range a {
		seen[h.ID] = struct{}{}
	}
	n := 0
	for _, h := range b {
		if _, ok := seen[h.ID]; ok {
			n++
		}
	}
	return n
}

func fatal(err error, what string) {
	if err != nil {
		fmt.Fprintln(os.Stderr, what+":", err)
		os.Exit(1)
	}
}
