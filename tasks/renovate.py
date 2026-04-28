"""
Renovate maintenance tasks.

So far this module exposes a single check that verifies every native dep in
``deps/repos.MODULE.bazel`` is either tracked by a Renovate ``customManager`` in
``renovate.json`` or listed in ``deps/.renovate-untracked.json`` with a
rationale. It runs in CI via ``.github/workflows/validate-renovate-deps.yml``.
"""

from __future__ import annotations

import json
import os
import re
from pathlib import Path

from invoke import task
from invoke.context import Context
from invoke.exceptions import Exit

REPO_ROOT = Path(__file__).resolve().parent.parent
MODULE_FILE = REPO_ROOT / "deps" / "repos.MODULE.bazel"
RENOVATE_FILE = REPO_ROOT / "renovate.json"
ALLOWLIST_FILE = REPO_ROOT / "deps" / ".renovate-untracked.json"

HTTP_ARCHIVE_NAME_RE = re.compile(r'http_archive\s*\(\s*name\s*=\s*"([^"]+)"')


def _parse_module_bazel(path: Path) -> set[str]:
    return set(HTTP_ARCHIVE_NAME_RE.findall(path.read_text()))


def _parse_renovate_json(path: Path) -> set[str]:
    raw = path.read_text()
    # renovate.json is JSON5 (trailing commas allowed); strip them so json.loads accepts.
    stripped = re.sub(r",(\s*[}\]])", r"\1", raw)
    data = json.loads(stripped)
    return {cm["depNameTemplate"] for cm in data.get("customManagers", []) if "depNameTemplate" in cm}


def _parse_allowlist(path: Path) -> dict[str, str]:
    if not path.exists():
        return {}
    data = json.loads(path.read_text())
    entries = data.get("intentionally_untracked", {})
    bad = [k for k, v in entries.items() if not (isinstance(v, str) and v.strip())]
    if bad:
        raise Exit(
            f"Allowlist {path} has empty rationale for: {', '.join(bad)}. "
            "Every entry must include a non-empty justification string."
        )
    return entries


def _emit_failure_report(untracked: set[str], allowlist: dict[str, str]) -> str:
    lines = [
        "## ❌ Renovate coverage check failed",
        "",
        f"The following deps in `{MODULE_FILE.relative_to(REPO_ROOT)}` have no "
        f"matching `customManager` in `renovate.json`:",
        "",
        "| dep | suggested fix |",
        "|---|---|",
    ]
    for dep in sorted(untracked):
        lines.append(
            f"| `{dep}` | Add a `customManagers` entry with "
            f'`depNameTemplate: "{dep}"`, or add to '
            "`deps/.renovate-untracked.json` with a rationale. |"
        )
    lines += [
        "",
        "See `renovate.json` for existing patterns (linux-images, windows-images, ...).",
        "",
        f"Currently allowlisted ({len(allowlist)}): "
        + (", ".join(f"`{k}`" for k in sorted(allowlist)) if allowlist else "_none_"),
    ]
    return "\n".join(lines)


@task
def check_bazel_coverage(_: Context) -> None:
    """
    Fail if any http_archive in deps/repos.MODULE.bazel lacks a Renovate customManager.

    A dep is considered covered when either:
      * its name appears as `depNameTemplate` in one of `renovate.json`'s customManagers, or
      * it is listed in `deps/.renovate-untracked.json` with a non-empty rationale.

    Writes a markdown report to ``$GITHUB_STEP_SUMMARY`` when running in GitHub Actions.
    """
    bazel_names = _parse_module_bazel(MODULE_FILE)
    tracked_names = _parse_renovate_json(RENOVATE_FILE)
    allowlist = _parse_allowlist(ALLOWLIST_FILE)

    untracked = bazel_names - tracked_names - set(allowlist)
    if untracked:
        report = _emit_failure_report(untracked, allowlist)
        summary_path = os.environ.get("GITHUB_STEP_SUMMARY")
        if summary_path:
            Path(summary_path).write_text(report + "\n", encoding="utf-8")
        raise Exit(report, code=1)

    print(
        f"OK: {len(bazel_names)} http_archive deps, "
        f"{len(bazel_names) - len(allowlist)} tracked by Renovate, "
        f"{len(allowlist)} intentionally untracked."
    )
