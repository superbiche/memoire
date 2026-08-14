// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// byokstub is the standalone smoke's BYOK endpoint (docs/BYOK-DERIVE-DESIGN.md
// §5): a local HTTP server speaking BOTH ruled dialects — OpenAI-compatible v1
// chat completions and Anthropic messages — so the smoke can prove the byok
// derive leg end-to-end with no network and no real keys. The stub enforces
// each dialect's auth header shape (bearer vs x-api-key + anthropic-version),
// grounds its canned observation on a real event id parsed from the incoming
// prompt ("### event <id>" blocks), and answers with the dialect's envelope.
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
)

// stubKey is the placeholder credential the smoke exports; never a real key.
const stubKey = "byok-stub-placeholder-key"

var eventIDRe = regexp.MustCompile(`### event (\d+)`)

// deriveResponse builds a valid strict-parser payload grounded on the first
// event id found in the prompt. The dialect is baked into the text so the
// smoke can tell the two legs' observations apart.
func deriveResponse(prompt, dialect string) (string, bool) {
	m := eventIDRe.FindStringSubmatch(prompt)
	if m == nil {
		return "", false
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return "", false
	}
	payload := map[string]any{
		"schema_version": "1",
		"observations": []map[string]any{
			{
				"kind":             "fact",
				"text":             "the byok " + dialect + " stub answered the standalone derive leg",
				"confidence":       0.9,
				"source_event_ids": []int64{id},
			},
		},
		"unknown_candidates": []any{},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", false
	}
	return string(b), true
}

func openaiHandler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+stubKey {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "bad bearer"}})
		return
	}
	var body struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) == 0 || body.MaxTokens <= 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	content, ok := deriveResponse(body.Messages[0].Content, "openai-v1")
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []map[string]any{{"message": map[string]any{"content": content}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 25, "total_tokens": 125},
	})
}

func anthropicHandler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("x-api-key") != stubKey || r.Header.Get("anthropic-version") == "" {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]any{"type": "authentication_error", "message": "bad x-api-key"}})
		return
	}
	var body struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) == 0 || body.MaxTokens <= 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	content, ok := deriveResponse(body.Messages[0].Content, "anthropic")
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"content":     []map[string]any{{"type": "text", "text": content}},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 100, "output_tokens": 25},
	})
}

func main() {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "byokstub:", err)
		os.Exit(1)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", openaiHandler)
	mux.HandleFunc("/v1/messages", anthropicHandler)
	// The chosen address is the stub's ONLY stdout line; the smoke reads it.
	fmt.Printf("LISTEN %s\n", ln.Addr().String())
	if err := http.Serve(ln, mux); err != nil {
		fmt.Fprintln(os.Stderr, "byokstub:", err)
		os.Exit(1)
	}
}
