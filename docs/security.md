# Security

Five layers of defence around the credentials the AI references but
never sees.

## Layer 1: at rest

`internal/vault/` stores every credential AES-256-GCM with PBKDF2-SHA256
600k iterations, 32-byte salt, 12-byte nonce, version byte for
transparent re-encrypt on master-key rotation. The master key is the
only thing protecting the blob; **loss of the master key is total,
unrecoverable loss of every credential.**

There is no recovery mechanism. Earlier versions of this document
promised a "BIP39 24-word recovery encoded on init"; that was never
implemented and no mnemonic is ever shown. `reactor init` writes 32
random bytes to `<state>/master.key` (mode 0600) and prints only the
path. **Back that file up out of band before you store anything in the
vault**, and see docs/operations.md for the rotation window
(`REACTOR_MASTER_KEY_PREVIOUS`).

## Layer 2: MCP surface is metadata-only

`internal/mcp/mcp.go` registers read tools that return `Credential`
rows (id, name, service, provider, provider_meta, rotation state, audit
history). The blob is never serialised; `RotationTargets` are stripped
before send because `secret_id` cross-references would be a meta-leak.
There is no `get_credential_value` tool. The AI literally cannot ask
MCP for plaintext.

## Layer 3: Secret type redacts every accidental sink

`internal/vault/secret.go` -- the `Secret` type's `String`, `GoString`,
`MarshalJSON`, `MarshalText`, `LogValue` all return `[REDACTED]`. The
only reveal path is `.Reveal() []byte`, a single greppable call.
`slog.Info("got", "key", sec)` prints `[REDACTED]` even if the
workflow author messes up.

## Layer 4: AI references by id

The codegen system prompt teaches `vault.MustGet(id)`. The JSON schema
slot for triggers carries `secret_id` not the value. AI gets the slot,
never the secret.

The reactor lint blocks `os/exec`, `syscall`, `unsafe`, `math/rand` and
(in the build allowlist) `C`, `plugin`, `os/signal`.

**It does NOT block `net/http`, and it does not block `os`.** This
document previously claimed otherwise. Workflow code therefore has
unrestricted outbound network access and can read any file the daemon
user can read, including `<state>/master.key`. Treat this layer as
"the AI is steered toward referencing secrets by id", not as
containment: a workflow author (or an AI writing on their behalf) who
wants to exfiltrate a granted secret can, and the only real boundary is
the OS the daemon runs as. Run the daemon as a dedicated unprivileged
user with a `0700` state directory if that matters to you.

## Layer 5: run-time fetch is brokered + ACL'd

`internal/runtime/supervisor/supervisor.go:handleSecretFetch` --
workflow subprocess sends `secret_fetch{id}` over the pipe; the host
resolves `workflow_id` from slug, checks `workflow_secret_grants`,
denies with `NotFound` on miss, then `vault.Get` + pipe-send the bytes.
Every read audits. The subprocess never holds DB credentials.

Default-deny on empty grants since v0.1: an operator who provisions
credentials but forgets `reactor vault grant` gets a silently
permissive vault on the v0 path, fixed in `7883cb3d`. Opt out via
`--vault-acl-permissive` or `REACTOR_VAULT_ACL_PERMISSIVE=1`.

## Process isolation

The workflow subprocess runs under:

- **prlimit** (Linux): RLIMIT_AS, RLIMIT_CPU, RLIMIT_NOFILE,
  RLIMIT_NPROC. Defaults: 512 MiB / 60s / 256 / 64.
- **cgroup v2** (Linux, opt-in via `--cgroup-root /sys/fs/cgroup`):
  `memory.max` + `pids.max` preset; child clones into the cgroup at
  clone3 time so the prlimit race window doesn't matter for memory +
  pid bounds.

macOS / FreeBSD: prlimit no-op; rely on the outer container or VM.

## Network

- HTTPS via `--tls-cert` / `--tls-key`, or behind a reverse proxy that
  sets `X-Forwarded-Proto: https`.
- SecurityHeaders middleware: CSP (`script-src 'self'`, which is what the
  dashboard's Cytoscape island under `/assets/` needs; inline scripts stay
  blocked, so DAG data rides a `<script type="application/json">` island
  rather than an inline block), X-Frame-Options
  DENY, X-Content-Type-Options nosniff, Referrer-Policy
  strict-origin-when-cross-origin, Permissions-Policy that nukes
  camera/mic/geo, HSTS when TLS.
- BasicAuth (constant-time SHA-256 + constant-time username compare;
  set REACTOR_BASIC_AUTH_USER + REACTOR_BASIC_AUTH_PASSWORD_SHA256).
- Per-IP token-bucket rate limiter (60 burst / 10 sustained by default,
  tunable via REACTOR_RATE_BURST / REACTOR_RATE_REFILL).

## Audit

Every credential touch writes a `credential_audit` row. The dashboard's
`/audit` aggregates these across credentials; per-credential audits
show on `/credentials/{id}`. Actor types:

- `seed` -- migration-time seeding
- `operator` + `*.cli` action -- CLI command (`create.cli` from
  `reactor vault add`, `grant.cli`, `revoke.cli`). `actor_id` carries
  `$USER` when the environment provides it.
- `operator` + `*.dashboard` action -- dashboard interaction
- `scheduler` -- automatic rotation tick
- `workflow:<slug>` -- runtime SecretFetch from a workflow

The audit log distinguishes who did what, when, and why every time.
