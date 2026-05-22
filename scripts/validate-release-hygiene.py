#!/usr/bin/env python3
"""Validate release and git hygiene invariants for CI/local verification."""

from __future__ import annotations

import json
import os
import re
import subprocess
import sys
from pathlib import Path


SEMVER_RE = re.compile(r"^v?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:[-+][0-9A-Za-z.-]+)?$")


def fail(message: str) -> None:
    raise SystemExit(message)


def read(path: Path) -> str:
    if not path.is_file():
        fail(f"missing required release hygiene file: {path}")
    return path.read_text(encoding="utf-8")


def check_version_metadata(root: Path) -> None:
    package_json = json.loads(read(root / "web/package.json"))
    web_version = str(package_json.get("version", ""))
    if not SEMVER_RE.match(web_version):
        fail(f"web/package.json version must be semver, got {web_version!r}")

    main_go = read(root / "server/cmd/server/main.go")
    required_main_patterns = [
        r'\bversion\s*=\s*"dev"',
        r'\bbuildTime\s*=\s*"unknown"',
        r'\bbuildCommit\s*=\s*"unknown"',
        r'"build_commit"\s*:\s*buildCommit',
    ]
    for pattern in required_main_patterns:
        if not re.search(pattern, main_go):
            fail(f"server version metadata missing pattern: {pattern}")

    dockerfile = read(root / "Dockerfile")
    required_docker_tokens = [
        "ARG BUILD_VERSION=dev",
        "ARG BUILD_COMMIT=unknown",
        "-X main.version=${BUILD_VERSION}",
        "-X main.buildTime=${BUILD_TIME}",
        "-X main.buildCommit=${BUILD_COMMIT}",
        "org.opencontainers.image.version",
        "org.opencontainers.image.revision",
    ]
    for token in required_docker_tokens:
        if token not in dockerfile:
            fail(f"Dockerfile release metadata missing token: {token}")

    compose = read(root / "docker-compose.yml")
    for token in ("BUILD_VERSION: ${BUILD_VERSION:-dev}", "BUILD_COMMIT: ${BUILD_COMMIT:-unknown}"):
        if token not in compose:
            fail(f"docker-compose build args missing token: {token}")


def check_git_hygiene_files(root: Path) -> None:
    gitignore = read(root / ".gitignore")
    for token in (".env", "!.env.example", "web/node_modules/", "web/dist/", "server/server"):
        if token not in gitignore:
            fail(f".gitignore missing required pattern: {token}")

    dockerignore = read(root / ".dockerignore")
    for token in (".git", ".github", ".env", "web/node_modules", "**/dist"):
        if token not in dockerignore:
            fail(f".dockerignore missing required pattern: {token}")

    gitattributes = read(root / ".gitattributes")
    for token in ("* text=auto eol=lf", "*.go text eol=lf", "*.png binary"):
        if token not in gitattributes:
            fail(f".gitattributes missing required rule: {token}")


def check_ci_release_paths(root: Path) -> None:
    workflow = read(root / ".github/workflows/ci.yml")
    required = [
        "branches: [main]",
        "tags: [\"v*\"]",
        "release-hygiene:",
        "python3 scripts/validate-release-hygiene.py",
        "BUILD_VERSION: ${{ github.ref_name }}",
        "BUILD_COMMIT: ${{ github.sha }}",
    ]
    for token in required:
        if token not in workflow:
            fail(f"CI workflow missing release hygiene token: {token}")


def check_abtest_shadow_schema(root: Path) -> None:
    migration = read(root / "server/migrations/022_abtest_shadow_variants.sql")
    required = [
        "CREATE TABLE IF NOT EXISTS ab_test_variants",
        "CREATE TABLE IF NOT EXISTS ab_test_variant_nav",
        "CREATE TABLE IF NOT EXISTS ab_test_variant_trades",
        "CREATE TABLE IF NOT EXISTS ab_test_decision_diffs",
        "CREATE TABLE IF NOT EXISTS ab_test_variant_memory",
        "CREATE TABLE IF NOT EXISTS ab_test_agent_learning_events",
        "CREATE TABLE IF NOT EXISTS ab_test_learning_promotions",
        "UNIQUE (test_id, variant_key)",
        "UNIQUE (variant_id, trading_date)",
        "UNIQUE (variant_id, agent_id, trading_date)",
        "idx_ab_test_variant_nav_test_date",
        "idx_ab_test_variant_trades_test_date",
        "idx_ab_test_agent_learning_events_agent",
    ]
    for token in required:
        if token not in migration:
            fail(f"A/B shadow schema migration missing token: {token}")


def check_strict_release_env(root: Path) -> None:
    release_version = os.environ.get("RELEASE_VERSION", "").strip()
    github_ref_type = os.environ.get("GITHUB_REF_TYPE", "").strip()
    github_ref_name = os.environ.get("GITHUB_REF_NAME", "").strip()
    strict = os.environ.get("RELEASE_CHECK_STRICT", "0") == "1" or github_ref_type == "tag"

    if release_version and not SEMVER_RE.match(release_version):
        fail(f"RELEASE_VERSION must be semver or v-prefixed semver, got {release_version!r}")
    if github_ref_type == "tag":
        if not SEMVER_RE.match(github_ref_name):
            fail(f"release tag must be semver-like vX.Y.Z, got {github_ref_name!r}")
        if release_version and release_version.lstrip("v") != github_ref_name.lstrip("v"):
            fail(f"RELEASE_VERSION {release_version!r} does not match git tag {github_ref_name!r}")
    if strict and not (release_version or github_ref_name):
        fail("strict release validation requires RELEASE_VERSION or a GitHub tag ref")

    enforce_clean_tree = strict and os.environ.get("CI", "").lower() == "true"
    if enforce_clean_tree and shutil_git_available(root):
        try:
            output = subprocess.check_output(["git", "-C", str(root), "status", "--porcelain", "-uall"], text=True)
        except subprocess.CalledProcessError:
            output = ""
        if output.strip():
            fail("strict release validation requires a clean working tree")


def shutil_git_available(root: Path) -> bool:
    try:
        subprocess.check_output(["git", "-C", str(root), "rev-parse", "--is-inside-work-tree"], stderr=subprocess.DEVNULL)
        return True
    except (OSError, subprocess.CalledProcessError):
        return False


def main() -> int:
    root = Path(sys.argv[1]) if len(sys.argv) > 1 else Path(__file__).resolve().parents[1]
    root = root.resolve()
    check_version_metadata(root)
    check_git_hygiene_files(root)
    check_ci_release_paths(root)
    check_abtest_shadow_schema(root)
    check_strict_release_env(root)
    print("release_hygiene=ok")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
