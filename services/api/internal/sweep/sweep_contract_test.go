// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package sweep

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"memoire/services/api/internal/redact"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

type sweepFixture struct {
	root          string
	rawDir        string
	outputDir     string
	quarantineOut string
	reportPath    string
	evidencePath  string
	keyPath       string
	identityPath  string
	recipient     string
	inputs        map[string][]byte
}

func TestGenerationCorrectnessUsesSemanticRedactionOracle(t *testing.T) {
	fx := newSweepFixture(t)
	cleanName := "1700000000000-clean-aaaaaaaa.json"
	hitName := "1700000000001-hit-bbbbbbbb.json"
	clean := cleanEnvelope()
	hit := hitEnvelope()
	writeRawFixture(t, fx, cleanName, clean)
	writeRawFixture(t, fx, hitName, hit)

	_, err := Run(context.Background(), fx.options())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	cleanOut := readFile(t, filepath.Join(fx.outputDir, cleanName))
	if !bytes.Equal(cleanOut, clean) {
		t.Fatalf("clean file changed; non-hit files must be byte-identical\ninput:  %q\noutput: %q", clean, cleanOut)
	}

	hitOut := readFile(t, filepath.Join(fx.outputDir, hitName))
	assertJSONRedactedSemantically(t, testKey, hit, hitOut)
	for _, secret := range plantedSecrets() {
		if bytes.Contains(hitOut, []byte(secret)) {
			t.Fatalf("secret %q survived in hit output:\n%s", secret, string(hitOut))
		}
	}
	for _, marker := range expectedMarkers(t, testKey) {
		if !bytes.Contains(hitOut, []byte(marker)) {
			t.Fatalf("expected marker %q missing from hit output:\n%s", marker, string(hitOut))
		}
	}
}

func TestQuarantineCompletenessRecoverableOriginals(t *testing.T) {
	fx := newSweepFixture(t)
	writeRawFixture(t, fx, "1700000000000-clean-aaaaaaaa.json", cleanEnvelope())
	writeRawFixture(t, fx, "1700000000001-hit-bbbbbbbb.json", hitEnvelope())

	_, err := Run(context.Background(), fx.options())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	extracted := decryptQuarantine(t, fx)
	for name, want := range fx.inputs {
		got := readFile(t, filepath.Join(extracted, name))
		if !bytes.Equal(got, want) {
			t.Fatalf("quarantined original %s not byte-identical", name)
		}
	}
	if got, want := countJSONFiles(t, extracted), len(fx.inputs); got != want {
		t.Fatalf("quarantine file count = %d, want %d", got, want)
	}
}

func TestReportTruthfulnessAndReconciliationArithmetic(t *testing.T) {
	fx := newSweepFixture(t)
	writeRawFixture(t, fx, "1700000000000-clean-aaaaaaaa.json", cleanEnvelope())
	writeRawFixture(t, fx, "1700000000001-hit-bbbbbbbb.json", hitEnvelope())
	writeRawFixture(t, fx, "1700000000002-second-hit-cccccccc.json", secondHitEnvelope())

	_, err := Run(context.Background(), fx.options())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	reportBytes := readFile(t, fx.reportPath)
	evidenceBytes := readFile(t, fx.evidencePath)
	if !bytes.Equal(reportBytes, evidenceBytes) {
		t.Fatalf("report and registered evidence artifact must contain identical JSON")
	}

	report := decodeReport(t, reportBytes)
	assertReportHasOnlyKnownFields(t, reportBytes)

	if report.Tool != ToolName {
		t.Fatalf("tool = %q, want %q", report.Tool, ToolName)
	}
	if report.RulesVersion != redact.RulesVersion {
		t.Fatalf("rules_version = %q, want %q", report.RulesVersion, redact.RulesVersion)
	}
	if report.RawDir != fx.rawDir || report.OutputDir != fx.outputDir || report.QuarantineOut != fx.quarantineOut {
		t.Fatalf("report paths do not match inputs: %+v", report)
	}
	if got, want := report.FilesScanned, len(fx.inputs); got != want {
		t.Fatalf("files_scanned = %d, want %d", got, want)
	}
	if got, want := report.FilesWithHits, expectedFilesWithHits(t, testKey, fx.inputs); got != want {
		t.Fatalf("files_with_hits = %d, want %d", got, want)
	}
	if !reflect.DeepEqual(report.HitsByRule, expectedHitsByRule(t, testKey, fx.inputs)) {
		t.Fatalf("hits_by_rule = %#v, want %#v", report.HitsByRule, expectedHitsByRule(t, testKey, fx.inputs))
	}
	if report.TotalInputBytes != totalBytes(fx.inputs) {
		t.Fatalf("total_input_bytes = %d, want %d", report.TotalInputBytes, totalBytes(fx.inputs))
	}
	if report.TotalOutputBytes != totalOutputBytes(t, fx.outputDir) {
		t.Fatalf("total_output_bytes = %d, want actual output bytes", report.TotalOutputBytes)
	}
	extracted := decryptQuarantine(t, fx)
	wantCount := len(fx.inputs)
	if report.Reconciliation.Scanned != wantCount ||
		report.Reconciliation.RedactedOutputs != countJSONFiles(t, fx.outputDir) ||
		report.Reconciliation.QuarantinedOriginals != countJSONFiles(t, extracted) ||
		!report.Reconciliation.Balanced {
		t.Fatalf("bad reconciliation: %+v", report.Reconciliation)
	}
}

func TestNeverClobberRefusalsAreAtomic(t *testing.T) {
	tests := []struct {
		name      string
		prepare   func(t *testing.T, fx *sweepFixture)
		wantError string
		assert    func(t *testing.T, fx *sweepFixture)
	}{
		{
			name: "non-empty output dir",
			prepare: func(t *testing.T, fx *sweepFixture) {
				if err := os.MkdirAll(fx.outputDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(fx.outputDir, "sentinel"), []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantError: "output-dir must be empty",
			assert: func(t *testing.T, fx *sweepFixture) {
				if got := readFile(t, filepath.Join(fx.outputDir, "sentinel")); string(got) != "keep" {
					t.Fatalf("sentinel modified: %q", string(got))
				}
				assertPathMissing(t, fx.reportPath)
				assertPathMissing(t, fx.quarantineOut)
			},
		},
		{
			name: "pre-existing quarantine",
			prepare: func(t *testing.T, fx *sweepFixture) {
				if err := os.WriteFile(fx.quarantineOut, []byte("existing quarantine"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantError: "quarantine-out already exists",
			assert: func(t *testing.T, fx *sweepFixture) {
				if got := readFile(t, fx.quarantineOut); string(got) != "existing quarantine" {
					t.Fatalf("quarantine modified: %q", string(got))
				}
				assertNoOutputFiles(t, fx.outputDir)
				assertPathMissing(t, fx.reportPath)
			},
		},
		{
			name: "pre-existing report",
			prepare: func(t *testing.T, fx *sweepFixture) {
				if err := os.WriteFile(fx.reportPath, []byte("existing report"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantError: "report already exists",
			assert: func(t *testing.T, fx *sweepFixture) {
				if got := readFile(t, fx.reportPath); string(got) != "existing report" {
					t.Fatalf("report modified: %q", string(got))
				}
				assertNoOutputFiles(t, fx.outputDir)
				assertPathMissing(t, fx.quarantineOut)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := newSweepFixture(t)
			writeRawFixture(t, fx, "1700000000000-hit-bbbbbbbb.json", hitEnvelope())
			tc.prepare(t, fx)

			_, err := Run(context.Background(), fx.options())
			if err == nil {
				t.Fatalf("expected refusal error")
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantError)
			}
			tc.assert(t, fx)
		})
	}
}

func TestRefusesTargetsInsideRawDirBeforeAnyWrite(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(fx *sweepFixture)
		wantError string
	}{
		{
			name: "output dir nested under raw dir",
			mutate: func(fx *sweepFixture) {
				fx.outputDir = filepath.Join(fx.rawDir, "out-inside")
			},
			wantError: "output-dir must not be inside raw-dir",
		},
		{
			name: "output dir equals raw dir",
			mutate: func(fx *sweepFixture) {
				fx.outputDir = fx.rawDir
			},
			wantError: "output-dir must not be inside raw-dir",
		},
		{
			name: "quarantine nested under raw dir",
			mutate: func(fx *sweepFixture) {
				fx.quarantineOut = filepath.Join(fx.rawDir, "quarantine.age")
			},
			wantError: "quarantine-out must not be inside raw-dir",
		},
		{
			name: "quarantine equals raw dir",
			mutate: func(fx *sweepFixture) {
				fx.quarantineOut = fx.rawDir
			},
			wantError: "quarantine-out must not be inside raw-dir",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := newSweepFixture(t)
			writeRawFixture(t, fx, "1700000000000-clean-aaaaaaaa.json", cleanEnvelope())
			writeRawFixture(t, fx, "1700000000001-hit-bbbbbbbb.json", hitEnvelope())
			beforeRaw := snapshotTree(t, fx.rawDir)
			tc.mutate(fx)

			_, err := Run(context.Background(), fx.options())
			if err == nil {
				t.Fatalf("expected raw-fence refusal")
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantError)
			}
			assertTreeUnchanged(t, fx.rawDir, beforeRaw)
			assertPathMissing(t, fx.reportPath)
			assertPathMissing(t, fx.evidencePath)
			if !pathIsEqualOrInside(t, fx.rawDir, fx.quarantineOut) {
				assertPathMissing(t, fx.quarantineOut)
			}
			if !pathIsEqualOrInside(t, fx.rawDir, fx.outputDir) {
				assertNoOutputFiles(t, fx.outputDir)
			}
		})
	}
}

func TestNoHitsCorpusPassesCleanly(t *testing.T) {
	fx := newSweepFixture(t)
	writeRawFixture(t, fx, "1700000000000-clean-aaaaaaaa.json", cleanEnvelope())
	writeRawFixture(t, fx, "1700000000001-clean-bbbbbbbb.json", secondCleanEnvelope())

	_, err := Run(context.Background(), fx.options())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	for name, want := range fx.inputs {
		got := readFile(t, filepath.Join(fx.outputDir, name))
		if !bytes.Equal(got, want) {
			t.Fatalf("clean output %s changed", name)
		}
	}
	report := decodeReport(t, readFile(t, fx.reportPath))
	if report.FilesWithHits != 0 {
		t.Fatalf("files_with_hits = %d, want 0", report.FilesWithHits)
	}
	if len(report.HitsByRule) != 0 {
		t.Fatalf("hits_by_rule = %#v, want empty map", report.HitsByRule)
	}
	assertHitsByRuleIsObject(t, readFile(t, fx.reportPath))
	extracted := decryptQuarantine(t, fx)
	for name, want := range fx.inputs {
		got := readFile(t, filepath.Join(extracted, name))
		if !bytes.Equal(got, want) {
			t.Fatalf("clean quarantined original %s changed", name)
		}
	}
}

func TestQuarantineImplementationStreamsTarToAge(t *testing.T) {
	src := string(readFile(t, "sweep.go"))
	forbidden := []string{
		"bytes.Buffer",
		"[]rawFile",
		"type rawFile struct",
		"originals = append",
		"cmd.Stdin = &tarBuf",
	}
	for _, pattern := range forbidden {
		if strings.Contains(src, pattern) {
			t.Fatalf("sweep.go must stream quarantine tar to age, but found forbidden in-memory staging pattern %q", pattern)
		}
	}
	streamingPipe := strings.Contains(src, "io.Pipe(") ||
		strings.Contains(src, "os.Pipe(") ||
		strings.Contains(src, ".StdoutPipe()")
	if !streamingPipe {
		t.Fatalf("sweep.go must connect tar production to age stdin with io.Pipe, os.Pipe, or exec.Cmd.StdoutPipe")
	}
	if !strings.Contains(src, "cmd.Stdin") {
		t.Fatalf("sweep.go must wire a streaming tar reader into the age command's stdin")
	}
}

func TestRawDirIsReadOnlyAfterSweep(t *testing.T) {
	// D97 source-of-truth invariant: --raw-dir is read-only input. After a
	// successful sweep every original must remain present and byte-identical
	// under --raw-dir; the sweep must not delete, truncate, rewrite, or add
	// files there. A worker that "moves" originals by archiving then pruning
	// raw would silently destroy the canonical corpus — the exact failure this
	// slice exists to prevent.
	fx := newSweepFixture(t)
	writeRawFixture(t, fx, "1700000000000-clean-aaaaaaaa.json", cleanEnvelope())
	writeRawFixture(t, fx, "1700000000001-hit-bbbbbbbb.json", hitEnvelope())

	_, err := Run(context.Background(), fx.options())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	entries, err := os.ReadDir(fx.rawDir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(entries), len(fx.inputs); got != want {
		names := make([]string, 0, len(entries))
		for _, ent := range entries {
			names = append(names, ent.Name())
		}
		t.Fatalf("raw-dir file count changed: got %d %v, want %d; --raw-dir must be read-only", got, names, want)
	}
	for name, want := range fx.inputs {
		got := readFile(t, filepath.Join(fx.rawDir, name))
		if !bytes.Equal(got, want) {
			t.Fatalf("raw input %s mutated by sweep; --raw-dir must be read-only", name)
		}
	}
}

func TestMissingAgeBinaryIsReported(t *testing.T) {
	// The age shell-out is resolved via PATH; when age is absent the sweep must
	// refuse with a stable, contract-quoted error. tar stays available so this
	// isolates the missing-age behavior regardless of archive-step ordering.
	fx := newSweepFixture(t)
	writeRawFixture(t, fx, "1700000000000-hit-bbbbbbbb.json", hitEnvelope())

	binDir := filepath.Join(fx.root, "bin-no-age")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tarPath, err := exec.LookPath("tar")
	if err != nil {
		t.Skipf("tar not available: %v", err)
	}
	if err := os.Symlink(tarPath, filepath.Join(binDir, "tar")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	_, err = Run(context.Background(), fx.options())
	if err == nil {
		t.Fatalf("expected error when age binary is missing")
	}
	if !strings.Contains(err.Error(), "age binary not found") {
		t.Fatalf("error = %q, want substring %q", err.Error(), "age binary not found")
	}
}

func newSweepFixture(t *testing.T) *sweepFixture {
	t.Helper()
	requireAgeToolchain(t)
	root := t.TempDir()
	rawDir := filepath.Join(root, "raw")
	if err := os.MkdirAll(rawDir, 0o700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(root, "redaction-key")
	if err := os.WriteFile(keyPath, testKey, 0o600); err != nil {
		t.Fatal(err)
	}
	identity, recipient := makeAgeIdentity(t, root)
	return &sweepFixture{
		root:          root,
		rawDir:        rawDir,
		outputDir:     filepath.Join(root, "redacted"),
		quarantineOut: filepath.Join(root, "quarantine.age"),
		reportPath:    filepath.Join(root, "report.json"),
		evidencePath:  filepath.Join(root, "evidence.json"),
		keyPath:       keyPath,
		identityPath:  identity,
		recipient:     recipient,
		inputs:        map[string][]byte{},
	}
}

func (fx *sweepFixture) options() Options {
	return Options{
		RawDir:           fx.rawDir,
		OutputDir:        fx.outputDir,
		QuarantineOut:    fx.quarantineOut,
		AgeRecipient:     fx.recipient,
		ReportPath:       fx.reportPath,
		RedactionKeyPath: fx.keyPath,
		EvidencePath:     fx.evidencePath,
	}
}

func requireAgeToolchain(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"age", "age-keygen", "tar"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available: %v", bin, err)
		}
	}
}

func makeAgeIdentity(t *testing.T, root string) (identityPath, recipient string) {
	t.Helper()
	identityPath = filepath.Join(root, "age-identity.txt")
	cmd := exec.Command("age-keygen", "-o", identityPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("age-keygen: %v\n%s", err, string(out))
	}
	const prefix = "Public key:"
	line := strings.TrimSpace(string(out))
	if !strings.HasPrefix(line, prefix) {
		t.Fatalf("unexpected age-keygen output: %q", line)
	}
	return identityPath, strings.TrimSpace(strings.TrimPrefix(line, prefix))
}

func decryptQuarantine(t *testing.T, fx *sweepFixture) string {
	t.Helper()
	archive := filepath.Join(fx.root, "quarantine.tar")
	cmd := exec.Command("age", "-d", "-i", fx.identityPath, "-o", archive, fx.quarantineOut)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("age decrypt: %v\n%s", err, string(out))
	}
	extracted := filepath.Join(fx.root, "extracted")
	if err := os.MkdirAll(extracted, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("tar", "-xf", archive, "-C", extracted)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tar extract: %v\n%s", err, string(out))
	}
	return extracted
}

func writeRawFixture(t *testing.T, fx *sweepFixture, name string, body []byte) {
	t.Helper()
	if strings.Contains(name, "/") || strings.Contains(name, `\`) {
		t.Fatalf("fixture name must be flat: %q", name)
	}
	if err := os.WriteFile(filepath.Join(fx.rawDir, name), body, 0o600); err != nil {
		t.Fatal(err)
	}
	fx.inputs[name] = append([]byte(nil), body...)
}

func cleanEnvelope() []byte {
	return []byte("{\n  \"schema_version\": 1,\n  \"project\": \"memoire-test\",\n  \"session_id\": \"clean-session\",\n  \"source_adapter\": \"manual\",\n  \"event_type\": \"note\",\n  \"text\": \"ordinary note\",\n  \"payload\": {\"message\": \"nothing secret here\", \"n\": 9007199254740993},\n  \"metadata\": {\"safe\": \"op://Vault/db/password\"}\n}\n")
}

func secondCleanEnvelope() []byte {
	return []byte("{\n  \"schema_version\": 1,\n  \"project\": \"memoire-test\",\n  \"session_id\": \"clean-session-2\",\n  \"source_adapter\": \"curl\",\n  \"event_type\": \"note\",\n  \"payload\": {\"message\": \"still clean\", \"template\": \"${API_TOKEN}\"},\n  \"metadata\": {}\n}\n")
}

func hitEnvelope() []byte {
	return []byte("{\n  \"schema_version\": 1,\n  \"project\": \"memoire-test\",\n  \"session_id\": \"hit-session\",\n  \"source_adapter\": \"claude-code\",\n  \"event_type\": \"claude_PostToolUse\",\n  \"text\": \"Authorization: Bearer topsecretbearer99\",\n  \"payload\": {\n    \"message\": \"AWS key AKIAIOSFODNN7EXAMPLE leaked\",\n    \"nested\": {\"env\": \"DATABASE_PASSWORD=hunter2hunter2\"}\n  },\n  \"metadata\": {\n    \"ghp_abcdefghijklmnopqrstuvwxyz0123456789ABCD\": \"secret-shaped key must be redacted\",\n    \"safe\": \"ordinary\"\n  }\n}\n")
}

func secondHitEnvelope() []byte {
	return []byte("{\n  \"schema_version\": 1,\n  \"project\": \"memoire-test\",\n  \"session_id\": \"hit-session-2\",\n  \"source_adapter\": \"manual\",\n  \"event_type\": \"note\",\n  \"text\": \"token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9P\",\n  \"payload\": {\"pem\": \"-----BEGIN RSA PRIVATE KEY-----\\nMIIEfakefakefakefake\\nfakefakefake==\\n-----END RSA PRIVATE KEY-----\"},\n  \"metadata\": {}\n}\n")
}

func plantedSecrets() []string {
	return []string{
		"topsecretbearer99",
		"AKIAIOSFODNN7EXAMPLE",
		"hunter2hunter2",
		"ghp_abcdefghijklmnopqrstuvwxyz0123456789ABCD",
	}
}

func expectedMarkers(t *testing.T, key []byte) []string {
	t.Helper()
	r := redact.New(key)
	inputs := []struct {
		text string
		rule string
	}{
		{"Authorization: Bearer topsecretbearer99", "bearer-header"},
		{"AKIAIOSFODNN7EXAMPLE", "aws-access-key"},
		{"DATABASE_PASSWORD=hunter2hunter2", "env-secret-assignment"},
		{"ghp_abcdefghijklmnopqrstuvwxyz0123456789ABCD", "github-token"},
	}
	markers := make([]string, 0, len(inputs))
	for _, in := range inputs {
		out, _ := r.String(in.text)
		markers = append(markers, extractMarker(t, out, in.rule))
	}
	return markers
}

func extractMarker(t *testing.T, s, rule string) string {
	t.Helper()
	prefix := "[REDACTED:" + rule + ":"
	start := strings.Index(s, prefix)
	if start < 0 {
		t.Fatalf("marker for rule %s missing in %q", rule, s)
	}
	end := strings.Index(s[start:], "]")
	if end < 0 {
		t.Fatalf("unterminated marker in %q", s)
	}
	return s[start : start+end+1]
}

func assertJSONRedactedSemantically(t *testing.T, key, original, output []byte) {
	t.Helper()
	r := redact.New(key)
	wantRaw, hits := r.JSON(json.RawMessage(original))
	if len(hits) == 0 {
		t.Fatalf("fixture invalid: hit file produced no redaction hits")
	}
	want := decodeJSONUseNumber(t, wantRaw)
	got := decodeJSONUseNumber(t, output)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("semantic redaction mismatch\nwant: %s\ngot:  %s", string(wantRaw), string(output))
	}
}

func decodeJSONUseNumber(t *testing.T, b []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, string(b))
	}
	return v
}

func expectedFilesWithHits(t *testing.T, key []byte, inputs map[string][]byte) int {
	t.Helper()
	r := redact.New(key)
	n := 0
	for _, raw := range inputs {
		_, hits := r.JSON(json.RawMessage(raw))
		if len(hits) > 0 {
			n++
		}
	}
	return n
}

func expectedHitsByRule(t *testing.T, key []byte, inputs map[string][]byte) map[string]int {
	t.Helper()
	r := redact.New(key)
	out := map[string]int{}
	for _, raw := range inputs {
		_, hits := r.JSON(json.RawMessage(raw))
		for _, h := range hits {
			out[h.Rule] += h.Count
		}
	}
	return out
}

func totalBytes(inputs map[string][]byte) int64 {
	var n int64
	for _, b := range inputs {
		n += int64(len(b))
	}
	return n
}

func totalOutputBytes(t *testing.T, dir string) int64 {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			t.Fatal(err)
		}
		n += info.Size()
	}
	return n
}

func countJSONFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ent := range entries {
		if !ent.IsDir() && strings.HasSuffix(ent.Name(), ".json") {
			n++
		}
	}
	return n
}

func decodeReport(t *testing.T, b []byte) Report {
	t.Helper()
	var report Report
	if err := json.Unmarshal(b, &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, string(b))
	}
	return report
}

func assertReportHasOnlyKnownFields(t *testing.T, b []byte) {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("decode raw report: %v", err)
	}
	allowed := map[string]bool{
		"tool": true, "rules_version": true, "raw_dir": true, "output_dir": true,
		"quarantine_out": true, "files_scanned": true, "files_with_hits": true,
		"hits_by_rule": true, "total_input_bytes": true, "total_output_bytes": true,
		"reconciliation": true,
	}
	for k := range raw {
		if !allowed[k] {
			t.Fatalf("report contains non-contract field %q", k)
		}
	}
}

func assertHitsByRuleIsObject(t *testing.T, b []byte) {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	hits, ok := raw["hits_by_rule"].(map[string]any)
	if !ok {
		t.Fatalf("hits_by_rule must be an object, got %#v", raw["hits_by_rule"])
	}
	if len(hits) != 0 {
		t.Fatalf("hits_by_rule = %#v, want empty object", hits)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func assertPathMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path %s exists or stat failed unexpectedly: %v", path, err)
	}
}

func snapshotTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			out[rel+string(os.PathSeparator)] = nil
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertTreeUnchanged(t *testing.T, root string, want map[string][]byte) {
	t.Helper()
	got := snapshotTree(t, root)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tree %s changed\nwant: %#v\ngot:  %#v", root, want, got)
	}
}

func pathIsEqualOrInside(t *testing.T, root, candidate string) bool {
	t.Helper()
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	candAbs, err := filepath.Abs(candidate)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(rootAbs, candAbs)
	if err != nil {
		t.Fatal(err)
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

func assertNoOutputFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, ent := range entries {
		names = append(names, ent.Name())
	}
	sort.Strings(names)
	if len(names) != 0 {
		t.Fatalf("unexpected output writes: %s", strings.Join(names, ", "))
	}
}

func TestValidateOptionsRequiresMandatoryFlags(t *testing.T) {
	valid := Options{
		RawDir:           "raw",
		OutputDir:        "out",
		QuarantineOut:    "quarantine.age",
		AgeRecipient:     "age1example",
		ReportPath:       "report.json",
		RedactionKeyPath: "redaction-key",
	}
	tests := []struct {
		name string
		mut  func(*Options)
		want string
	}{
		{"raw", func(o *Options) { o.RawDir = "" }, "missing required --raw-dir"},
		{"output", func(o *Options) { o.OutputDir = "" }, "missing required --output-dir"},
		{"quarantine", func(o *Options) { o.QuarantineOut = "" }, "missing required --quarantine-out"},
		{"recipient", func(o *Options) { o.AgeRecipient = "" }, "missing required --age-recipient"},
		{"report", func(o *Options) { o.ReportPath = "" }, "missing required --report"},
		{"key", func(o *Options) { o.RedactionKeyPath = "" }, "missing required --redaction-key"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := valid
			tc.mut(&opts)
			err := ValidateOptions(opts)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("ValidateOptions error = %v, want %q", err, tc.want)
			}
		})
	}
}

func ExampleReport_schema() {
	report := Report{
		Tool:             ToolName,
		RulesVersion:     redact.RulesVersion,
		RawDir:           "/tmp/raw",
		OutputDir:        "/tmp/redacted",
		QuarantineOut:    "/tmp/quarantine.age",
		FilesScanned:     2,
		FilesWithHits:    1,
		HitsByRule:       map[string]int{"aws-access-key": 1},
		TotalInputBytes:  100,
		TotalOutputBytes: 120,
		Reconciliation: Reconciliation{
			Scanned:              2,
			RedactedOutputs:      2,
			QuarantinedOriginals: 2,
			Balanced:             true,
		},
	}
	b, _ := json.Marshal(report)
	fmt.Println(json.Valid(b))
	// Output: true
}
