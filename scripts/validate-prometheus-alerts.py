#!/usr/bin/env python3
"""Validate the repository's Prometheus alert rules without extra deps.

If promtool is installed, this script delegates full rule validation to it.
Otherwise it performs a strict structural scan tailored to our alert file so
CI still catches broken indentation, missing fields, duplicate alert names,
and rules that reference metrics not exported by the service.
"""

from __future__ import annotations

import re
import shutil
import subprocess
import sys
from pathlib import Path


REQUIRED_RULE_FIELDS = {"alert", "expr", "for", "labels", "annotations"}
EXPORTED_METRICS = {
    "fundai_http_requests_total",
    "fundai_http_request_duration_avg_ms",
    "fundai_http_panics_total",
    "fundai_llm_calls_total",
    "fundai_llm_latency_avg_ms",
    "fundai_workflow_transitions_total",
    "fundai_hard_risk_rejections_total",
    "fundai_marketplace_reconciliations_total",
    "fundai_marketplace_reconciliation_last",
    "fundai_db_open_connections",
    "fundai_db_in_use_connections",
    "fundai_db_idle_connections",
    "fundai_db_max_open_connections",
    "fundai_db_wait_count_total",
    "fundai_db_wait_duration_seconds_total",
    "fundai_scheduler_leader_state",
}


def run_promtool(path: Path) -> bool:
    promtool = shutil.which("promtool")
    if not promtool:
        return False
    subprocess.run([promtool, "check", "rules", str(path)], check=True)
    return True


def read_alerts(path: Path) -> str:
    if not path.is_file():
        raise SystemExit(f"missing alert rules file: {path}")
    text = path.read_text(encoding="utf-8")
    if not text.strip():
        raise SystemExit(f"empty alert rules file: {path}")
    if "\t" in text:
        raise SystemExit("YAML must use spaces, not tabs")
    return text


def parse_rule_blocks(text: str) -> list[list[str]]:
    blocks: list[list[str]] = []
    current: list[str] | None = None
    for line in text.splitlines():
        if re.match(r"^\s{6}- alert:\s+\S+", line):
            if current:
                blocks.append(current)
            current = [line]
        elif current is not None:
            if re.match(r"^\s{2}- name:\s+\S+", line):
                blocks.append(current)
                current = None
            else:
                current.append(line)
    if current:
        blocks.append(current)
    return blocks


def validate_structure(text: str) -> None:
    if not re.search(r"^groups:\s*$", text, re.MULTILINE):
        raise SystemExit("alert file must start with a groups: section")
    groups = re.findall(r"^\s{2}- name:\s+([A-Za-z0-9_-]+)\s*$", text, re.MULTILINE)
    if not groups:
        raise SystemExit("alert file must contain at least one named group")

    blocks = parse_rule_blocks(text)
    if not blocks:
        raise SystemExit("alert file must contain at least one alert rule")

    seen_alerts: set[str] = set()
    for block in blocks:
        joined = "\n".join(block)
        alert_match = re.search(r"^\s{6}- alert:\s+([A-Za-z0-9_:]+)\s*$", joined, re.MULTILINE)
        if not alert_match:
            raise SystemExit(f"rule missing alert name:\n{joined}")
        alert = alert_match.group(1)
        if alert in seen_alerts:
            raise SystemExit(f"duplicate alert name: {alert}")
        seen_alerts.add(alert)

        fields = set(re.findall(r"^\s{8}([A-Za-z_]+):", joined, re.MULTILINE))
        fields.add("alert")
        missing = REQUIRED_RULE_FIELDS - fields
        if missing:
            raise SystemExit(f"alert {alert} missing required fields: {', '.join(sorted(missing))}")

        for required_annotation in ("summary", "description"):
            if not re.search(rf"^\s{{10}}{required_annotation}:\s+\S+", joined, re.MULTILINE):
                raise SystemExit(f"alert {alert} missing annotations.{required_annotation}")


def validate_metric_references(text: str) -> None:
    referenced = set(re.findall(r"\b(fundai_[a-zA-Z_:][a-zA-Z0-9_:]*)\b", text))
    unknown = referenced - EXPORTED_METRICS
    if unknown:
        raise SystemExit(f"alert file references unknown exported metrics: {', '.join(sorted(unknown))}")
    missing_coverage = {
        "fundai_http_requests_total",
        "fundai_http_panics_total",
        "fundai_llm_calls_total",
        "fundai_workflow_transitions_total",
        "fundai_hard_risk_rejections_total",
        "fundai_db_in_use_connections",
        "fundai_scheduler_leader_state",
        "fundai_marketplace_reconciliations_total",
    } - referenced
    if missing_coverage:
        raise SystemExit(f"alert file is missing required metric coverage: {', '.join(sorted(missing_coverage))}")


def main() -> int:
    path = Path(sys.argv[1]) if len(sys.argv) > 1 else Path("prometheus/alerts.yml")
    text = read_alerts(path)
    used_promtool = run_promtool(path)
    validate_structure(text)
    validate_metric_references(text)
    if used_promtool:
        print(f"validated {path} with promtool and built-in static checks")
    else:
        print(f"validated {path} with built-in static checks")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
