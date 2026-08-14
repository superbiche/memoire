// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package modelderive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// Provider is the abstraction over a model backend. Slice 4 ships only the
// interface, the registry, and the orchestration that drives it with fake
// providers in tests. Real adapters (local-qwen, openai) arrive in later slices.
//
// Generate must not be called by the orchestrator until provider/model/window/
// limits and data-boundary discipline have been validated and the prompt has
// been built (docs/MODEL-PROVIDER.md "Data Boundary Discipline").
type Provider interface {
	// Name is the provider id, one of the contract providers, e.g.
	// "local-qwen" or "openai". It must equal Request.Provider for a run.
	Name() string
	// DataBoundary is "local" or "external" and must match the provider family
	// (local-qwen/manual -> local, openai -> external).
	DataBoundary() string
	// Generate calls the model backend. The orchestrator enforces the timeout
	// via ctx; implementations must honor ctx cancellation.
	Generate(ctx context.Context, req Request) (ProviderResponse, error)
}

// Usage is best-effort token/cost accounting reported by a provider. Fields are
// zero when the provider does not report them.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ProviderResponse is the raw, unparsed result of a provider call. The
// orchestrator owns parsing (via ParseResponse), validation, and all store
// writes; a provider never writes observations.
type ProviderResponse struct {
	// RawResponse is the strict-JSON model response to be parsed. It is empty
	// when the provider refused or failed before producing output.
	RawResponse json.RawMessage
	// Usage is best-effort accounting recorded in run metadata.
	Usage Usage
	// Refusal is a non-empty operator-facing reason when the model refused.
	Refusal string
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Provider{}
)

// RegisterProvider records a provider under its Name() for later lookup.
// Registration is last-write-wins so repeated test registration of the same
// name does not error; an empty name is rejected.
func RegisterProvider(p Provider) error {
	if p == nil {
		return errors.New("modelderive: nil provider")
	}
	name := p.Name()
	if name == "" {
		return errors.New("modelderive: provider name is required")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = p
	return nil
}

// LookupProvider returns the registered provider for name.
func LookupProvider(name string) (Provider, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	p, ok := registry[name]
	return p, ok
}

// validateDataBoundary mirrors the store's provider/data-boundary discipline so
// the orchestrator fails before prompt construction on a mismatch.
func validateDataBoundary(provider, boundary string) error {
	switch provider {
	case "openai":
		if boundary != "external" {
			return fmt.Errorf("modelderive: provider %q must declare external data boundary, got %q", provider, boundary)
		}
	case "local-qwen", "manual", "rules":
		if boundary != "local" {
			return fmt.Errorf("modelderive: provider %q must declare local data boundary, got %q", provider, boundary)
		}
	case ProviderBYOK:
		// The user declares the boundary (BYOK design Q2, operator-ruled
		// 2026-08-05); it must still be one of the two legal values.
		if boundary != "local" && boundary != "external" {
			return fmt.Errorf("modelderive: provider %q must declare a local or external data boundary, got %q", provider, boundary)
		}
	default:
		return fmt.Errorf("modelderive: unknown provider %q", provider)
	}
	return nil
}
