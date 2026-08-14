# Schema

`services/api` owns schema migration. Workers must treat the schema as already applied and fail clearly if required columns are missing.

## Additive-Only Rule

Allowed:

- `CREATE TABLE`
- `ALTER TABLE ... ADD COLUMN` with nullable/defaulted columns
- `CREATE INDEX`
- `CREATE VIRTUAL TABLE`
- `CREATE TRIGGER`

Allowed by narrow carve-out (operator-ruled 2026-08-05, BYOK slice —
strict-widening only):

- widening a `CHECK` constraint's value list to a STRICT SUPERSET of the
  previous list. Every existing row must pass the new constraint by
  construction; narrowing or reshaping a constraint stays forbidden.
  Realization per engine: PostgreSQL replaces the constraint in place
  (`DROP CONSTRAINT` + `ADD CONSTRAINT`, superset makes validation
  instant); SQLite uses the documented rebuild pattern (new table with
  the widened constraint, column-complete copy, drop, rename, full index
  recreation) inside one transaction — precedent `0016_byok_provider.sql`.

Forbidden:

- dropping tables or columns
- renaming tables or columns
- changing column types, nullability, or defaults in place
- narrowing or reshaping constraints (only strict value-list widening is
  allowed, per the carve-out above)
- data-deleting repair migrations

