# Notifications: Slack + generic webhook + email

Failed runs no longer land silently in `/runs`. Every workflow can route terminal status alerts to a Slack incoming webhook, a generic JSON webhook, or an SMTP email address, opt-in per workflow with per-route status filters.

## Quick start

1. Open `/notifications` in the dashboard.
2. Click **Add channel**, pick a kind, fill in the per-kind fields.
3. Click **Send test** on the new channel row to verify config without waiting for a real failure.
4. Open any `/workflows/{slug}` page and use the **Attach channel** form in the Notifications section.

The default route fires on `failed,failed_dlq`. An operator can broaden to `failed,failed_dlq,succeeded` for positive confirmations or narrow to `failed_dlq` only.

## Schema

Two tables added by migration 0011.

### `notification_channels`

| Column | Type | Notes |
|---|---|---|
| id | text PK | `nch_<hex>` |
| tenant_id | text NOT NULL | owning tenant, defaults to `default` |
| name | text | operator-chosen, UNIQUE **per tenant** |
| kind | text CHECK | `slack_webhook` \| `generic_webhook` \| `email_smtp` |
| config_json | text/jsonb | per-kind shape (see below) |
| created_at / updated_at | timestamp | |

`UNIQUE (tenant_id, name)`, so two tenants may each own a channel called
`ops-slack`. A duplicate inside one tenant returns `journal.ErrChannelNameTaken`,
which the dashboard renders as "you already have a channel named ...".

Reads are scoped accordingly. `/notifications` is admin-gated and admin is a
global role, so that page intentionally lists every channel in the install. The
member-facing workflow detail page uses `ListNotificationChannelsByTenant` for
its attach-channel picker, scoped to the workflow's owning tenant.

### `workflow_notification_routes`

| Column | Type | Notes |
|---|---|---|
| workflow_id | text FK CASCADE | |
| channel_id | text FK CASCADE | |
| on_statuses | text | comma-separated CSV; normalised on write (trim + lowercase + dedupe + sort) |
| created_at | timestamp | |

Primary key on `(workflow_id, channel_id)` so re-attaching the same pair upserts the status set.

## Channel kinds

### `slack_webhook`

```json
{ "url": "https://hooks.slack.com/services/T0/B0/abc123" }
```

The dashboard form validates that the URL starts with `https://hooks.slack.com/`.

Sender emits a Block Kit message:

- Header block: `[Reactor FAIL] demo-workflow: failed_dlq`
- Section block with markdown body (workflow, run id, trigger, status, started, duration, error)
- Optional "Open run" action button when `REACTOR_DASHBOARD_URL` is set

### `generic_webhook`

```json
{
  "url": "https://example.com/reactor",
  "headers": { "X-Auth-Token": "secret" }
}
```

Sender POSTs the full event with snake_case JSON tags:

```json
{
  "run_id": "run_5b06a401ff4bd96d",
  "workflow_id": "wf_demo",
  "workflow_slug": "demo-workflow",
  "status": "failed_dlq",
  "trigger_kind": "webhook",
  "started_at": "2026-05-31T19:14:07.172Z",
  "finished_at": "2026-05-31T19:14:08.012Z",
  "error_text": "step 'charge_card' returned 500",
  "dashboard_url": "https://reactor.example.com/runs/run_5b06a401ff4bd96d"
}
```

`Content-Type: application/json`. `User-Agent: reactor-notifier/1.0`. Per-route auth headers are forwarded verbatim.

### `email_smtp`

```json
{
  "host": "smtp.gmail.com",
  "port": 587,
  "username": "alerts@example.com",
  "password": "...",
  "from": "alerts@example.com",
  "to": "ops@example.com, oncall@example.com",
  "starttls": true
}
```

`port` defaults to 587; `starttls` defaults to true on 587. The `to` field accepts comma-separated recipients. PLAIN auth.

The email body is plain text mirroring the Slack message's content.

> **Keep the password out of `config_json`.** Instead of a plaintext `password`, set `"password_credential_id": "cred_..."` to reference a vault credential. The notifier resolves it from the vault at send time, so the secret never sits in the channel row at rest. The create form exposes both: leave the plaintext field blank and fill the credential-id field. The generic webhook channel takes the same treatment for an auth header via `header_name` + `header_credential_id`. A channel that references a credential the vault can't resolve fails the send closed (it is not sent with a blank secret).

## Fire path

Every workflow run that lands in a terminal status (`failed`, `failed_dlq`, `succeeded`) drives the dispatcher's `OnTerminal` callback. The callback calls `notifier.Notify(ctx, event)`. The notifier:

1. Looks up routed channels via `Journal.ChannelsForRunTerminal(workflow_id, status)` (one SQL query, returns matching channels with config already loaded).
2. Fans out to every channel in parallel under a per-send timeout (5 seconds default).
3. Logs sender failures at WARN. The dispatcher's terminal path is never blocked by a Slack outage.

`suspended` runs do not fire notifications. Only terminal statuses do.

## Operator surfaces

### `POST /notifications/{id}/test`

Fires a synthetic alert via the channel:

```json
{
  "run_id": "test_20260601T191407Z",
  "workflow_slug": "(test channel)",
  "status": "test",
  "error_text": "Test message from Reactor. Channel is wired correctly."
}
```

Operators run this immediately after creating a channel so they catch misconfiguration before a real failure surfaces.

### `POST /notifications/{id}/delete`

Refuses with `409 Conflict` when any workflow route still references the channel. Error message: `notification channel has active routes: N workflow(s) routed; detach the per-workflow routes first`.

### Per-workflow attach

`POST /workflows/{slug}/notifications` with form fields `channel_id` and `on_statuses` (CSV). The form on `/workflows/{slug}` excludes already-attached channels from the dropdown so an operator does not accidentally double-route.

## Status CSV normalisation

The CSV is normalised on write: trimmed, lowercased, deduplicated, sorted. `"Failed, failed_dlq, FAILED"` becomes `"failed,failed_dlq"`. This means two callers with the same intended set always produce identical rows.

Valid status values: `succeeded`, `failed`, `failed_dlq`. Other values are accepted but never match the dispatcher's terminal event so they silently no-op.

## Dashboard URL for clickable links

`REACTOR_DASHBOARD_URL` (env var) is the base URL the notifier uses to render "Open run" buttons in Slack messages and `dashboard_url` in webhook payloads. When unset, defaults to `http://<listen-addr>`. Operators behind a reverse proxy should set this explicitly.

## Cascade on workflow delete

The `DeleteWorkflow` transaction explicitly drops `workflow_notification_routes` rows for the workflow. Channel rows are untouched (a channel can route many workflows; deleting one should not orphan the others). Channels are FK-constrained so cascading still works if the explicit DELETE is bypassed.

## Tests

- Journal: channel CRUD, upsert normalisation, status filter, delete-in-use rejection, cascade-on-workflow-delete, empty-statuses rejection.
- Notifier: fanout, swallowed sender errors, unregistered-kind skip, `TestChannel` delivery, Slack + webhook POST round-trip, non-2xx surfacing, empty-URL rejection, per-send timeout honoured.
- Server: page renders, bad Slack URL 422, happy path 303, test button 503 when notifier nil, per-workflow attach + list + detach round-trip.
- Dispatcher integration: drives the OnTerminal closure with a synthetic TerminalEvent and asserts the receiver got the right JSON.
