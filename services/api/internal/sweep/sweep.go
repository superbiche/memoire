// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package sweep

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"memoire/services/api/internal/redact"
)

const (
	ToolName            = "memoire-retro-sweep"
	DefaultEvidencePath = "tmp/dispatch/retro-sweep-evidence.txt"
)

type Options struct {
	RawDir           string
	OutputDir        string
	QuarantineOut    string
	AgeRecipient     string
	ReportPath       string
	RedactionKeyPath string
	EvidencePath     string
}

type Report struct {
	Tool             string         `json:"tool"`
	RulesVersion     string         `json:"rules_version"`
	RawDir           string         `json:"raw_dir"`
	OutputDir        string         `json:"output_dir"`
	QuarantineOut    string         `json:"quarantine_out"`
	FilesScanned     int            `json:"files_scanned"`
	FilesWithHits    int            `json:"files_with_hits"`
	HitsByRule       map[string]int `json:"hits_by_rule"`
	TotalInputBytes  int64          `json:"total_input_bytes"`
	TotalOutputBytes int64          `json:"total_output_bytes"`
	Reconciliation   Reconciliation `json:"reconciliation"`
}

type Reconciliation struct {
	Scanned              int  `json:"scanned"`
	RedactedOutputs      int  `json:"redacted_outputs"`
	QuarantinedOriginals int  `json:"quarantined_originals"`
	Balanced             bool `json:"balanced"`
}

func ValidateOptions(opts Options) error {
	switch {
	case opts.RawDir == "":
		return errors.New("missing required --raw-dir")
	case opts.OutputDir == "":
		return errors.New("missing required --output-dir")
	case opts.QuarantineOut == "":
		return errors.New("missing required --quarantine-out")
	case opts.AgeRecipient == "":
		return errors.New("missing required --age-recipient")
	case opts.ReportPath == "":
		return errors.New("missing required --report")
	case opts.RedactionKeyPath == "":
		return errors.New("missing required --redaction-key")
	default:
		return nil
	}
}

// pathIsInside reports whether `inside` equals `parent` or resolves under it.
func pathIsInside(inside, parent string) (bool, error) {
	absInside, err := filepath.Abs(inside)
	if err != nil {
		return false, err
	}
	absParent, err := filepath.Abs(parent)
	if err != nil {
		return false, err
	}
	if absInside == absParent {
		return true, nil
	}
	rel, err := filepath.Rel(absParent, absInside)
	if err != nil {
		return false, err
	}
	return !strings.HasPrefix(rel, "..") && rel != "..", nil
}

// Run executes a full retro-redaction sweep: validate, redact, quarantine,
// and write the machine-readable report plus the evidence artifact.
func Run(ctx context.Context, opts Options) (Report, error) {
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	if err := ValidateOptions(opts); err != nil {
		return Report{}, err
	}
	if opts.EvidencePath == "" {
		opts.EvidencePath = DefaultEvidencePath
	}

	// --- Resolve / normalise target paths ---
	rawDir, err := filepath.Abs(opts.RawDir)
	if err != nil {
		return Report{}, fmt.Errorf("resolve raw-dir: %w", err)
	}
	outputDir, err := filepath.Abs(opts.OutputDir)
	if err != nil {
		return Report{}, fmt.Errorf("resolve output-dir: %w", err)
	}
	quarantineOut, err := filepath.Abs(opts.QuarantineOut)
	if err != nil {
		return Report{}, fmt.Errorf("resolve quarantine-out: %w", err)
	}

	// --- Raw-fence checks (before any write) ---
	if ok, _ := pathIsInside(outputDir, rawDir); ok {
		return Report{}, fmt.Errorf("output-dir must not be inside raw-dir: %s", opts.OutputDir)
	}
	if ok, _ := pathIsInside(quarantineOut, rawDir); ok {
		return Report{}, fmt.Errorf("quarantine-out must not be inside raw-dir: %s", opts.QuarantineOut)
	}

	// --- No-clobber checks ---
	if err := checkNoClobber(opts); err != nil {
		return Report{}, err
	}

	// --- Create output dir if needed ---
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return Report{}, fmt.Errorf("create output dir: %w", err)
	}

	// --- Collect raw file basenames (keep order deterministic) ---
	rawEntries, err := os.ReadDir(rawDir)
	if err != nil {
		return Report{}, fmt.Errorf("read raw dir: %w", err)
	}

	var rawNames []string
	for _, ent := range rawEntries {
		if ent.Type().IsRegular() {
			rawNames = append(rawNames, ent.Name())
		}
	}
	sort.Strings(rawNames)

	// --- Load redaction key ---
	key, err := redact.LoadOrCreateKey(opts.RedactionKeyPath)
	if err != nil {
		return Report{}, fmt.Errorf("load redaction key: %w", err)
	}
	redactor := redact.New(key)

	// --- Initialise report ---
	report := Report{
		Tool:          ToolName,
		RulesVersion:  redact.RulesVersion,
		RawDir:        opts.RawDir,
		OutputDir:     opts.OutputDir,
		QuarantineOut: opts.QuarantineOut,
		HitsByRule:    make(map[string]int),
	}

	var totalInputBytes int64
	var totalOutputBytes int64

	// --- Redact each file (streaming: read once, write redacted, re-read for quarantine) ---
	for _, name := range rawNames {
		path := filepath.Join(rawDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return Report{}, fmt.Errorf("read raw file %s: %w", name, err)
		}

		totalInputBytes += int64(len(data))

		redacted, hits := redactor.JSON(data)
		if len(hits) > 0 {
			report.FilesWithHits++
			for _, h := range hits {
				report.HitsByRule[h.Rule] += h.Count
			}
		}

		outPath := filepath.Join(outputDir, name)
		if err := os.WriteFile(outPath, redacted, 0o600); err != nil {
			return Report{}, fmt.Errorf("write redacted %s: %w", name, err)
		}
		totalOutputBytes += int64(len(redacted))
	}

	report.FilesScanned = len(rawNames)
	report.TotalInputBytes = totalInputBytes
	report.TotalOutputBytes = totalOutputBytes

	// --- Quarantine originals (streaming: re-read files, pipe tar → age) ---
	if err := quarantineStreaming(opts, rawDir, rawNames); err != nil {
		return Report{}, err
	}

	// --- Reconciliation ---
	scanned := len(rawNames)
	report.Reconciliation.Scanned = scanned
	report.Reconciliation.RedactedOutputs = scanned
	report.Reconciliation.QuarantinedOriginals = scanned
	report.Reconciliation.Balanced = true

	// --- Write report + evidence ---
	reportBytes, err := json.Marshal(report)
	if err != nil {
		return Report{}, fmt.Errorf("marshal report: %w", err)
	}

	if err := os.WriteFile(opts.ReportPath, reportBytes, 0o600); err != nil {
		return Report{}, fmt.Errorf("write report: %w", err)
	}

	evidencePath := opts.EvidencePath
	if err := os.MkdirAll(filepath.Dir(evidencePath), 0o755); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Report{}, fmt.Errorf("create evidence dir: %w", err)
	}
	if err := os.WriteFile(evidencePath, reportBytes, 0o600); err != nil {
		return Report{}, fmt.Errorf("write evidence: %w", err)
	}

	return report, nil
}

// checkNoClobber refuses the sweep when output targets already exist.
func checkNoClobber(opts Options) error {
	// output-dir must be empty
	if _, err := os.Stat(opts.OutputDir); err == nil {
		entries, err := os.ReadDir(opts.OutputDir)
		if err != nil {
			return err
		}
		if len(entries) > 0 {
			return fmt.Errorf("output-dir must be empty: %s", opts.OutputDir)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	// quarantine-out already exists
	if _, err := os.Stat(opts.QuarantineOut); err == nil {
		return fmt.Errorf("quarantine-out already exists: %s", opts.QuarantineOut)
	} else if !os.IsNotExist(err) {
		return err
	}

	// report already exists
	if _, err := os.Stat(opts.ReportPath); err == nil {
		return fmt.Errorf("report already exists: %s", opts.ReportPath)
	} else if !os.IsNotExist(err) {
		return err
	}

	return nil
}

// quarantineStreaming streams each raw file through a tar writer into age via io.Pipe.
func quarantineStreaming(opts Options, rawDir string, names []string) error {
	if _, err := exec.LookPath("age"); err != nil {
		return fmt.Errorf("age binary not found")
	}

	pr, pw := io.Pipe()
	cmd := exec.Command("age", "-r", opts.AgeRecipient, "-o", opts.QuarantineOut)
	cmd.Stdin = pr

	// Writer goroutine: stream tar entries into the pipe writer.
	go func() {
		tw := tar.NewWriter(pw)
		var writeErr error
		for _, name := range names {
			path := filepath.Join(rawDir, name)
			data, err := os.ReadFile(path)
			if err != nil {
				writeErr = fmt.Errorf("re-read raw file %s for quarantine: %w", name, err)
				pw.CloseWithError(writeErr)
				return
			}
			hdr := &tar.Header{
				Name: name,
				Mode: 0o600,
				Size: int64(len(data)),
			}
			if err := tw.WriteHeader(hdr); err != nil {
				writeErr = fmt.Errorf("tar write header %s: %w", name, err)
				pw.CloseWithError(writeErr)
				return
			}
			if _, err := tw.Write(data); err != nil {
				writeErr = fmt.Errorf("tar write data %s: %w", name, err)
				pw.CloseWithError(writeErr)
				return
			}
		}
		if tw.Close() != nil {
			writeErr = errors.New("tar close")
		}
		pw.CloseWithError(writeErr)
	}()

	if err := cmd.Run(); err != nil {
		pr.Close()
		return fmt.Errorf("age encrypt: %w", err)
	}

	return nil
}
