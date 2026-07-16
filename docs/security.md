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
3. **Token lifecycle.** Tokens are long-lived operator strings with no rotation or
   expiry; revocation = edit config + restart. Use **long, random** tokens
   (`openssl rand -hex 32`), store them only in `.env` / a secrets manager, and
   rotate on suspected compromise.
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
7. **Request-rate / DoS.** Storage quotas are not rate limits; each search triggers
   an embed. Add **rate limiting at the proxy** (Caddy) and network controls.

## Hardening checklist

- [ ] App bound to `127.0.0.1` (default); never publish `:8080` to the network.
- [ ] Caddy proxy profile on, with **TLS** (domain or `:443`) and a strong
      `BASIC_AUTH_HASH`.
- [ ] `AUTH_TOKEN` (and any `PRINCIPAL_TOKEN_*`) are long and random; in `.env`
      only.
- [ ] `WEBUI_MODE` left unset (read-only) unless you need interactive writes.
- [ ] For multi-team sensitive data: `principals:` configured (Layer 2), one
      namespace per team; the MCP process runs with `BRAINIAC_PRINCIPAL_TOKEN`.
- [ ] Extraction review **on** for any untrusted ingested source.
- [ ] Postgres on an **encrypted volume**; backups (`--profile backup`) shipped
      **off-box**.
- [ ] `brainiac audit` reviewed periodically; ship the access logs off the box.

See also: [deployment.md](deployment.md) (proxy/TLS setup), [operations.md](operations.md)
(backups), and the roadmap epics for the security follow-ups (audit reads,
per-human identity, token lifecycle, encryption at rest, per-fact erasure).
