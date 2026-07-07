# Operations

## Install paths

| Path             | When                                                    |
| ---------------- | ------------------------------------------------------- |
| `brew install`   | macOS workstation development                            |
| `.deb` package   | Debian / Ubuntu servers (systemd-managed)               |
| `.rpm` package   | RHEL / Fedora servers (systemd-managed)                 |
| `docker run`     | Container-first deployments + ephemeral CI              |
| `docker-compose` | The fastest path to a self-hosted instance              |
| systemd unit     | Manual bare-metal install (built from source)           |

Every package + image is built from the same `v*` tag by
`.github/workflows/release.yml` and signed via the GitHub Actions
sigstore signer (when the release CI runs on a repo with the relevant
secrets).

See `deploy/README.md` for the Docker + systemd walkthroughs and
`packaging/` for the Homebrew formula template + nfpm config.

## First boot

```bash
reactor init --root /var/lib/reactor
reactor migrate --db sqlite:///var/lib/reactor/reactor.db
# Add an admin via:
echo -n 'changeme' | sha256sum | awk '{print $1}'  # paste into REACTOR_BASIC_AUTH_PASSWORD_SHA256
reactor serve --root /var/lib/reactor --db sqlite:///var/lib/reactor/reactor.db
```

## Backup

The state dir is the entire backup target:

```bash
tar -czf reactor-$(date +%F).tar.gz /var/lib/reactor
```

Without `master.key` the database is unreadable; restore both or
neither. The vault re-encrypts on master-key rotation so an operator
can rotate the master key while the daemon is running without losing
data.

## Monitoring

`/metrics` is Prometheus text format. Scrape with basic auth:

```yaml
scrape_configs:
  - job_name: reactor
    metrics_path: /metrics
    basic_auth:
      username: admin
      password: changeme
    static_configs:
      - targets: ["127.0.0.1:7777"]
```

Counters: `reactor_runs_{started,succeeded,failed,dlq}_total`,
`_rotations_{run,error}_total`, `_mcp_calls_total`,
`_webhook_calls_total`. Gauges: `_uptime_seconds`, `_goroutines`,
`_memory_alloc_bytes`, `_memory_sys_bytes`, `_memory_gc_cycles_total`.

`/healthz` is unauthenticated (liveness probes) and returns
`{"ok":true,"version":"v0.1.0"}`.

The dashboard's `/runs` page is the human equivalent; `/audit`
aggregates credential audits for incident review.

## Scaling

v0.1 is single-tenant single-node. Scale-out path:

1. Move `REACTOR_DB_URL` to Postgres. Migrations are engine-aware; LISTEN/
   NOTIFY for cron live-reload activates automatically.
2. Run multiple `reactor serve` instances behind a load balancer. The
   webhook + signal receivers are stateless; the runs table is the
   shared coordination point.
3. The scheduler currently runs in every replica with optimistic row
   leasing via `FOR UPDATE SKIP LOCKED` on Postgres. (SQLite
   single-writer makes multi-replica deployment a Postgres-only
   topology.)
4. Workflow binaries live under `<root>/workflows/<slug>/workflow`;
   shared NFS or a CI-built artifact pulled to each replica's local
   disk both work.

Multi-tenant request path is v0.3 scope. Schema is ready
(`tenant_id` on every table from migration 1), wiring is not.

## Upgrade

Tag-to-tag upgrades are safe within v0.x:

```bash
# Stop the daemon (systemd: systemctl stop reactor)
apt install ./reactor_NEW_VERSION_amd64.deb
reactor migrate --db sqlite:///var/lib/reactor/reactor.db  # idempotent
# Restart
systemctl start reactor
```

The migration set is forward-only; downgrades require a manual schema
fix (see the relevant migration's Down block).

## Disaster recovery

Lost master key + no BIP39 recovery = total credential data loss. The
workflows + run history survive (those are plaintext); only the vault
blob is encrypted-at-rest. Recovery options:

- Restore from backup (the tar from above).
- Re-mint every credential via the rotation engine + grant chain;
  workflows resume with new credentials, no schema changes needed.
