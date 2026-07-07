#!/bin/sh
# Pre-remove script for reactor .deb / .rpm.
# Stops the daemon if it's running. Leaves the state dir + user behind
# so an apt purge can be re-installed without losing the master key
# (purge with apt purge --auto-remove + manually rm -rf if you really
# mean it).
set -eu

if command -v systemctl >/dev/null 2>&1; then
    systemctl stop reactor 2>/dev/null || true
    systemctl disable reactor 2>/dev/null || true
fi
