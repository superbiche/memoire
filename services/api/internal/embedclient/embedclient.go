// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

// Package embedclient is the API-side consumer of the embed worker's
// cluster-local query-embedding service (services/embed-worker, D218):
// POST /embed {"text"} -> {"model","dim","vector"}. It implements
// db.QueryEmbedder for the hybrid search lane. Deadlines come from the
// caller's context (the store's embed budget); the client adds none of
// its own so the budget stays defined in one place.
package embedclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type Client struct {
	base  string
	token string
	model string
	http  *http.Client
}

// New points at the embed service base URL (e.g. http://memoire-embed:37779).
// token is caller-supplied and topology-owned: in the k8s split it is the
// DEDICATED embed credential (F4 — the serve pod validates a Secret that
// grants nothing on this API), in local-first single-token setups the
// deployment's own custody token. This client never resolves credentials
// itself. model is the identity the stored vectors carry
// (db.DefaultEmbedModel at wiring):
// a response from a different model is an ERROR, not a vector — API and
// embed Deployments roll independently, and a wrong-model 384-dim vector
// would otherwise produce silent cross-model cosine rankings instead of
// the promised fail-open (review round-1 F2).
func New(baseURL, token, model string) *Client {
	return &Client{
		base:  strings.TrimRight(baseURL, "/"),
		token: token,
		model: model,
		http:  &http.Client{},
	}
}

func (c *Client) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	payload, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/embed", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(res.Body, 500))
		return nil, fmt.Errorf("embed service: HTTP %d %s", res.StatusCode, strings.TrimSpace(string(detail)))
	}
	var body struct {
		Model  string    `json:"model"`
		Dim    int       `json:"dim"`
		Vector []float32 `json:"vector"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("embed service: decode: %w", err)
	}
	if body.Model != c.model {
		return nil, fmt.Errorf("embed service: model %q, want %q — API and embed images are out of step", body.Model, c.model)
	}
	if len(body.Vector) == 0 || len(body.Vector) != body.Dim {
		return nil, fmt.Errorf("embed service: vector/dim mismatch (%d values, dim %d)", len(body.Vector), body.Dim)
	}
	return body.Vector, nil
}
