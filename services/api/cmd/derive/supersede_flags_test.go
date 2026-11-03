// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package main

// The #344 CLI surface: the -no-supersede kill switch, the -provisional
// marker, their -execute coupling, and the counters both output shapes print.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestCLISupersedeFlagsRequireExecute(t *testing.T) {
	// Both flags govern a persisted run's write path, so on a dry-run they
	// would silently mean nothing — the same coupling -force-rerun has.
	for _, tc := range []struct{ flag, want string }{
		{"-no-supersede", "-no-supersede requires -execute"},
		{"-provisional", "-provisional requires -execute"},
	} {
		setupCLIEnv(t)
		err := run([]string{
			"-strategy=model", "-dry-run", tc.flag,
			"-provider=local-qwen", "-model=qwen-7b", "-endpoint-id=hl3",
			"-project=p", "-session-id=s",
		}, io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s on a dry-run must error with %q, got: %v", tc.flag, tc.want, err)
		}
	}
}

// executeWithFlags drives the -execute path through the fake provider with
// extra flags, returning the decoded JSON output.
func executeWithFlags(t *testing.T, session string, base int64, text string, extra ...string) map[string]any {
	t.Helper()
	home := setupCLIEnv(t)
	ids := seedSubstantiveSession(t, "memoire-test", session, base)
	modelJSON := fmt.Sprintf(`{"schema_version":"1","observations":[{"kind":"decision","text":%q,"confidence":0.86,"source_event_ids":[%d],"rationale":"explicit"}]}`, text, ids[0])
	srv := fakeQwenServer(t, modelJSON)
	defer srv.Close()
	writeEndpointsConfig(t, home, srv.URL, "MEMOIRE_TEST_QWEN_KEY", "qwen2.5-7b")
	t.Setenv("MEMOIRE_TEST_QWEN_KEY", "placeholder-not-a-real-token")

	args := append([]string{
		"-strategy=model", "-execute", "-json",
		"-provider=local-qwen", "-model=qwen2.5-7b", "-endpoint-id=hl3",
		"-project=memoire-test", "-session-id=" + session,
	}, extra...)
	var stdout bytes.Buffer
	if err := run(args, &stdout, io.Discard); err != nil {
		t.Fatalf("run: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout.String())
	}
	return out
}

func TestCLIExecutePrintsSupersededCount(t *testing.T) {
	// The JSON surface always carries the counter (0 on an ordinary run, so a
	// log reader never has to distinguish absent from zero).
	out := executeWithFlags(t, "sess-sup-json", 7300, "use Go for the API service")
	if out["status"] != "completed" {
		t.Fatalf("status = %v", out["status"])
	}
	if out["written_count"].(float64) != 1 {
		t.Fatalf("written_count = %v, want 1", out["written_count"])
	}
	n, present := out["superseded_count"].(float64)
	if !present {
		t.Fatalf("superseded_count must be present in the execute JSON: %v", out)
	}
	if n != 0 {
		t.Fatalf("superseded_count = %v, want 0 on an ordinary run", n)
	}
	if _, present := out["provisional"]; present {
		t.Fatalf("an ordinary run must not print a provisional field: %v", out["provisional"])
	}
}

func TestCLIExecuteHumanLineCarriesSuperseded(t *testing.T) {
	home := setupCLIEnv(t)
	ids := seedSubstantiveSession(t, "memoire-test", "sess-sup-human", 7400)
	modelJSON := fmt.Sprintf(`{"schema_version":"1","observations":[{"kind":"decision","text":"use Go for the API service","confidence":0.86,"source_event_ids":[%d],"rationale":"explicit"}]}`, ids[0])
	srv := fakeQwenServer(t, modelJSON)
	defer srv.Close()
	writeEndpointsConfig(t, home, srv.URL, "MEMOIRE_TEST_QWEN_KEY", "qwen2.5-7b")
	t.Setenv("MEMOIRE_TEST_QWEN_KEY", "placeholder-not-a-real-token")

	var stdout bytes.Buffer
	if err := run([]string{
		"-strategy=model", "-execute",
		"-provider=local-qwen", "-model=qwen2.5-7b", "-endpoint-id=hl3",
		"-project=memoire-test", "-session-id=sess-sup-human",
	}, &stdout, io.Discard); err != nil {
		t.Fatalf("run: %v", err)
	}
	line := stdout.String()
	if !strings.Contains(line, "written=1 superseded=0 rejected=0") {
		t.Fatalf("human line must carry the supersede counter beside written: %q", line)
	}
}

func TestCLIExecuteProvisionalFlagIsAccepted(t *testing.T) {
	// The flag reaches the orchestrator (its run-meta and observation-meta
	// effects are pinned in the orchestrator battery); here the CLI contract:
	// accepted with -execute, and the run still completes normally.
	out := executeWithFlags(t, "sess-prov-cli", 7500, "mark the open tail provisional", "-provisional")
	if out["status"] != "completed" {
		t.Fatalf("status = %v, want completed", out["status"])
	}
	if out["written_count"].(float64) != 1 {
		t.Fatalf("written_count = %v, want 1", out["written_count"])
	}
}

func TestCLIExecuteNoSupersedeFlagIsAccepted(t *testing.T) {
	out := executeWithFlags(t, "sess-nosup-cli", 7600, "use Go for the API service", "-no-supersede")
	if out["status"] != "completed" {
		t.Fatalf("status = %v, want completed", out["status"])
	}
	if out["superseded_count"].(float64) != 0 {
		t.Fatalf("superseded_count = %v, want 0 under the kill switch", out["superseded_count"])
	}
}
