// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"memoire/services/api/internal/sweep"
)

func TestRunDrivesRealCLIDoor(t *testing.T) {
	for _, bin := range []string{"age", "age-keygen", "tar"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available: %v", bin, err)
		}
	}
	root := t.TempDir()
	t.Chdir(root)

	rawDir := filepath.Join(root, "raw")
	if err := os.MkdirAll(rawDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rawName := "1700000000000-cli-aaaaaaaa.json"
	rawBody := []byte(`{"schema_version":1,"project":"memoire-test","session_id":"cli","source_adapter":"manual","event_type":"note","payload":{"message":"key AKIA` +
		`IOSFODNN7EXAMPLE leaked"},"metadata":{}}`)
	if err := os.WriteFile(filepath.Join(rawDir, rawName), rawBody, 0o600); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(root, "redaction-key")
	if err := os.WriteFile(keyPath, []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	identityPath := filepath.Join(root, "age-identity.txt")
	out, err := exec.Command("age-keygen", "-o", identityPath).CombinedOutput()
	if err != nil {
		t.Fatalf("age-keygen: %v\n%s", err, string(out))
	}
	recipient := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "Public key:"))
	if !strings.HasPrefix(recipient, "age1") {
		t.Fatalf("bad recipient from age-keygen: %q", recipient)
	}

	outputDir := filepath.Join(root, "redacted")
	quarantineOut := filepath.Join(root, "quarantine.age")
	reportPath := filepath.Join(root, "report.json")
	var stdout, stderr bytes.Buffer
	err = run([]string{
		"--raw-dir", rawDir,
		"--output-dir", outputDir,
		"--quarantine-out", quarantineOut,
		"--age-recipient", recipient,
		"--report", reportPath,
		"--redaction-key", keyPath,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v\nstderr:\n%s", err, stderr.String())
	}

	var stdoutReport sweep.Report
	if err := json.Unmarshal(stdout.Bytes(), &stdoutReport); err != nil {
		t.Fatalf("stdout is not a report JSON object: %v\n%s", err, stdout.String())
	}
	if stdoutReport.Tool != sweep.ToolName {
		t.Fatalf("stdout report tool = %q, want %q", stdoutReport.Tool, sweep.ToolName)
	}
	reportBytes, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(reportBytes) {
		t.Fatalf("report file is not valid JSON:\n%s", string(reportBytes))
	}
	evidenceBytes, err := os.ReadFile(filepath.Join(root, sweep.DefaultEvidencePath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reportBytes, evidenceBytes) {
		t.Fatalf("CLI report and evidence artifact differ")
	}
	redacted, err := os.ReadFile(filepath.Join(outputDir, rawName))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(redacted, []byte("AKIA" + "IOSFODNN7EXAMPLE")) {
		t.Fatalf("CLI output still contains secret:\n%s", string(redacted))
	}
	if _, err := os.Stat(quarantineOut); err != nil {
		t.Fatalf("quarantine archive missing: %v", err)
	}
}
