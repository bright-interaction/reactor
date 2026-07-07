#!/bin/sh
# Post-install script for reactor .deb / .rpm.
#
# Creates a system user + state directory, then leaves the daemon
# disabled so the operator runs `reactor init` first to mint a master
# key + then `systemctl enable --now reactor`. We deliberately do NOT
# auto-enable because the daemon refuses to start without a master key
# (refusing to ship with a default one is a deliberate security choice).
set -eu

if ! getent passwd reactor >/dev/null 2>&1; then
    useradd --system --no-create-home --shell /usr/sbin/nologin \
        --home-dir /var/lib/reactor reactor
fi

install -d -m 0700 -o reactor -g reactor /var/lib/reactor
install -d -m 0750 -o reactor -g reactor /etc/reactor

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
fi

cat <<EOF

Reactor installed. Next steps:

  sudo -u reactor reactor init --root /var/lib/reactor
  sudo -u reactor reactor migrate --db sqlite:///var/lib/reactor/reactor.db

Then add credentials to /etc/reactor/reactor.env (REACTOR_BASIC_AUTH_USER +
REACTOR_BASIC_AUTH_PASSWORD_SHA256 at minimum) and:

  sudo systemctl enable --now reactor

See /usr/share/doc/reactor/DEPLOY.md for the full walkthrough.
EOF
