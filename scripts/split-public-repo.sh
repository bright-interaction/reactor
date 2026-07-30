#!/usr/bin/env bash
# Produce the public fair-code source-available mirror of Reactor at
# github.com/bright-interaction/reactor, so `go install
# github.com/bright-interaction/reactor/cmd/reactor@latest` resolves.
#
# Reactor is single-license (the Reactor Sustainable Use License, fair-code, like
# n8n's core): the whole engine ships in the mirror, there is no stripped pro
# layer. This script strips only internal-ONLY artifacts (the private security
# audit report) and redacts internal infra hostnames from all history, then
# secret-scans and build-checks the result before any push.
#
# Safe by default: with no --push it produces + checks the filtered tree and
# prints what it WOULD push. --push performs the outward mirror (requires the
# public repo to exist: gh repo create bright-interaction/reactor --public).
#
# Pattern (single-branch split-clone + gitleaks gate) mirrors mesh's
# scripts/split-public-repo.sh; see the Hive gotcha
# "mesh-mirror-split-clone-drags-in-monorepo-branch-secrets".
set -euo pipefail

PUSH=0
REMOTE_URL="git@github.com:bright-interaction/reactor.git"
PREFIX="reactor"
SPLIT_BRANCH="reactor-public-split"

# Internal-ONLY files (not code): stripped from the mirror's entire history.
# Paths are relative to reactor/ (the subtree split strips the prefix).
STRIP_PATHS=(
  # The private deep-audit reports: they name internal infra (host env-file
  # paths, the deploy cutover runbook, dockyard/sentinel/flare) and, worse,
  # findings that are still open. Kept private.
  #
  # Every new audit filename MUST be added here. filter-repo --path takes exact
  # paths, not globs, so nothing covers a new report automatically, and moving
  # it out of reactor/ on main does NOT help: git subtree split rebuilds the
  # payload from subtree HISTORY, so the commit that added it still carries it
  # into the public repo. Only a strip entry rewrites it out of every commit.
  # This exact hole was found open on pare on 2026-07-29, one queued publish
  # away from shipping four open findings on a live product.
  SECURITY-AUDIT-2026-07-07.md
  # Not on main yet: lives on agent/reactor-audit-fixes-2026-07-25 and records
  # 13 findings from the 2026-07-27 pass with NO fixes applied, including
  # cross-tenant leaks. Listed ahead of the merge so it can never publish.
  AUDIT-2026-07-27.md
)

for arg in "$@"; do
  case "$arg" in
    --push) PUSH=1 ;;
    --remote=*) REMOTE_URL="${arg#--remote=}" ;;
    -h|--help) echo "usage: $0 [--push] [--remote=git@github.com:org/repo.git]"; exit 0 ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

command -v git-filter-repo >/dev/null 2>&1 || {
  echo "error: git-filter-repo is required (pip install git-filter-repo)." >&2; exit 1; }

ROOT="$(git rev-parse --show-toplevel)"
cd "$ROOT"
[ -d "$PREFIX" ] || { echo "error: $PREFIX/ not found at $ROOT" >&2; exit 1; }

# Coarse pre-flight secret guard. Shared by every product mirror, in ONE file, so
# it cannot drift again. It did drift: five copies, five different regexes, one of
# which could not fire at all. See scripts/mirror-secret-preflight.sh for both bugs.
# This is the fast pre-check; the gitleaks scan on the filtered clone below is the
# authoritative gate.
# shellcheck source=../../scripts/mirror-secret-preflight.sh
. "$ROOT/scripts/mirror-secret-preflight.sh"
. "$ROOT/scripts/mirror-enterprise-check.sh"
# shellcheck source=../../scripts/mirror-module-path.sh
. "$ROOT/scripts/mirror-module-path.sh"
mirror_secret_preflight "$PREFIX" "$ROOT/$PREFIX/scripts/mirror-secret-allowlist.txt"

echo "Splitting $PREFIX/ subtree (history-preserving) into $SPLIT_BRANCH ..."
git branch -D "$SPLIT_BRANCH" >/dev/null 2>&1 || true
git subtree split --prefix="$PREFIX" -b "$SPLIT_BRANCH"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
CLONE="$WORK/reactor-public"
# --single-branch + --no-tags: the throwaway clone holds ONLY the disjoint
# reactor subtree history, never the monorepo's other branches (which carry
# unrelated project CI secrets). The clone == the publish payload, which makes
# the gitleaks scan below authoritative. file:// disables the hardlink path.
echo "Cloning $SPLIT_BRANCH -> $CLONE (single-branch) ..."
git clone --quiet --single-branch --no-tags --branch "$SPLIT_BRANCH" "file://$ROOT" "$CLONE"

if [ "${#STRIP_PATHS[@]}" -gt 0 ]; then
  FR_ARGS=(); for p in "${STRIP_PATHS[@]}"; do FR_ARGS+=(--path "$p"); done
  echo "Stripping internal-only paths from all history: ${STRIP_PATHS[*]}"
  ( cd "$CLONE" && git filter-repo --force --invert-paths "${FR_ARGS[@]}" )
fi

# Redact internal infra references from ALL history (file contents + commit
# messages). Distinctive tokens only, so a literal global replace is safe.
REDACT="$WORK/redactions.txt"
# Redactions come from ONE shared list plus this product's extras, because the
# per-product copies drifted: slab's never got the estate host IP or the internal
# service hostnames, so the production IP sat in its test fixtures labelled "prod
# host" and 98 occurrences of an internal SaaS hostname stayed in its history.
# shellcheck source=../../scripts/mirror-redactions.sh
. "$ROOT/scripts/mirror-redactions.sh"
mirror_redaction_file "$ROOT" "$ROOT/reactor/scripts/mirror-redactions.txt" "$REDACT"
echo "Redacting internal infra hostnames from all history ..."
( cd "$CLONE" && git filter-repo --force --replace-text "$REDACT" --replace-message "$REDACT" )

# Rewrite authors BEFORE asserting. The redaction check reads COMMIT objects too,
# so it sees the author field; running it first flagged identities that this very
# pass was about to fix. Assertions belong last, over the final state.
mirror_rewrite_authors "$CLONE"

# Assert the redaction actually took. Rewriting a token is a hope; checking it is
# gone is the guarantee, and this walks every blob in every commit because that is
# what the push publishes (mesh found names neutralised at HEAD still present in 61
# of 156 published commits).
mirror_redaction_check "$CLONE" "$ROOT" "$ROOT/reactor/scripts/mirror-redactions.txt"

# Blobs that text redaction cannot fix: a committed binary or archive, or a
# maintainer home path baked into build metadata. Nothing at HEAD reveals these.
mirror_blob_sanity_check "$CLONE" "$ROOT/reactor/scripts/mirror-blob-allowlist.txt"

# Refuse closed-source code in the mirror, at HEAD and in history. A no-op for
# products that ship no enterprise surface, which is most of them; wired in
# anyway so a product that GAINS one is covered from the first commit rather
# than after someone remembers to back-port the gate.
mirror_enterprise_check "$CLONE" || exit 1


# Defense in depth: fail if a stripped path survived.
for p in "${STRIP_PATHS[@]}"; do
  [ -e "$CLONE/$p" ] && { echo "REFUSING: stripped path '$p' still present." >&2; exit 1; }
done

echo "Build-checking the mirror ..."
# `cmd && echo OK` does NOT fail the script when cmd fails, even under `set -e`:
# bash exempts every command in an AND-OR list except the last, so a broken build
# printed nothing and the publish sailed on to the push. slab's mirror failed to
# link against DuckDB on musl and this gate said nothing at all; only the push
# being rejected revealed the run was unhealthy. Check the status explicitly.
if command -v go >/dev/null 2>&1; then
  if ( cd "$CLONE" && go build ./... ); then
    echo "  builds standalone: OK"
  else
    echo "REFUSING: the filtered mirror does not build standalone." >&2
    echo "  A public repo that cannot compile is not publishable. Either a stripped path" >&2
    echo "  is still referenced, or the build needs a toolchain this container lacks." >&2
    exit 1
  fi
  # RUN the tests, do not merely compile them. This used to be `go test -run='^$'`,
  # which compiles every _test.go and executes none, and that hole shipped a red
  # suite to the public: the publish redaction rewrites the maintainer's own address
  # (user@example.com ==> user@example.com), which silently inverted a Shield test that
  # asserts a PERSONAL domain is classified as personal, because example.com is a
  # business domain. Compiling proved nothing about it.
  #
  # The filtered tree is the artifact a contributor clones, and redaction, path
  # stripping and commit rewriting can all change its BEHAVIOUR, not just its text.
  # The only honest check is to run it.
  if ( cd "$CLONE" && go test ./... -count=1 >"$WORK/mirror-test.log" 2>&1 ); then
    echo "  tests PASS: OK"
  else
    echo "REFUSING: the filtered mirror FAILS its own tests. Publishing would hand" >&2
    echo "          contributors a red suite. This usually means a publish transform" >&2
    echo "          (redaction, path strip) changed behaviour, not just text." >&2
    grep -E '^(--- FAIL|FAIL|\s+.*_test\.go:)' "$WORK/mirror-test.log" | head -20 >&2
    exit 1
  fi
else
  echo "  (go not found; skipping build check)" >&2
fi

# Authoritative secret scan: the single-branch clone IS the publish payload.
echo "Checking the published module path resolves to this repo ..."
mirror_assert_module_path "$CLONE" "$REMOTE_URL"

if command -v gitleaks >/dev/null 2>&1; then
  echo "Scanning mirror history for secrets (gitleaks) ..."
  if ! ( cd "$CLONE" && gitleaks detect --source . --config .gitleaks.toml --no-banner --redact >/dev/null 2>&1 ); then
    echo "REFUSING: gitleaks found a secret in the mirror history:" >&2
    ( cd "$CLONE" && gitleaks detect --source . --config .gitleaks.toml --no-banner --redact ) >&2 || true
    exit 1
  fi
  echo "  no secrets in mirror history: OK"
else
  echo "  WARNING: gitleaks not installed; the secret-scan gate is SKIPPED." >&2
  echo "  Install it before pushing: brew install gitleaks" >&2
  [ "$PUSH" -eq 1 ] && { echo "REFUSING to --push without the gitleaks gate." >&2; exit 1; }
fi

if [ "$PUSH" -eq 0 ]; then
  echo; echo "DRY RUN. Filtered mirror ready at: $CLONE"
  echo "Would push its HEAD -> $REMOTE_URL main"
  echo "Re-run with --push once the public repo exists (gh repo create bright-interaction/reactor --public)."
  trap - EXIT  # keep $WORK so the operator can inspect the dry-run tree
  exit 0
fi

echo "Pushing filtered mirror -> $REMOTE_URL main ..."
# Force-with-lease, because mirror_rewrite_authors and the redaction passes rewrite
# EVERY commit id, so the mirror can never fast-forward over what is published.
# The lease pins the overwrite to the head we actually observed, so a commit
# landed directly on the public repo aborts the push instead of vanishing.
# shellcheck source=../../scripts/mirror-push.sh
. "$ROOT/scripts/mirror-push.sh"
mirror_force_publish "$CLONE" "$REMOTE_URL"
echo "Done. Cleanup: git branch -D $SPLIT_BRANCH"
