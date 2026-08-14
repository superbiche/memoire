// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"memoire/services/api/internal/db"
)

// The HTTP endpoints are half of memoire's versioned API surface (D172); the
// other half is the MCP tool list, checked by the twin test in services/mcp.
// docs/API-SURFACE.json is the tracked inventory, and these tests are what
// make editing it the ratification act: they derive the surface the code
// ACTUALLY serves — the route table, the real auth-exemption predicate, the
// real requiredScope rules, the declared body postures, and the real behavior
// of the server New() builds — and fail on any divergence.
const apiSurfacePath = "../../../../docs/API-SURFACE.json"

type apiSurface struct {
	HTTP []httpSurfaceEntry `json:"http"`
}

type httpSurfaceEntry struct {
	Method        string   `json:"method"`
	Path          string   `json:"path"`
	Auth          string   `json:"auth"`
	RequiredQuery []string `json:"required_query"`
	RequiredBody  []string `json:"required_body"`
	BodyMode      string   `json:"body_mode"`
}

func (e httpSurfaceEntry) key() string { return e.Method + " " + e.Path }

func loadAPISurface(t *testing.T) apiSurface {
	t.Helper()
	raw, err := os.ReadFile(apiSurfacePath)
	if err != nil {
		t.Fatalf("read API surface inventory: %v", err)
	}
	var doc apiSurface
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", apiSurfacePath, err)
	}
	return doc
}

// concretePath substitutes path wildcards so a route pattern can be used to
// build a real request. The id is deliberately one no fixture creates: a
// request that clears validation and auth dies at the row lookup, which is
// the cheapest honest way to observe "it reached its handler".
func concretePath(pattern string) string {
	return strings.ReplaceAll(pattern, "{id}", "123")
}

func TestHTTPSurfaceMatchesInventory(t *testing.T) {
	inventory := map[string]string{}
	for _, entry := range loadAPISurface(t).HTTP {
		if _, dup := inventory[entry.key()]; dup {
			t.Fatalf("%s lists %s twice", apiSurfacePath, entry.key())
		}
		inventory[entry.key()] = entry.Auth
	}

	served := map[string]string{}
	for _, rt := range routes {
		path := concretePath(rt.path)
		class := "exempt"
		if !authExempt(path) {
			class = requiredScope(httptest.NewRequest(rt.method, path, nil))
		}
		served[rt.method+" "+rt.path] = class
	}

	for _, key := range sortedKeys(served) {
		want, listed := inventory[key]
		switch {
		case !listed:
			t.Errorf("route %s is served but missing from %s (a surface change is a contract change: add it, and rule whether the bump is BREAKING or ADDITIVE)", key, apiSurfacePath)
		case want != served[key]:
			t.Errorf("route %s serves auth class %q but %s declares %q", key, served[key], apiSurfacePath, want)
		}
	}
	for _, key := range sortedKeys(inventory) {
		if _, ok := served[key]; !ok {
			t.Errorf("%s lists %s but no route serves it", apiSurfacePath, key)
		}
	}
}

// TestHTTPSurfaceRequiredParamsAreReal proves every required_query /
// required_body claim behaviorally: with a valid token and every other
// required field present, dropping the one field must be refused with 400.
// The other side of that claim — a request carrying all of them is NOT
// refused — is one column of the contract matrix below, which covers the
// routes declaring no requirement at all too.
func TestHTTPSurfaceRequiredParamsAreReal(t *testing.T) {
	handler, _ := testServer(t, "secret-token")

	for _, entry := range loadAPISurface(t).HTTP {
		for _, omitted := range requiredParams(entry) {
			t.Run(entry.key()+" without "+omitted, func(t *testing.T) {
				target, body := probeRequest(entry, omitted)
				rec := probe(t, handler, entry.Method, target, "secret-token", body)
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("%s %s body=%s status = %d, want %d: %s claims %q is required but the handler does not enforce it (response: %s)",
						entry.Method, target, body, rec.Code, http.StatusBadRequest, apiSurfacePath, omitted, strings.TrimSpace(rec.Body.String()))
				}
			})
		}
	}
}

// The matrix credentials are PURE single-scope tokens: each proves one cell
// of the truth table and nothing more. testServer's own token carries all
// three scopes and is deliberately never used by the matrix.
const (
	tokenless  = ""
	readToken  = "read-only-token"
	writeToken = "write-only-token"
	adminToken = "admin-only-token"
	localToken = "local-all-scopes-token"
)

type transport string

const (
	bearerHeader transport = "Authorization bearer"
	queryToken   transport = "?token= query"
)

// reachesHandler is the matrix's PASS: an outcome, not a status.
const reachesHandler = 0

type credentialProbe struct {
	credential string
	transport  transport
	want       int
}

// authMatrix is the auth contract oracle: for each declared class, the exact
// answer the server owes every credential over every transport.
//
// admin is a SUPERSET, not a peer scope — authctx.Context.HasScope answers
// true for any scope once the credential carries "admin", so an admin-only
// token reaches read and write routes. The matrix pins the real semantics:
// flattening admin later trips these rows, which is the deliberate ruling
// such a change deserves.
//
// The query rows carry the half of the auth contract no derived label shows:
// the ?token= fallback is consulted ONLY when the required scope is "read"
// (auth() in server.go). On a write or admin route the query token is never
// read, so the request is tokenless and owes 401 — including when the query
// carries the very credential that satisfies the route over a bearer header.
// On a read route the fallback authenticates whatever credential the query
// carries and then applies the SAME scope rule as the header, so every
// credential is enumerated over both transports: a source-conditional rule
// that loosened one transport alone would otherwise change the effective
// auth class with no cell to catch it.
var authMatrix = map[string][]credentialProbe{
	"exempt": {
		{tokenless, bearerHeader, reachesHandler},
	},
	"read": {
		{tokenless, bearerHeader, http.StatusUnauthorized},
		{readToken, bearerHeader, reachesHandler},
		{writeToken, bearerHeader, http.StatusForbidden},
		{adminToken, bearerHeader, reachesHandler},
		{readToken, queryToken, reachesHandler},
		{writeToken, queryToken, http.StatusForbidden},
		{adminToken, queryToken, reachesHandler},
	},
	"write": {
		{tokenless, bearerHeader, http.StatusUnauthorized},
		{readToken, bearerHeader, http.StatusForbidden},
		{writeToken, bearerHeader, reachesHandler},
		{adminToken, bearerHeader, reachesHandler},
		{readToken, queryToken, http.StatusUnauthorized},
		{writeToken, queryToken, http.StatusUnauthorized},
	},
	"admin": {
		{tokenless, bearerHeader, http.StatusUnauthorized},
		{readToken, bearerHeader, http.StatusForbidden},
		{writeToken, bearerHeader, http.StatusForbidden},
		{adminToken, bearerHeader, reachesHandler},
		{readToken, queryToken, http.StatusUnauthorized},
		{adminToken, queryToken, http.StatusUnauthorized},
	},
}

// TestHTTPSurfaceContractMatrix is the one behavioral gate over the whole
// inventory: for EVERY route — including the ones declaring no required
// parameter — it builds the canonical complete request and runs the full
// credential x transport truth table against the real New()-built server.
//
// The complete request doubles as the parameter positive control: it carries
// everything the inventory declares required and nothing else, so a handler
// that starts requiring anything unlisted answers 400 and every
// reachesHandler cell of that route trips. That is what stands between "the
// inventory is the contract" and "the inventory is a comment".
func TestHTTPSurfaceContractMatrix(t *testing.T) {
	handler := scopedCredentialServer(t)
	assertFixturesMatchInventory(t)

	for _, entry := range loadAPISurface(t).HTTP {
		probes, known := authMatrix[entry.Auth]
		if !known {
			t.Errorf("%s declares auth class %q for %s, outside the closed vocabulary exempt|read|write|admin", apiSurfacePath, entry.Auth, entry.key())
			continue
		}
		target, body := probeRequest(entry, "")
		t.Run(entry.key(), func(t *testing.T) {
			for _, p := range probes {
				assertProbe(t, handler, entry, p, target, body)
			}
		})
	}
}

// scopedCredentialServer builds the real New()-assembled server together with
// the pure single-scope credentials the behavioral probes authenticate with.
func scopedCredentialServer(t *testing.T) http.Handler {
	t.Helper()
	handler, store := testServer(t, localToken)
	for _, cred := range []struct{ token, scope string }{
		{readToken, "read"},
		{writeToken, "write"},
		{adminToken, "admin"},
	} {
		if _, err := store.EnsureTokenCredential(t.Context(), cred.token, cred.scope+"-only", []string{cred.scope}); err != nil {
			t.Fatal(err)
		}
	}
	return handler
}

func assertProbe(t *testing.T, handler http.Handler, entry httpSurfaceEntry, p credentialProbe, target, body string) {
	t.Helper()
	requestTarget, header := target, p.credential
	if p.transport == queryToken {
		requestTarget, header = withQueryToken(target, p.credential), tokenless
	}
	rec := probe(t, handler, entry.Method, requestTarget, header, body)

	if p.want == reachesHandler {
		if !reachedHandler(rec.Code) {
			t.Errorf("%s %s [%s over %s] status = %d, want a status showing the request reached its handler: %s declares auth class %q, which this credential satisfies, and the request carries every parameter the inventory declares required — 400 means the handler requires something unlisted, 401/403 means the auth contract moved, 500 means the auth path itself failed (response: %s)",
				entry.Method, requestTarget, credentialName(p.credential), p.transport, rec.Code, apiSurfacePath, entry.Auth, strings.TrimSpace(rec.Body.String()))
		}
		return
	}
	if rec.Code != p.want {
		t.Errorf("%s %s [%s over %s] status = %d, want exactly %d: %s declares auth class %q (response: %s)",
			entry.Method, requestTarget, credentialName(p.credential), p.transport, rec.Code, p.want, apiSurfacePath, entry.Auth, strings.TrimSpace(rec.Body.String()))
	}
}

// reachedHandler is what PASS means: the request cleared parameter validation
// (not 400) and auth (not 401/403) without the auth path itself faulting
// (not 500 — auth() answers 500 when the credential store fails, and an
// outage must never certify as a pass). 2xx, 3xx, 404, 409 and 503 all mean
// the request arrived at its handler, which is the whole claim.
func reachedHandler(code int) bool {
	switch code {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError:
		return false
	}
	return true
}

func credentialName(credential string) string {
	if credential == tokenless {
		return "tokenless"
	}
	return credential
}

// probeFixtures carries the values a handler needs to accept a declared
// required parameter that the dummy "probe" string cannot satisfy — a real
// review state, a batch shaped like the embed worker's. Test fixture, never
// contract data: the inventory says WHICH parameters are required, this map
// only lets the complete request reach past validation.
var probeFixtures = map[string]map[string]any{
	"POST /v1/observations/{id}/review": {"to_state": "accepted"},
	"PUT /v1/embeddings":                {"embeddings": []map[string]any{probeEmbedding()}},
}

// probeEmbedding is a batch item at the real contract dimension (db.EmbedDim)
// rather than a token two-element vector. On the SQLite test store the upsert
// answers 409 ErrNoVectorLane before any dimension check, so what the matrix
// proves here is that the batch clears the handler's own field validation —
// which is all it claims: the per-item requirements are stated in the
// inventory comment as the handlers' contract, deliberately outside the gate.
func probeEmbedding() map[string]any {
	vector := make([]float32, db.EmbedDim)
	for i := range vector {
		vector[i] = 0.1
	}
	return map[string]any{
		"observation_id": 1,
		"model":          db.DefaultEmbedModel,
		"vector":         vector,
		"text_hash":      "sha256:probe",
	}
}

// probeBodies carries the canonical body for routes needing one although they
// declare no required field: POST /v1/events/batch refuses an unparseable
// body. The body carries NO field name the inventory does not declare —
// assertFixturesMatchInventory enforces that mechanically — because a
// pre-supplied name is exactly what hides a name becoming required: the
// omission loop has no row for it and every positive cell hands it over.
// With the empty object, `events` turning required trips the matrix.
//
// The empty object parses, so a parse-before-queue change on POST /v1/events
// does not trip THIS matrix — a named-field requirement on either route does.
// That posture is no longer unclaimed: the body_mode marker states it and
// TestHTTPSurfaceBodyModesAreReal probes it with bytes no parser accepts.
var probeBodies = map[string]string{
	"POST /v1/events":       `{}`,
	"POST /v1/events/batch": `{}`,
}

// assertFixturesMatchInventory keeps both fixture maps honest: a stale route
// or field would silently stop applying and quietly weaken the matrix.
func assertFixturesMatchInventory(t *testing.T) {
	t.Helper()
	declared := map[string]map[string]bool{}
	for _, entry := range loadAPISurface(t).HTTP {
		fields := map[string]bool{}
		for _, name := range requiredParams(entry) {
			fields[name] = true
		}
		declared[entry.key()] = fields
	}
	for key, fixture := range probeFixtures {
		fields, listed := declared[key]
		if !listed {
			t.Errorf("probe fixture for %s matches no entry in %s", key, apiSurfacePath)
			continue
		}
		for name := range fixture {
			if !fields[name] {
				t.Errorf("probe fixture %s.%s is not a required parameter in %s", key, name, apiSurfacePath)
			}
		}
	}
	for key, body := range probeBodies {
		fields, listed := declared[key]
		if !listed {
			t.Errorf("probe body for %s matches no entry in %s", key, apiSurfacePath)
			continue
		}
		if len(fields) > 0 {
			t.Errorf("probe body for %s would shadow the required fields %s declares: build it from probeFixtures instead", key, apiSurfacePath)
			continue
		}
		var supplied map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &supplied); err != nil {
			t.Errorf("probe body for %s must be a JSON object, got %s: %v", key, body, err)
			continue
		}
		for name := range supplied {
			if !fields[name] {
				t.Errorf("probe body for %s carries %q, which %s does not declare required: an undeclared name in the canonical request hides that name becoming required", key, name, apiSurfacePath)
			}
		}
	}
}

// The closed body_mode vocabulary. A route carries the marker only when its
// body handling DEVIATES from the ordinary named-fields posture; the inventory
// paragraph states what each mode promises.
const (
	bodyModeOpaqueQueue  = "opaque_queue"
	bodyModeJSONEnvelope = "json_envelope"
)

// malformedBody is truncated JSON. No parser accepts it, so only a route that
// never parses the body at all can answer it with anything but 400 — which is
// what makes it the probe that tells the two postures apart.
const malformedBody = `{"broken":`

// satisfyingCredential is the pure single-scope token each auth class admits.
// The body_mode probes are about bodies, not auth: they authenticate with the
// credential the matrix already proved sufficient, so a failure here can only
// be a body-posture failure.
var satisfyingCredential = map[string]string{
	"exempt": tokenless,
	"read":   readToken,
	"write":  writeToken,
	"admin":  adminToken,
}

// TestHTTPSurfaceBodyModesAreReal proves the declared body postures against the
// real server, which is what turns body_mode from a note into a contract.
//
// opaque_queue is the capture-never-blocked custody promise: POST /v1/events
// queues the raw bytes unread and answers 202 to ANYTHING, so a malformed event
// is preserved and dies at drain instead of being lost at the door. The probe
// demands EXACTLY 202 — not merely "reached its handler" — because a
// parse-before-queue change would still reach the handler while quietly moving
// the promise, and that change must be ruled, not slipped in.
//
// json_envelope is the opposite claim, equally load-bearing: POST
// /v1/events/batch DOES parse, so unparseable bytes are refused with 400 while
// the empty object — no member required — is still accepted.
func TestHTTPSurfaceBodyModesAreReal(t *testing.T) {
	handler := scopedCredentialServer(t)

	for _, entry := range loadAPISurface(t).HTTP {
		if entry.BodyMode == "" {
			continue
		}
		if len(entry.RequiredBody) > 0 {
			t.Errorf("%s declares body_mode %q for %s together with required_body %v: the two contradict each other — both modes accept a body carrying no named field, and a required name is the ordinary posture the marker exists to say this route does NOT have",
				apiSurfacePath, entry.BodyMode, entry.key(), entry.RequiredBody)
			continue
		}
		credential, known := satisfyingCredential[entry.Auth]
		if !known {
			continue // TestHTTPSurfaceContractMatrix owns the auth vocabulary.
		}
		target, _ := probeRequest(entry, "")

		t.Run(entry.key()+" "+entry.BodyMode, func(t *testing.T) {
			switch entry.BodyMode {
			case bodyModeOpaqueQueue:
				assertBodyModeStatus(t, handler, entry, credential, target, malformedBody, http.StatusAccepted,
					"opaque_queue promises the raw bytes are queued unread and ANY body is accepted, so malformed input dies at drain with the bytes preserved; a route that now parses before queueing has changed the capture custody contract")
			case bodyModeJSONEnvelope:
				assertBodyModeStatus(t, handler, entry, credential, target, malformedBody, http.StatusBadRequest,
					"json_envelope promises the body must parse as JSON, so unparseable bytes are refused at the door; accepting them would make this route opaque_queue")
				assertBodyModeStatus(t, handler, entry, credential, target, `{}`, http.StatusAccepted,
					"json_envelope requires no member, so the empty object is accepted; refusing it would make some field required")
			default:
				t.Errorf("%s declares body_mode %q for %s, outside the closed vocabulary %s|%s",
					apiSurfacePath, entry.BodyMode, entry.key(), bodyModeOpaqueQueue, bodyModeJSONEnvelope)
			}
		})
	}
}

func assertBodyModeStatus(t *testing.T, handler http.Handler, entry httpSurfaceEntry, credential, target, body string, want int, promise string) {
	t.Helper()
	rec := probe(t, handler, entry.Method, target, credential, body)
	if rec.Code != want {
		t.Errorf("%s %s [%s, body=%s] status = %d, want exactly %d: %s declares body_mode %q and %s (response: %s)",
			entry.Method, target, credentialName(credential), body, rec.Code, want, apiSurfacePath, entry.BodyMode, promise, strings.TrimSpace(rec.Body.String()))
	}
}

func requiredParams(entry httpSurfaceEntry) []string {
	params := make([]string, 0, len(entry.RequiredQuery)+len(entry.RequiredBody))
	params = append(params, entry.RequiredQuery...)
	return append(params, entry.RequiredBody...)
}

// probeRequest builds the canonical request for an entry: every declared
// required parameter except the omitted one (pass "" to omit nothing).
func probeRequest(entry httpSurfaceEntry, omitted string) (target, body string) {
	values := url.Values{}
	for _, name := range entry.RequiredQuery {
		if name != omitted {
			values.Set(name, fmt.Sprint(probeValue(entry.key(), name)))
		}
	}
	target = concretePath(entry.Path)
	if encoded := values.Encode(); encoded != "" {
		target += "?" + encoded
	}
	if len(entry.RequiredBody) == 0 {
		return target, probeBodies[entry.key()]
	}
	fields := map[string]any{}
	for _, name := range entry.RequiredBody {
		if name != omitted {
			fields[name] = probeValue(entry.key(), name)
		}
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		panic(fmt.Sprintf("probe body for %s: %v", entry.key(), err))
	}
	return target, string(raw)
}

func probeValue(routeKey, name string) any {
	if fixture, ok := probeFixtures[routeKey]; ok {
		if value, ok := fixture[name]; ok {
			return value
		}
	}
	return "probe"
}

func probe(t *testing.T, handler http.Handler, method, target, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	if token != tokenless {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	handler.ServeHTTP(rec, req)
	return rec
}

func withQueryToken(target, token string) string {
	if strings.Contains(target, "?") {
		return target + "&token=" + token
	}
	return target + "?token=" + token
}

// TestHTTPRoutesRegisterOnlyThroughTheTable binds this PACKAGE's registrations
// to the table: a registration written anywhere else in it — beside the loop,
// inside a helper taking the mux under any receiver name, or via a method
// value like `register := mux.HandleFunc` — would be served while staying out
// of both `routes` and the inventory, and no diff of derived values could show
// it. The walk counts every OCCURRENCE of a Handle/HandleFunc selector, not
// only calls, which is what catches the method-value form; parsing rather than
// text-searching is what makes it blind to receiver names and comments.
//
// What it does NOT prove, deliberately: it is an occurrence tripwire over this
// package's own sources, not a semantic proof that the mux New builds serves
// exactly the table. Registration delegated to another package, and any
// wrapper placed around the handler New returns where the server is assembled
// (services/api/cmd/api), are outside this gate — those are review's job.
func TestHTTPRoutesRegisterOnlyThroughTheTable(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	var sites []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name == "Handle" || sel.Sel.Name == "HandleFunc" {
				sites = append(sites, fset.Position(sel.Pos()).String())
			}
			return true
		})
	}
	if len(sites) != 1 {
		t.Fatalf("found %d Handle/HandleFunc occurrences %v in the server package, want exactly 1 (the loop over `routes` in New): every route this package registers must ride the table, or neither the contract test against %s nor the inventory itself can see it",
			len(sites), sites, apiSurfacePath)
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
