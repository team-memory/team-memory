-- +goose Up
ALTER TABLE memory_revisions ADD COLUMN attribution jsonb NOT NULL DEFAULT '{}'::jsonb
 CHECK (jsonb_typeof(attribution) = 'object' AND octet_length(attribution::text) <= 16384);
-- Existing revisions deliberately remain unattributed.
