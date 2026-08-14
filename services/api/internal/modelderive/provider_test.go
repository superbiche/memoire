// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package modelderive

import (
	"context"
	"testing"
)

type stubProvider struct {
	name     string
	boundary string
}

func (s stubProvider) Name() string         { return s.name }
func (s stubProvider) DataBoundary() string { return s.boundary }
func (s stubProvider) Generate(context.Context, Request) (ProviderResponse, error) {
	return ProviderResponse{}, nil
}

func TestRegisterAndLookupProvider(t *testing.T) {
	p := stubProvider{name: "test-fake", boundary: "local"}
	if err := RegisterProvider(p); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, ok := LookupProvider("test-fake")
	if !ok {
		t.Fatalf("expected provider registered")
	}
	if got.Name() != "test-fake" {
		t.Fatalf("unexpected provider name %q", got.Name())
	}
	if _, ok := LookupProvider("absent"); ok {
		t.Fatalf("expected absent provider lookup to fail")
	}
}

func TestRegisterProviderRejectsEmptyName(t *testing.T) {
	if err := RegisterProvider(stubProvider{name: ""}); err == nil {
		t.Fatalf("expected error for empty provider name")
	}
	if err := RegisterProvider(nil); err == nil {
		t.Fatalf("expected error for nil provider")
	}
}

func TestRegisterProviderLastWriteWins(t *testing.T) {
	if err := RegisterProvider(stubProvider{name: "dup", boundary: "local"}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := RegisterProvider(stubProvider{name: "dup", boundary: "external"}); err != nil {
		t.Fatalf("second register should not error (last-write-wins): %v", err)
	}
	got, _ := LookupProvider("dup")
	if got.DataBoundary() != "external" {
		t.Fatalf("expected last registration to win, got boundary %q", got.DataBoundary())
	}
}

func TestValidateDataBoundary(t *testing.T) {
	cases := []struct {
		provider string
		boundary string
		wantErr  bool
	}{
		{"openai", "external", false},
		{"openai", "local", true},
		{"local-qwen", "local", false},
		{"local-qwen", "external", true},
		{"manual", "local", false},
		{"bogus", "local", true},
	}
	for _, c := range cases {
		err := validateDataBoundary(c.provider, c.boundary)
		if (err != nil) != c.wantErr {
			t.Fatalf("validateDataBoundary(%q,%q) err=%v wantErr=%v", c.provider, c.boundary, err, c.wantErr)
		}
	}
}
