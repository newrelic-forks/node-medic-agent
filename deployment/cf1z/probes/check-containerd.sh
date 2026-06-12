#!/usr/bin/env bash
# check-containerd.sh — hack-node-problem-detector probe for containerd liveness.
#
# Coarse liveness check that does NOT depend on crictl, grpcurl, or any
# binary that isn't in NPD's debian-base image. Two assertions:
#   1. /var/run/containerd/containerd.sock exists and is a unix socket.
#   2. A process named "containerd" exists in the host PID namespace.
#
# Requires:
#   - hostPID: true                          (so /proc/<pid> is host-wide)
#   - hostPath /var/run/containerd mounted   (so the socket is visible)
#   - the pod's /host mount layout this script assumes (see paths below)
#
# Known limitation: this catches "containerd died" cleanly but cannot detect
# "containerd is hung but its goroutines are alive". That class of failure
# is partly covered by the helm-managed NPD's kernel-monitor rule
# `task containerd:N blocked for more than N seconds` (kmsg pattern).
# A proper CRI gRPC Status() probe is tracked as a follow-up.
#
# Exit 0 = healthy (NPD condition False), exit 1 = unhealthy (condition True).

set -uo pipefail

SOCK=${CONTAINERD_SOCK:-/host/run/containerd/containerd.sock}
PROC=${CONTAINERD_PROC:-/host/proc}
COMM_NAME=${CONTAINERD_COMM:-containerd}

if [ ! -S "$SOCK" ]; then
  echo "ERROR containerd socket missing or not a socket: $SOCK"
  exit 1
fi

# Walk host PID namespace looking for a process named exactly "containerd".
# /proc/<pid>/comm holds the (truncated, 15-char) command name.
PID_FOUND=
for d in "$PROC"/[0-9]*; do
  # Skip if the entry vanished between glob and read (process exited).
  [ -r "$d/comm" ] || continue
  read -r comm <"$d/comm" 2>/dev/null || continue
  if [ "$comm" = "$COMM_NAME" ]; then
    PID_FOUND=${d##*/}
    break
  fi
done

if [ -z "$PID_FOUND" ]; then
  echo "ERROR no process named '$COMM_NAME' in host PID namespace"
  exit 1
fi

echo "containerd OK (pid $PID_FOUND, sock $SOCK)"
exit 0
