# Security Policy

This policy covers **this fork**, `vaultdr0p/backend`. It is not the upstream
Stellar Disbursement Platform's policy — see [Scope](#scope) for which
findings belong where.

## Supported versions

| Version | Supported |
|---|---|
| Latest commit on `develop` | ✅ Yes — the only supported version |
| Any older commit, tag or branch | ❌ No |
| Upstream SDP releases | ❌ Not here — report upstream |

This fork publishes no releases and no container images. `develop` is the
default branch and the only one that receives fixes. There is no backporting:
a fix lands on `develop` and that is the fix.

## Status: testnet only, and not audited

The vault integration added by this fork has **not been audited**. It is
exercised against Stellar **testnet only**, where tokens have no monetary
value, and it is not deployed on mainnet by this project. Treat it as
unreviewed software.

Do not use it to custody real funds. If you run it anywhere that holds value,
you are doing so against this advice and the risk is entirely yours — see the
warranty and liability disclaimers in [LICENSE](LICENSE).

The vault's Soroban contract lives in
[`vaultdr0p/contract`](https://github.com/vaultdr0p/contract) and is unaudited
and testnet-only on the same terms.

## Reporting a vulnerability

**Report privately. Do not open a public issue for a security problem.**

Use GitHub's private vulnerability reporting:

**[→ Report a vulnerability](https://github.com/vaultdr0p/backend/security/advisories/new)**

(Also reachable from the repository's **Security** tab → **Report a
vulnerability**.) The report stays visible only to you and the maintainers
until an advisory is published.

GitHub's form is the only reporting channel this project offers. This project
publishes no email address for security reports, deliberately: the form keeps
the report, the discussion, the fix and the eventual advisory attached to the
repository, rather than in a mailbox only one person can read.

### What to include

A report is easiest to act on when it has:

- the endpoint, handler or job affected, and the commit you tested
- the conditions required — which role or tenant, whether `ENABLE_VAULT` is on
- what an attacker gains: data from another tenant, an unauthorised write, an
  unsigned transaction built for the wrong network
- a failing Go test, or a `curl` sequence that reproduces it

A reproduction as a Go test is the most useful single thing you can send,
because it goes straight into the regression suite.

**Never include secret keys, seeds, tokens or production data** in a report.
Testnet addresses and transaction hashes are fine.

## Scope

| In scope | |
|---|---|
| `internal/vault/`, `internal/serve/httphandler/vault_*` | the vault endpoints — authorisation, tenant isolation, input validation |
| The vault event indexer and its jobs | missed, duplicated or forged events; a degraded state reported as healthy |
| `db/migrations/**` vault tables | a migration that widens access or loses data |
| The vault transaction builder | building an envelope for the wrong network, the wrong source, or the wrong amount |
| `.github/workflows/` | anything letting untrusted input run with write access to this repository |

| Out of scope | Where it belongs |
|---|---|
| Upstream SDP code this fork did not modify | [stellar/stellar-disbursement-platform-backend](https://github.com/stellar/stellar-disbursement-platform-backend) — its own security policy applies. Tell us too if the fork is exploitable because of it |
| Bugs in Go dependencies, the Stellar SDK or the RPC server | Report to that project. Tell us too, if this fork is exploitable because of it |
| The Soroban contract itself | [`vaultdr0p/contract`](https://github.com/vaultdr0p/contract/security/advisories/new) |
| The web dashboard | [`vaultdr0p/frontend`](https://github.com/vaultdr0p/frontend) |
| Any testnet deployment's state or balances | Not a vulnerability: throwaway data holding valueless tokens |
| Missing mainnet hardening, no audit | Known and documented above, not separate findings |

The vault admin is trusted by design. An admin funding, registering,
cancelling or withdrawing within the documented rules is the system working as
intended. A report is in scope if it lets a **non-admin** act as one, crosses
a tenant boundary, or makes the API misreport what the contract holds.

## Response times

Maintained by volunteers, so these are deliberately conservative — they are
what can be met, not a best case:

| Stage | Target |
|---|---|
| Acknowledgement that a human has read it | **5 business days** |
| Initial assessment — in scope or not, and rough severity | **10 business days** |
| Fix or a documented decision not to fix | **30 business days** for something exploitable in scope |

If 10 business days pass with no acknowledgement, the report was probably
missed. Open a public issue containing **no details** — just that you are
waiting on a security contact — and that will be treated as a priority.

Since nothing is deployed on mainnet by this project, a fix here means a
merged patch on `develop`; there is no emergency upgrade path to coordinate
and no funds at risk while a fix is pending.

## Disclosure

Coordinated disclosure. We will agree a publication date with you, defaulting
to **the earlier of: the fix landing on `develop`, or 90 days** from the
acknowledgement. Reporters are credited in the advisory by whatever name they
ask for, or not at all if they prefer.

There is **no bug bounty** and no payment. Reports are still genuinely
welcome, and this policy exists so you know what happens to one.
