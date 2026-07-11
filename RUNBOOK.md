# Deployment & Backfill Runbook

Written 2026-07-11. Current state at time of writing:

- **Infra:** old ARM template deployed — VM `matrix-server` (Standard_B2s) in
  RG `matrix-rg`, westus2, public IP **20.59.22.133**. The hardened template
  (SSH allowlist, managed identity, backup storage account) is in the repo but
  **not yet applied**.
- **Ansible:** never ran to completion (died at a callback-plugin error before
  any tasks executed). The VM is bare Ubuntu.
- **DNS:** `nasa.matrix.cobular.com` and `element.cobular.com` both resolve to
  **Cloudflare proxy IPs**, not the VM — must be fixed (Phase 0).
- **Secrets:** ansible-vault is set up; password in `ansible/.vault-pass`
  (gitignored).

Phases 0–4 happen now. Phase 5 is the resume point after backfill finishes
(a few weeks out).

---

## Phase 0 — Prep (laptop, ~10 min)

1. **Save `ansible/.vault-pass` to your password manager.** It decrypts
   `group_vars/matrix/vault.yml`, which holds every secret including the
   backup decryption key. Lose it and the vault + all backups are gone.

2. **Fix DNS in Cloudflare.** Both records currently sit behind the orange
   cloud (proxy), which breaks Matrix federation — Cloudflare doesn't proxy
   port 8448 — and fights Caddy's Let's Encrypt issuance. Set both to
   **DNS only (gray cloud)**:

   ```
   nasa.matrix.cobular.com  → A → 20.59.22.133   (proxy OFF)
   element.cobular.com      → A → 20.59.22.133   (proxy OFF)
   ```

   Verify (both must return exactly the VM IP):
   ```bash
   dig +short nasa.matrix.cobular.com   # expect 20.59.22.133
   dig +short element.cobular.com       # expect 20.59.22.133
   ```

3. **Check your egress IP** (T-Mobile CGNAT — it rotates):
   ```bash
   curl -4 icanhazip.com
   ```
   If it no longer matches `sshAllowedSourceIP` in
   `arm/matrix-infra.parameters.json`, update the parameter before Phase 1.

## Phase 1 — Apply hardened infra (~5 min)

```bash
./deploy.sh infra
```

Idempotent on the existing VM (no reboot). Applies: SSH allowlisted to your
IP, system-assigned managed identity, backup storage account
`cobularmatrixbackups` + container + 30-day lifecycle + blob role assignment.

Verify SSH still works:
```bash
ssh cobular@20.59.22.133 'echo ok'
```
If locked out (IP rotated between check and deploy), see **Break-glass** below.

## Phase 2 — Up-spec for backfill (~10 min, ~5 min downtime)

Sized per the earlier analysis: D4as_v5 (4 vCPU / 16 GiB, no B-series burst
throttling) + Premium SSD (fsync-heavy Postgres ingest). Run-rate ≈ $168/mo,
prorated ≈ $85–125 for 2–3 weeks — inside the $150/mo credit.

```bash
az vm deallocate -g matrix-rg -n matrix-server
OSDISK=$(az vm show -g matrix-rg -n matrix-server --query "storageProfile.osDisk.name" -o tsv)
az disk update -g matrix-rg -n "$OSDISK" --sku Premium_LRS
az vm resize -g matrix-rg -n matrix-server --size Standard_D4as_v5
az vm start -g matrix-rg -n matrix-server
```

Then flip the tuning knob in `ansible/group_vars/matrix/vars.yml`:
```yaml
backfill_mode: true
```
(Applied by the Phase 3 configure run — sizes Postgres shared_buffers/WAL,
disables synchronous_commit, enlarges Synapse caches.)

> ⚠️ **While up-specced, never run `./deploy.sh infra`** — the template says
> B2s and will resize you back mid-backfill. If your SSH IP rotates during
> the window, update the NSG rule directly (see Break-glass), not via the
> template.

## Phase 3 — Configure + first login (~30 min)

```bash
./deploy.sh configure
```
First run: installs packages, generates the Synapse signing key (permanent
server identity), brings up Postgres/Synapse/Caddy, generates bridge config +
registration, starts the bridge. Re-running is safe and is also how config
changes get applied later.

**Create the admin account** — username must be `admin` (the bridge grants
admin rights to `@admin:nasa.matrix.cobular.com`):
```bash
ssh cobular@20.59.22.133
cd ~/matrix && docker exec -it synapse register_new_matrix_user \
  -c /data/homeserver.yaml -u admin -a http://localhost:8008
```

**Health checks:**
```bash
curl https://nasa.matrix.cobular.com/_matrix/client/versions   # JSON, not HTML
# Log in at https://element.cobular.com as @admin
# Federation: https://federationtester.matrix.org/#nasa.matrix.cobular.com
```

**Backup smoke test** (on the VM):
```bash
sudo ~/matrix/backup.sh
sudo rclone ls bk:backups     # expect pg_*.sql.gz.age and keys_*.tar.gz.age
```
If rclone returns 403: the managed-identity role assignment can take a few
minutes to propagate after Phase 1 — wait and retry.

## Phase 4 — Bridge Discord + backfill (days–weeks, mostly unattended)

Create the Discord bot token (README § Connect Discord Bridge), then in
Element DM `@discordbot:nasa.matrix.cobular.com`:
```
login-token bot YOUR_DISCORD_BOT_TOKEN
guilds status
guilds bridge <guild_id>
```

Backfill pacing is dominated by Discord's API rate limits — expect days to
weeks of wall clock for years of history. Monitoring (on the VM):
```bash
docker compose -f ~/matrix/docker-compose.yml logs -f mautrix-discord
docker stats --no-stream
df -h /
sudo du -sh ~/matrix/synapse-data/media_store
docker exec postgres psql -U postgres -c \
  "SELECT datname, pg_size_pretty(pg_database_size(datname)) FROM pg_database;"
```

- **Disk hits 80%** (cron alert via syslog `matrix-disk`): grow the disk —
  capacity can only ever grow, so do it in modest steps:
  ```bash
  az vm deallocate -g matrix-rg -n matrix-server
  az disk update -g matrix-rg -n "$OSDISK" --size-gb 512
  az vm start -g matrix-rg -n matrix-server   # Ubuntu auto-grows the FS on boot
  ```
- **Postgres IO-bound** (`iostat -x 5` shows the disk pegged): bump the disk's
  performance tier without growing it — reversible:
  ```bash
  az disk update -g matrix-rg -n "$OSDISK" --set tier=P20
  ```

Backfill is done when the bridge logs go quiet and CPU settles to idle.

---

## Phase 5 — Down-spec  ⟵ **RESUME HERE after backfill**

1. In `ansible/group_vars/matrix/vars.yml` set:
   ```yaml
   backfill_mode: false
   ```
2. Re-apply (re-enables synchronous_commit, shrinks caches for 4 GiB):
   ```bash
   ./deploy.sh configure
   ```
3. Resize down:
   ```bash
   az vm deallocate -g matrix-rg -n matrix-server
   OSDISK=$(az vm show -g matrix-rg -n matrix-server --query "storageProfile.osDisk.name" -o tsv)
   az disk update -g matrix-rg -n "$OSDISK" --sku StandardSSD_LRS
   # If you bumped the perf tier in Phase 4, also drop it: --tier P15 (or your size's baseline)
   az vm resize -g matrix-rg -n matrix-server --size Standard_B2s
   az vm start -g matrix-rg -n matrix-server
   ```
   If you grew the disk in Phase 4, leave `osDiskSizeGB` in
   `arm/matrix-infra.parameters.json` updated to match — it can't shrink.
4. `./deploy.sh infra` is safe to run again from this point (template matches
   reality).
5. Verify: Element loads, `curl .../_matrix/client/versions`, and the next
   6-hourly backup lands in blob (`sudo rclone ls bk:backups`).

---

## Break-glass & gotchas

- **SSH IP rotated** (CGNAT) — update the NSG rule directly from any
  `az`-authenticated machine, then mirror the value into
  `arm/matrix-infra.parameters.json` at leisure:
  ```bash
  az network nsg rule update -g matrix-rg --nsg-name matrix-nsg -n AllowSSH \
    --source-address-prefixes "$(curl -4 -s icanhazip.com)/32"
  ```
- **No SSH at all** — Azure fabric path, ignores the NSG:
  ```bash
  az vm run-command invoke -g matrix-rg -n matrix-server \
    --command-id RunShellScript --scripts "uptime"
  ```
- **Restore from backup:** README § Recovery (needs `.vault-pass` + SSH key).
- **Later hardening (post-backfill, optional):** Tailscale on the VM, then
  delete the `AllowSSH` NSG rule entirely; add the media-store `rclone sync`
  leg to backups (media is not yet backed up — only Postgres + signing key).
