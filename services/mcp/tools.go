// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package main

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// memoryRule rides on every retrieval tool description; memoryFetchBodies
// rides on the tools that return compact hits (not on
// memoire_get_observations, which is the fetch). L0010 R1 = A rewrote
// memoryRule (Hindsight owns what was decided or learned; Memoire is a
// dated raw capture log) and retired memorySearchShapes, which steered
// searches at derived memory Hindsight now owns. The tool declaration
// remains the carrier agents always read (docs/rework/retrieval-consumer/
// README.md, unit 4 arms tooldesc/tooldesc2: the rule alone moved citations
// from 0/3 to 2/3), so the citation discipline is carried into the new text.
const memoryRule = " Memoire is a dated capture log: its hits are raw records of what happened in past sessions — prompts, tool calls, tool output, ids — and a hit is dated evidence, not current truth. For what was decided or learned, use Hindsight; a Memoire hit corroborates and dates. Cite a hit's observation id in brackets, e.g. [123456], whenever you rely on it; verify it against the files when one carries it, and when none is found, say the hit is the only carrier found."

const memoryFetchBodies = " Fetch the full body of every hit you rely on with memoire_get_observations."

// L0010 R1 = A: the tools are a dated recent raw lookup. They serve raw
// capture projections only — the API serves them by default, so no
// include_derived is sent — never ask for derived observations (no
// kind/exclude_kind/include_proposed parameters), and they bound the read
// to the last `days` days by occurred time (default 30; 0 lifts the bound).
func registerTools(s *server.MCPServer, c *apiClient) {
	s.AddTool(
		mcp.NewTool("memoire_search",
			mcp.WithDescription("Full-text search over recent raw Memoire captures — prompts, tool calls, tool output, ids. Returns compact hits — enough to judge relevance in one trip; fetch full bodies with memoire_get_observations. One line per hit: `[id] kind~ YYYY-MM-DD project provenance/review_state[!]: snippet`. A raw capture renders as `raw` with `observed/accepted`; the date is when the event occurred."+memoryRule+memoryFetchBodies),
			mcp.WithString("q", mcp.Required(), mcp.Description("FTS5 query (words, phrases, OR, NEAR)")),
			mcp.WithString("project", mcp.Description("Limit to one project (e.g. 'liaison')")),
			mcp.WithNumber("days", mcp.Description("Look back this many days by occurred time; default 30, 0 lifts the bound")),
			mcp.WithNumber("limit", mcp.Description("Max hits, default 20")),
		),
		handleSearch(c),
	)

	s.AddTool(
		mcp.NewTool("memoire_timeline",
			mcp.WithDescription("Most recent raw Memoire captures, newest first (no query needed), bounded to the last `days` days by occurred time (default 30; 0 lifts the bound). Same compact hit format as memoire_search: `[id] kind~ YYYY-MM-DD project provenance/review_state[!]: snippet`."+memoryRule+memoryFetchBodies),
			mcp.WithString("project", mcp.Description("Limit to one project")),
			mcp.WithNumber("days", mcp.Description("Look back this many days by occurred time; default 30, 0 lifts the bound")),
			mcp.WithNumber("limit", mcp.Description("Max hits, default 15")),
		),
		handleTimeline(c),
	)

	s.AddTool(
		mcp.NewTool("memoire_get_observations",
			mcp.WithDescription("Fetch full observation bodies by id (ids come from memoire_search / memoire_timeline hits)."+memoryRule),
			mcp.WithArray("ids", mcp.Required(), mcp.WithNumberItems(), mcp.Description("Observation ids, e.g. [82446, 82452]")),
		),
		handleGetObservations(c),
	)

	s.AddTool(
		mcp.NewTool("memoire_context",
			mcp.WithDescription("Prompt-ready context block for a topic: searches recent raw Memoire captures and returns a pre-formatted text block suitable for direct inclusion in a prompt. Bounded to the last `days` days by occurred time (default 30; 0 lifts the bound)."+memoryRule+memoryFetchBodies),
			mcp.WithString("q", mcp.Required(), mcp.Description("Topic query (FTS5)")),
			mcp.WithString("project", mcp.Description("Limit to one project")),
			mcp.WithNumber("days", mcp.Description("Look back this many days by occurred time; default 30, 0 lifts the bound")),
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
	intParam(params, "limit", request.GetInt("limit", defaultLimit))
	if after := occurredAfter(request); after > 0 {
		params.Set("occurred_after", strconv.FormatInt(after, 10))
	}
	return params
}

// maxLookbackDays caps `days` so the bound never overflows a Duration.
const maxLookbackDays = 36500

// occurredAfter turns the `days` argument into the occurred_after epoch the
// API takes: default 30, 0 lifts the bound (returns 0), and a negative value
// is treated as the default rather than silently lifting the bound.
func occurredAfter(request mcp.CallToolRequest) int64 {
	days := request.GetInt("days", 30)
	if days == 0 {
		return 0
	}
	if days < 0 {
		days = 30
	}
	if days > maxLookbackDays {
		days = maxLookbackDays
	}
	return time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
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
				// Siblings sit beside the record, not inside it: the API
				// attaches them per response, from the same run lookup the
				// search surfaces use.
				Siblings []observationSibling `json:"siblings,omitempty"`
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
			if line := siblingContinuationLine(resp.Siblings); line != "" {
				header += "\n" + line
			}
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
			"q":     q,
			"limit": request.GetInt("limit", 10),
		}
		if after := occurredAfter(request); after > 0 {
			body["occurred_after"] = after
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
