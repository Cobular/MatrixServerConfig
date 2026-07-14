#!/bin/bash
# Build and run the one-time Discord history-key exporter on the Matrix VM.

set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
INVENTORY="$ROOT/ansible/inventory.ini"
TOOL_DIR="$ROOT/tools/history-export"
IMAGE="matrix-history-export:local"

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
    ' "$INVENTORY"
}

HOST=$(inventory_value ansible_host)
USER=$(inventory_value ansible_user)
USER=${USER:-cobular}
TARGET="$USER@$HOST"
REMOTE_HOME="/home/$USER"
REMOTE_TOOL="$REMOTE_HOME/matrix/history-export-tool"
SSH_OPTS=(
    -o BatchMode=yes
    -o ConnectTimeout=10
    -o ConnectionAttempts=1
    -o ServerAliveInterval=5
    -o ServerAliveCountMax=2
)

usage() {
    cat <<'EOF'
Usage:
  scripts/history-export.sh build
  scripts/history-export.sh inventory
  scripts/history-export.sh validate MANIFEST.yml
  scripts/history-export.sh export MANIFEST.yml [REMOTE_OUTPUT_DIR]

The build command copies only tools/history-export to the VM and builds a local
Docker image. Inventory and validate are read-only. Export writes encrypted key
packages and separate passphrase files under the specified remote directory.
EOF
}

require_target() {
    if [[ -z "$HOST" || "$HOST" == "PASTE_VM_IP_HERE" ]]; then
        echo "ERROR: Set ansible_host in $INVENTORY" >&2
        exit 1
    fi
}

remote_run() {
    local mode="$1"
    shift
    ssh "${SSH_OPTS[@]}" "$TARGET" bash -s -- "$mode" "$IMAGE" "$REMOTE_HOME" "$@" <<'REMOTE'
set -euo pipefail
mode="$1"
image="$2"
home_dir="$3"
shift 3

network=$(docker inspect mautrix-discord --format '{{range $name, $_ := .NetworkSettings.Networks}}{{$name}}{{end}}')
if [[ -z "$network" ]]; then
    echo "ERROR: Could not identify the mautrix-discord Docker network" >&2
    exit 1
fi

args=(
    --rm
    --network "$network"
    --volume "$home_dir/matrix/discord-data/config.yaml:/config/bridge.yaml:ro"
    --volume "$home_dir/matrix/synapse-data/homeserver.yaml:/config/homeserver.yaml:ro"
)

case "$mode" in
    inventory)
        docker run "${args[@]}" "$image" inventory
        ;;
    validate)
        manifest="$1"
        docker run "${args[@]}" \
            --volume "$manifest:/input/manifest.yml:ro" \
            "$image" validate --manifest /input/manifest.yml
        ;;
    export)
        manifest="$1"
        output="$2"
        install -d -m 0700 "$output"
        docker run "${args[@]}" \
            --volume "$manifest:/input/manifest.yml:ro" \
            --volume "$output:/output" \
            "$image" export --manifest /input/manifest.yml --output-dir /output
        sudo -n chown -R "$(id -u):$(id -g)" "$output"
        printf 'Encrypted packages written to %s\n' "$output"
        ;;
esac
REMOTE
}

require_target
command=${1:-}
case "$command" in
    build)
        tar -C "$TOOL_DIR" -cf - . | ssh "${SSH_OPTS[@]}" "$TARGET" \
            "set -e; rm -rf '$REMOTE_TOOL'; install -d -m 0700 '$REMOTE_TOOL'; tar -xf - -C '$REMOTE_TOOL'; docker build --pull -t '$IMAGE' '$REMOTE_TOOL'"
        ;;
    inventory)
        remote_run inventory
        ;;
    validate|export)
        manifest=${2:-}
        if [[ -z "$manifest" || ! -f "$manifest" ]]; then
            echo "ERROR: $command requires an existing manifest file" >&2
            exit 1
        fi
        if [[ "$manifest" == *:* ]]; then
            echo "ERROR: Manifest paths containing ':' are not supported" >&2
            exit 1
        fi
        remote_manifest="$REMOTE_TOOL/run/manifest.yml"
        ssh "${SSH_OPTS[@]}" "$TARGET" "install -d -m 0700 '$REMOTE_TOOL/run'"
        scp "${SSH_OPTS[@]}" "$manifest" "$TARGET:$remote_manifest"
        ssh "${SSH_OPTS[@]}" "$TARGET" "chmod 0600 '$remote_manifest'"
        if [[ "$command" == "validate" ]]; then
            remote_run validate "$remote_manifest"
        else
            output=${3:-"$REMOTE_HOME/history-exports/$(date -u +%Y%m%d-%H%M%S)"}
            remote_run export "$remote_manifest" "$output"
        fi
        ;;
    *)
        usage
        exit 1
        ;;
esac