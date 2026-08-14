// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Michel Tomas

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Vector lane (PORT-DESIGN, D218). PG-only, mirroring the pg_search
// posture (D160): capability probe at Open, provisioning as a deployment
// step, graceful absence everywhere else. The SQLite engine never grows a
// vector lane; the 0002 sketch table stays inert.

// DefaultEmbedModel is the single active embedding model identifier. The
// embed worker and the API must agree on it — a row is CURRENT iff its
// model matches this AND its text_hash matches the observation text.
const DefaultEmbedModel = "all-MiniLM-L6-v2-q8"

// EmbedDim is the dimensionality DefaultEmbedModel produces; the
// provisioned column type is vector(EmbedDim).
const EmbedDim = 384

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// pgVectorSchemaUsable is the single validator for the vector lane,
// shared by the Open-time probe and the provisioning postcondition (the
// pg_search precedent's round-2 N1 lesson: IF NOT EXISTS matches by name
// only, so provisioning must re-validate shape). Requires the pgvector
// extension, the observation_embeddings_pg table WITH the ratified
// column shape (round-1 F3: a same-name malformed table used to certify
// as usable — upserts then failed while health advertised the lane),
// the observations FK with ON DELETE CASCADE (purge semantics ride it),
// and a valid+ready HNSW cosine index on the embedding column. Any
// error means "not usable".
func pgVectorSchemaUsable(ctx context.Context, conn rowQuerier) bool {
	var n int
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_extension WHERE extname = 'vector'`).Scan(&n); err != nil || n == 0 {
		return false
	}
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_tables WHERE schemaname = current_schema() AND tablename = 'observation_embeddings_pg'`).Scan(&n); err != nil || n == 0 {
		return false
	}
	// All six ratified columns, exact types, NOT NULL. The embedding
	// column's declared dimension is checked via format_type (atttypmod).
	if err := conn.QueryRowContext(ctx, `SELECT count(*)
		FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'observation_embeddings_pg'
		  AND is_nullable = 'NO'
		  AND ((column_name = 'observation_id' AND data_type = 'bigint')
		    OR (column_name = 'model' AND data_type = 'text')
		    OR (column_name = 'dim' AND data_type = 'integer')
		    OR (column_name = 'embedding' AND udt_name = 'vector')
		    OR (column_name = 'text_hash' AND data_type = 'text')
		    OR (column_name = 'embedded_at_epoch' AND data_type = 'bigint'))`).Scan(&n); err != nil || n != 6 {
		return false
	}
	var embType string
	if err := conn.QueryRowContext(ctx, `SELECT format_type(atttypid, atttypmod)
		FROM pg_attribute
		WHERE attrelid = 'observation_embeddings_pg'::regclass
		  AND attname = 'embedding'`).Scan(&embType); err != nil || embType != fmt.Sprintf("vector(%d)", EmbedDim) {
		return false
	}
	// Primary key on observation_id + cascading FK FROM observation_id to
	// observations(id). Both checked by exact constraint definition
	// (round-2 F3 delta: confrelid+confdeltype alone certified a decoy FK
	// on some other column, leaving observation_id cascade-less).
	if err := conn.QueryRowContext(ctx, `SELECT count(*)
		FROM pg_constraint
		WHERE conrelid = 'observation_embeddings_pg'::regclass
		  AND ((contype = 'p' AND pg_get_constraintdef(oid) = 'PRIMARY KEY (observation_id)')
		    OR (contype = 'f' AND pg_get_constraintdef(oid) = 'FOREIGN KEY (observation_id) REFERENCES observations(id) ON DELETE CASCADE'))`).Scan(&n); err != nil || n != 2 {
		return false
	}
	var ok bool
	err := conn.QueryRowContext(ctx, `SELECT
			i.indisvalid AND i.indisready
			AND am.amname = 'hnsw'
			AND pg_get_indexdef(i.indexrelid) LIKE '%USING hnsw (embedding vector_cosine_ops)%'
		FROM pg_index i
		JOIN pg_class ic ON ic.oid = i.indexrelid
		JOIN pg_am am ON am.oid = ic.relam
		WHERE i.indrelid = 'observation_embeddings_pg'::regclass
		  AND ic.relname = 'idx_obs_emb_hnsw'`).Scan(&ok)
	if err != nil {
		return false
	}
	return ok
}

// detectPGVector is the Open-time capability probe: read-only, never
// provisions, never fails Open. Surfaced on /v1/health as vector_lane so
// an accidental fallback is visible instead of silent.
func detectPGVector(conn dbc) bool {
	return pgVectorSchemaUsable(context.Background(), conn)
}

// HasPGVector reports whether the vector lane is usable on this store.
func (s *Store) HasPGVector() bool { return s.hasPGVector }

// VectorLane names the lane for health reporting.
func (s *Store) VectorLane() string {
	if s.hasPGVector {
		return "pgvector"
	}
	return "absent"
}

// ProvisionVectorSchema creates the vector-lane table and HNSW index when
// the pgvector extension is installed (clean no-op otherwise). Deployment
// step — called from pgshadow Apply, deliberately never from Open. The
// table starts empty, so the HNSW index is maintained incrementally from
// the first insert; no blocking build ever happens on this path.
func ProvisionVectorSchema(ctx context.Context, conn *sql.DB) error {
	var n int
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_extension WHERE extname = 'vector'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS observation_embeddings_pg (
		observation_id    BIGINT PRIMARY KEY REFERENCES observations(id) ON DELETE CASCADE,
		model             TEXT   NOT NULL,
		dim               INT    NOT NULL,
		embedding         vector(%d) NOT NULL,
		text_hash         TEXT   NOT NULL,
		embedded_at_epoch BIGINT NOT NULL
	)`, EmbedDim)); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_obs_emb_hnsw
		ON observation_embeddings_pg USING hnsw (embedding vector_cosine_ops)`); err != nil {
		return err
	}
	if !pgVectorSchemaUsable(ctx, conn) {
		return errors.New("vector lane provisioning postcondition failed: observation_embeddings_pg/idx_obs_emb_hnsw exist but do not validate as the expected pgvector table + hnsw cosine index")
	}
	return nil
}

// ErrNoVectorLane is returned by vector-lane methods on stores without a
// usable lane (SQLite engine, or PG without provisioned pgvector).
var ErrNoVectorLane = errors.New("vector lane not available on this store")

// MissingEmbedding is one observation the embed worker must (re)embed.
type MissingEmbedding struct {
	ObservationID int64  `json:"observation_id"`
	Text          string `json:"text"`
}

// missingWhere is the single definition of "derived, visible, lacking a
// CURRENT embedding" shared by the work-list and the freshness stats, so
// the worker and the 2h look-line can never disagree about what missing
// means. A row is current iff model matches AND text_hash matches
// sha256(text) — model upgrades and rewrites both invalidate naturally.
const missingWhere = `FROM observations o
	LEFT JOIN observation_embeddings_pg e
	  ON e.observation_id = o.id
	 AND e.model = $1
	 AND e.text_hash = encode(sha256(convert_to(o.text, 'UTF8')), 'hex')
	WHERE o.deleted_at_epoch IS NULL
	  AND o.kind <> 'raw'
	  AND e.observation_id IS NULL`

// ListObservationsMissingEmbedding returns the oldest-first work list for
// the embed worker.
func (s *Store) ListObservationsMissingEmbedding(ctx context.Context, model string, limit int) ([]MissingEmbedding, error) {
	if !s.hasPGVector {
		return nil, ErrNoVectorLane
	}
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	rows, err := s.rdb.QueryContext(ctx,
		`SELECT o.id, o.text `+missingWhere+` ORDER BY o.id LIMIT $2`, model, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MissingEmbedding
	for rows.Next() {
		var m MissingEmbedding
		if err := rows.Scan(&m.ObservationID, &m.Text); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// EmbeddingFreshness feeds the health surface and the RUNBOOK 2h
// look-line: how many rows are missing a current embedding, and the
// creation epoch of the oldest one (0 when nothing is missing).
type EmbeddingFreshness struct {
	Model              string `json:"model"`
	Missing            int64  `json:"missing"`
	OldestMissingEpoch int64  `json:"oldest_missing_epoch"`
}

func (s *Store) EmbeddingFreshnessStats(ctx context.Context, model string) (EmbeddingFreshness, error) {
	f := EmbeddingFreshness{Model: model}
	if !s.hasPGVector {
		return f, ErrNoVectorLane
	}
	err := s.rdb.QueryRowContext(ctx,
		`SELECT count(*), COALESCE(min(o.created_at_epoch), 0) `+missingWhere, model).
		Scan(&f.Missing, &f.OldestMissingEpoch)
	return f, err
}

// EmbeddingUpsert is one worker-produced embedding. TextHash is the
// sha256 hex of the exact text the worker embedded — stored verbatim so a
// rewrite racing the worker leaves a visibly stale row (re-picked next
// run) instead of a silently wrong one.
type EmbeddingUpsert struct {
	ObservationID int64     `json:"observation_id"`
	Model         string    `json:"model"`
	Vector        []float32 `json:"vector"`
	TextHash      string    `json:"text_hash"`
	EpochNow      int64     `json:"-"`
}

// UpsertObservationEmbeddings writes a worker batch in one transaction.
// One embedding per observation (PK): a different model or hash simply
// replaces the row — single-active-model posture per PORT-DESIGN.
func (s *Store) UpsertObservationEmbeddings(ctx context.Context, batch []EmbeddingUpsert) error {
	if !s.hasPGVector {
		return ErrNoVectorLane
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, u := range batch {
		if len(u.Vector) != EmbedDim {
			return fmt.Errorf("observation %d: vector has %d dims, want %d", u.ObservationID, len(u.Vector), EmbedDim)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO observation_embeddings_pg
			(observation_id, model, dim, embedding, text_hash, embedded_at_epoch)
			VALUES ($1, $2, $3, $4::vector, $5, $6)
			ON CONFLICT (observation_id) DO UPDATE SET
				model = EXCLUDED.model, dim = EXCLUDED.dim,
				embedding = EXCLUDED.embedding, text_hash = EXCLUDED.text_hash,
				embedded_at_epoch = EXCLUDED.embedded_at_epoch`,
			u.ObservationID, u.Model, len(u.Vector), vectorLiteral(u.Vector), u.TextHash, u.EpochNow); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// vectorLiteral renders a pgvector input literal: "[0.1,0.2,...]".
func vectorLiteral(v []float32) string {
	var b strings.Builder
	b.Grow(len(v)*10 + 2)
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
