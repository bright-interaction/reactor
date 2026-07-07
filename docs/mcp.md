# MCP server + tool reference

Reactor ships an MCP (Model Context Protocol) server so AI clients can introspect the daemon's state and (with `--allow-write`) make changes. Two transports are supported: stdio for local subprocess clients (Claude Code, Claude Desktop, Cursor, Continue.dev) and Streamable HTTP for remote clients (cloud-hosted Claude, web IDEs).

## Transports

### stdio

```sh
reactor mcp stdio
```

The daemon reads JSON-RPC requests on stdin and writes responses to stdout. Standard MCP framing per protocol version `2024-11-05`. The `reactor mcp install` subcommand registers this transport with every supported client in one step:

```sh
reactor mcp install --client claude-code
reactor mcp install --client claude-desktop
reactor mcp install --client cursor
reactor mcp install --client continue-dev
reactor mcp install --client all
```

### Streamable HTTP

Mounted at `POST /mcp` on the dashboard server. Same BasicAuth + RateLimit middleware applies; CSRF is exempt by design (the request body carries the auth contract).

```sh
curl -X POST https://reactor.example.com/mcp \
  -H "Authorization: Bearer rtr_..." \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc": "2.0", "id": 1, "method": "initialize"}'
```

Batch requests are accepted (array of requests in one POST). Notifications (no `id` field) return HTTP `202` with no body. Parse errors return a JSON-RPC error envelope, not an HTTP error code.

## Server identity

`tools/initialize` returns:

```json
{
  "protocolVersion": "2024-11-05",
  "serverInfo": { "name": "reactor", "version": "<release-tag>" },
  "capabilities": { "tools": {} }
}
```

## Tool catalogue

20 tools total. Read tools are always available; write tools require `--allow-write` on the `reactor mcp stdio` invocation (the HTTP transport mirrors the same flag from the daemon start).

### Read tools (always available)

#### `reactor_list_workflows`

List every registered workflow with id, slug, sdk_version, and timestamps.

**Input:** `{}` (no parameters)

**Returns:** array of workflow rows.

#### `reactor_list_runs`

List recent runs newest-first.

**Input:**

```json
{
  "limit": 50,
  "workflow_id": "wf_x",
  "status": "succeeded"
}
```

#### `reactor_get_run`

Get one run's metadata + step timeline.

**Input:** `{"run_id": "run_..."}`

**Returns:** run info + steps array (name, attempt, status, duration_ms, output JSONB, error_text).

#### `reactor_get_run_logs`

Get a run's persisted log lines (the dispatcher + workflow log tail), so an AI can debug a run it triggered even after the run finished.

**Input:** `{"run_id": "run_..."}`

**Returns:** `{"run_id": "run_...", "lines": ["...", "..."]}` (empty when the run produced no logs).

#### `reactor_get_analytics`

Get the run-analytics rollup (counts by terminal status, total + succeeded runs, avg + p95 duration, per-workflow stats) -- the same numbers the dashboard home shows.

**Input:** `{}`

#### `reactor_list_dead_letters`

List dead-letter items (runs whose final attempt failed), newest-first, so an AI can find what needs a retry.

**Input:** `{"limit": 50, "offset": 0}` (both optional)

#### `reactor_list_notification_channels`

List configured notification channels.

**Input:** `{}`

**Returns:** array of `{id, name, kind, created_at}`. The channel `config_json` is intentionally NOT returned because it can hold secrets (SMTP password, webhook auth header).

#### `reactor_list_credentials`

List every credential with rotation state. **Values never leave the daemon** (the `value` field is intentionally omitted from the response).

**Input:** `{}`

**Returns:** array with id, name, service, provider, auto_rotate, rotation_interval_days, last_rotated_at, last_rotation_error, granted_workflows.

#### `reactor_get_credential_audit`

Read the append-only audit log for one credential.

**Input:**

```json
{
  "credential_id": "cred_...",
  "limit": 50
}
```

**Returns:** array of audit entries (at, action, actor_kind, actor_id, detail).

#### `reactor_search_knowledge`

BM25 search across the knowledge corpus. Use before generating workflow code so prior lessons are honoured.

**Input:**

```json
{
  "query": "step idempotency keys",
  "limit": 5
}
```

**Returns:** top-N entries with id, topic, title, body excerpt.

#### `reactor_query_graph`

Query the runtime graph (workflows, credentials, triggers, recent runs, knowledge) by free-text. Returns a subgraph in one call instead of forcing 5+ list/get round-trips.

**Input:**

```json
{
  "query": "what fires the daily-report workflow",
  "limit": 20
}
```

#### `reactor_get_neighbors`

Walk the graph from a node outward by depth steps.

**Input:**

```json
{
  "node_id": "wf_demo",
  "depth": 2,
  "edge_kinds": ["USES", "FIRES"]
}
```

Edge kinds: `USES`, `FIRES`, `BELONGS_TO`, `FROM`, `DERIVED_FROM`, `CITED_BY`, `SUPERSEDES`.

### Write tools (require `--allow-write`)

Every write tool emits a row in the audit log keyed by `actor_kind="mcp"`.

#### `reactor_register_workflow`

Register a workflow row. Idempotent: re-registering an existing slug returns its current id.

**Input:**

```json
{
  "slug": "hourly-report",
  "sdk_version": "0.1.0",
  "code_hash": "sha256:abc..."
}
```

**Returns:** `{"workflow_id": "wf_..."}`.

#### `reactor_grant_secret`

Grant a workflow read-access to a credential. Idempotent.

**Input:**

```json
{
  "workflow_id": "wf_demo",
  "credential_id": "cred_stripe-api-key"
}
```

#### `reactor_revoke_secret`

Inverse of grant. Errors if no grant exists.

**Input:** same shape as grant.

#### `reactor_dispatch_workflow`

Trigger a workflow run with an operator-supplied JSON payload. Returns the new run_id.

**Input:**

```json
{
  "workflow_id": "wf_demo",
  "payload": { "any": "JSON" }
}
```

**Returns:** `{"run_id": "run_..."}`.

#### `reactor_cancel_run`

Stop a run. A suspended run is cancelled immediately; a running run is flagged and the daemon kills its subprocess within ~2s.

**Input:** `{"run_id": "run_..."}`

**Returns:** `{"run_id": "run_...", "outcome": "cancelled" | "requested" | "not_cancellable"}`.

#### `reactor_add_knowledge`

Append a new entry to the corpus. Body is scanned for PII / secrets and rejected on hit.

**Input:**

```json
{
  "topic": "patterns",
  "title": "Idempotency keys must hash the payload",
  "tags": ["retry"],
  "body": "...markdown..."
}
```

#### `reactor_revise_knowledge`

Supersede an existing entry with new body. The old entry stays on disk; the supersedes chain links them.

**Input:**

```json
{
  "old_id": "k_abc",
  "title": "...",
  "body": "...",
  "reason": "fixed example"
}
```

#### `reactor_record_postmortem`

Append a post-mortem for a failed run. The dispatcher auto-fires this for `failed_dlq` runs when an Anthropic API key is configured; operators can also call it directly for an old run.

**Input:** `{"run_id": "run_..."}`

## Recommended client posture

- **Read-only clients** (Claude Code in chat mode, web IDEs reading state) should start the daemon without `--allow-write` so an LLM cannot accidentally dispatch a production workflow.
- **Operator clients** (a human iterating on workflows with Claude Code) should run with `--allow-write` so the AI can iterate without round-tripping through the dashboard.
- **CI clients** should prefer the REST API ([API reference](/docs/api)) over MCP unless the workflow specifically benefits from the graph + knowledge tools.

## Initialisation example (Streamable HTTP)

```sh
curl -sS -X POST https://reactor.example.com/mcp \
  -H "Authorization: Bearer rtr_..." \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc": "2.0",
    "id": 1,
    "method": "initialize",
    "params": {
      "protocolVersion": "2024-11-05",
      "capabilities": {},
      "clientInfo": {"name": "ops-script", "version": "1.0"}
    }
  }'
```

Then `tools/list`, then call any of the tools above with `tools/call`.

## Permission model

The MCP server inherits the calling user's role from the auth middleware:

- **Member calling a write tool that maps to an admin-only dashboard endpoint** (e.g. an MCP-equivalent of workflow delete, were one to exist) gets the same 403 path. Today none of the write tools cross that line, but the RBAC plumbing is in place.
- **Vault grants** are still required on every `reactor_dispatch_workflow` invocation: the dispatched run's supervisor enforces grants regardless of who triggered it.
- **No tool returns credential values.** `reactor_list_credentials` strips them; `secret_fetch` happens through the supervisor pipe only.
