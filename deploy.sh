#!/bin/bash
# deploy.sh — Full deployment: Azure infrastructure + Ansible configuration.
#
# Prerequisites:
#   - Azure CLI installed and logged in (az login)
#   - Ansible installed (pip install ansible)
#   - Edit arm/matrix-infra.parameters.json (SSH key)
#   - Edit ansible/group_vars/matrix.yml (domain, password)
#   - Edit ansible/inventory.ini (after ARM deploy prints the IP)
#
# Usage:
#   ./deploy.sh infra     # Step 1: Provision Azure VM
#   ./deploy.sh configure # Step 2: Run Ansible playbook
#   ./deploy.sh backup    # Run and verify an encrypted offsite backup
#   ./deploy.sh all       # Both steps (pauses between for DNS setup)
#   ./deploy.sh destroy   # Tear down everything

set -euo pipefail

RESOURCE_GROUP="matrix-rg"
ARM_DIR="arm"
ANSIBLE_DIR="ansible"

inventory_value() {
    local key="$1"
    awk -v key="$key" '
        /^[[:space:]]*#/ || /^[[:space:]]*$/ { next }
        $1 !~ /^\[/ {
            for (i = 1; i <= NF; i++) {
                if ($i ~ ("^" key "=")) {
                    sub("^" key "=", "", $i)
                    print $i
                    exit
                }
            }
        }
    ' "$ANSIBLE_DIR/inventory.ini"
}

cmd_infra() {
    echo "=== Step 1: Provisioning Azure Infrastructure ==="
    echo ""

    if grep -q "PASTE_YOUR_SSH_PUBLIC_KEY_HERE" "$ARM_DIR/matrix-infra.parameters.json"; then
        echo "ERROR: Paste your SSH public key in $ARM_DIR/matrix-infra.parameters.json"
        exit 1
    fi

    LOCATION=$(jq -r '.parameters.location.value' "$ARM_DIR/matrix-infra.parameters.json")

    echo "Creating resource group in $LOCATION..."
    az group create --name "$RESOURCE_GROUP" --location "$LOCATION" --output none

    echo "Deploying ARM template (2-3 minutes)..."
    RESULT=$(az deployment group create \
        --resource-group "$RESOURCE_GROUP" \
        --template-file "$ARM_DIR/matrix-infra.json" \
        --parameters "@$ARM_DIR/matrix-infra.parameters.json" \
        --output json)

    PUBLIC_IP=$(echo "$RESULT" | jq -r '.properties.outputs.publicIpAddress.value')
    SSH_CMD=$(echo "$RESULT" | jq -r '.properties.outputs.sshCommand.value')

    echo ""
    echo "=== Infrastructure Ready ==="
    echo "Public IP:  $PUBLIC_IP"
    echo "SSH:        $SSH_CMD"
    echo ""
    echo "Next:"
    echo "  1. Point your DNS at $PUBLIC_IP"
    echo "  2. Update ansible/inventory.ini with: ansible_host=$PUBLIC_IP"
    echo "  3. Run: ./deploy.sh configure"
}

cmd_configure() {
    echo "=== Step 2: Configuring Server with Ansible ==="
    echo ""

    if grep -q "PASTE_VM_IP_HERE" "$ANSIBLE_DIR/inventory.ini"; then
        echo "ERROR: Update the VM IP in $ANSIBLE_DIR/inventory.ini"
        exit 1
    fi

    if grep -q "yourdomain.com" "$ANSIBLE_DIR/group_vars/matrix/vars.yml"; then
        echo "ERROR: Set matrix_server_name and matrix_hostname in $ANSIBLE_DIR/group_vars/matrix/vars.yml"
        exit 1
    fi

    if [ ! -f "$ANSIBLE_DIR/.vault-pass" ]; then
        echo "ERROR: $ANSIBLE_DIR/.vault-pass not found (ansible-vault password)."
        echo "Restore it from your password manager — secrets in group_vars/matrix/vault.yml"
        echo "cannot be decrypted without it."
        exit 1
    fi

    echo "Installing Ansible Galaxy dependencies..."
    cd "$ANSIBLE_DIR"
    ansible-galaxy collection install -r requirements.yml

    echo ""
    echo "Running playbook..."
    ansible-playbook -i inventory.ini playbook.yml

    cd ..
}

cmd_backup() {
    local backup_host backup_home backup_user ssh_target result

    backup_host=$(inventory_value ansible_host)
    backup_user=$(inventory_value ansible_user)
    backup_user=${backup_user:-cobular}
    backup_home="/home/$backup_user"

    if [ -z "$backup_host" ] || [ "$backup_host" = "PASTE_VM_IP_HERE" ]; then
        echo "ERROR: Set ansible_host in $ANSIBLE_DIR/inventory.ini"
        exit 1
    fi

    ssh_target="$backup_user@$backup_host"
    echo "=== Running encrypted server backup on $ssh_target ==="
    result=$(ssh \
        -o BatchMode=yes \
        -o ConnectTimeout=10 \
        -o ConnectionAttempts=1 \
        -o ServerAliveInterval=5 \
        -o ServerAliveCountMax=2 \
        "$ssh_target" \
        bash -s -- "$backup_home" <<'REMOTE'
set -euo pipefail
backup_home="$1"

timeout 30m sudo -n "$backup_home/matrix/backup.sh"
timeout 30s sudo -n bash -s -- "$backup_home" <<'VERIFY'
set -euo pipefail
backup_home="$1"
stamp=$(tail -n 1 "$backup_home/backups/backup.log" | sed -n 's/.*pg_\([0-9-]*\)\.sql\.gz\.age.*/\1/p')
test -n "$stamp"
pg="pg_${stamp}.sql.gz.age"
keys="keys_${stamp}.tar.gz.age"
test -s "$backup_home/backups/$pg"
test -s "$backup_home/backups/$keys"
remote=$(rclone lsf bk:backups --files-only --include "$pg" --include "$keys")
printf '%s\n' "$remote" | grep -Fx "$pg" >/dev/null
printf '%s\n' "$remote" | grep -Fx "$keys" >/dev/null
printf 'Verified local and offsite artifacts for %s\n' "$stamp"
VERIFY
REMOTE
    )
    printf '%s\n' "$result"
}

cmd_all() {
    cmd_infra
    echo ""
    echo "════════════════════════════════════════════════════════"
    echo " Before continuing, you need to:"
    echo "   1. Point your DNS at the IP above"
    echo "   2. Update ansible/inventory.ini with the IP"
    echo "   3. Update ansible/group_vars/matrix.yml with your config"
    echo "════════════════════════════════════════════════════════"
    echo ""
    read -p "Press Enter when DNS and config are ready..."
    echo ""
    cmd_configure
}

cmd_destroy() {
    echo "This will DELETE everything: VM, disk, all data."
    read -p "Are you sure? Type 'yes' to confirm: " confirm
    if [ "$confirm" = "yes" ]; then
        az group delete --name "$RESOURCE_GROUP" --yes --no-wait
        echo "Deletion started (runs in background)."
    else
        echo "Cancelled."
    fi
}

case "${1:-help}" in
    infra)     cmd_infra ;;
    configure) cmd_configure ;;
    backup)    cmd_backup ;;
    all)       cmd_all ;;
    destroy)   cmd_destroy ;;
    *)
        echo "Usage: ./deploy.sh {infra|configure|backup|all|destroy}"
        echo ""
        echo "  infra      Provision Azure VM (ARM template)"
        echo "  configure  Configure server (Ansible playbook)"
        echo "  backup     Run and verify an encrypted offsite backup"
        echo "  all        Both steps with a pause for DNS setup"
        echo "  destroy    Delete everything"
        ;;
esac
