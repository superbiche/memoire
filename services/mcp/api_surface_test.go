// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/mark3labs/mcp-go/server"
)

// The MCP tools are half of memoire's versioned API surface (D172); the other
// half is the HTTP route set, checked by the twin test in
// services/api/internal/server. This test asks the server for the tool list
// exactly as a harness does (tools/list over the in-process JSON-RPC entry
// point) and diffs what it serves against the tracked inventory, so a tool,
// parameter, type, or requiredness change cannot land without a deliberate
// inventory edit.
const apiSurfacePath = "../../docs/API-SURFACE.json"

type apiSurface struct {
	MCPTools []toolSurface `json:"mcp_tools"`
}

type toolSurface struct {
	Name   string         `json:"name"`
	Params []paramSurface `json:"params"`
}

type paramSurface struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	ItemsType string `json:"items_type,omitempty"`
	Required  bool   `json:"required"`
}

func TestMCPSurfaceMatchesInventory(t *testing.T) {
	raw, err := os.ReadFile(apiSurfacePath)
	if err != nil {
		t.Fatalf("read API surface inventory: %v", err)
	}
	var doc apiSurface
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", apiSurfacePath, err)
	}

	want := map[string]string{}
	for _, tool := range doc.MCPTools {
		if _, dup := want[tool.Name]; dup {
			t.Fatalf("%s lists tool %q twice", apiSurfacePath, tool.Name)
		}
		want[tool.Name] = renderParams(tool.Params)
	}

	got := map[string]string{}
	for _, tool := range servedTools(t) {
		got[tool.Name] = renderParams(tool.Params)
	}

	for _, name := range sortedKeys(got) {
		listed, ok := want[name]
		switch {
		case !ok:
			t.Errorf("tools/list serves %q but %s does not list it (a surface change is a contract change: add it, and rule whether the bump is BREAKING or ADDITIVE)", name, apiSurfacePath)
		case listed != got[name]:
			t.Errorf("tool %q serves params %s but %s declares %s", name, got[name], apiSurfacePath, listed)
		}
	}
	for _, name := range sortedKeys(want) {
		if _, ok := got[name]; !ok {
			t.Errorf("%s lists tool %q but tools/list does not serve it", apiSurfacePath, name)
		}
	}
}

// servedTools builds the server exactly as main does and reads the tool list
// off the JSON-RPC surface, so the comparison is against what a client is
// served rather than against the registration code.
func servedTools(t *testing.T) []toolSurface {
	t.Helper()
	// An unreachable API and an absent token are deliberate: tools/list must
	// answer without either (no tool is called here).
	client := newAPIClient("http://127.0.0.1:1", filepath.Join(t.TempDir(), "token"), true, "")
	s := server.NewMCPServer(serverName, serverVersion, server.WithToolCapabilities(false))
	registerTools(s, client)

	var tools []toolSurface
	cursor := ""
	for {
		req := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"cursor":%q}}`, cursor)
		resp, err := json.Marshal(s.HandleMessage(t.Context(), json.RawMessage(req)))
		if err != nil {
			t.Fatalf("marshal tools/list response: %v", err)
		}
		var page struct {
			Result *struct {
				Tools []struct {
					Name        string `json:"name"`
					InputSchema struct {
						Properties map[string]struct {
							Type  string `json:"type"`
							Items *struct {
								Type string `json:"type"`
							} `json:"items"`
						} `json:"properties"`
						Required []string `json:"required"`
					} `json:"inputSchema"`
				} `json:"tools"`
				NextCursor string `json:"nextCursor"`
			} `json:"result"`
			Error *json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(resp, &page); err != nil {
			t.Fatalf("parse tools/list response: %v", err)
		}
		if page.Result == nil {
			t.Fatalf("tools/list failed: %s", string(resp))
		}
		for _, served := range page.Result.Tools {
			required := map[string]bool{}
			for _, name := range served.InputSchema.Required {
				required[name] = true
			}
			tool := toolSurface{Name: served.Name}
			for name, prop := range served.InputSchema.Properties {
				param := paramSurface{Name: name, Type: prop.Type, Required: required[name]}
				if prop.Items != nil {
					param.ItemsType = prop.Items.Type
				}
				tool.Params = append(tool.Params, param)
			}
			tools = append(tools, tool)
		}
		cursor = page.Result.NextCursor
		if cursor == "" {
			return tools
		}
	}
}

// renderParams is the order-independent comparison form: one sorted line per
// tool, so a diff names the parameter that moved.
func renderParams(params []paramSurface) string {
	lines := make([]string, 0, len(params))
	for _, p := range params {
		line := p.Name + ":" + p.Type
		if p.ItemsType != "" {
			line += "[" + p.ItemsType + "]"
		}
		if p.Required {
			line += " (required)"
		}
		lines = append(lines, line)
	}
	sort.Strings(lines)
	return "[" + fmt.Sprint(lines) + "]"
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
