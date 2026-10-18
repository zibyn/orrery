-- M5 ticket #94: a deleted Node stays as a tombstone (ADR-0014, schema.md).
ALTER TABLE nodes ADD COLUMN deleted_at timestamptz;
