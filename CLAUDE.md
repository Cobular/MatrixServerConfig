# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this repo is

Infrastructure-as-code for a single Matrix (Synapse) homeserver on Azure with a
Discord history bridge. There is **no application code** — it is an ARM template
plus an Ansible playbook that stands up Docker containers on one Ubuntu VM. The
target is a real, running production server (`nasa.matrix.cobular.com`); most
work here is diagnosing a live deployment, not building software.

Read `RUNBOOK.md` before touching anything operational — it holds the current
server state, the backfill up-spec/down-spec procedure, and break-glass access.
`README.md` is the from-scratch setup guide.

## Production safety rule

**Never run a deployment or make an operational change without clear, explicit
permission from the user for that specific action.** In particular, do not run
any `deploy.sh` mode, a non-check-mode Ansible playbook, or a command that
creates, updates, restarts, resizes, or deletes live infrastructure or services
on your own. Editing and validating repository files does not grant deployment
permission. Present the proposed operational command and wait for approval;
do not infer approval from a request to investigate, diagnose, or edit code.

## Commands

Everything goes through `deploy.sh` (from the repo root):

```bash
./deploy.sh infra       # apply ARM template → provisions/updates the Azure VM
./deploy.sh configure   # run the full Ansible playbook against the VM
./deploy.sh all         # infra, then pause for DNS, then configure
./deploy.sh destroy     # delete the whole resource group
```

`configure` is idempotent — re-running is the normal way to apply any change to
a role or template. It fails fast if `inventory.ini` still has a placeholder IP,
if `vars.yml` still says `yourdomain.com`, or if `.vault-pass` is missing.

Ansible operations run from **inside `ansible/`** (that's where `ansible.cfg`
lives, and it references `.vault-pass` by a cwd-relative path — commands run from
elsewhere won't find the vault password):

```bash
cd ansible
ansible-playbook -i inventory.ini playbook.yml --check --diff   # dry run
ansible-playbook -i inventory.ini playbook.yml --tags synapse   # (no tags defined yet; roles run in order)
ansible-vault view group_vars/matrix/vault.yml                  # inspect secrets
ansible-vault edit group_vars/matrix/vault.yml                  # rotate secrets
ansible-galaxy collection install -r requirements.yml           # deps (deploy.sh does this)
```

There is no local Python with jinja2 on PATH. To validate template rendering
offline, use Ansible's pipx venv interpreter:
`/Users/cobular/.local/pipx/venvs/ansible/bin/python`.

Static checks (offline, vault-free — the same ones CI runs; see "Dependency
updates & CI" below). Run before every deploy:

```bash
python scripts/validate-templates.py   # render every YAML/JSON *.j2 (both
                                        # backfill_mode states) → parse; also
                                        # verifies the Renovate image-pin regex
yamllint -c .yamllint.yml .             # lint plain YAML (skips vault.yml, *.j2)
```

The VM is reached over **Tailscale** at `100.97.163.84` (see `inventory.ini`);
`ssh cobular@100.97.163.84` works directly. The public IP still exists but SSH
to it is NSG-allowlisted to a single, CGNAT-rotating source IP.

## Architecture

Two independently re-runnable phases, orchestrated by `deploy.sh`:

1. **`arm/matrix-infra.json`** — one VM, disk, NIC, NSG, public IP, plus a
   backup storage account with a system-assigned managed identity and a blob
   role assignment. `matrix-infra.parameters.json` holds the knobs (VM size,
   disk size, `sshAllowedSourceIP`). The template is the source of truth for VM
   size: if you manually resize the VM (e.g. for backfill), **do not run
   `./deploy.sh infra`** or it will resize you back.

2. **`ansible/playbook.yml`** — five roles, run in this order, each layering
   onto `~/matrix` on the VM:
   - **base** — Docker, packages, 4G swap, unattended-upgrades, weekly
     container-image-pull cron, the `~/matrix` project dir. The cron pulls only
     the **pinned** image tags now (see "Dependency updates & CI"), so it no
     longer jumps versions silently — a version bump reaches the server only
     when a re-templated `docker-compose.yml` lands via `./deploy.sh configure`.
   - **synapse** — templates `docker-compose.yml`, runs `synapse generate` once
     (creates the permanent signing key), brings up Postgres + Synapse.
   - **caddy** — downloads the latest Element Web release, templates the
     Caddyfile + Element config, starts Caddy (the only internet-facing service).
   - **discord_bridge** — generates mautrix-discord config + registration,
     wires the registration into Synapse, starts the bridge.
   - **backups** — root cron: age-encrypted Postgres dump + signing key to Azure
     Blob every 6h via managed identity, plus a disk-usage alert.

All services are Docker containers (`docker-compose.yml.j2`) on one bridge
network. Only Caddy publishes ports (80/443/8448); Synapse (8008), Postgres
(5432), and the bridge (29334) are reachable **only inside the container
network**. This is why the health check in `synapse/tasks/main.yml` probes with
`docker exec synapse curl localhost:8008` rather than hitting the host — the
host has nothing on 8008.

### The `backfill_mode` knob

`group_vars/matrix/vars.yml` has `backfill_mode`. `true` sizes Postgres
(2GB shared_buffers, `synchronous_commit=off`) and Synapse caches for the
temporarily up-specced 16 GiB VM during Discord history import; `false` is
steady-state for the 4 GiB B2s. It is toggled **together with** an Azure VM
resize — full procedure in `RUNBOOK.md` Phases 2/5. It threads through
`docker-compose.yml.j2` and the bridge config, so changing it touches multiple
templates' behavior at once.

### Dependency updates & CI

Container image versions are **pinned** as `*_image` vars in
`group_vars/matrix/vars.yml` (`postgres_image`, `caddy_image`, `synapse_image`,
`discord_image`, `synapse_admin_image`) and threaded into `docker-compose.yml.j2`
and the two `docker run` tasks (`synapse generate`, bridge config/registration
gen). There are no more `:latest`/floating tags in the templates — change a
version by editing the var, not the template. synapse-admin ships only a rolling
`:latest` (no semver tags), so it's pinned by `@sha256:` **digest** instead, which
Renovate bumps. mautrix-discord is on a semver release tag (`:vX.Y.Z`) tracked like
any other image — it was formerly on the rolling `:latest` dev build, digest-pinned,
until a release caught up to what was running. Ansible Galaxy collections in `requirements.yml`
are likewise pinned to **exact** versions, never an open `>=` range — an open
range reads as permanently satisfied, so Renovate would never bump it (silent
drift).

`renovate.json` (repo root) drives updates via the hosted **Mend Renovate**
GitHub App. A regex custom manager reads the `# renovate:` annotation comment
above each `*_image` line — that comment is load-bearing; keep it, and keep the
image value as `repo:tag` so the manager captures a bare tag (a whole `repo:tag`
in `currentValue` makes Renovate skip the dep as `invalid-value`). Two tiers:

- **Safe (auto-merge, grouped weekly):** Postgres/Caddy minor+patch, Ansible
  Galaxy collection minor+patch, and synapse-admin digest (static admin frontend,
  no migrations). Merged without review once CI is green.
- **Ping (PR assigned to `Cobular`, `needs-migration` label, never auto-merge):**
  Synapse (runs one-way DB-schema migrations on boot), mautrix-discord (semver
  release tag; bridge updates can carry DB/config migrations), and
  any **major** bump (Postgres, Caddy, or a collection). Read notes + take a
  backup, then merge, then `./deploy.sh configure`.

**Merging a Renovate PR is not a deploy** — it only changes the pin in git. The
version reaches the server on the next `configure`. Deploys stay deliberate by
design (no GitOps auto-apply: it would push Synapse/bridge migrations unattended
and hand CI standing prod + vault access). A pinned version must stay
**at-or-ahead of what's live** — a pin *behind* the running server downgrades
Synapse onto a newer DB schema on the next `configure` and crash-loops it. To
read live versions: `ssh cobular@100.97.163.84 'docker exec synapse curl -sf
localhost:8008/_synapse/admin/v1/server_version'`.

`.github/workflows/ci.yml` runs `scripts/validate-templates.py` + `yamllint` on
every PR (Renovate's included) and on pushes to main. Both are offline and
vault-free — they never touch the server or need the vault password, so they're
safe on this public repo. They also gate Renovate auto-merge (it waits for green
checks). `--syntax-check`/`ansible-lint` are intentionally **not** in CI: they
must decrypt `vault.yml`, which would require putting `.vault-pass` in Actions
secrets. CI validates *static* correctness only; that a deploy actually applies
(e.g. a Synapse schema migration completes) is still verified by running
`configure` against the real host, with the 6-hourly backups as the net.

## Landmines specific to this repo

**Container UIDs own their data dirs, not the admin user.** Synapse runs as UID
991, alpine Postgres as UID 70, mautrix-discord as UID 1337. The Ansible tasks
`chown` `synapse-data`/`postgres-data`/`discord-data` to those numeric UIDs on
purpose — `synapse generate` also chowns `/data` to 991. Never "fix" these to
`admin_user`: it causes `EACCES`/`PermissionError` crash-loops and fights the
image's own chown. On the host, `ls` shows UID 991 as `systemd-resolve`. To read
these dirs over SSH you need `sudo` (they're 0700).

**Jinja whitespace inside YAML templates is dangerous.** Two real bugs have come
from this: (1) `{%- ... -%}` trim markers inside a `command: >-` folded scalar
ate newlines and folded the `environment:` block into the command string,
erasing `POSTGRES_PASSWORD`; (2) an **indented** `{# comment #}` left orphaned
leading whitespace that corrupted the next YAML line's indentation. Rules that
hold here: keep `{% ... %}` and `{# ... #}` at column 0, prefer explicit YAML
lists over folded scalars for anything with conditionals, and after editing any
`*.j2` under `roles/*/templates/`, render it through jinja2 (`trim_blocks=True`)
+ `yaml.safe_load` before deploying — `scripts/validate-templates.py` does
exactly this for every YAML/JSON template in both `backfill_mode` states, and CI
runs it on every PR. `ansible.cfg` sets `result_format = yaml`, and Ansible
renders templates with `trim_blocks=True`.

**Least-privilege Postgres.** `init-db.sql.j2` runs once on first initdb and
creates `synapse`/`bridge` login roles owning their own databases. Services
never connect as the `postgres` superuser — that's admin/backup only. Three
distinct passwords live in the vault; don't collapse them.

**Secrets.** Everything sensitive is in `group_vars/matrix/vault.yml`
(ansible-vault). `vars.yml` maps vaulted `vault_*` values to readable names for
templates. The vault password is in `ansible/.vault-pass` (gitignored). The
vault contains the **age backup-decryption private key** — losing `.vault-pass`
makes every offsite backup unrecoverable. Never print live secret values into a
transcript; check length/emptiness instead. Do not modify shared live Azure
resources (e.g. NSG rules) unless explicitly asked — hand the user the command.

**The bridge creates rooms lazily.** In `create-on-message` mode the bridge only
makes a Matrix room for a Discord channel when a new message arrives. Use
`guilds bridge <id> --entire` in the bridge management room to create all
portals and kick off backfill immediately. Pre-2021 Discord attachments render
as `m.file` (no `mimetype`) rather than inline images — a Discord API-era
artifact, not data loss.
