#!/usr/bin/env python3
"""Offline validation of the Ansible Jinja templates.

Guards against this repo's #1 landmine: Jinja whitespace/comment handling that
silently corrupts the YAML the templates produce (see CLAUDE.md). Renders every
YAML/JSON template with trim_blocks=True (matching Ansible) in BOTH backfill_mode
states, then parses the output. Uses dummy values for all variables — it checks
STRUCTURE, not secret correctness — so it needs no vault password and is safe to
run in CI on a public repo.

Also asserts the container-image pins in vars.yml still match the regex that
Renovate's custom manager keys off (renovate.json) and each resolves to a bare
tag, so a refactor can't silently make Renovate go blind or skip a dependency.

Run from the repo root:  python scripts/validate-templates.py
"""
import json
import re
import sys
from pathlib import Path

import yaml
from jinja2 import ChainableUndefined, Environment, FileSystemLoader

REPO = Path(__file__).resolve().parent.parent
ANSIBLE = REPO / "ansible"
VARS_FILE = ANSIBLE / "group_vars" / "matrix" / "vars.yml"

# YAML/JSON templates to render + parse.  (Shell/SQL/Caddyfile/ini templates are
# skipped: they aren't structured formats we can meaningfully parse here.)
YAML_TEMPLATES = [
    "roles/synapse/templates/docker-compose.yml.j2",
    "roles/synapse/templates/homeserver.yaml.j2",
    "roles/discord_bridge/templates/config.yaml.j2",
]
JSON_TEMPLATES = [
    "roles/caddy/templates/element-config.json.j2",
    "roles/caddy/templates/synapse-admin-config.json.j2",
]

# The exact regex Renovate's custom manager uses (keep in sync with renovate.json).
# currentValue must capture ONLY the tag (e.g. "16.14-alpine"), not the whole
# "repo:tag" — otherwise Renovate rejects it as an unversioned value and silently
# skips the dependency. That's why the image name is consumed by a non-capturing
# `[^"@]*:` before the currentValue group.
RENOVATE_REGEX = (
    r'# renovate: datasource=(?P<datasource>\S+) depName=(?P<depName>\S+)'
    r'(?: versioning=(?P<versioning>\S+))?\s+\w+_image:\s*'
    r'"[^"@]*:(?P<currentValue>[^"@:]+)(?:@(?P<currentDigest>sha256:[a-f0-9]+))?"'
)
EXPECTED_PINS = 5


def load_fixture():
    """vars.yml values (vaulted refs stay literal strings — fine for structure)
    plus stubs for facts that tasks set at runtime."""
    ctx = yaml.safe_load(VARS_FILE.read_text()) or {}
    # backfill_mode is toggled per-render below, so don't let the file's value
    # collide with the keyword argument.
    ctx.pop("backfill_mode", None)
    ctx.update(
        ansible_managed="Ansible managed: do not edit",
        # set_fact in discord_bridge/tasks/main.yml, not present in vars.yml:
        bridge_as_token="dummy_as_token",
        bridge_hs_token="dummy_hs_token",
    )
    return ctx


def render(rel_path, backfill_mode, fixture):
    tpl_path = ANSIBLE / rel_path
    env = Environment(
        loader=FileSystemLoader(str(tpl_path.parent)),
        trim_blocks=True,           # matches Ansible's rendering
        undefined=ChainableUndefined,  # missing vars -> '' rather than crash
        keep_trailing_newline=True,
    )
    tpl = env.get_template(tpl_path.name)
    return tpl.render(backfill_mode=backfill_mode, **fixture)


def main():
    fixture = load_fixture()
    failures = []

    for rel in YAML_TEMPLATES:
        for bf in (True, False):
            try:
                out = render(rel, bf, fixture)
                yaml.safe_load(out)
            except Exception as e:  # noqa: BLE001 - report, don't crash
                failures.append(f"{rel} (backfill_mode={bf}): {type(e).__name__}: {e}")
            else:
                print(f"  ok  {rel} (backfill_mode={bf})")

    for rel in JSON_TEMPLATES:
        for bf in (True, False):
            try:
                out = render(rel, bf, fixture)
                json.loads(out)
            except Exception as e:  # noqa: BLE001
                failures.append(f"{rel} (backfill_mode={bf}): {type(e).__name__}: {e}")
            else:
                print(f"  ok  {rel} (backfill_mode={bf})")

    # Renovate regex still matches every image pin?
    pins = list(re.finditer(RENOVATE_REGEX, VARS_FILE.read_text()))
    if len(pins) != EXPECTED_PINS:
        failures.append(
            f"Renovate regex matched {len(pins)} image pins in vars.yml, "
            f"expected {EXPECTED_PINS} — renovate.json would go blind."
        )
    else:
        print(f"  ok  Renovate regex matches all {EXPECTED_PINS} image pins")

    # currentValue must be a bare tag. If it still contains '/' or ':' it's the
    # whole "repo:tag" ref, which Renovate rejects as invalid-value and skips
    # (the bug that made the first Renovate run produce zero updates).
    for m in pins:
        tag = m.group("currentValue")
        if "/" in tag or ":" in tag:
            failures.append(
                f"Renovate currentValue for {m.group('depName')} is '{tag}' — "
                f"expected a bare tag, not a repo:tag ref (Renovate would skip it)."
            )
        else:
            print(f"  ok  {m.group('depName')} → tag '{tag}'")

    if failures:
        print("\nFAILED:", file=sys.stderr)
        for f in failures:
            print(f"  - {f}", file=sys.stderr)
        sys.exit(1)
    print("\nAll template checks passed.")


if __name__ == "__main__":
    main()
