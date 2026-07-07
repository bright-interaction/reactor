# RFC: Command Automations + Supervised Execution

Status: draft / design. Not yet implemented. This document is the security
contract the implementation must satisfy.

## Motivation

Operators accumulate shell-command automations (install a package on a host,
run a maintenance script, apply a fix) as loose scripts scattered across
servers with no versioning, no visibility, and no audit trail. The goal is to
bring those under Reactor's supervision: stored, versioned, visualized, and
observed, instead of running unattended and unrecorded on the hardware.

This is deliberately NOT "let workflows shell out." Reactor's codegen sandbox
bans `os/exec`, `syscall`, and friends in workflow Go on purpose (workflows are
AI-authored and potentially untrusted). That fence stays. Command execution is
added as a first-class, daemon-controlled, heavily gated capability, never as
arbitrary exec inside workflow code.

## Goals

1. Store command automations as first-class, git-versioned, visual entities.
2. Optionally execute them under supervision: durable, sandboxed, audited.
3. Do all of this without weakening the AI-authored-workflow sandbox.

## Non-goals

- Multi-tenant hosted command execution (v1). Self-host / single-tenant only.
- Replacing a CI system or a host-management agent. Reactor is the visual,
  versioned, audited store plus a sandboxed runner for the operator's own
  automations; heavier fleet execution can delegate to an external host agent.

## Design

### 1. The Command Automation model (the safe core, zero exec)

A command automation is a named, versioned entity:

- `name`, `description`, `tags`, target descriptor (host/context label)
- ordered `steps`, each with: the command (declarative), purpose, expected
  exit code, timeout, working dir, and credential references (vault key names,
  never inline secrets)

It is stored, git-versioned (Reactor's existing "your git log is the audit
trail"), and rendered in the DAG / flow view. This half adds no execution and
no new attack surface: it is data plus rendering. It already delivers the core
value (visibility, ownership, versioning, diffs, audit) with zero risk.

### 2. Supervised execution (gated)

A first-class command step type that the DAEMON runs, never workflow `os/exec`:

- Executes in a throwaway, locked-down container (non-root, no host filesystem
  mounts by default, CPU/memory limits, restricted network).
- stdout / stderr / exit code stream to the durable run journal, so every run
  is observable and replayable.
- Timeout plus a stuck-command reaper (claim-keyed, paired with a result
  idempotency guard so a long-running command is not false-reaped or run twice).
- An SSH command variant connects to a target host with a key resolved from the
  vault at run time (grant-ACL gated), runs the command, and captures output.
- Two backends: a local sandbox (container on the daemon host) or delegation to
  an external host-management agent that already owns the host-command path.

### 3. The gates (defense in depth, each layer independent)

```
off by default (feature flag)
  -> single-tenant only (hard-refused in any hosted / multi-tenant mode)
    -> admin-only (never members / viewers)
      -> step-up auth (fresh passkey assertion) to enable / run / reveal
        -> daemon-sandboxed execution (container, not workflow os/exec)
          -> credentials from the grant-ACL vault, never inline
            -> every run + command + output + actor in the durable audit journal
```

1. Off by default: a feature flag (e.g. `REACTOR_ENABLE_COMMAND_STEPS=1`)
   registers the step type at all. Absent, the capability does not exist, so it
   is not a standing liability for anyone who has not opted in.
2. Single-tenant only: hard-refuse when a hosted / multi-tenant mode flag is
   set. Multi-tenant command execution on shared infrastructure is a
   cross-tenant / host compromise vector and is out of scope until per-tenant
   isolated runners exist (ephemeral VM/container per job).
3. Admin-only: only admins may author, enable, or run command steps. Members
   and viewers never can. Ungated command routes reachable by non-admins are
   the single worst failure mode for this class of feature.
4. Step-up auth: a fresh WebAuthn passkey assertion (or TOTP fallback) is
   required to enable the feature, run a command, or reveal / rotate a secret.
   A valid session cookie alone never authorizes these.
5. Sandboxed: container isolation, non-root, no host mounts by default,
   resource and network limits.
6. Vaulted credentials: SSH keys and tokens live in the vault, gated by the
   per-workflow grant ACL (strict-deny default), never inline in the automation
   definition. Plaintext is materialized only inside the sandboxed run.
7. Audited: every run, its command, output, exit code, and the acting admin are
   recorded in the durable journal.
8. AI-builder fence stays: the codegen import allowlist still bans `os/exec` in
   workflow Go. A command step is DECLARATIVE data the AI (or an operator) emits;
   the daemon runs it through the gated path. The AI cannot smuggle raw exec,
   and any command step it produces is still subject to every gate above.

### 4. WebAuthn / step-up auth (the foundation)

Reactor currently has no MFA (argon2id sessions plus API tokens only). Before
command execution ships, add:

- WebAuthn passkeys (platform authenticators / hardware keys) for phishing-
  resistant MFA.
- TOTP plus one-time recovery codes as the fallback MFA path.
- A step-up "sudo mode": a short-lived elevated-auth window minted by a fresh
  passkey / TOTP assertion, required for the dangerous actions (enable command
  steps, run a command, reveal / rotate a secret). This closes Reactor's MFA
  gap generally, not just for this feature.

## Threat model

| Threat | Mitigation |
|---|---|
| Non-admin triggers a command -> RCE | admin-only + step-up; members cannot reach it |
| Tenant attacks shared host in multi-tenant hosting | hard-disabled in hosted mode; single-tenant self-host only |
| AI-authored workflow smuggles `os/exec` | import allowlist bans it; command step is declarative, daemon-run, gated |
| Uploaded workflow tarball includes a command step | same gates; runs only if enabled + admin + step-up |
| Stolen session cookie runs a command | step-up passkey required; cookie alone is insufficient |
| Command exfiltrates a secret | vault grant ACL (strict-deny); creds only in the sandboxed run; output redaction |
| Runaway / destructive command | container sandbox (no host mount), resource limits, timeout, reaper, audit |
| Secret hardcoded in the automation definition | credentials are vault references, never inline; secret scanning on the repo |

## Phasing

- Phase 1: WebAuthn passkeys + TOTP + step-up sudo mode (auth foundation; also
  fixes the general MFA gap).
- Phase 2: Command Automation store + visual DAG + git versioning (safe, no exec).
- Phase 3: Sandboxed command runner (container) behind the flag + all gates.
- Phase 4: SSH step + external-host-agent delegation option.

## Open questions

- Execution backend for v1: daemon-local container vs. external-host-agent
  delegation vs. both.
- Sandbox technology: a locked `docker run --rm` profile initially; a stronger
  isolation layer (microVM) if untrusted execution is ever hosted.
