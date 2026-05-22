#!/usr/bin/env python3
"""F31 contract tests for the migration roundtrip drill.

Two categories of test live here:

1. Script preconditions — verifies the drill exits with documented codes
   when prerequisites aren't met. Same pattern as F30's backup script
   tests; lets CI catch packaging/refactor regressions without needing
   a live postgres.

2. Down-migration coverage — verifies that every recently added .sql
   migration ships with a matching .down.sql. The drill window is
   defined as the last ROUNDTRIP_DEPTH=4 migrations; this test enforces
   the convention so adding migration #030 without a #030 down would
   fail CI before it ever reaches a deployed environment.

Run with:  python3 scripts/test_migration_roundtrip.py
"""

from __future__ import annotations

import os
import re
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

SCRIPTS_DIR = Path(__file__).resolve().parent
REPO_ROOT = SCRIPTS_DIR.parent
DRILL = SCRIPTS_DIR / "migration_roundtrip_drill.sh"
MIGRATIONS_DIR = REPO_ROOT / "server" / "migrations"
DEFAULT_DEPTH = 4

UP_RE = re.compile(r"^(\d+)_.+\.sql$")
DOWN_SUFFIX = ".down.sql"


def run_drill(env_extra: dict[str, str] | None = None, args: list[str] | None = None) -> subprocess.CompletedProcess:
    env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin")}
    if env_extra:
        env.update(env_extra)
    return subprocess.run(
        ["bash", str(DRILL), *(args or [])],
        env=env,
        capture_output=True,
        text=True,
        timeout=15,
    )


def list_up_migrations(directory: Path) -> list[str]:
    """Return up migrations sorted by their numeric prefix (matches the
    bash script's sort order)."""
    out = []
    for name in sorted(os.listdir(directory)):
        if name.endswith(DOWN_SUFFIX):
            continue
        if UP_RE.match(name):
            out.append(name)
    return out


class DrillPreconditions(unittest.TestCase):
    """Validates fail-fast behaviour when the operator misconfigures
    the drill. Mirrors the patterns in F30's test suite."""

    def test_missing_db_url_exits_2(self) -> None:
        # No ROUNDTRIP_DB_URL → exit 2. Otherwise the script would try
        # to psql against an empty DSN and produce confusing errors.
        res = run_drill()
        self.assertEqual(res.returncode, 2, msg=res.stderr)
        self.assertIn("ROUNDTRIP_DB_URL", res.stderr)

    def test_missing_psql_exits_2(self) -> None:
        # Hide psql but keep enough core tools in PATH for bash to run.
        fake_path = Path(tempfile.mkdtemp(prefix="f31_path_"))
        try:
            for tool in ("bash", "mkdir", "ls", "grep", "sort", "command", "awk", "cat"):
                src = shutil.which(tool)
                if src:
                    os.symlink(src, fake_path / tool)
            res = run_drill(env_extra={
                "PATH": str(fake_path),
                "ROUNDTRIP_DB_URL": "postgres://u:p@h/db",
            })
            self.assertEqual(res.returncode, 2, msg=res.stderr)
            self.assertIn("psql", res.stderr)
        finally:
            shutil.rmtree(fake_path, ignore_errors=True)

    def test_missing_migrations_dir_exits_2(self) -> None:
        # When MIGRATIONS_DIR doesn't exist, the script should refuse
        # rather than silently proceed with an empty migration set.
        fake_psql = Path(tempfile.mkdtemp(prefix="f31_psql_"))
        try:
            psql_path = fake_psql / "psql"
            psql_path.write_text("#!/bin/sh\nexit 0\n")
            psql_path.chmod(0o755)
            env_path = f"{fake_psql}:{os.environ.get('PATH', '/usr/bin:/bin')}"
            res = run_drill(env_extra={
                "PATH": env_path,
                "ROUNDTRIP_DB_URL": "postgres://u:p@h/db",
                "MIGRATIONS_DIR": "/no/such/dir/exists",
            })
            self.assertEqual(res.returncode, 2, msg=res.stderr)
            self.assertIn("migrations dir", res.stderr)
        finally:
            shutil.rmtree(fake_psql, ignore_errors=True)


class DownMigrationCoverage(unittest.TestCase):
    """Enforces the project convention: every migration in the drill
    window MUST have a matching .down.sql.

    Why only the window: the convention starts with F31. Predating
    migrations (001-025) lack down files and are out of scope.
    """

    def test_drill_window_has_full_down_coverage(self) -> None:
        ups = list_up_migrations(MIGRATIONS_DIR)
        self.assertTrue(len(ups) > 0, msg=f"no migrations found in {MIGRATIONS_DIR}")
        window_start = max(0, len(ups) - DEFAULT_DEPTH)
        window = ups[window_start:]
        missing = []
        for up in window:
            down = up.replace(".sql", ".down.sql")
            if not (MIGRATIONS_DIR / down).exists():
                missing.append(down)
        self.assertEqual(
            missing,
            [],
            msg=(
                f"Migration drill window has missing down files: {missing}. "
                f"Every up in the last {DEFAULT_DEPTH} migrations must ship with a matching .down.sql. "
                "See scripts/migration_roundtrip_drill.sh for the rationale."
            ),
        )

    def test_each_down_targets_the_same_objects_as_its_up(self) -> None:
        """A lightweight sanity check: a .down.sql that contains zero
        DROP statements is almost certainly wrong. This won't catch
        every mistake (e.g. dropping the WRONG table) but does catch
        empty-stub down files committed by accident.
        """
        ups = list_up_migrations(MIGRATIONS_DIR)
        window = ups[max(0, len(ups) - DEFAULT_DEPTH):]
        empty_downs = []
        for up in window:
            down = up.replace(".sql", ".down.sql")
            path = MIGRATIONS_DIR / down
            if not path.exists():
                continue
            body = path.read_text(encoding="utf-8").lower()
            if "drop" not in body and "alter table" not in body:
                empty_downs.append(down)
        self.assertEqual(
            empty_downs,
            [],
            msg=f"These down migrations have no DROP/ALTER statements: {empty_downs}",
        )


class ScriptContractDocs(unittest.TestCase):
    """Pins documented exit codes so they don't drift from the operator
    runbook."""

    def test_drill_documented_exit_codes(self) -> None:
        body = DRILL.read_text(encoding="utf-8")
        for snippet in ("exit 2", "exit 3", "exit 4", "exit 5", "exit 6"):
            self.assertIn(snippet, body, msg=f"drill script missing documented {snippet}")


if __name__ == "__main__":
    unittest.main(verbosity=2)
