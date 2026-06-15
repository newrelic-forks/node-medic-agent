#!/usr/bin/env bash
# check-kubelet-healthz.sh — node-medic-agent probe for kubelet liveness.
#
# Probes kubelet's read-only healthz endpoint (default 127.0.0.1:10248/healthz).
# Same endpoint kubelet itself uses for startup/liveness probes — if this
# returns 200, the kubelet's main reconcile loops are running.
#
# Requires the pod to run with hostNetwork=true so 127.0.0.1 reaches the
# host's kubelet (kubelet binds healthz to localhost by default).
#
# Exit code 0 = healthy (NPD will set the condition to False),
# exit code 1 = unhealthy (NPD will set the condition to True).
# Stdout is captured by NPD as the message and is bounded by max_output_length.

set -uo pipefail

HOST=${KUBELET_HEALTHZ_HOST:-127.0.0.1}
PORT=${KUBELET_HEALTHZ_PORT:-10248}
URI=${KUBELET_HEALTHZ_PATH:-/healthz}
TIMEOUT_SECS=${KUBELET_HEALTHZ_TIMEOUT:-3}

# bash /dev/tcp lets us speak HTTP without curl. The NPD image is debian-base
# without curl/wget; bash 5 ships /dev/tcp built-in.
if ! exec 3<>/dev/tcp/${HOST}/${PORT} 2>/dev/null; then
  echo "ERROR connect kubelet ${HOST}:${PORT}: refused or unreachable"
  exit 1
fi

printf 'GET %s HTTP/1.0\r\nHost: %s\r\nConnection: close\r\n\r\n' "$URI" "$HOST" >&3

# Read only the status line; healthz returns "ok" but we trust the HTTP code.
if ! read -r -t "$TIMEOUT_SECS" STATUS <&3; then
  exec 3>&- 2>/dev/null
  echo "ERROR kubelet ${HOST}:${PORT}${URI}: read timeout (${TIMEOUT_SECS}s)"
  exit 1
fi

# Drain & close.
exec 3>&- 2>/dev/null

if [[ "$STATUS" == HTTP/*\ 200* ]]; then
  echo "kubelet healthz OK"
  exit 0
fi

# Trim CR; bound message length to keep NPD output sane.
STATUS=${STATUS%$'\r'}
echo "ERROR kubelet ${HOST}:${PORT}${URI}: ${STATUS:-<no response>}"
exit 1
