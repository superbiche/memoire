// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package authctx

import "context"

const PrincipalLocal = "human-local"

type Context struct {
	PrincipalID  string
	CredentialID string
	RequestID    string
	Source       string
	Scopes       []string
}

func (c Context) HasScope(scope string) bool {
	for _, have := range c.Scopes {
		if have == scope || have == "admin" {
			return true
		}
	}
	return false
}

type contextKey struct{}

func With(ctx context.Context, auth Context) context.Context {
	return context.WithValue(ctx, contextKey{}, auth)
}

func From(ctx context.Context) (Context, bool) {
	auth, ok := ctx.Value(contextKey{}).(Context)
	return auth, ok
}
