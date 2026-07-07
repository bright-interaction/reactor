# Deploying Reactor

Two paths covered: a Docker image (recommended for fresh installs) and
a systemd unit (recommended for an existing Linux host).

## Docker

```bash
# Single-arch (host CPU):
docker build -t reactor:latest .

# Multi-arch via buildx (linux/amd64 + linux/arm64). Required if you
# build on Apple Silicon / ARM dev box but deploy to x86 servers,
# or vice versa. --push uploads each platform variant to your registry.
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -t registry.example.com/reactor:latest --push .

# First boot: init + migrate inside the volume.
docker run --rm \
  -v reactor-state:/var/lib/reactor \
  reactor:latest init --root /var/lib/reactor

docker run --rm \
  -v reactor-state:/var/lib/reactor \
  reactor:latest migrate --db sqlite:///var/lib/reactor/reactor.db

# Steady state: run the daemon. Set REACTOR_BASIC_AUTH_USER +
# REACTOR_BASIC_AUTH_PASSWORD_SHA256 so the status pages aren't open.
docker run -d --name reactor \
  -p 127.0.0.1:7777:7777 \
  -v reactor-state:/var/lib/reactor \
  -e REACTOR_DB_URL=sqlite:///var/lib/reactor/reactor.db \
  -e REACTOR_BASIC_AUTH_USER=admin \
  -e REACTOR_BASIC_AUTH_PASSWORD_SHA256=$(printf '%s' 'changeme' | sha256sum | awk '{print $1}') \
  reactor:latest

curl -u admin:changeme http://127.0.0.1:7777/
```

For HTTPS, mount a cert + key and pass the flags:

```bash
docker run -d --name reactor \
  -p 7777:7777 \
  -v reactor-state:/var/lib/reactor \
  -v /etc/letsencrypt/live/reactor.example.com:/etc/tls:ro \
  -e REACTOR_DB_URL=sqlite:///var/lib/reactor/reactor.db \
  reactor:latest serve \
    --root /var/lib/reactor \
    --tls-cert /etc/tls/fullchain.pem \
    --tls-key  /etc/tls/privkey.pem
```

## systemd

```bash
# 1. Install the binary.
sudo install -m 0755 bin/reactor /usr/local/bin/reactor

# 2. Create the system user + state dir.
sudo useradd --system --no-create-home --shell /usr/sbin/nologin \
  --home-dir /var/lib/reactor reactor
sudo install -d -m 0700 -o reactor -g reactor /var/lib/reactor

# 3. Run the setup wizard (interactive) OR pass --non-interactive for CI.
sudo -u reactor /usr/local/bin/reactor setup \
  --root /var/lib/reactor \
  --non-interactive --admin-user admin --admin-password "$ADMIN_PASSWORD"
# Setup writes /var/lib/reactor/reactor.env (mode 0600, owned by reactor)
# containing REACTOR_DB_URL + REACTOR_BASIC_AUTH_USER + REACTOR_BASIC_AUTH_PASSWORD_SHA256.

# 4. (Optional) Add rate-limit overrides to the env file.
sudo tee -a /var/lib/reactor/reactor.env >/dev/null <<EOF
export REACTOR_RATE_BURST=120
export REACTOR_RATE_REFILL=20
EOF

# 5. Install the unit (the unit references /var/lib/reactor/reactor.env).
sudo install -m 0644 deploy/reactor.service /etc/systemd/system/reactor.service
sudo systemctl daemon-reload
sudo systemctl enable --now reactor

# 6. Verify.
sudo systemctl status reactor
curl -u admin:"$ADMIN_PASSWORD" http://127.0.0.1:7777/healthz
```

The pre-setup-wizard manual path (separate `reactor init` + `reactor
migrate` + hand-rolled `sha256sum` for the password hash) still works;
the wizard is just a one-command shortcut.

## Behind a reverse proxy

When fronted by Caddy / Nginx / Traefik that already terminates TLS,
omit the `--tls-cert` / `--tls-key` flags. The proxy needs to set
`X-Forwarded-Proto: https` so Reactor emits HSTS on responses; the
trusted-proxy logic in `internal/server/middleware.go` accepts the
header from loopback / RFC1918 ranges only.

Tighten `REACTOR_RATE_BURST` + `REACTOR_RATE_REFILL` against the source-IP
distribution your proxy passes in `X-Forwarded-For`.

## Backups

The state dir is the entire backup target:

- `master.key` (mode 0600). Without this no credentials can be decrypted.
- `reactor.db` (sqlite) or your Postgres dump.
- `workflows/` (built workflow binaries; can be rebuilt from source).

A nightly `tar -czf reactor-$(date +%F).tar.gz /var/lib/reactor`
plus offsite copy is enough for v1.
