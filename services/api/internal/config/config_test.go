// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadModelEndpoints(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing file is a named operator error", func(t *testing.T) {
		path := filepath.Join(dir, "absent.json")
		_, err := LoadModelEndpoints(path)
		if err == nil {
			t.Fatalf("expected error for missing file")
		}
		if !strings.Contains(err.Error(), path) {
			t.Fatalf("error should name the path, got: %v", err)
		}
	})

	t.Run("invalid JSON errors", func(t *testing.T) {
		path := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadModelEndpoints(path); err == nil {
			t.Fatalf("expected error for invalid JSON")
		}
	})

	t.Run("empty endpoint list errors", func(t *testing.T) {
		path := filepath.Join(dir, "empty.json")
		if err := os.WriteFile(path, []byte(`{"endpoints":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadModelEndpoints(path); err == nil {
			t.Fatalf("expected error for empty endpoint list")
		}
	})

	t.Run("valid registry loads with api_key_env as a name only", func(t *testing.T) {
		path := filepath.Join(dir, "good.json")
		body := `{"endpoints":[{"endpoint_id":"hl3","base_url":"http://host:8000","api_key_env":"HL3_QWEN_API_KEY","models":["m1","m2"]}]}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		eps, err := LoadModelEndpoints(path)
		if err != nil {
			t.Fatalf("LoadModelEndpoints: %v", err)
		}
		if len(eps) != 1 {
			t.Fatalf("len = %d, want 1", len(eps))
		}
		if eps[0].EndpointID != "hl3" || eps[0].BaseURL != "http://host:8000" {
			t.Fatalf("unexpected endpoint: %+v", eps[0])
		}
		if eps[0].APIKeyEnv != "HL3_QWEN_API_KEY" {
			t.Fatalf("api_key_env = %q, want the env var NAME", eps[0].APIKeyEnv)
		}
		if len(eps[0].Models) != 2 {
			t.Fatalf("models = %v, want 2", eps[0].Models)
		}
	})
}

func TestLoadDeriveDenylist(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing file is an empty denylist, not an error", func(t *testing.T) {
		d, err := LoadDeriveDenylist(filepath.Join(dir, "absent.json"))
		if err != nil {
			t.Fatalf("missing file must not error: %v", err)
		}
		if d.Denies("tmp") {
			t.Fatalf("empty denylist must deny nothing")
		}
	})

	t.Run("malformed JSON is a loud operator error", func(t *testing.T) {
		path := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadDeriveDenylist(path)
		if err == nil {
			t.Fatalf("expected error for malformed denylist")
		}
		if !strings.Contains(err.Error(), path) {
			t.Fatalf("error should name the path, got: %v", err)
		}
	})

	t.Run("valid file loads", func(t *testing.T) {
		path := filepath.Join(dir, "good.json")
		if err := os.WriteFile(path, []byte(`{"deny_projects":["tmp",".agents","agent-*"]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		d, err := LoadDeriveDenylist(path)
		if err != nil {
			t.Fatalf("LoadDeriveDenylist: %v", err)
		}
		if len(d.DenyProjects) != 3 {
			t.Fatalf("deny_projects = %v, want 3 entries", d.DenyProjects)
		}
	})
}

func TestDeriveDenylistDenies(t *testing.T) {
	d := DeriveDenylist{DenyProjects: []string{"tmp", ".agents", "agent-*", "observer-sessions", ""}}

	cases := []struct {
		project string
		want    bool
	}{
		{"tmp", true},
		{".agents", true},
		{"agent-a3ed1dd23680b2e9b", true}, // glob: subagent transcript capture
		{"agent-", true},
		{"observer-sessions", true}, // memoire:D148: denied (turn-ingest supersede ratified)
		{"memoire", false},
		{"agents", false}, // no substring matching
		{"tmp2", false},   // exact means exact
		{"my-agent-x", false},
		{"", false}, // empty project never matches the empty pattern
	}
	for _, tc := range cases {
		if got := d.Denies(tc.project); got != tc.want {
			t.Fatalf("Denies(%q) = %v, want %v", tc.project, got, tc.want)
		}
	}
}

func TestLoadSetsDeriveDenylistPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MEMOIRE_HOME", home)
	t.Setenv("MEMOIRE_DERIVE_DENYLIST", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(home, "derive-denylist.json"); cfg.DeriveDenylistPath != want {
		t.Fatalf("DeriveDenylistPath = %q, want %q", cfg.DeriveDenylistPath, want)
	}

	t.Setenv("MEMOIRE_DERIVE_DENYLIST", "/custom/denylist.json")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DeriveDenylistPath != "/custom/denylist.json" {
		t.Fatalf("override not honored: %q", cfg.DeriveDenylistPath)
	}
}

func TestDeriveDenylistFollowsEndpointRegistryDir(t *testing.T) {
	// The denylist is documented as living NEXT TO the endpoint registry: an
	// overridden registry path must move the denylist default with it, or a
	// co-located denylist silently loads as empty (orthogonal review, MAJOR).
	home := t.TempDir()
	t.Setenv("MEMOIRE_HOME", home)
	t.Setenv("MEMOIRE_DERIVE_DENYLIST", "")
	t.Setenv("MEMOIRE_MODEL_ENDPOINTS", "/shared/registry/model-endpoints.json")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := "/shared/registry/derive-denylist.json"; cfg.DeriveDenylistPath != want {
		t.Fatalf("DeriveDenylistPath = %q, want %q (co-located with registry)", cfg.DeriveDenylistPath, want)
	}
}

func TestLoadSetsModelEndpointsPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MEMOIRE_HOME", home)
	t.Setenv("MEMOIRE_MODEL_ENDPOINTS", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := filepath.Join(home, "model-endpoints.json")
	if cfg.ModelEndpointsPath != want {
		t.Fatalf("ModelEndpointsPath = %q, want %q", cfg.ModelEndpointsPath, want)
	}

	t.Setenv("MEMOIRE_MODEL_ENDPOINTS", "/custom/registry.json")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ModelEndpointsPath != "/custom/registry.json" {
		t.Fatalf("override not honored: %q", cfg.ModelEndpointsPath)
	}
}

func TestDefaultSchemaDirPrefersCwdThenExeSibling(t *testing.T) {
	// cwd with a schema/ dir wins (repo flow).
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "schema"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	if got := defaultSchemaDir(); got != "schema" {
		t.Fatalf("cwd schema present: got %q, want \"schema\"", got)
	}

	// No cwd schema and no exe-adjacent schema (the test binary lives in a
	// go-build temp dir): the bare relative fallback is preserved so the
	// store error keeps naming it.
	t.Chdir(t.TempDir())
	if got := defaultSchemaDir(); got != "schema" {
		t.Fatalf("no schema anywhere: got %q, want \"schema\" fallback", got)
	}
}

// F4 (D218 review): the /embed bearer is the dedicated credential when
// configured; unset falls back to the custody token; a configured file
// that is missing or empty fails loudly — never a silent downgrade to
// the admin bearer.
func TestEmbedToken(t *testing.T) {
	unset := Config{}
	if got, err := unset.EmbedToken("custody"); err != nil || got != "custody" {
		t.Fatalf("unset: got %q err %v, want custody fallback", got, err)
	}

	dir := t.TempDir()
	file := filepath.Join(dir, "embed-token")
	if err := os.WriteFile(file, []byte("embed-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{EmbedTokenFile: file}
	if got, err := cfg.EmbedToken("custody"); err != nil || got != "embed-secret" {
		t.Fatalf("configured: got %q err %v, want embed-secret", got, err)
	}

	missing := Config{EmbedTokenFile: filepath.Join(dir, "absent")}
	if _, err := missing.EmbedToken("custody"); err == nil {
		t.Fatal("missing configured file: want loud error, got fallback")
	}

	if err := os.WriteFile(file, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.EmbedToken("custody"); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty configured file: want empty error, got %v", err)
	}

	// Round-1 F1 (operator-ruled): a configured token equal to the custody
	// token voids the privilege split — refused loudly, values not echoed.
	// Round-2 N1: the fixture is a distinct non-label value so the no-echo
	// assertion cannot false-green on prose words like "custody".
	const shared = "tok-3f9c2a71d"
	if err := os.WriteFile(file, []byte(shared+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := cfg.EmbedToken(shared)
	if err == nil || !strings.Contains(err.Error(), "must be distinct") {
		t.Fatalf("configured token equal to custody: want refusal, got %v", err)
	}
	if strings.Contains(err.Error(), shared) {
		t.Fatalf("refusal must not echo the token value: %v", err)
	}
}

func TestLoadReadsEmbedTokenFile(t *testing.T) {
	t.Setenv("MEMOIRE_EMBED_TOKEN_FILE", "/secrets/embed/token")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.EmbedTokenFile != "/secrets/embed/token" {
		t.Fatalf("EmbedTokenFile: %q", cfg.EmbedTokenFile)
	}
}
