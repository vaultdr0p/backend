-- +migrate Up

-- Indexing position and health, one row per vault contract.
CREATE TABLE vault_indexer_state (
    contract_id       VARCHAR(56)  PRIMARY KEY,
    last_cursor       TEXT         NOT NULL,
    last_ledger       INTEGER      NOT NULL,
    status            TEXT         NOT NULL DEFAULT 'healthy',
    gap_from_ledger   INTEGER,
    gap_to_ledger     INTEGER,
    degraded_reason   TEXT,
    last_indexed_at   TIMESTAMPTZ,
    bootstrapped_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT vault_indexer_state_status_check
        CHECK (status IN ('healthy', 'degraded')),
    -- Gap bounds are meaningful only while degraded, and must be complete.
    CONSTRAINT vault_indexer_state_gap_check CHECK (
        (status = 'healthy'  AND gap_from_ledger IS NULL AND gap_to_ledger IS NULL)
     OR (status = 'degraded' AND gap_from_ledger IS NOT NULL AND gap_to_ledger IS NOT NULL)
    )
);

-- Append-only record of every observed contract event. This is the durable
-- history: it is never pruned, so it outlives the RPC event retention window
-- (~120,960 ledgers / 7 days on soroban-testnet.stellar.org), and event_id is
-- the idempotency key that makes replay safe.
CREATE TABLE vault_events (
    id                BIGSERIAL     PRIMARY KEY,
    event_id          TEXT          NOT NULL,
    contract_id       VARCHAR(56)   NOT NULL,
    event_type        TEXT          NOT NULL,
    ledger            INTEGER       NOT NULL,
    ledger_closed_at  TIMESTAMPTZ   NOT NULL,
    tx_hash           VARCHAR(64)   NOT NULL,
    address           VARCHAR(56),
    amount            NUMERIC(40,0),
    payload           JSONB         NOT NULL,
    created_at        TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    CONSTRAINT vault_events_event_id_key UNIQUE (event_id),
    CONSTRAINT vault_events_type_check CHECK (event_type IN (
        'initialized', 'funded', 'registered', 'claimed', 'cancelled', 'withdrawn'
    ))
);

CREATE INDEX vault_events_contract_ledger_idx
    ON vault_events (contract_id, ledger DESC, id DESC);
CREATE INDEX vault_events_address_idx
    ON vault_events (contract_id, address)
    WHERE address IS NOT NULL;

-- Current allocation per recipient: a projection of vault_events, rebuildable
-- by replay at any time.
--
-- Amounts are NUMERIC(40,0), not BIGINT: contract amounts are i128 stroops and
-- i128::MAX (~1.7e38) overflows int64 (~9.2e18) by 19 orders of magnitude.
CREATE TABLE vault_allocations (
    contract_id        VARCHAR(56)   NOT NULL,
    recipient          VARCHAR(56)   NOT NULL,
    amount             NUMERIC(40,0) NOT NULL,
    status             TEXT          NOT NULL,
    registered_ledger  INTEGER       NOT NULL,
    registered_at      TIMESTAMPTZ   NOT NULL,
    settled_ledger     INTEGER,
    settled_at         TIMESTAMPTZ,
    last_event_id      TEXT          NOT NULL,
    created_at         TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    PRIMARY KEY (contract_id, recipient),
    CONSTRAINT vault_allocations_status_check
        CHECK (status IN ('PENDING', 'CLAIMED', 'CANCELLED')),
    CONSTRAINT vault_allocations_amount_check CHECK (amount > 0),
    -- PENDING has not settled; the terminal states have.
    CONSTRAINT vault_allocations_settled_check CHECK (
        (status = 'PENDING'  AND settled_ledger IS NULL AND settled_at IS NULL)
     OR (status <> 'PENDING' AND settled_ledger IS NOT NULL AND settled_at IS NOT NULL)
    )
);

CREATE INDEX vault_allocations_status_idx
    ON vault_allocations (contract_id, status);

-- Cached contract-level snapshot, so the overview does not need an RPC round
-- trip per request and stays readable when RPC is briefly unavailable. Always
-- refreshable; never authoritative.
CREATE TABLE vault_state (
    contract_id       VARCHAR(56)   PRIMARY KEY,
    admin_address     VARCHAR(56)   NOT NULL,
    token_address     VARCHAR(56)   NOT NULL,
    deadline          TIMESTAMPTZ   NOT NULL,
    total_allocated   NUMERIC(40,0) NOT NULL DEFAULT 0,
    total_claimed     NUMERIC(40,0) NOT NULL DEFAULT 0,
    balance           NUMERIC(40,0) NOT NULL DEFAULT 0,
    snapshot_ledger   INTEGER       NOT NULL,
    refreshed_at      TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    created_at        TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    CONSTRAINT vault_state_totals_check
        CHECK (total_allocated >= 0 AND total_claimed >= 0 AND balance >= 0)
);

-- +migrate Down

DROP INDEX IF EXISTS vault_allocations_status_idx;
DROP TABLE IF EXISTS vault_allocations;

DROP TABLE IF EXISTS vault_state;

DROP INDEX IF EXISTS vault_events_address_idx;
DROP INDEX IF EXISTS vault_events_contract_ledger_idx;
DROP TABLE IF EXISTS vault_events;

DROP TABLE IF EXISTS vault_indexer_state;
