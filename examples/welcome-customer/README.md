# welcome-customer

The full-surface Reactor demo: one workflow exercising every primitive
the SDK exposes.

| Step | Primitive |
|------|-----------|
| `fetch-customer` | `Step` + `ExpBackoff` retry + `IdempotencyKey` + `Timeout` + `vault.MustGet` + `reactor.Permanent` |
| `send-welcome`   | `Step` + `vault.MustGet` + `flow.SignalToken(name)` to embed an approval URL |
| `manager-approve` | `flow.AwaitSignal(name, 7*24*time.Hour)` -> supervisor suspend -> scheduler resume |
| `record-approval` | `Step` + decode of the signal payload + `reactor.Permanent` rejection |
| `wait-3-days`     | `flow.Sleep(72h)` -> long-suspend / scheduler resume |
| `send-nudge`      | final `Step` + `vault.MustGet` |

## Run it

```bash
# 1. Bootstrap the demo state dir + DB.
make demo

# 2. Start the daemon (separate terminal).
bin/reactor serve --root /tmp/reactor-demo --db sqlite:///tmp/reactor-demo/reactor.db

# 3. Open the dashboard.
open http://127.0.0.1:7777/
```

The make demo target builds + registers both `cron-echo` and
`welcome-customer`; you'll see them both on the home page with
"deployed" badges. Fire `welcome-customer` by seeding a webhook trigger
and POSTing the trigger URL with `{"customer_id":"cus_demo"}`.

When the workflow reaches `manager-approve`, the run shows up at
`/runs/<id>` with status `suspended`. The daemon log prints the
`approve_url` (a `POST /signal/<token>` URL); hit that with
`{"approved":true,"approver":"alice"}` to resume the run. The next
suspension is the 72-hour `Sleep`, which you can fast-forward by
editing the schedules row's `wake_at` column.

Pass `{"customer_id":"cus_invalid"}` to demonstrate the dead-letter
path: the `fetch-customer` step returns `reactor.Permanent` and the
run lands in DLQ visible at `/runs` (status `failed_dlq`) and via
`reactor dlq list --db ...`. Retry with
`reactor dlq retry <dlq-id> --root /tmp/reactor-demo`.
