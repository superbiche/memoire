// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"
)

// memoire:D0249 — a Postgres store backing memoire is C-collated (bytewise
// order, SQLite BINARY parity). The decision half is pure and runs without a
// server: "C" and its byte-identical alias "POSIX" pass, everything else is
// out of contract. "C.UTF-8" is the trap — a UTF-8 locale, NOT C ordering.
func TestPGCollationOK(t *testing.T) {
	cases := []struct {
		datcollate string
		want       bool
	}{
		{"C", true},
		{"POSIX", true},
		{"C.UTF-8", false},
		{"C.utf8", false},
		{"en_US.utf8", false},
		{"en_US.UTF-8", false},
		{"und-x-icu", false},
		{"c", false}, // datcollate is exact; a lowercase locale name is not "C"
		{"", false},
	}
	for _, c := range cases {
		if got := pgCollationOK(c.datcollate); got != c.want {
			t.Errorf("pgCollationOK(%q) = %v, want %v", c.datcollate, got, c.want)
		}
	}
}

// The provider half (review residual 3): libc passes on datcollate alone,
// builtin passes only pinned to 'C', ICU and anything unknown refuse.
func TestPGLocaleProviderOK(t *testing.T) {
	cases := []struct {
		provider, datlocale string
		want                bool
	}{
		{"c", "", true},
		{"b", "C", true},
		{"b", "C.UTF-8", false}, // code-point order ≠ ruled contract; fail closed
		{"b", "", false},
		{"i", "", false},
		{"i", "C", false}, // the blind spot itself: ICU behind a C-looking collate
		{"", "", false},
	}
	for _, c := range cases {
		if got := pgLocaleProviderOK(c.provider, c.datlocale); got != c.want {
			t.Errorf("pgLocaleProviderOK(%q, %q) = %v, want %v", c.provider, c.datlocale, got, c.want)
		}
	}
}

// TestPGCollationRefusesNonC is the live half: Open must REFUSE a
// non-C-collated database, naming the found and the required collation.
// The accept path needs no dedicated test — every PG leg in this package
// opens the C-collated conformance database and would fail here first.
func TestPGCollationRefusesNonC(t *testing.T) {
	admin := os.Getenv("MEMOIRE_TEST_PG_URL")
	if admin == "" {
		t.Skip("MEMOIRE_TEST_PG_URL not set; postgres collation refusal leg skipped")
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("MEMOIRE_TEST_PG_URL: %v", err)
	}
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	dbName := "memoire_collate_" + hex.EncodeToString(suffix[:])

	adminConn, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	defer adminConn.Close()
	// template0 + an explicit non-C locale: creating with the server default
	// would prove nothing on a C-collated server (the refusal would never
	// fire and the test would fail for the wrong reason). A server without
	// this locale skips rather than asserts.
	if _, err := adminConn.Exec("CREATE DATABASE " + dbName +
		" TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE 'en_US.utf8' LC_CTYPE 'en_US.utf8'"); err != nil {
		t.Skipf("cannot create an en_US.utf8 database on this server (locale absent?): %v", err)
	}
	t.Cleanup(func() {
		_, _ = adminConn.Exec("DROP DATABASE IF EXISTS " + dbName + " WITH (FORCE)")
	})

	u.Path = "/" + dbName
	store, err := Open(u.String(), schemaDirForTest(t))
	if err == nil {
		_ = store.Close()
		t.Fatal("Open accepted an en_US.utf8 database; D0249 requires a refusal")
	}
	msg := err.Error()
	for _, want := range []string{"en_US.utf8", `"C"`, "D0249"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal does not name %s: %v", want, msg)
		}
	}
	if strings.Contains(msg, u.String()) {
		t.Error("refusal leaked the DSN")
	}

	// The no-DDL invariant (review F1): the refusal must fire BEFORE migrate,
	// so the rejected database stays untouched — reordering the preflight
	// after migrate would pass every assertion above and still violate D0249.
	fixtureConn, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer fixtureConn.Close()
	var reg sql.NullString
	if err := fixtureConn.QueryRow(
		`SELECT to_regclass('public.schema_version')::text`).Scan(&reg); err != nil {
		t.Fatalf("no-DDL probe: %v", err)
	}
	if reg.Valid {
		t.Errorf("refused database carries migration object %q; the preflight must run before any DDL", reg.String)
	}
}

// TestPGCollationRefusesICUProvider is the blind-spot fixture (review
// residual 3): an ICU-provider database created with LC_COLLATE 'C'
// reports datcollate='C' — the collate leg alone would accept it while ICU
// governs ordering. The provider leg must refuse it.
func TestPGCollationRefusesICUProvider(t *testing.T) {
	admin := os.Getenv("MEMOIRE_TEST_PG_URL")
	if admin == "" {
		t.Skip("MEMOIRE_TEST_PG_URL not set; postgres ICU-provider refusal leg skipped")
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("MEMOIRE_TEST_PG_URL: %v", err)
	}
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	dbName := "memoire_icu_" + hex.EncodeToString(suffix[:])

	adminConn, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	defer adminConn.Close()
	// A server without ICU support (or pre-PG15) skips rather than asserts.
	if _, err := adminConn.Exec("CREATE DATABASE " + dbName +
		" TEMPLATE template0 ENCODING 'UTF8' LOCALE_PROVIDER icu ICU_LOCALE 'en-US'" +
		" LC_COLLATE 'C' LC_CTYPE 'C'"); err != nil {
		t.Skipf("cannot create an ICU-provider database on this server: %v", err)
	}
	t.Cleanup(func() {
		_, _ = adminConn.Exec("DROP DATABASE IF EXISTS " + dbName + " WITH (FORCE)")
	})

	u.Path = "/" + dbName
	store, err := Open(u.String(), schemaDirForTest(t))
	if err == nil {
		_ = store.Close()
		t.Fatal("Open accepted an ICU-provider database; D0249 requires a refusal")
	}
	msg := err.Error()
	for _, want := range []string{`"i"`, "D0249"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal does not name %s: %v", want, msg)
		}
	}
	if strings.Contains(msg, u.String()) {
		t.Error("refusal leaked the DSN")
	}

	// The observer warn-path (D0249, operator-ruled 2026-08-23): the same
	// out-of-contract store must still OPEN read-only — observation is
	// never blocked, the violation is a stderr warning only. Stderr is
	// captured so the warning itself is regression-pinned (review F1: a
	// deleted Fprintf must not leave this test green).
	origStderr := os.Stderr
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = pw
	obs, openErr := OpenReadOnly(u.String())
	_ = pw.Close()
	os.Stderr = origStderr
	captured, _ := io.ReadAll(pr)
	_ = pr.Close()
	if openErr != nil {
		t.Fatalf("OpenReadOnly refused an out-of-contract store; the observer path must warn, not refuse: %v", openErr)
	}
	_ = obs.Close()
	warn := string(captured)
	for _, want := range []string{"WARNING: observer open of out-of-contract store", `"i"`, "D0249"} {
		if !strings.Contains(warn, want) {
			t.Errorf("observer warning does not carry %s: %q", want, warn)
		}
	}
	if strings.Contains(warn, u.String()) {
		t.Error("observer warning leaked the DSN")
	}
}
