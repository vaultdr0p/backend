-- +migrate Up

-- Where a vault_state row came from (docs/VAULT_DESIGN.md §10).
--
-- 'events'   — seeded by the contract's `initialized` event and kept current
--              by later events. bootstrap_ledger is NULL.
-- 'snapshot' — read from contract state by simulation because `initialized`
--              was older than the RPC retention window. bootstrap_ledger is the
--              ledger the reads were taken at: every event at or before it is
--              already reflected in the row and must not be applied again.
ALTER TABLE vault_state
    ADD COLUMN source TEXT NOT NULL DEFAULT 'events',
    ADD COLUMN bootstrap_ledger INTEGER,
    ADD CONSTRAINT vault_state_source_check CHECK (
        (source = 'events'   AND bootstrap_ledger IS NULL)
     OR (source = 'snapshot' AND bootstrap_ledger IS NOT NULL)
    );

-- start_ledger: the first ledger the indexer scanned for this vault. Recorded
-- from now on so a later snapshot can say exactly where the unindexed history
-- ends. NULL for rows bootstrapped before this migration: that value was never
-- stored and is not reconstructed.
--
-- snapshot_*: persisted backoff for the summary snapshot. The indexer is built
-- afresh on every job tick, so in-memory backoff would reset every 10 seconds.
-- snapshot_last_error holds a category only, never raw RPC error text, which
-- can carry a provider URL or key.
ALTER TABLE vault_indexer_state
    ADD COLUMN start_ledger INTEGER,
    ADD COLUMN snapshot_attempts INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN snapshot_next_attempt_at TIMESTAMPTZ,
    ADD COLUMN snapshot_last_error TEXT,
    ADD CONSTRAINT vault_indexer_state_snapshot_attempts_check CHECK (snapshot_attempts >= 0),
    ADD CONSTRAINT vault_indexer_state_snapshot_error_check CHECK (
        snapshot_last_error IS NULL OR snapshot_last_error IN (
            'rpc_unreachable', 'contract_not_initialized', 'inconsistent_ledgers',
            'simulation_failed', 'invalid_response'
        )
    );

-- A degraded vault may now have an unknown gap start (NULL). That is the
-- snapshot case: the contract's creation ledger cannot be read from RPC, and
-- recording a made-up start would be inventing data. The end of the gap is
-- still required.
ALTER TABLE vault_indexer_state DROP CONSTRAINT vault_indexer_state_gap_check;
ALTER TABLE vault_indexer_state ADD CONSTRAINT vault_indexer_state_gap_check CHECK (
    (status = 'healthy'  AND gap_from_ledger IS NULL AND gap_to_ledger IS NULL)
 OR (status = 'degraded' AND gap_to_ledger IS NOT NULL)
);

-- +migrate Down

-- The old constraint requires both gap bounds while degraded, so an unknown
-- start cannot survive the rollback. It becomes 0 ("from the beginning of the
-- chain"): a superset of the true gap, so the rolled-back row still reports
-- *at least* everything that is missing rather than less.
UPDATE vault_indexer_state
SET gap_from_ledger = 0
WHERE status = 'degraded' AND gap_from_ledger IS NULL;

ALTER TABLE vault_indexer_state DROP CONSTRAINT vault_indexer_state_gap_check;
ALTER TABLE vault_indexer_state ADD CONSTRAINT vault_indexer_state_gap_check CHECK (
    (status = 'healthy'  AND gap_from_ledger IS NULL AND gap_to_ledger IS NULL)
 OR (status = 'degraded' AND gap_from_ledger IS NOT NULL AND gap_to_ledger IS NOT NULL)
);

ALTER TABLE vault_indexer_state
    DROP CONSTRAINT vault_indexer_state_snapshot_error_check,
    DROP CONSTRAINT vault_indexer_state_snapshot_attempts_check,
    DROP COLUMN snapshot_last_error,
    DROP COLUMN snapshot_next_attempt_at,
    DROP COLUMN snapshot_attempts,
    DROP COLUMN start_ledger;

-- Snapshot rows stay: they hold real contract values and remain valid summary
-- rows. Only the provenance columns go.
ALTER TABLE vault_state
    DROP CONSTRAINT vault_state_source_check,
    DROP COLUMN bootstrap_ledger,
    DROP COLUMN source;
