# Security guide & threat model

Brainiac stores a company's memory — often sensitive. This guide states what it
protects, what it does **not**, and how to deploy it safely. Read it before
pointing it at real data or a network.

## Trust model at a glance

| Layer | Reads | Writes | Identity |
|-------|-------|--------|----------|
| **Layer 1** (default) | **open** — anyone who can reach the port | single shared `AUTH_TOKEN` (bearer) | none |
| **Layer 2** (opt-in, `principals:`) | per-token, walled to a namespace set | per-token, pinned to one namespace | per-token principal |

- **MCP** is stdio-only — never network-exposed. It runs as one process bound to a
  principal by its **secret token** (`BRAINIAC_PRINCIPAL_TOKEN`), not a name (#266).
- **Every write is audited** (`audit_log`; `brainiac audit`) with the acting
  principal, operation, target, and namespace (#267).

## The threats it treats

- **SQL injection** — every query binds parameters; no user data is interpolated
  into SQL.
- **Cross-namespace access (Layer 2)** — reads are walled in the core (a
  compiler-forced `store.Wall` on every multi-row read), writes are pinned, and
  **every by-id mutation** (supersede/disambiguate/split/merge/proposal-approve)
  is namespace-checked (#265). An id/name guess across the wall reads as
  "not found", never a leak.
- **Secret leakage in logs** — DB passwords and bearer tokens are redacted at
  capture; under isolation `/api/logs` is not exposed (it carries other tenants'
  query strings) (#268).
- **Write abuse** — per-namespace row quotas (`max_nodes`/`max_chunks`) cap
  storage (#186).

## The threats it does NOT (yet) treat — you must

1. **Unauthenticated reads in Layer 1.** Anyone who can reach `:8080` reads
   everything. **Bind to localhost** (the default) and put the whole surface behind
   the **Caddy reverse proxy with TLS + auth** (`--profile proxy`) for any network
   exposure. Do not expose the app port directly.
2. **Cleartext tokens over `:80`.** If `SITE_ADDRESS=:80`, bearer/basic-auth
   credentials cross the wire in cleartext. **Require TLS in production** — use a
   real domain (auto-HTTPS) or `:443` with `tls internal`; never `:80` off-box.
3. **Token lifecycle (#269).** Principal tokens support entropy floors,
   hash-at-rest, expiry, and hot revocation. Generate a strong token with
   `brainiac token gen` (256-bit hex; a plaintext principal token must be ≥ 32
   chars). To keep the live secret out of `config.yaml`, store its hash instead:
   `brainiac token gen | brainiac token hash` → put the result under the
   principal's `token_sha256:` and hand the plaintext to the client. Set an
   optional `expires:` (RFC3339) for an automatic, restart-free cutoff. To
   **revoke or rotate without downtime**, edit `config.yaml` (`revoked: true`, a
   new `token_sha256`, or remove/re-add the principal) and reload the app:
   `docker compose kill -s HUP app` — the HTTP server swaps the roster atomically
   (a config that no longer validates is rejected and the old roster stays).
   Store secrets only in `.env` / a secrets manager. *The single Layer-1
   `AUTH_TOKEN` has no such lifecycle — rotate it by restart.*
4. **Prompt-injection into memory.** Memory is agent-writable and recalled content
   is fed back to agents. A poisoned ingested document can steer a downstream
   agent, and (with `EXTRACTION_REVIEW=false`) the local extractor. **Keep
   extraction review on** for untrusted sources, trust `source_uri` provenance, and
   treat recalled content as untrusted input in your agent's prompt.
5. **Encryption at rest.** Not built in — rely on **encrypted volumes / an
   encrypted managed Postgres** for at-rest protection.
6. **Right-to-erasure at fact granularity.** Supersede/merge keep history; only
   whole-namespace `namespace delete` hard-deletes. Per-node/per-chunk erasure is a
   roadmap item — for GDPR today, isolate erasable data into its own namespace.
7. **Request-rate / DoS (#270).** Storage quotas cap rows, not request rate, and
   each search triggers an Ollama embed. The app now has two opt-in controls:
   **per-client rate limiting** (`http.rate_limit_rps` + `rate_limit_burst`, or
   `HTTP_RATE_LIMIT_RPS`/`_BURST`) — a token bucket keyed by principal, else bearer
   token, else source IP, returning `429` + `Retry-After` — and an
   **embed-concurrency cap** (`embedding.max_concurrency` / `EMBED_MAX_CONCURRENCY`)
   bounding in-flight embed round-trips to Ollama. Both default off. Still add
   **rate limiting at the proxy** (Caddy) and network controls for network-level
   abuse; the app-level limit is per-identity, not a substitute.

## Hardening checklist

- [ ] App bound to `127.0.0.1` (default); never publish `:8080` to the network.
- [ ] Caddy proxy profile on, with **TLS** (domain or `:443`) and a strong
      `BASIC_AUTH_HASH`.
- [ ] `AUTH_TOKEN` (and any `PRINCIPAL_TOKEN_*`) are long and random
      (`brainiac token gen`); in `.env` only. Prefer `token_sha256:` (hash-at-rest)
      for principals, and set `expires:` where a cutoff applies.
- [ ] `WEBUI_MODE` left unset (read-only) unless you need interactive writes.
- [ ] For multi-team sensitive data: `principals:` configured (Layer 2), one
      namespace per team; the MCP process runs with `BRAINIAC_PRINCIPAL_TOKEN`.
- [ ] Extraction review **on** for any untrusted ingested source.
- [ ] `http.rate_limit_rps` and `embedding.max_concurrency` set for any
      multi-client / exposed deployment (plus proxy-level rate limiting).
- [ ] Postgres on an **encrypted volume**; backups (`--profile backup`) shipped
      **off-box**.
- [ ] `brainiac audit` reviewed periodically; ship the access logs off the box.

See also: [deployment.md](deployment.md) (proxy/TLS setup), [operations.md](operations.md)
(backups), and the roadmap epics for the security follow-ups (audit reads,
per-human identity, encryption at rest, per-fact erasure).
