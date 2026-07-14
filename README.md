# Matrix Server in a Box

A complete, reproducible Matrix homeserver deployment with Discord bridge migration, targeting Azure but portable to any Linux VPS.

Two-phase deployment: ARM template provisions Azure infrastructure, Ansible configures everything on the server. Each phase is independently re-runnable.

> **Deploying or resuming right now?** Follow [RUNBOOK.md](RUNBOOK.md) — it has
> the exact current state, the backfill up-spec/down-spec procedure, and
> break-glass commands.

## What You Get

- **Synapse** homeserver with E2E encryption enabled by default
- **PostgreSQL** database with least-privilege roles (services never run as superuser)
- **Caddy** reverse proxy with automatic Let's Encrypt TLS (admin API not exposed)
- **Element Web** client on its own domain (XSS containment from the homeserver)
- **mautrix-discord** bridge configured for full history backfill
- **Encrypted offsite backups** — age-encrypted Postgres dumps + signing key to
  Azure Blob every 6 hours, via managed identity (no storage keys on the VM)
- **Automatic updates** — unattended security upgrades for the host OS, weekly
  image pulls for the containers
- **Secrets in ansible-vault** — nothing sensitive in plaintext

## Prerequisites

On your local machine:

```bash
# Azure CLI
brew install azure-cli  # macOS
# or: curl -sL https://aka.ms/InstallAzureCLIDeb | sudo bash  # Ubuntu

# Ansible
pip install ansible

# Log into Azure
az login
```

## Project Structure

```
matrix-server-in-a-box/
├── deploy.sh                          # Orchestrates both phases
├── arm/
│   ├── matrix-infra.json              # ARM template (VM, network, disk)
│   └── matrix-infra.parameters.json   # Your Azure settings
└── ansible/
    ├── ansible.cfg                    # Ansible settings
    ├── .vault-pass                    # Vault password (gitignored — back it up!)
    ├── inventory.ini                  # Target host (VM IP goes here)
    ├── requirements.yml               # Galaxy dependencies
    ├── playbook.yml                   # Main entry point
    ├── group_vars/
    │   └── matrix/
    │       ├── vars.yml               # All non-secret configuration
    │       └── vault.yml              # Secrets (ansible-vault encrypted)
    └── roles/
        ├── base/                      # Docker, packages, security
        ├── synapse/                   # Homeserver + Postgres
        ├── caddy/                     # Reverse proxy + Element Web
        ├── discord_bridge/            # mautrix-discord
        └── backups/                   # Cron jobs, monitoring
```

## Quick Start

### 1. Configure

```bash
# Azure infrastructure settings
nano arm/matrix-infra.parameters.json
# → Paste your SSH public key (cat ~/.ssh/id_rsa.pub)
# → Set sshAllowedSourceIP to your IP (curl -4 icanhazip.com), as CIDR: 1.2.3.4/32
# → Pick a globally-unique backupStorageAccountName

# Matrix server settings
nano ansible/group_vars/matrix/vars.yml
# → Set matrix_server_name (permanent! e.g. "yourdomain.com")
# → Set matrix_hostname (e.g. "matrix.yourdomain.com")
# → Set element_hostname (e.g. "element.yourdomain.com")
# → Set backup_storage_account to match the ARM parameter
```

### Secrets (ansible-vault)

All secrets live encrypted in `ansible/group_vars/matrix/vault.yml`. The vault
password sits in `ansible/.vault-pass` (gitignored) and is used automatically.

**Store a copy of `.vault-pass` in your password manager.** If you lose it,
the vault — including the backup decryption key — is unrecoverable.

```bash
cd ansible
ansible-vault view group_vars/matrix/vault.yml   # inspect secrets
ansible-vault edit group_vars/matrix/vault.yml   # rotate/change secrets
```

### 2. Deploy Infrastructure

```bash
chmod +x deploy.sh
./deploy.sh infra
```

This creates the Azure VM and prints its public IP.

### 3. DNS

Create two DNS A records:
```
matrix.yourdomain.com   →  <public IP from step 2>
element.yourdomain.com  →  <public IP from step 2>
```

### 4. Update Inventory

```bash
nano ansible/inventory.ini
# → Replace PASTE_VM_IP_HERE with the public IP
```

### 5. Deploy Software

```bash
./deploy.sh configure
```

This SSHes into the VM and runs the full Ansible playbook. You'll see each task's status in real time. If anything fails, fix it and re-run — Ansible skips already-completed tasks.

### 6. Create Accounts

```bash
ssh cobular@<ip>
cd ~/matrix
docker exec -it synapse register_new_matrix_user \
  -c /data/homeserver.yaml -u admin -a http://localhost:8008
```

### 7. Connect Discord Bridge

> **RAM note:** full-history backfill (`backfill_*: -1`) is the most
> memory-hungry thing this server will ever do. Resize up for the initial
> bridge, then back down (a few dollars for a few days):
> ```bash
> az vm deallocate -g matrix-rg -n matrix-server
> az vm resize -g matrix-rg -n matrix-server --size Standard_B4ms  # 16 GiB
> az vm start -g matrix-rg -n matrix-server
> # ...after backfill completes, repeat with --size Standard_B2s
> ```
> Disk and static IP survive deallocation; expect ~5 min downtime each way.
> While resized, don't run `./deploy.sh infra` — the template still says
> B2s and would resize you back. (If backfill pegs the CPU for days, a
> D4s_v5 avoids B-series burst-credit throttling.)

Log into Element Web at `https://element.yourdomain.com`, then DM `@discordbot:yourdomain.com`:

```
login-token bot YOUR_DISCORD_BOT_TOKEN
guilds status
guilds bridge <guild_id>
```

To create a Discord bot token:
1. Go to https://discord.com/developers/applications
2. New Application → Bot → copy token
3. Enable "Server Members Intent" and "Message Content Intent"
4. OAuth2 → URL Generator → `bot` scope → select permissions
5. Use generated URL to add bot to your Discord server

## Day-to-Day Operations

```bash
# Re-run after config changes (safe, idempotent)
./deploy.sh configure

# Preview what would change without doing it
cd ansible && ansible-playbook -i inventory.ini playbook.yml --check --diff

# SSH in and check status
ssh cobular@<ip>
cd ~/matrix
docker compose ps              # container status
docker compose logs -f synapse # synapse logs
docker compose logs -f mautrix-discord  # bridge logs
df -h /                        # disk usage

# Update all containers now (also runs automatically Sundays 04:30)
docker compose pull && docker compose up -d

# Create more user accounts
docker exec -it synapse register_new_matrix_user \
  -c /data/homeserver.yaml http://localhost:8008

# Manual backup (root — it reads the 991-owned signing key)
sudo ./backup.sh

# List offsite backups
sudo rclone ls bk:backups
```

## Recovery

Backups live age-encrypted in Azure Blob (they survive the VM). You need two
things from your password manager: the SSH key and the vault password
(`ansible/.vault-pass`). The backup decryption key is inside the vault:

```bash
# 0. Get the age private key out of the vault (starts with AGE-SECRET-KEY-)
cd ansible && ansible-vault view group_vars/matrix/vault.yml

# 1. Deploy fresh infrastructure
./deploy.sh infra
# 2. Update inventory.ini and DNS with new IP
# 3. Run Ansible (brings up a fresh, empty server)
./deploy.sh configure

# 4. SSH in and restore
ssh cobular@<new-ip>
cd ~/matrix
echo 'AGE-SECRET-KEY-...' > /tmp/age.key && chmod 600 /tmp/age.key

# Pull the newest backup down from blob
sudo rclone copy bk:backups/pg_<DATE>.sql.gz.age /tmp/
sudo rclone copy bk:backups/keys_<DATE>.tar.gz.age /tmp/

# Stop everything except Postgres — the dump has DROP DATABASE statements,
# which fail while Synapse/the bridge hold connections
docker compose stop synapse mautrix-discord caddy
docker compose up -d postgres

# Restore (dumps are taken with --clean, so this replaces the fresh DBs)
age -d -i /tmp/age.key /tmp/pg_<DATE>.sql.gz.age | gunzip \
  | docker exec -i postgres psql -U postgres

# Restore the signing key (server identity — federation breaks without it)
age -d -i /tmp/age.key /tmp/keys_<DATE>.tar.gz.age \
  | sudo tar xzf - -C synapse-data/

docker compose up -d
rm /tmp/age.key
```

## Teardown

```bash
# Deletes EVERYTHING — VM, disk, all data
./deploy.sh destroy
```

## Architecture

For the one-time encrypted Discord history export and per-user onboarding
workflow, see [`docs/history-key-migration.md`](docs/history-key-migration.md).

```
Internet
  │
  ├── DNS (matrix.yourdomain.com + element.yourdomain.com → VM public IP)
  │
  ├── Azure VM (Standard_B2s, 2 vCPU, 4 GiB RAM + 4 GiB swap, 256 GiB SSD)
  │     │  (system-assigned managed identity → blob write access)
  │     │
  │     ├── Caddy (:443, :80, :8448) — TLS termination, static files
  │     ├── Synapse (:8008 internal) — Matrix homeserver
  │     ├── PostgreSQL (:5432 internal) — all persistent state
  │     └── mautrix-discord (:29334 internal) — Discord bridge
  │
  └── Azure Blob Storage — age-encrypted backups, 30-day lifecycle expiry
```

All services are Docker containers on a shared bridge network. Only Caddy is
exposed to the internet; SSH is allowlisted to a single source IP
(`sshAllowedSourceIP` in `arm/matrix-infra.parameters.json`). If your IP
changes, update the parameter and re-run `./deploy.sh infra` — or use
`az vm run-command invoke` as break-glass access.

## Cost

~$54/month on Azure (B2s + 256 GiB SSD + static IP; backup storage is
pennies). Can drop to ~$22/month on Hetzner — the Ansible playbook works on
any Ubuntu 24.04 host (the blob backup upload is the only Azure-specific bit).
