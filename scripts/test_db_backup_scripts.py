#!/usr/bin/env python3
"""F30 contract tests for db_backup.sh and db_restore_drill.sh.

We don't have postgres available in CI for these scripts, so the tests
exercise the *prerequisite-failure* code paths instead. Specifically we
prove:

1. Both scripts exit 2 when DATABASE_URL / PGDATABASE are both unset
   (catches a regression that would silently dump nothing).
2. Both scripts exit 2 when the required pg_dump / pg_restore tool is
   missing (catches a packaging regression that ships the script
   without its runtime dependency).
3. db_restore_drill exits 3 when no backups exist (so an empty backup
   directory doesn't silently "succeed").
4. The script bodies hold the documented contract strings (exit codes,
   filename pattern). These are documentation-as-test: a refactor that
   re-numbers exit codes would also need to update operator runbooks.

Run with:  python3 scripts/test_db_backup_scripts.py
"""

from __future__ import annotations

import os
import shutil
import subprocess
import sys
import tempfile
import textwrap
import unittest
from pathlib import Path

SCRIPTS_DIR = Path(__file__).resolve().parent
BACKUP_SH = SCRIPTS_DIR / "db_backup.sh"
RESTORE_SH = SCRIPTS_DIR / "db_restore_drill.sh"


def run_script(
    script: Path,
    env_extra: dict[str, str] | None = None,
    args: list[str] | None = None,
    cwd: Path | None = None,
) -> subprocess.CompletedProcess:
    """Invoke a backup script with a CLEAN environment to avoid leaking
    the developer's local PGDATABASE / DATABASE_URL into the test run.
    Only PATH is carried over so the script can find bash / coreutils.
    """
    env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin")}
    if env_extra:
        env.update(env_extra)
    return subprocess.run(
        ["bash", str(script), *(args or [])],
        env=env,
        cwd=str(cwd) if cwd else None,
        capture_output=True,
        text=True,
        timeout=30,
    )


def fake_path_dir(missing_tools: list[str]) -> Path:
    """Build a temp PATH directory that hides specific tools. Anything
    NOT in missing_tools is symlinked from the real PATH so other shell
    builtins (mkdir, date, awk, etc.) keep working.
    """
    fake_dir = Path(tempfile.mkdtemp(prefix="f30_path_"))
    needed = {"bash", "mkdir", "date", "awk", "tr", "grep", "sort", "tail",
              "find", "rm", "ls", "command", "sha256sum", "shasum", "basename",
              "echo", "printf", "cd", "cat"}
    for tool in needed - set(missing_tools):
        real = shutil.which(tool)
        if real:
            os.symlink(real, fake_dir / tool)
    return fake_dir


class BackupScriptPreconditions(unittest.TestCase):
    """Validates that db_backup.sh fails fast on misconfiguration."""

    def test_missing_pg_dump_exits_2(self) -> None:
        # No PG_DUMP override and the fake PATH excludes pg_dump.
        fake_path = fake_path_dir(missing_tools=["pg_dump"])
        try:
            res = run_script(BACKUP_SH, env_extra={"PATH": str(fake_path), "DATABASE_URL": "postgres://x/y"})
            self.assertEqual(res.returncode, 2, msg=res.stderr)
            self.assertIn("pg_dump", res.stderr)
        finally:
            shutil.rmtree(fake_path, ignore_errors=True)

    def test_missing_database_url_exits_2(self) -> None:
        # The script must refuse to dump if neither env hint is set —
        # otherwise it would happily produce an empty dump file from
        # whatever DB libpq chooses by accident.
        res = run_script(
            BACKUP_SH,
            env_extra={"PG_DUMP": shutil.which("true") or "/bin/true"},
        )
        self.assertEqual(res.returncode, 2, msg=res.stderr)
        self.assertIn("DATABASE_URL", res.stderr)


class RestoreScriptPreconditions(unittest.TestCase):
    """Validates that db_restore_drill.sh fails fast on misconfiguration."""

    def test_missing_pg_restore_exits_2(self) -> None:
        fake_path = fake_path_dir(missing_tools=["pg_restore"])
        try:
            res = run_script(RESTORE_SH, env_extra={"PATH": str(fake_path), "DATABASE_URL": "postgres://x/y"})
            self.assertEqual(res.returncode, 2, msg=res.stderr)
            self.assertIn("pg_restore", res.stderr)
        finally:
            shutil.rmtree(fake_path, ignore_errors=True)

    def test_missing_psql_exits_2(self) -> None:
        # pg_restore must exist but psql must not — proves the script
        # checks ALL its tool dependencies, not just the first.
        fake_path = fake_path_dir(missing_tools=["psql"])
        # Provide a fake pg_restore that satisfies command -v.
        pgr = fake_path / "pg_restore"
        pgr.write_text("#!/bin/sh\nexit 0\n")
        pgr.chmod(0o755)
        try:
            res = run_script(RESTORE_SH, env_extra={"PATH": str(fake_path), "DATABASE_URL": "postgres://x/y"})
            self.assertEqual(res.returncode, 2, msg=res.stderr)
            self.assertIn("psql", res.stderr)
        finally:
            shutil.rmtree(fake_path, ignore_errors=True)

    def test_missing_backup_exits_3(self) -> None:
        # Empty backup dir → exit 3, not exit 0. An "ok" exit with no
        # backups would let a cron-driven drill silently succeed when
        # the backup job was actually broken.
        with tempfile.TemporaryDirectory() as tmp:
            fake_path = fake_path_dir(missing_tools=[])
            try:
                # Provide minimal fake tools so the prereqs pass.
                for name in ("pg_restore", "psql"):
                    p = fake_path / name
                    p.write_text("#!/bin/sh\nexit 0\n")
                    p.chmod(0o755)
                res = run_script(
                    RESTORE_SH,
                    env_extra={
                        "PATH": str(fake_path),
                        "DATABASE_URL": "postgres://u:p@h/db1",
                        "BACKUP_DIR": tmp,
                    },
                )
                self.assertEqual(res.returncode, 3, msg=res.stderr)
                self.assertIn("no backups", res.stderr.lower())
            finally:
                shutil.rmtree(fake_path, ignore_errors=True)

    def test_unknown_flag_exits_2(self) -> None:
        # CLI hygiene: an unknown flag must NOT silently fall through
        # to the rest of the script (could otherwise hide a typo'd
        # --no-keep that accidentally drops the drill DB).
        res = run_script(RESTORE_SH, args=["--not-a-real-flag"], env_extra={"DATABASE_URL": "postgres://x/y"})
        self.assertEqual(res.returncode, 2, msg=res.stderr)
        self.assertIn("unknown flag", res.stderr)


class ContractDocStringTests(unittest.TestCase):
    """Pins the documented exit codes so a refactor can't quietly
    re-number them without also touching the operator runbook (which
    references these numbers verbatim)."""

    def _read(self, p: Path) -> str:
        return p.read_text(encoding="utf-8")

    def test_backup_documented_exit_codes_present(self) -> None:
        body = self._read(BACKUP_SH)
        for snippet in ("exit 2", "exit 3", "exit 4"):
            self.assertIn(snippet, body, msg=f"db_backup.sh missing documented {snippet}")

    def test_restore_documented_exit_codes_present(self) -> None:
        body = self._read(RESTORE_SH)
        for snippet in ("exit 2", "exit 3", "exit 4", "exit 5", "exit 6"):
            self.assertIn(snippet, body, msg=f"db_restore_drill.sh missing documented {snippet}")

    def test_filename_pattern_stable(self) -> None:
        # The drill script grep on ^${db_label}-[0-9TZ]+\.dump$ — if the
        # backup script ever changes its filename format both must change
        # together. This is a documentation-as-test against drift.
        backup = self._read(BACKUP_SH)
        restore = self._read(RESTORE_SH)
        self.assertIn('${db_label}-${timestamp}.dump', backup)
        self.assertIn('^${db_label}-[0-9TZ]+\\.dump$', restore)


if __name__ == "__main__":
    if not BACKUP_SH.exists() or not RESTORE_SH.exists():
        print("Required scripts missing:", BACKUP_SH, RESTORE_SH, file=sys.stderr)
        sys.exit(1)
    # textwrap.dedent forces a clean diff if anyone scripts this output.
    print(textwrap.dedent(f"""
        Running F30 backup-script contract tests.
          script dir: {SCRIPTS_DIR}
    """).strip())
    unittest.main(verbosity=2)
