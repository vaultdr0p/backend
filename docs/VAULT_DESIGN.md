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
        gap_from_ledger: { type: [integer, 'null'] }
        gap_to_ledger: { type: [integer, 'null'] }
        degraded_reason:
          type: [string, 'null']
          description: >
            Set when status is degraded. A gap means events fell outside the
            RPC retention window, so the recipient list may be incomplete.
            On-chain funds are unaffected.

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
        extras: { type: object, additionalProperties: true }

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
        '404': { description: vault not configured for this tenant, or flag off }

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
