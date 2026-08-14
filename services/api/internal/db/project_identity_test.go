// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// Identity-conflict detection at the ingest chokepoint (liaison:D239 item 4,
// adapter-passed roots): roots are learned from payload identity fields,
// non-curated new roots for a known key raise alerts, registry-sourced roots
// join silently and clear their alert.
func TestProjectIdentityConflictLifecycle(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "memoire.db"), "../../../../schema")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	ingest := func(n int, project, root, source string) {
		t.Helper()
		payload := `{}`
		if root != "" {
			payload = fmt.Sprintf(`{"memoire_project_root":%q,"memoire_project_source":%q}`, root, source)
		}
		_, err := store.Ingest(ctx, EventInput{
			Project:       project,
			SessionID:     "ident-session",
			SourceAdapter: "test",
			EventType:     "note",
			Text:          fmt.Sprintf("event %d", n),
			SourceEventID: fmt.Sprintf("ident-%d", n),
			Payload:       []byte(payload),
		})
		if err != nil {
			t.Fatalf("ingest %d: %v", n, err)
		}
	}
	alerts := func() []ProjectIdentityAlert {
		t.Helper()
		out, err := store.ListProjectIdentityAlerts(ctx, "proj")
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	// 1. First root is learned silently, whatever the source.
	ingest(1, "proj", "/home/u/dev/proj", "memoire-file")
	if got := alerts(); len(got) != 0 {
		t.Fatalf("first root must not alert, got %+v", got)
	}

	// 2. Same root again: still quiet.
	ingest(2, "proj", "/home/u/dev/proj", "memoire-file")
	if got := alerts(); len(got) != 0 {
		t.Fatalf("known root must not alert, got %+v", got)
	}

	// 3. A NEW root via basename: alert, counting events.
	ingest(3, "proj", "/tmp/scratch/proj", "basename")
	ingest(4, "proj", "/tmp/scratch/proj", "basename")
	got := alerts()
	if len(got) != 1 {
		t.Fatalf("expected 1 alert, got %+v", got)
	}
	if got[0].KnownRoot != "/home/u/dev/proj" || got[0].Root != "/tmp/scratch/proj" || got[0].EventCount != 2 {
		t.Fatalf("alert fields wrong: %+v", got[0])
	}
	if !strings.Contains(got[0].Text(), "proj") || !strings.Contains(got[0].Text(), "/tmp/scratch/proj") {
		t.Fatalf("alert text must name key and root: %s", got[0].Text())
	}

	// 4. The same root arriving via the registry (operator-curated multi-home):
	// joins silently and clears the alert.
	ingest(5, "proj", "/tmp/scratch/proj", "registry")
	if got := alerts(); len(got) != 0 {
		t.Fatalf("registry-sourced root must clear the alert, got %+v", got)
	}
	// ...and stays quiet afterwards even via basename (now a known root).
	ingest(6, "proj", "/tmp/scratch/proj", "basename")
	if got := alerts(); len(got) != 0 {
		t.Fatalf("root known after registry join must not re-alert, got %+v", got)
	}

	// 5. Events without identity payload never touch tracking.
	ingest(7, "other", "", "")
	other, err := store.ListProjectIdentityAlerts(ctx, "other")
	if err != nil || len(other) != 0 {
		t.Fatalf("no-identity events must not track: %v %+v", err, other)
	}

	// 6. A .memoire-sourced duplicate key from a different root still alerts
	// (a duplicated .memoire file is a real conflict).
	ingest(8, "proj", "/home/u/elsewhere/copy", "memoire-file")
	if got := alerts(); len(got) != 1 || got[0].Source != "memoire-file" {
		t.Fatalf("duplicated .memoire key must alert, got %+v", got)
	}
}
