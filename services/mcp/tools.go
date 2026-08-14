// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Defaults chosen for the memory-query use case: raw capture projections are
// excluded and unreviewed (proposed or lapsed) observations are included,
// marked with `~`. Pass exclude_kind="" to search raw events too.
func registerTools(s *server.MCPServer, c *apiClient) {
	s.AddTool(
		mcp.NewTool("memoire_search",
			mcp.WithDescription("Full-text search over Memoire observations. Returns compact hits — enough to judge relevance in one trip; fetch full bodies with memoire_get_observations. One line per hit: `[id] kind~ YYYY-MM-DD project provenance/review_state[!]: snippet`. `~` after kind marks unreviewed observations (proposed or lapsed). provenance is how the observation came to exist: observed (captured verbatim), inferred (derived by a model), user_confirmed (the operator confirmed it), imported, generated; `-` means the server sent none. review_state is the governance state: proposed, accepted, rejected, superseded, lapsed. A trailing `!` after review_state means can_instruct — an elevated operating note the operator promoted to instruction-grade; weigh those above ordinary hits."),
			mcp.WithString("q", mcp.Required(), mcp.Description("FTS5 query (words, phrases, OR, NEAR)")),
			mcp.WithString("project", mcp.Description("Limit to one project (e.g. 'liaison')")),
			mcp.WithString("kind", mcp.Description("Limit to one kind: fact, decision, gotcha, preference, task, open_question, constraint, raw")),
			mcp.WithString("exclude_kind", mcp.Description("Kind to exclude. Default 'raw' (capture noise); pass '' to include raw events")),
			mcp.WithNumber("limit", mcp.Description("Max hits, default 20")),
			mcp.WithBoolean("include_proposed", mcp.Description("Include unreviewed (proposed/lapsed) observations, default true")),
		),
		handleSearch(c),
	)

	s.AddTool(
		mcp.NewTool("memoire_timeline",
			mcp.WithDescription("Most recent Memoire observations, newest first (no query needed). Same compact hit format as memoire_search: `[id] kind~ YYYY-MM-DD project provenance/review_state[!]: snippet` — `~` marks unreviewed (proposed/lapsed), provenance is observed|inferred|user_confirmed|imported|generated (`-` when absent), and a trailing `!` marks can_instruct, an elevated operating note."),
			mcp.WithString("project", mcp.Description("Limit to one project")),
			mcp.WithString("exclude_kind", mcp.Description("Kind to exclude. Default 'raw'; pass '' to include raw events")),
			mcp.WithNumber("limit", mcp.Description("Max hits, default 15")),
			mcp.WithBoolean("include_proposed", mcp.Description("Include unreviewed (proposed/lapsed) observations, default true")),
		),
		handleTimeline(c),
	)

	s.AddTool(
		mcp.NewTool("memoire_get_observations",
			mcp.WithDescription("Fetch full observation bodies by id (ids come from memoire_search / memoire_timeline hits)."),
			mcp.WithArray("ids", mcp.Required(), mcp.WithNumberItems(), mcp.Description("Observation ids, e.g. [82446, 82452]")),
		),
		handleGetObservations(c),
	)

	s.AddTool(
		mcp.NewTool("memoire_context",
			mcp.WithDescription("Prompt-ready context block for a topic: searches observations and returns a pre-formatted text block suitable for direct inclusion in a prompt."),
			mcp.WithString("q", mcp.Required(), mcp.Description("Topic query (FTS5)")),
			mcp.WithString("project", mcp.Description("Limit to one project")),
			mcp.WithNumber("limit", mcp.Description("Max observations, default 10")),
		),
		handleContext(c),
	)
}

type toolHandler = func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error)

func readFilterParams(request mcp.CallToolRequest, defaultLimit int) url.Values {
	params := url.Values{}
	if p := request.GetString("project", ""); p != "" {
		params.Set("project", p)
	}
	if k := request.GetString("kind", ""); k != "" {
		params.Set("kind", k)
	}
	if ek := request.GetString("exclude_kind", "raw"); ek != "" {
		params.Set("exclude_kind", ek)
	}
	if request.GetBool("include_proposed", true) {
		params.Set("include_proposed", "true")
	}
	intParam(params, "limit", request.GetInt("limit", defaultLimit))
	return params
}

func handleSearch(c *apiClient) toolHandler {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		q, err := request.RequireString("q")
		if err != nil {
			return mcp.NewToolResultError("q is required"), nil
		}
		params := readFilterParams(request, 20)
		params.Set("q", q)
		var resp hitsResponse
		if err := c.get("/v1/search", params, &resp); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(withIdentityAlerts(resp.IdentityAlerts, compactLines(resp.Hits))), nil
	}
}

func handleTimeline(c *apiClient) toolHandler {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		params := readFilterParams(request, 15)
		var resp hitsResponse
		if err := c.get("/v1/timeline", params, &resp); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(withIdentityAlerts(resp.IdentityAlerts, compactLines(resp.Hits))), nil
	}
}

type observationRecord struct {
	ID              int64   `json:"id"`
	Project         string  `json:"project"`
	SessionID       string  `json:"session_id,omitempty"`
	Kind            string  `json:"kind"`
	Text            string  `json:"text"`
	OccurredAtEpoch int64   `json:"occurred_at_epoch"`
	ReviewState     string  `json:"review_state"`
	Confidence      float64 `json:"confidence,omitempty"`
	DerivationRunID string  `json:"derivation_run_id,omitempty"`
}

func handleGetObservations(c *apiClient) toolHandler {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ids, err := request.RequireIntSlice("ids")
		if err != nil || len(ids) == 0 {
			return mcp.NewToolResultError("ids is required and must be a non-empty array of observation ids"), nil
		}
		if len(ids) > 50 {
			return mcp.NewToolResultError("too many ids (max 50 per call)"), nil
		}
		// Auth/config failure fails the CALL (isError), like the other three
		// tools — the per-id ERROR lines below are for partial per-id
		// failures only, never for a token problem that dooms every fetch.
		if _, err := c.bearer(); err != nil {
			return mcp.NewToolResultError("memoire auth: " + err.Error()), nil
		}
		// Ids come from search/timeline surfaces that include proposed and
		// noise-flagged observations, so the by-id fetch opts into the same
		// visibility (rejected/superseded/soft-deleted stay hidden).
		params := url.Values{}
		params.Set("include_proposed", "true")
		params.Set("include_noise", "true")
		var blocks []string
		for _, id := range ids {
			var resp struct {
				Observation observationRecord `json:"observation"`
			}
			if err := c.get(fmt.Sprintf("/v1/observations/%d", id), params, &resp); err != nil {
				// A rejected bearer dooms every id — fail the CALL (isError),
				// matching the local-token pre-flight above; only per-id
				// conditions stay as inline ERROR lines.
				if isAuthRejection(err) {
					return mcp.NewToolResultError("memoire auth: " + err.Error()), nil
				}
				blocks = append(blocks, fmt.Sprintf("[%d] ERROR: %v", id, err))
				continue
			}
			o := resp.Observation
			day := time.Unix(o.OccurredAtEpoch, 0).UTC().Format("2006-01-02 15:04")
			header := fmt.Sprintf("[%d] %s (%s, %s UTC, %s", o.ID, o.Kind, o.Project, day, o.ReviewState)
			if o.SessionID != "" {
				header += ", session " + o.SessionID
			}
			header += ")"
			blocks = append(blocks, header+"\n"+o.Text)
		}
		return mcp.NewToolResultText(strings.Join(blocks, "\n\n---\n\n")), nil
	}
}

func handleContext(c *apiClient) toolHandler {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		q, err := request.RequireString("q")
		if err != nil {
			return mcp.NewToolResultError("q is required"), nil
		}
		body := map[string]any{
			"q":                q,
			"exclude_kind":     "raw",
			"include_proposed": true,
			"limit":            request.GetInt("limit", 10),
		}
		if p := request.GetString("project", ""); p != "" {
			body["project"] = p
		}
		var resp struct {
			Context        string   `json:"context"`
			IdentityAlerts []string `json:"identity_alerts,omitempty"`
		}
		if err := c.post("/v1/context", body, &resp); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if strings.TrimSpace(resp.Context) == "" {
			return mcp.NewToolResultText(withIdentityAlerts(resp.IdentityAlerts, "no observations matched")), nil
		}
		return mcp.NewToolResultText(withIdentityAlerts(resp.IdentityAlerts, resp.Context)), nil
	}
}
