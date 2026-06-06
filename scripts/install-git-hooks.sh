#!/usr/bin/env bash
# scripts/install-git-hooks.sh — opt-in installer for the
# repo-tracked git hooks under `.githooks/`.
#
# WHAT IT DOES
# ------------
# Points `git config core.hooksPath` at the in-repo `.githooks/`
# directory so any executable file there is picked up as a hook
# on the next commit/push. We don't enable this automatically
# (e.g. via a postinstall in package.json) for two reasons:
#
#   1. The repo is a multi-language monorepo (Go server, React
#      web, RN android, miniapp). A backend-only contributor
#      doesn't need the JS hooks fired on their commit and may
#      not even have Node installed.
#   2. Modifying core.hooksPath is a per-clone toggle — the right
#      place to do it is an explicit, named script the contributor
#      runs once after cloning.
#
# Safe to re-run: idempotent.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HOOKS_DIR="${ROOT_DIR}/.githooks"

if [ ! -d "${HOOKS_DIR}" ]; then
  printf 'install-git-hooks: %s does not exist\n' "${HOOKS_DIR}" >&2
  exit 1
fi

# Ensure every checked-in hook is executable. Git silently ignores
# non-executable hook files, which would mask a typo in the file
# permissions and let bad commits slip through unchallenged.
chmod -R +x "${HOOKS_DIR}"

current="$(git -C "${ROOT_DIR}" config --get core.hooksPath || true)"
target=".githooks"

if [ "${current}" = "${target}" ]; then
  printf 'install-git-hooks: core.hooksPath already %s — nothing to do.\n' "${target}"
  exit 0
fi

git -C "${ROOT_DIR}" config core.hooksPath "${target}"
printf 'install-git-hooks: core.hooksPath set to %s\n' "${target}"
printf 'install-git-hooks: hooks now active —\n'
ls -1 "${HOOKS_DIR}" | sed 's/^/  /'
printf 'To disable, run: git config --unset core.hooksPath\n'
