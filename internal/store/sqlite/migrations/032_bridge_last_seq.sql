-- Monotonic ingest sequence counter per bridge. The plugin sends each
-- ingest event with a client-chosen `seq` (monotonic within its bridge);
-- the server rejects regressions or duplicate-seq-with-different-event.
-- `last_seq` tracks the high-water mark so racing ingests are serialized
-- by a conditional UPDATE rather than a row lock.
ALTER TABLE bridge_tokens ADD COLUMN last_seq INTEGER NOT NULL DEFAULT 0;
