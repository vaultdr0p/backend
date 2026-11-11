# Disbursement Vault Integration — Design

Integrates the Soroban `disbursement-vault` contract into SDP so disbursements
can be funded and claimed through a vault, without changing any existing SDP
behaviour.

- **Contract:** `vaultdr0p/contract`, testnet `CB5HTYC4GZW67ET23SACDCWCCDQ3AKU4TYX7SAUQJSKS6LLQLYA4HG53`
- **Feature flag:** `ENABLE_VAULT`, **default `false`**
- **Status:** approved; implementation in progress on `feat/vault-integration`.
  Landed so far: the per-tenant migration and `RPCClient.GetEvents` with the
  retention-window constants. Section 2.2's event filter was corrected during
  implementation (see the note there).

---

## 1. Shape of the integration

The contract requires the **vault admin's Stellar signature** for `fund`,
`register`, `cancel` and `withdraw`. SDP must never hold that key, so the
backend is deliberately **not** a transaction signer for vault operations.

```
                        ┌──────────────────────────────┐
   Soroban RPC          │  SDP backend (this repo)     │
   getEvents  ─────────►│  indexer job  → Postgres     │
                        │  read API     ← projections  │
   simulate   ◄─────────│  XDR builder  → unsigned tx  │
                        └──────────────┬───────────────┘
                                       │ JSON (+ unsigned XDR)
                                       ▼
                        ┌──────────────────────────────┐
                        │  Dashboard (frontend)        │
                        │  Freighter signs + submits   │
                        └──────────────────────────────┘
```

Three responsibilities, and nothing else:

1. **Index** the contract's events into per-tenant Postgres tables, so the
   dashboard gets a list endpoint, history, and sane pagination.
2. **Serve** that state over the existing Dashboard API, under the existing
   auth and tenancy rules.
3. **Build** unsigned, pre-simulated transaction envelopes on request. The
   browser signs with Freighter and submits. The backend sees only XDR.

This keeps the key-handling guarantee structural rather than procedural: there
is no code path in which SDP could sign a vault operation, because it never
receives a vault signing key.

### Why index at all

The contract stores one persistent entry per recipient and exposes
`get_allocation(address)` — but **no way to enumerate recipients**. That is a
deliberate contract design choice (see the contract's `docs/DESIGN.md`): it is
what keeps per-call cost O(1) and avoids a ledger-entry size ceiling.

Enumeration is therefore an off-chain indexing problem, and the event log is
the *only* source of the recipient set. This has a hard consequence that drives
section 2: **our database must become the durable record, because Soroban RPC
retains only a recent window of events.**

---

## 2. Event indexing

### 2.1 Source

`internal/stellar.RPCClient` currently exposes only `SimulateTransaction` and
`GetLatestLedgerSequence`. It gains one method; the underlying
`clients/rpcclient.Client` already implements `GetEvents`, so this is a thin
wrapper in the existing style:

```go
type RPCClient interface {
    SimulateTransaction(ctx context.Context, request protocol.SimulateTransactionRequest) (*SimulationResult, *SimulationError)
    GetLatestLedgerSequence(ctx context.Context) (uint32, error)

    // new
    GetEvents(ctx context.Context, request protocol.GetEventsRequest) (*protocol.GetEventsResponse, error)
}
```

Request, per poll:

```go
protocol.GetEventsRequest{
    Filters: []protocol.EventFilter{{
        EventType:   protocol.EventTypeSet{"contract": nil},
        ContractIDs: []string{contractID},
    }},
    Pagination: &protocol.PaginationOptions{
        Cursor: &cursor,  // resume point; mutually exclusive with StartLedger
        Limit:  200,
    },
    Format: "json",       // populates TopicJSON/ValueJSON, so no manual XDR decode
}
```

`xdrFormat: "json"` matters: it makes RPC return `topicJson` / `valueJson`,
avoiding hand-rolled `xdr.ScVal` decoding in SDP for every event shape.

### 2.2 Events consumed

The contract emits six typed events. Each has its indexed address as a topic.

| Event | Topic | Data | Effect on projection |
|---|---|---|---|
| `initialized` | `admin` | `token`, `deadline` | seed `vault_state` |
| `funded` | `from` | `amount`, `balance` | update `vault_state.balance` |
| `registered` | `recipient` | `amount`, `total_allocated` | upsert allocation as `PENDING` |
| `claimed` | `recipient` | `amount` | allocation → `CLAIMED` |
| `cancelled` | `recipient` | `amount` | allocation → `CANCELLED` |
| `withdrawn` | `to` | `amount` | update `vault_state.balance` |

Filtering to `EventType: contract` is what keeps failed invocations out. An
earlier draft of this document said to check
`EventInfo.InSuccessfulContractCall`; that field is **deprecated in the SDK
with no replacement** (`// Deprecated: remove in v24`), because contract events
are only emitted by successful invocations — a failed call surfaces as a
`diagnostic` event instead, and those are never requested. Reading the
deprecated field also fails `golangci-lint` under staticcheck SA1019.

### 2.3 Cursor storage

Soroban events are totally ordered by `(ledger, tx, op, event)`, which is
exactly `protocol.Cursor`. RPC returns an opaque `cursor` string on every
response; that string is the resume token.

The cursor lives in `vault_indexer_state`, one row per `(contract_id)` inside
the **tenant's own schema**, so tenants cannot observe or corrupt each other's
indexing position.

The cursor is advanced **in the same transaction that applies the batch**:

```sql
BEGIN;
  -- for each event in the batch:
  INSERT INTO vault_events (...) VALUES (...) ON CONFLICT (event_id) DO NOTHING;
  -- projections applied only for rows that actually inserted
  UPDATE vault_indexer_state
     SET last_cursor = $1, last_ledger = $2, last_indexed_at = now()
   WHERE contract_id = $3;
COMMIT;
```

A crash anywhere inside the transaction rolls back both the projection and the
cursor, so the batch is simply re-fetched. A crash after commit has nothing to
redo. There is no window in which the cursor has advanced past unapplied
events.

### 2.4 Reorg handling — and why there is none

**Stellar does not reorg.** SCP reaches consensus before a ledger closes;
a closed ledger is final, and there is no competing-chain rule that could
orphan it. This is a real difference from EVM chains, where an indexer must
keep N confirmations of rollback buffer and be able to unwind applied state.

So this design has **no rollback path, and deliberately no confirmation-depth
delay** — adding either would cost latency and complexity to defend against a
failure mode the network does not have. Concretely: no `removed`/`reverted`
event handling, no block-hash chain validation, no "unwind to ledger N".

What *does* threaten correctness is different, and is handled explicitly below:
duplicate delivery, and the RPC retention window.

### 2.5 Replay and idempotency

Polling gives **at-least-once** delivery: a timeout after RPC has served a
batch, a retried job, or an operator-triggered resync will all re-present
events already applied.

Idempotency is keyed on the RPC event id (`EventInfo.ID`, the stringified
cursor — unique and stable for the life of the network):

- `vault_events.event_id` carries a `UNIQUE` constraint.
- Inserts use `ON CONFLICT (event_id) DO NOTHING`.
- **Projections are applied only for rows that actually inserted**, detected
  with `INSERT ... RETURNING`. A re-presented event therefore cannot
  double-count a balance or resurrect a cancelled allocation.

Because of this, a full replay from an arbitrarily old cursor is always safe
and converges to the same state. That makes resync an operational tool rather
than a risk.

Note also that allocation transitions are monotonic in the contract
(`Pending → Claimed | Cancelled`, never back), so projection is a
last-writer-wins upsert ordered by cursor, with no need to reason about
out-of-order arrival within a batch — RPC returns events in cursor order.

### 2.6 The retention gap — the one real failure mode

`GetEventsResponse` reports `OldestLedger`. Soroban RPC retains only a recent
window (commonly ~7 days); older events are gone from RPC permanently.

Before applying a batch the indexer checks:

```
if state.last_ledger + 1 < resp.OldestLedger  →  gap detected
```

A gap means events were emitted that RPC can no longer serve. Since the event
log is the only way to discover recipients (§1), **a gap can permanently lose
the identity of recipients registered during it.** On-chain funds are never at
risk — the contract remains authoritative and any known recipient can still
claim — but SDP's *list* may be incomplete.

Rather than silently serving a partial list, the indexer:

1. marks the vault `degraded` in `vault_indexer_state` (`status`, `gap_from_ledger`, `gap_to_ledger`),
2. keeps serving known allocations, each still refreshable from the contract,
3. surfaces the condition on `GET /vault/{contract_id}/indexer` and as a
   banner-able field on the overview, and
4. re-seeds `vault_state` totals from the contract by simulation, which *are*
   recoverable, and resumes from `OldestLedger`.

Recovery from a gap requires operator input (a recipient list to re-attest, or
re-registration). This is documented rather than hidden because no amount of
code can recreate data the network no longer has.

Two things keep gaps rare in practice: the indexer interval is 10s against a
window measured in days, and `vault_events` is append-only and never pruned, so
SDP's own history outlives RPC's.

### 2.7 Bootstrap

On first run for a contract, with no cursor:

- If the vault's first ledger is still inside the retention window, index from
  `OldestLedger` and replay the whole history — this reconstructs every
  allocation exactly.
- Otherwise record `status = 'degraded'` with the gap bounds, seed `vault_state`
  from the contract by simulation, and start from `OldestLedger`.

`VAULT_INDEX_FROM_LEDGER` may be set to pin an explicit start ledger when an
operator knows the deployment ledger.

> The "seed `vault_state` from the contract by simulation" step above was not
> implemented in the first version. §10 specifies it.

### 2.8 Job wiring

A scheduler job, following the existing `internal/scheduler/jobs` convention:

```go
const vaultEventIndexerJobName = "vault_event_indexer_job"

func (j vaultEventIndexerJob) GetName() string           { return vaultEventIndexerJobName }
func (j vaultEventIndexerJob) GetInterval() time.Duration { return j.interval } // default 10s
func (j vaultEventIndexerJob) IsJobMultiTenant() bool     { return true }
func (j vaultEventIndexerJob) Execute(ctx context.Context) error { ... }
```

`IsJobMultiTenant() == true` means the scheduler runs it per tenant with the
tenant in context, exactly as `payment_from_submitter_job` does, so the
per-tenant schema routing is inherited rather than reinvented. The job is
registered **only when `ENABLE_VAULT` is true** (§6), and it is skipped for any
tenant with no vault configured.

Interval is validated against the existing `DefaultMinimumJobIntervalSeconds`
floor.

---

## 3. Postgres schema

Vault state is **per-tenant**, so the migration goes in
`db/migrations/sdp-migrations/` — the router that applies to each tenant's
schema — not `admin-migrations` or `tss-migrations`. Tenant isolation is
therefore structural: a query in one tenant's schema cannot see another's rows.

File: `db/migrations/sdp-migrations/2026-11-02.0-add-vault-tables.sql`

### Amounts

Contract amounts are `i128` stroops. **`BIGINT` cannot hold them** —
`i128::MAX` is ~1.7×10³⁸ against int64's ~9.2×10¹⁸. All amounts are
`NUMERIC(40, 0)`, which is exact and has the headroom. They are serialised in
JSON as **decimal strings**, since IEEE-754 doubles cannot represent them
either.

### Migration — up

```sql
-- +migrate Up

-- Indexing position and health, one row per vault contract.
CREATE TABLE vault_indexer_state (
    contract_id       VARCHAR(56)  PRIMARY KEY,
    last_cursor       TEXT         NOT NULL,
    last_ledger       INTEGER      NOT NULL,
    status            TEXT         NOT NULL DEFAULT 'healthy',
    gap_from_ledger   INTEGER,
    gap_to_ledger     INTEGER,
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
-- history: it is never pruned, so it outlives the RPC retention window, and
-- event_id is the idempotency key that makes replay safe.
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
CREATE TABLE vault_allocations (
    contract_id        VARCHAR(56)  NOT NULL,
    recipient          VARCHAR(56)  NOT NULL,
    amount             NUMERIC(40,0) NOT NULL,
    status             TEXT         NOT NULL,
    registered_ledger  INTEGER      NOT NULL,
    registered_at      TIMESTAMPTZ  NOT NULL,
    settled_ledger     INTEGER,
    settled_at         TIMESTAMPTZ,
    last_event_id      TEXT         NOT NULL,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
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
```

### Migration — down

Dropped in reverse dependency order. No existing SDP table is altered by the
up migration, so the down migration cannot affect pre-existing data.

```sql
-- +migrate Down

DROP INDEX IF EXISTS vault_allocations_status_idx;
DROP TABLE IF EXISTS vault_allocations;

DROP TABLE IF EXISTS vault_state;

DROP INDEX IF EXISTS vault_events_address_idx;
DROP INDEX IF EXISTS vault_events_contract_ledger_idx;
DROP TABLE IF EXISTS vault_events;

DROP TABLE IF EXISTS vault_indexer_state;
```

`DROP TABLE` removes its own indexes and constraints; the explicit
`DROP INDEX IF EXISTS` lines are harmless and keep the down migration
readable as an exact inverse.

### Deliberately absent

- **No foreign key from `vault_allocations` to `receivers`.** A vault recipient
  is a Stellar address, which may not correspond to any SDP receiver. Coupling
  them would make the vault unusable for non-SDP recipients and would create a
  delete-ordering constraint on an existing table.
- **No `disbursement_id` link yet.** Associating a vault with a disbursement is
  a product decision; the schema keys on `contract_id` so the link can be added
  later as a nullable column without a rewrite.
- **Multiple vaults per tenant work already**, since every table keys on
  `contract_id`. Only the default comes from configuration.

---

## 4. HTTP API

### OpenAPI 3.1

```yaml
openapi: 3.1.0
info:
  title: SDP Disbursement Vault API
  version: 0.1.0
  description: >
    Read access to indexed vault state, plus construction of unsigned
    transaction envelopes. All routes require ENABLE_VAULT=true; when the flag
    is off they are not registered and return 404.

    The server never signs vault transactions and never accepts a secret key.
    Write operations return an unsigned, pre-simulated XDR envelope that the
    client signs with a browser wallet and submits itself.

servers:
  - url: https://{tenant}.{domain}
    description: Tenant is resolved from the request subdomain.
    variables:
      tenant: { default: bluecorp }
      domain: { default: stellar.local:8000 }

components:
  securitySchemes:
    bearerAuth:
      type: http
      scheme: bearer
      bearerFormat: JWT
      description: Existing SDP dashboard session token.

  schemas:
    Amount:
      type: string
      pattern: '^[0-9]+$'
      description: >
        i128 stroops as a decimal string. Not a JSON number: these values
        exceed both int64 and IEEE-754 double precision.
      examples: ["100000000"]

    StellarAddress:
      type: string
      pattern: '^[GC][A-Z2-7]{55}$'
      description: Stellar account (G...) or contract (C...) address.

    AllocationStatus:
      type: string
      enum: [PENDING, CLAIMED, CANCELLED]

    Allocation:
      type: object
      required: [recipient, amount, status, registered_at, registered_ledger]
      properties:
        recipient: { $ref: '#/components/schemas/StellarAddress' }
        amount: { $ref: '#/components/schemas/Amount' }
        status: { $ref: '#/components/schemas/AllocationStatus' }
        registered_at: { type: string, format: date-time }
        registered_ledger: { type: integer }
        settled_at: { type: [string, 'null'], format: date-time }
        settled_ledger: { type: [integer, 'null'] }

    VaultOverview:
      type: object
      required:
        [contract_id, admin, token, deadline, is_expired, total_allocated,
         total_claimed, balance, available, indexer]
      properties:
        contract_id: { $ref: '#/components/schemas/StellarAddress' }
        admin: { $ref: '#/components/schemas/StellarAddress' }
        token: { $ref: '#/components/schemas/StellarAddress' }
        deadline: { type: string, format: date-time }
        is_expired:
          type: boolean
          description: >
            True once the deadline has passed. Claiming is then closed and
            cancelling is open — the contract makes exactly one of the two
            valid at any instant.
        total_allocated: { $ref: '#/components/schemas/Amount' }
        total_claimed: { $ref: '#/components/schemas/Amount' }
        balance: { $ref: '#/components/schemas/Amount' }
        available:
          allOf: [{ $ref: '#/components/schemas/Amount' }]
          description: balance - total_allocated; the most that can be withdrawn.
        snapshot_ledger: { type: integer }
        refreshed_at: { type: string, format: date-time }
        source:
          type: string
          enum: [events, snapshot]
          description: >
            How this summary was created (§10). "events": seeded by the
            contract's initialized event. "snapshot": read from contract state
            because initialized was older than the RPC retention window; the
            indexer is then also marked degraded, because recipients registered
            before indexing began are not listed.
        bootstrap_ledger:
          type: [integer, 'null']
          description: The ledger a snapshot was read at. Null for source "events".
        indexer: { $ref: '#/components/schemas/IndexerStatus' }

    IndexerStatus:
      type: object
      required: [status, last_ledger, latest_ledger, lag_ledgers]
      properties:
        status: { type: string, enum: [healthy, degraded] }
        last_ledger: { type: integer }
        latest_ledger: { type: integer }
        lag_ledgers:
          type: integer
          description: latest_ledger - last_ledger.
        last_indexed_at: { type: [string, 'null'], format: date-time }
        gap_from_ledger:
          type: [integer, 'null']
          description: >
            Null while healthy. Null while degraded means the gap's start is
            unknown: the summary was read from contract state and the
            contract's creation ledger cannot be read from RPC (§10.5).
        gap_to_ledger: { type: [integer, 'null'] }
        degraded_reason:
          type: [string, 'null']
          description: >
            Set when status is degraded. A gap means events fell outside the
            RPC retention window, so the recipient list may be incomplete.
            On-chain funds are unaffected.

    IndexerHealth:
      type: object
      description: >
        Live indexer health: the stored cursor compared against the chain tip.
        See §9. Unlike IndexerStatus, this calls RPC (through a short-lived
        cache), and an unreachable RPC is reported as status "stalled", never
        as a 5xx.
      required:
        - contract_id
        - status
        - reason
        - detail
        - degraded
        - cursor_ledger
        - rpc_latest_ledger
        - lag_ledgers
        - lag_seconds_estimate
        - last_event_at
        - retention_headroom
        - checked_at
        - summary
      properties:
        contract_id: { type: string }
        status: { type: string, enum: [ok, lagging, stalled] }
        reason:
          $ref: '#/components/schemas/IndexerHealthReason'
        detail:
          type: string
          description: >
            Free-text, human-readable explanation of status and reason, with
            the numbers that produced it. For people, not for parsing: switch
            on `reason`. Never contains the RPC error text, which can carry
            the RPC URL and any API key in it.
        degraded:
          type: boolean
          description: >
            A retention gap has been recorded (see IndexerStatus). Independent
            of status: a caught-up indexer can still be degraded.
        cursor_ledger:
          type: [integer, 'null']
          description: Last ledger scanned. Null if the vault was never indexed.
        rpc_latest_ledger:
          type: [integer, 'null']
          description: Chain tip from RPC. Null when RPC is unreachable.
        lag_ledgers:
          type: [integer, 'null']
          description: rpc_latest_ledger - cursor_ledger, floored at 0.
        lag_seconds_estimate:
          type: [integer, 'null']
          description: lag_ledgers x 5s average ledger close time. An estimate.
        last_event_at:
          type: [string, 'null']
          format: date-time
          description: Close time of the newest indexed event. Null if none.
        retention_headroom:
          type: [integer, 'null']
          description: >
            Ledgers until the cursor falls outside the RPC event retention
            window (120,960 - lag_ledgers, floored at 0). At 0, unread events
            are aging out.
        checked_at:
          type: string
          format: date-time
          description: >
            When this status was computed (RFC 3339, UTC). The chain tip it
            was compared against comes from a cache and may be up to
            VAULT_STATUS_RPC_CACHE_SECONDS older than this.
        summary: { $ref: '#/components/schemas/VaultSummaryHealth' }

    VaultSummaryHealth:
      type: object
      description: >
        The summary behind GET /vault/{contract_id} (§10.6). Reported
        separately from status and reason, whose enums are unchanged.
      required: [state, bootstrap_ledger, bootstrap]
      properties:
        state: { type: string, enum: [missing, events, snapshot] }
        bootstrap_ledger:
          type: [integer, 'null']
          description: The ledger a snapshot was read at; null otherwise.
        bootstrap:
          type: object
          required: [attempts, last_error, next_attempt_at]
          properties:
            attempts:
              type: integer
              description: Consecutive failed snapshot attempts. 0 after a success.
            last_error:
              type: [string, 'null']
              enum: [rpc_unreachable, contract_not_initialized, inconsistent_ledgers,
                     simulation_failed, invalid_response, null]
              description: A category, never raw RPC error text.
            next_attempt_at: { type: [string, 'null'], format: date-time }

    IndexerHealthReason:
      type: string
      description: >
        Closed enum: why `status` has its value. Exactly one per status:
        rpc_unreachable, never_indexed and no_recent_commit go with stalled;
        lag_over_threshold goes with lagging; none goes with ok.
      enum:
        - rpc_unreachable     # chain-tip lookup failed or timed out
        - never_indexed       # no indexer row, or no batch ever committed
        - no_recent_commit    # last commit older than VAULT_STATUS_STALL_SECONDS
        - lag_over_threshold  # lag_ledgers >= VAULT_STATUS_LAGGING_LEDGERS
        - none                # status is ok

    VaultEvent:
      type: object
      required: [event_id, event_type, ledger, ledger_closed_at, tx_hash]
      properties:
        event_id: { type: string }
        event_type:
          type: string
          enum: [initialized, funded, registered, claimed, cancelled, withdrawn]
        ledger: { type: integer }
        ledger_closed_at: { type: string, format: date-time }
        tx_hash: { type: string }
        address: { oneOf: [{ $ref: '#/components/schemas/StellarAddress' }, { type: 'null' }] }
        amount: { oneOf: [{ $ref: '#/components/schemas/Amount' }, { type: 'null' }] }

    BuildTransactionRequest:
      type: object
      required: [operation]
      properties:
        operation:
          type: string
          enum: [fund, register, cancel, withdraw]
        source:
          allOf: [{ $ref: '#/components/schemas/StellarAddress' }]
          description: >
            Account that will sign. Defaults to the vault admin from
            vault_state. Must be a public address.
        recipient:
          allOf: [{ $ref: '#/components/schemas/StellarAddress' }]
          description: Required for register and cancel.
        destination:
          allOf: [{ $ref: '#/components/schemas/StellarAddress' }]
          description: Required for withdraw.
        amount:
          allOf: [{ $ref: '#/components/schemas/Amount' }]
          description: Required for fund, register and withdraw.

    BuildTransactionResponse:
      type: object
      required: [xdr, network_passphrase, operation, summary]
      properties:
        xdr:
          type: string
          description: Unsigned transaction envelope, already simulated.
        network_passphrase: { type: string }
        operation: { type: string }
        min_resource_fee: { type: string }
        summary:
          type: object
          description: Field/value pairs for a pre-signing confirmation screen.
          additionalProperties: { type: string }

    Problem:
      type: object
      description: Matches SDP's existing error envelope.
      required: [error]
      properties:
        error: { type: string }
        error_code: { type: string }
        extras: { type: object, additionalProperties: true }

    VaultError:
      description: >
        A vault 404 with a stable, machine-readable code next to the unchanged
        human-readable message (§10.7). vault_not_found: the contract is not
        this tenant's vault (every vault route). vault_not_indexed: the vault
        has no summary or indexer row yet (overview, resync, transactions).
        Not used when ENABLE_VAULT is off: the routes are then unregistered
        and the router answers a plain-text "404 page not found".
      allOf:
        - { $ref: '#/components/schemas/Problem' }
        - type: object
          required: [error_code]
          properties:
            error_code: { type: string, enum: [vault_not_found, vault_not_indexed] }

  parameters:
    ContractId:
      name: contract_id
      in: path
      required: true
      schema: { $ref: '#/components/schemas/StellarAddress' }
    Page:
      name: page
      in: query
      schema: { type: integer, minimum: 1, default: 1 }
    PageLimit:
      name: page_limit
      in: query
      schema: { type: integer, enum: [20, 50, 100], default: 20 }

security:
  - bearerAuth: []

paths:
  /vault/{contract_id}:
    get:
      summary: Vault overview
      description: >
        Served from the cached snapshot. Pass refresh=true to re-read the
        contract by simulation before responding.
      parameters:
        - $ref: '#/components/parameters/ContractId'
        - name: refresh
          in: query
          schema: { type: boolean, default: false }
      responses:
        '200':
          description: OK
          content:
            application/json:
              schema: { $ref: '#/components/schemas/VaultOverview' }
        '401': { description: missing or invalid token }
        '403': { description: role lacks read permission }
        '404':
          description: >
            Not this tenant's vault (error_code vault_not_found), or no summary
            yet (vault_not_indexed; with snapshot bootstrap this lasts only
            until the first full indexer run, see summary on /status). With the
            flag off, a plain-text router 404 instead.
          content:
            application/json:
              schema: { $ref: '#/components/schemas/VaultError' }

  /vault/{contract_id}/allocations:
    get:
      summary: List allocations with claim status
      parameters:
        - $ref: '#/components/parameters/ContractId'
        - $ref: '#/components/parameters/Page'
        - $ref: '#/components/parameters/PageLimit'
        - name: status
          in: query
          schema: { $ref: '#/components/schemas/AllocationStatus' }
        - name: recipient
          in: query
          schema: { $ref: '#/components/schemas/StellarAddress' }
      responses:
        '200':
          description: OK
          content:
            application/json:
              schema:
                type: object
                required: [data, pagination]
                properties:
                  data:
                    type: array
                    items: { $ref: '#/components/schemas/Allocation' }
                  pagination:
                    type: object
                    properties:
                      total: { type: integer }
                      page: { type: integer }
                      pages: { type: integer }
                  indexer: { $ref: '#/components/schemas/IndexerStatus' }
        '401': { description: unauthenticated }
        '403': { description: forbidden }

  /vault/{contract_id}/allocations/{recipient}:
    get:
      summary: Single allocation, read live from the contract
      description: >
        Bypasses the projection and simulates get_allocation, for confirming a
        recipient's status authoritatively.
      parameters:
        - $ref: '#/components/parameters/ContractId'
        - name: recipient
          in: path
          required: true
          schema: { $ref: '#/components/schemas/StellarAddress' }
      responses:
        '200':
          description: OK
          content:
            application/json:
              schema: { $ref: '#/components/schemas/Allocation' }
        '404': { description: no allocation for this address }

  /vault/{contract_id}/events:
    get:
      summary: Indexed event history
      parameters:
        - $ref: '#/components/parameters/ContractId'
        - $ref: '#/components/parameters/Page'
        - $ref: '#/components/parameters/PageLimit'
        - name: event_type
          in: query
          schema: { type: string }
        - name: address
          in: query
          schema: { $ref: '#/components/schemas/StellarAddress' }
      responses:
        '200':
          description: OK
          content:
            application/json:
              schema:
                type: object
                properties:
                  data:
                    type: array
                    items: { $ref: '#/components/schemas/VaultEvent' }
                  pagination: { type: object }

  /vault/{contract_id}/indexer:
    get:
      summary: Indexer health
      parameters: [{ $ref: '#/components/parameters/ContractId' }]
      responses:
        '200':
          description: OK
          content:
            application/json:
              schema: { $ref: '#/components/schemas/IndexerStatus' }

  /vault/{contract_id}/status:
    get:
      summary: Live indexer health against the chain tip
      description: >
        Compares the stored cursor with RPC's latest ledger (cached for a few
        seconds). Always 200 for a configured vault: RPC being unreachable is
        a health result (status stalled, reason rpc_unreachable), not a
        server error.
      parameters: [{ $ref: '#/components/parameters/ContractId' }]
      responses:
        '200':
          description: OK
          content:
            application/json:
              schema: { $ref: '#/components/schemas/IndexerHealth' }
        '404':
          description: >
            Not this tenant's vault (error_code vault_not_found). With
            ENABLE_VAULT off, a plain-text router 404 instead.
          content:
            application/json:
              schema: { $ref: '#/components/schemas/VaultError' }

  /vault/{contract_id}/transactions:
    post:
      summary: Build an unsigned, simulated transaction envelope
      description: >
        Builds and simulates the requested operation and returns unsigned XDR.
        The server does not sign and does not submit. A simulation failure is
        returned as the contract's typed error, so an operation the contract
        would reject is reported before the user is asked to sign.
      parameters: [{ $ref: '#/components/parameters/ContractId' }]
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: '#/components/schemas/BuildTransactionRequest' }
      responses:
        '200':
          description: Envelope built
          content:
            application/json:
              schema: { $ref: '#/components/schemas/BuildTransactionResponse' }
        '400':
          description: >
            Validation failure, or the contract rejected the simulation
            (e.g. InsufficientFunds, DeadlinePassed, AllocationExists).
          content:
            application/json:
              schema: { $ref: '#/components/schemas/Problem' }
        '403': { description: role lacks write permission }

  /vault/{contract_id}/resync:
    post:
      summary: Force the indexer to re-scan
      description: >
        Rewinds the cursor and replays. Safe at any time: event application is
        idempotent on event_id, so a replay converges to the same state.
      parameters: [{ $ref: '#/components/parameters/ContractId' }]
      requestBody:
        required: false
        content:
          application/json:
            schema:
              type: object
              properties:
                from_ledger:
                  type: integer
                  description: >
                    Defaults to the RPC retention floor (oldestLedger).
      responses:
        '202': { description: resync scheduled }
        '403': { description: owner role required }
```

### Notes for the frontend

- Amounts are **strings**, everywhere. Parse with `BigInt`, not `Number`.
- `POST /transactions` returns XDR to sign with Freighter; the backend neither
  signs nor submits. Submission and polling stay client-side.
- Every list response carries `indexer`, so the UI can show a staleness or
  degraded banner without a second request.
- When `ENABLE_VAULT` is off, these paths are **not registered** and return
  404 — not 403 — so the dashboard can feature-detect with one call.

---

## 5. Fit with SDP's multi-tenant and auth model

### Tenancy

Nothing new is introduced.

- Tenant is resolved from the request **subdomain** by the existing middleware.
- All four tables live in the **tenant's schema** via `sdp-migrations`, so
  isolation is enforced by the existing `db/router`, not by application-level
  `WHERE tenant_id = ?` filters that could be forgotten.
- The indexer job sets `IsJobMultiTenant() == true`, so the scheduler iterates
  tenants and puts each in context; the job reads its tenant with the same
  `tenant.GetTenantFromContext(ctx)` helper the existing jobs use.
- Each tenant has its own vault contract and its own cursor. One tenant's
  degraded indexer cannot affect another's.

### Auth and roles

Routes are registered on the **Dashboard API** (port 8000) in
`internal/serve/serve.go`, wrapped in the existing
`middleware.RequirePermission(...)`, mirroring how `/disbursements` and
`/payments` are gated:

| Route | Roles |
|---|---|
| `GET /vault/*` (overview, allocations, events, indexer) | `owner`, `financial_controller`, `business`, `initiator`, `approver` |
| `POST /vault/{id}/transactions` | `owner`, `financial_controller` |
| `POST /vault/{id}/resync` | `owner` |

Reads match the read set used for disbursements. Envelope construction is
restricted to the two money-movement roles, matching how disbursement creation
is restricted — even though the envelope is unsigned and therefore inert
without the admin's key, so this is defence in depth rather than the only
control.

The **Admin API (8003) is not touched**; vaults are tenant-scoped resources,
not instance configuration.

### Secrets

- No new secret is introduced. `VAULT_CONTRACT_ID` is a public contract address.
- The backend holds no vault signing key, and the API has no field that
  accepts one. The existing `DISTRIBUTION_SEED` and channel-account keys are
  not used for vault operations.
- RPC credentials reuse the existing `RPC_REQUEST_AUTH_HEADER_KEY` /
  `RPC_REQUEST_AUTH_HEADER_VALUE` configuration.

---

## 6. Feature flag

| Variable | CLI flag | Default | Notes |
|---|---|---|---|
| `ENABLE_VAULT` | `--enable-vault` | **`false`** | Master switch |
| `VAULT_CONTRACT_ID` | `--vault-contract-id` | *(empty)* | **Required when enabled**; startup fails fast otherwise |
| `VAULT_INDEXER_INTERVAL_SECONDS` | `--vault-indexer-interval-seconds` | `10` | Validated against the existing minimum-interval floor |
| `VAULT_INDEX_FROM_LEDGER` | `--vault-index-from-ledger` | `0` (auto) | Pin a bootstrap ledger |
| `VAULT_STATUS_LAGGING_LEDGERS` | `--vault-status-lagging-ledgers` | `60` | Lag (ledgers) at or above which `GET /vault/{contract_id}/status` reports `lagging`. Must be `≥ 1` and below the 120,960-ledger RPC event retention window |
| `VAULT_STATUS_STALL_SECONDS` | `--vault-status-stall-seconds` | `300` | Seconds without an indexer commit before `/status` reports `stalled`. Must be longer than `VAULT_INDEXER_INTERVAL_SECONDS` |
| `VAULT_STATUS_RPC_CACHE_SECONDS` | `--vault-status-rpc-cache-seconds` | `5` | How long `/status` caches the RPC latest-ledger lookup (1–60). The reported chain tip may be up to this old |

This follows the established `ENABLE_SEP45` / `ENABLE_EMBEDDED_WALLETS`
pattern in `cmd/serve.go`: a `ConfigKey` wired into `serveOpts` and consumed
through `dependencyinjection`, with the dependent option required when the flag
is on.

**With the flag off — the default — the integration is inert:**

- vault routes are not registered (404),
- the indexer job is not scheduled,
- no RPC calls are made,
- no code path touches the new tables.

The migration still runs, because migrations are not feature-flagged in SDP and
creating four unused tables is harmless and keeps rollout ordering simple. The
down migration removes them cleanly.

This is what makes the change non-breaking: an existing deployment that does
not set `ENABLE_VAULT` behaves identically to today, and the quality gates
(`go vet`, `golangci-lint`, `go test ./... -race`) run against both states.

---

## 7. Testing plan

- **Unit, table-driven**, per the repo convention: event decoding for all six
  event types; projection transitions including the illegal ones
  (`CLAIMED → PENDING` must be rejected); amount parsing at `i128::MAX`;
  cursor round-tripping; gap detection boundaries (`last_ledger + 1` exactly at
  and just below `OldestLedger`).
- **Idempotency**: apply the same batch twice and assert the projection and
  all totals are byte-identical.
- **Replay**: index to ledger N, rewind the cursor to 0, replay, assert the
  final state matches.
- **Gap**: a mocked RPC whose `OldestLedger` jumps past the stored cursor must
  produce `status = 'degraded'` with correct bounds, and must not silently
  serve a partial list as healthy.
- **Handlers**: auth matrix per role per route; validation rejections; 404
  when the flag is off.
- **Multi-tenant**: two tenants with different vaults, asserting no row
  crosses schemas and that one degraded indexer leaves the other healthy.
- **Integration** against the local stack plus testnet, using the already
  deployed contract, driving fund → register → claim and asserting the indexer
  converges.

Mocks follow the existing `internal/stellar/mocks` generated-mock style, so
`RPCClient` gains its `GetEvents` mock the same way.

---

## 8. Out of scope

- Signing or submitting vault transactions server-side — structurally excluded.
- Linking vaults to SDP disbursements or receivers (schema leaves room).
- Automatic recovery from a retention gap, which needs data the network no
  longer has.
- Vault deployment or `initialize` from SDP; vaults are deployed out-of-band
  and referenced by contract id.
- Multiple tokens per vault — the contract is single-token by design.

---

## 9. Indexer health status

`GET /vault/{contract_id}/status` reports whether the indexer is keeping up
with the chain, from outside the indexer job.

### 9.1 What already exists, and why this is not a duplicate

| Existing piece | What it answers | Gap |
|---|---|---|
| `vault_indexer_state` | Stored cursor (`last_ledger`), `last_indexed_at`, gap bounds | No chain tip, so no lag |
| `GET /vault/{id}/indexer` (also embedded as `indexer` in every list response) | Stored position and whether a gap was recorded (`healthy` / `degraded`) | Never calls RPC by design, so `lag_ledgers` is always `0` and `latest_ledger` echoes the cursor |
| `vault.AssessLag` + `LagMonitor` | Lag against the retention window (`ok` / `warning` / `critical`) | Computed only inside the job, after a run. If the job stops running, it stops reporting |
| `sdp_vault_indexer_lag_ledgers`, `sdp_vault_indexer_degraded` gauges | Same, as metrics | Needs a metrics backend, and goes silent rather than red when the job dies |

None of these can say "the indexer has stopped". Every existing signal is
produced *by* the job, so a dead job reads the same as a quiet, healthy one.
`/status` measures from outside the job, comparing the stored cursor and
commit time with the live chain tip.

**It is a new route, not a change to `/indexer`.** The `/indexer` shape is
embedded in every list response, and its `status` enum (`healthy` /
`degraded`) is already used by the frontend. Changing it would add an RPC call
to every dashboard page load (§4 deliberately avoids that) and would break the
enum. `/indexer` stays as it is: cheap, database-only, and about gaps.
`/status` is live and about progress. Its response includes `degraded`, so one
call gives a monitor the whole picture.

Code is reused, not copied: `stellar.EventRetentionWindowLedgers` for the
window, the same `>=` boundary convention as `AssessLag`, and the RPC client
`serve.go` already builds for the transaction builder.

### 9.2 Endpoint

`GET /vault/{contract_id}/status`, registered in the vault read group in
`internal/serve/serve.go`:

- **Auth:** the same `RequirePermission(data.ReadVault, AnyRoleMiddleware(owner,
  financial_controller, business, initiator, approver))` as the other vault
  reads, inside the authenticated router group. `/status` is a row in the
  same serve-level route table as every other vault route, so it is held to
  the same role matrix and API key checks.
- **Tenant scoping:** the same `contractIDFromRequest` check, so another
  contract returns 404, and the same tenant-routed `DBConnectionPool`, so the
  cursor read is per-tenant by construction.
- **Flag off:** not registered, so it returns 404, like every vault route.

The schema is `IndexerHealth` in the §4 spec. Example:

```json
{
  "contract_id": "CB5H...HG53",
  "status": "lagging",
  "reason": "lag_over_threshold",
  "detail": "indexer is 120 ledgers behind the chain tip (threshold 60); 120840 ledgers of retention headroom remain",
  "degraded": false,
  "cursor_ledger": 812340,
  "rpc_latest_ledger": 812460,
  "lag_ledgers": 120,
  "lag_seconds_estimate": 600,
  "last_event_at": "2026-11-11T09:12:05Z",
  "retention_headroom": 120840,
  "checked_at": "2026-11-11T09:20:31.402Z"
}
```

Fields:

- `reason` is a **closed enum** (`IndexerHealthReason`): `rpc_unreachable`,
  `never_indexed`, `no_recent_commit`, `lag_over_threshold`, `none`. It is never
  null. Monitors switch on it.
- `detail` is **free text** for people, with the numbers that produced the
  result. It is not a stable format. It never contains the RPC error text,
  because that can carry the RPC URL and any API key in it; the error is
  logged at warn instead.
- `checked_at` is when the status was computed (RFC 3339, UTC). **The chain
  tip it is compared against can be up to `VAULT_STATUS_RPC_CACHE_SECONDS`
  older than `checked_at`** (§9.5). So with the default 5s cache,
  `rpc_latest_ledger` can trail the real tip by about one ledger, and
  `lag_ledgers` can read low by the same amount.
- `lag_ledgers = max(0, rpc_latest_ledger - cursor_ledger)`. A cursor ahead
  of a cached tip means caught up, not negative lag.
- `lag_seconds_estimate = lag_ledgers × stellar.AverageLedgerCloseSeconds`
  (5s, the close time the retention window was measured at). It is an
  estimate by design.
- `retention_headroom = max(0, EventRetentionWindowLedgers - lag_ledgers)`.
  It is derived from the window constant rather than an extra `getEvents`
  probe for `oldestLedger`, so each cache refresh makes one RPC call, not
  two. The existing lag alert makes the same assumption.
- `last_event_at` comes from `VaultModel.GetLastEventAt`: `SELECT
  ledger_closed_at … ORDER BY ledger DESC, id DESC LIMIT 1`, served by
  `vault_events_contract_ledger_idx`. It is null when no event exists.
- When RPC is unreachable, `rpc_latest_ledger`, `lag_ledgers`,
  `lag_seconds_estimate` and `retention_headroom` are `null`, not `0`,
  because a zero would read as "caught up". `cursor_ledger`, `last_event_at`
  and `degraded` still come from the database. `cursor_ledger` is null only
  when no indexer row exists.

### 9.3 Status rules

`vault.AssessHealth(input, thresholds, now)` is a pure function with no I/O
and an injected clock. Rules are evaluated in order, and the first match wins:

| # | Condition | `status` | `reason` |
|---|---|---|---|
| 1 | Chain-tip lookup failed or timed out | `stalled` | `rpc_unreachable` |
| 2 | No `vault_indexer_state` row, or `last_indexed_at` is null | `stalled` | `never_indexed` |
| 3 | `now - last_indexed_at >= VAULT_STATUS_STALL_SECONDS` | `stalled` | `no_recent_commit` |
| 4 | `lag_ledgers >= VAULT_STATUS_LAGGING_LEDGERS` | `lagging` | `lag_over_threshold` |
| 5 | otherwise | `ok` | `none` |

Notes:

- Both thresholds use `>=`, which matches `AssessLag`, so "at the threshold"
  means the same thing everywhere in the vault code.
- Rule 3 is sound because an idle run still commits.
  `Test_Indexer_Run_idleRunsAdvanceLastIndexedAt` backdates `last_indexed_at`
  by an hour, runs the indexer with no events (with and without the tip
  moving), and asserts the timestamp jumps to now. The test fails if
  `AdvanceCursor` stops setting `last_indexed_at`. The commit depends on RPC
  returning a cursor for an empty page, which stellar-rpc always does.
- A commit time in the future (database clock ahead) is not treated as
  stalled.
- `never_indexed` is `stalled`, not `ok`. That is the honest reading, and with
  the default 10s interval it clears on the first run.
- `degraded` does not change `status`. A gap is permanent history, not current
  progress, and folding it in would leave a recovered indexer red forever.

### 9.4 Configuration

New `serve` options, following the existing `VAULT_*` `ConfigKey` pattern.
They are listed in §6 and in the README. They are validated only when
`ENABLE_VAULT` is true, and startup fails fast otherwise.

| Variable | CLI flag | Default | Validation |
|---|---|---|---|
| `VAULT_STATUS_LAGGING_LEDGERS` | `--vault-status-lagging-ledgers` | `60` (~5 min) | `1 ≤ v < 120,960`. A threshold at or past the retention window could only fire after data is already lost |
| `VAULT_STATUS_STALL_SECONDS` | `--vault-status-stall-seconds` | `300` | `v > VAULT_INDEXER_INTERVAL_SECONDS`, otherwise a healthy indexer would flap between runs |
| `VAULT_STATUS_RPC_CACHE_SECONDS` | `--vault-status-rpc-cache-seconds` | `5` (~1 ledger) | `1 ≤ v ≤ 60` |

Why these defaults: a healthy indexer on a 10s interval sits about 2 to 3
ledgers behind the tip. 60 ledgers is about 30 runs' worth of falling behind,
well clear of noise, and three orders of magnitude below the 60,480-ledger
data-loss warning, so `lagging` fires days before the existing alert would.
300s is about 30 consecutive runs with no commit.

### 9.5 Caching the chain tip

- A process-wide `vault.LatestLedgerCache` wraps `GetLatestLedgerSequence`.
  The chain tip is public, network-wide data, so sharing one cache across
  tenants exposes nothing tenant-specific. Tenant data (cursor, events) is
  never cached.
- The TTL is `VAULT_STATUS_RPC_CACHE_SECONDS`. That is the bound on how stale
  `rpc_latest_ledger` can be relative to `checked_at`.
- Concurrent misses are collapsed: the lock is held across the lookup, so N
  simultaneous requests make one RPC call.
- **Errors are cached for the same TTL.** During an outage, requests get an
  immediate `stalled` instead of each waiting out its own timeout.
- Each lookup has a **2s timeout** (`vault.DefaultLatestLedgerLookupTimeout`),
  so a hung RPC becomes `rpc_unreachable` quickly. The lookup does not inherit
  the triggering request's cancellation (`context.WithoutCancel`), so one
  client disconnecting cannot cache a "context canceled" error for everyone
  else.
- **No stale fallback.** If the cached value has expired and the refresh fails,
  the result is `stalled`. It never reuses an older tip, which could report
  `ok` during an outage.

### 9.6 Error handling

| Failure | Response |
|---|---|
| RPC unreachable, erroring or hung | `200`, `stalled` / `rpc_unreachable`; the error is logged at warn, not returned |
| Wrong or invalid contract id | `404` / `400`, as on the other vault routes |
| Database read fails | `500`, as on the other vault routes. The service's own store failing is a server error, not an indexer health result |

### 9.7 Tests

| Area | Where |
|---|---|
| Idle runs advance `last_indexed_at` (the rule-3 precondition) | `internal/vault/indexer_test.go` |
| Every status transition and threshold boundary (`-1` / `=` / `+1` for lag and commit age), rule precedence, derived fields and their floors, nulls when RPC is down, JSON shape | `internal/vault/status_test.go` |
| Cache: hit within TTL, expiry at exactly TTL, cached errors, no stale fallback, hung RPC times out, caller cancellation does not poison the cache, concurrent misses make one call | `internal/vault/latest_ledger_cache_test.go` (fake RPC with answer / error / hang modes) |
| `GetLastEventAt`: none, newest by ledger, scoped to the contract | `internal/data/vault_test.go` |
| Handler: full shape, each status end-to-end, RPC error text never echoed, hung RPC through the real cache, contract scoping | `internal/serve/httphandler/vault_handler_test.go` |
| Route: flag off gives 404, no token gives 401, every role, and API key permissions, in the same table as every other vault route | `internal/serve/serve_vault_routes_test.go` |
| Env var bounds; the `/status` body through the real mux | `internal/serve/serve_vault_status_test.go` |

A mutation check on the threshold logic in `AssessHealth` is run before each
PR. Its results are recorded in the PR description rather than in the repo.

### 9.8 Not in this change

- `/indexer`'s `latest_ledger` / `lag_ledgers = 0` fields stay as they are.
  They are documented as stored-position-only, and changing them would change
  every list response.
- Spec-conformance checks across all vault handlers (issue #10) and an
  operator runbook (issue #11) are open contributor issues.
- No new metric. The existing gauges already cover lag.

---

## 10. Summary bootstrap from contract state

### 10.1 The problem

`vault_state`, the summary behind `GET /vault/{contract_id}`, is created in
exactly one place: the projection of the contract's `initialized` event
(`UpsertState` in `Indexer.project`). Every later event only `UPDATE`s that row,
so an event that finds no row is silently a no-op.

On first sight of a vault the indexer starts at the oldest ledger RPC still
serves (§2.7, about 7 days back). If the vault was created before that,
`initialized` is never read: there is no summary row, the overview answers
`404 "Vault has not been indexed yet."` for good, and every `funded`,
`registered`, `claimed`, `cancelled` and `withdrawn` event updates nothing. Any
deployment that starts indexing more than a week after the contract was
created fails this way. The live testnet vault is in exactly this state.

§2.6 and §2.7 already said to "seed `vault_state` from the contract by
simulation". That was never implemented. This section specifies it and
supersedes those two sentences.

### 10.2 What the contract exposes, and what it does not

Read-only functions (`bindings/disbursement-vault`, `contracts/disbursement-vault/src/lib.rs`):

| Function | Returns | Summary field |
|---|---|---|
| `get_admin()` | `Result<Address>` | `admin_address` |
| `get_token()` | `Result<Address>` | `token_address` |
| `get_deadline()` | `Result<u64>` (unix seconds) | `deadline` |
| `total_allocated()` | `i128` | `total_allocated` |
| `total_claimed()` | `i128` | `total_claimed` |
| `available()` | `Result<i128>` | (derived, not stored) |
| `get_allocation(recipient)` | `Option<Allocation>` | (needs a known address) |

There is no `balance()` on the vault. The balance is the token's own SEP-41
`balance(id)` for the vault's address, read from the token contract.

**Recipients cannot be enumerated.** `DataKey::Allocation(Address)` is a lookup
by address, with no index or count (contract issue #8). So **the summary can be
bootstrapped, but per-recipient allocations cannot.** They stay event-derived,
and anyone registered before the first indexed ledger is unknown to SDP.
Bootstrap does not change that. It makes the gap visible instead of hiding it
(§10.5).

### 10.3 How it is read

The reads use `simulateTransaction` on the six view functions above (five on
the vault, plus `balance(vault)` on the token), through the existing
`stellar.RPCClient.SimulateTransaction`. Nothing is signed or submitted. The
source account is a fixed public placeholder, the all-zero ed25519 key
(`GAAAA…AWHF`), since read-only simulation does not load it. There is no secret
key for it anywhere, and none is needed.

Simulation is preferred over `getLedgerEntries`, which would read the
contract's instance storage and the token's balance entry directly. That would
be one atomic read, but it ties the backend to two private storage layouts. The
second one is a SAC detail that a non-SAC token would not share. Simulation
uses only public interfaces.

The cost is that six simulations are six reads. To keep them consistent, every
response must report **the same `latestLedger`**; that ledger is the snapshot
ledger. If it moves between calls, the whole set is re-read (at most 3 times
per attempt). If it still moves, the attempt fails like any other failure
(§10.6). A mixed-ledger snapshot is never stored.

### 10.4 When it runs, and the race with event indexing

At the end of `Indexer.Run`, the snapshot is taken **only if all three hold**:

1. there is no `vault_state` row, and
2. the run stopped because RPC had nothing more to return, not because it hit
   `maxBatchesPerRun`. Then the whole window up to the tip has been scanned,
   and no `initialized` event in it created the row, and
3. the backoff (§10.6) allows another attempt.

An `initialized` event inside the window therefore always wins, through the
normal event path.

Correctness rules. Snapshot ledger = `L`.

| Rule | How |
|---|---|
| Idempotent; never overwrites | `INSERT … ON CONFLICT (contract_id) DO NOTHING`. If an event created the row first, the snapshot is discarded |
| Events at or before `L` are not applied twice | The snapshot already includes them. A new immutable column `bootstrap_ledger = L`. Every `vault_state` update in `ApplyFundedBalance`, `ApplyTotalAllocated`, `ApplyClaim`, `ApplyCancellation` and `ApplyWithdrawal` gains `AND (bootstrap_ledger IS NULL OR $ledger > bootstrap_ledger)`. Strictly greater: simulation reads state *after* ledger `L` closed |
| A late `initialized` event cannot reset a snapshot | Its projection becomes insert-if-absent (`ON CONFLICT DO NOTHING`). `initialize` runs once per contract, so an existing row is always newer |
| Allocation rows are unaffected by the guard | `RegisterAllocation` and `SettleAllocation` still apply for every new event, at any ledger: events are the only source of recipients |
| No partial rows | All six values are decoded and validated, in memory and with the ledger check, before the single `INSERT`. Any failure means no write |
| Nothing invented | Every column comes from a contract read. If `get_admin` and the others fail with the contract's `NotInitialized`, no row is written and the failure is reported. No zeroed or default summary is ever written |

### 10.5 Marking the source, and being honest about allocations

The migration adds to `vault_state`:

- `source TEXT NOT NULL DEFAULT 'events' CHECK (source IN ('events','snapshot'))`
- `bootstrap_ledger INTEGER NULL`: the ledger the snapshot was read at, and
  `NULL` for event-derived rows. `snapshot_ledger` keeps its meaning: the
  ledger the row is current to

The overview gains two additive fields: `source` and `bootstrap_ledger`.

When a snapshot is taken, history before the window is known to be missing,
so the vault is **marked degraded** with reason "summary read from contract
state at ledger L; events before ledger X were never indexed, so recipients
registered before then are not listed (the contract cannot enumerate them)".
Today such a vault is reported `healthy`, which is wrong.

`X` is the first ledger the indexer scanned. That was not stored before, so
`vault_indexer_state.start_ledger` now records it at bootstrap. For rows
bootstrapped earlier it is `NULL`, and the RPC retention floor at the time of
the snapshot stands in. That is an upper bound, and the degraded reason says so.

The existing gap constraint requires both bounds while degraded, but the
**start** of this gap (the contract's creation ledger) is not knowable from RPC.
**Decided:** `vault_indexer_state_gap_check` now lets `gap_from_ledger` be
`NULL` (meaning unknown) while degraded; `gap_to_ledger` is still required.
A later retention gap keeps the unknown start, and only moves the end forward.

The migration is reversible. On the way down, an unknown start becomes `0`,
which is a superset of the true gap, because the old constraint requires a
value. Snapshot rows stay, since they hold real contract values; only the
provenance columns are dropped.

### 10.6 Failure handling

- **Never crashes the indexer.** A snapshot failure is logged and recorded. The
  event batches already committed in that run stay committed, and `Run`
  returns normally.
- **Backoff, persisted.** The job builds a new `Indexer` on every tick, so the
  backoff lives in the database. The migration adds columns to
  `vault_indexer_state`: `snapshot_attempts`, `snapshot_next_attempt_at` and
  `snapshot_last_error`. The delay is 10 s, 20 s, 40 s and so on, capped at
  5 minutes; a success clears it. Only a category is stored, never raw RPC
  error text, which can carry a provider URL or key (as in §9.6). The schema
  enforces this with a check constraint on the five categories:
  `rpc_unreachable`, `contract_not_initialized`, `inconsistent_ledgers`,
  `simulation_failed` and `invalid_response`.
- **Silent no-ops are now visible.** When an event's summary update finds no
  row, the update itself is unchanged (still a no-op). The indexer logs a
  warning naming the contract and the event type, and increments
  `sdp_vault_indexer_summary_update_missed_total{tenant_name,contract_id,event_type}`.
- **Reported through `GET /vault/{contract_id}/status`**, as an additive
  `summary` object. The closed `reason` enum is unchanged, because the frontend
  validates it strictly:
  ```json
  "summary": {
    "state": "missing | snapshot | events",
    "bootstrap_ledger": 123 | null,
    "bootstrap": {
      "attempts": 3,
      "last_error": "rpc_unreachable" | null,
      "next_attempt_at": "RFC 3339" | null
    }
  }
  ```

### 10.7 Machine-readable 404s (additive)

SDP's `HTTPError` already has an `error_code` JSON field (`WithErrorCode`).
Messages stay exactly as they are; only the code is added:

| Where | Message (unchanged) | `error_code` |
|---|---|---|
| `contractIDFromRequest`: not this tenant's vault (every vault route) | `Vault not found.` | `vault_not_found` |
| `GetVault`: no summary row | `Vault has not been indexed yet.` | `vault_not_indexed` |
| `ResyncVault` / `BuildVaultTransaction`: no indexer state or summary row | `Vault has not been indexed yet, so …` | `vault_not_indexed` |

The plain-text router 404 when `ENABLE_VAULT` is off is unchanged; the
frontend relies on it for feature detection.

OpenAPI: add a `VaultError` schema
`{ error: string, error_code?: enum [vault_not_found, vault_not_indexed] }`.
Give the vault routes' `404` responses a JSON body using it, and document the
plain-text 404 for the flag-off case. Also add `source` and `bootstrap_ledger`
to `VaultOverview`, and `summary` to the status response.

### 10.8 Tests

| Area | Cases |
|---|---|
| Snapshot reader (fake RPC) | success; RPC unreachable; one of six calls fails (no row); contract `NotInitialized` (no row, no zeros); `latestLedger` differs between calls, then retried until consistent; differs on every try (no row); undecodable value |
| Race with events (real test DB) | an `initialized` event in the window means no snapshot; snapshot at `L`, then events at `L-1`, `L` and `L+1` replayed (only `L+1` touches `vault_state`, and allocation rows are written for all three); a snapshot after an event-created row is a no-op; a late `initialized` does not reset a snapshot; a run capped at `maxBatchesPerRun` does not snapshot |
| Backoff | delay sequence and cap; no attempt before `next_attempt_at`; success clears it; the stored error is a category, never raw text |
| Degraded marking | a snapshot marks degraded with `gap_from` `NULL` and `gap_to` = window floor − 1; the relaxed constraint still rejects a degraded row with no `gap_to` |
| Handlers | `error_code` on each 404; the overview's `source` and `bootstrap_ledger`; `/status` `summary` in each state |

A mutation check on the guards and the trigger conditions is run before the PR
and recorded in its description.

### 10.9 Not in this change

- **Recovering the missing recipients.** That needs contract issue #8 or
  operator input.
- **The documented-but-unimplemented `?refresh=true`** on the overview. It is a
  spec/handler mismatch (issue #10).
- **A bounded re-scan command** (issue #7), which cannot help here anyway: the
  missing events are outside retention.
- **Frontend changes.** It will show the summary as soon as the overview
  returns `200`. Its empty-allocation text is a known follow-up for degraded
  vaults.
