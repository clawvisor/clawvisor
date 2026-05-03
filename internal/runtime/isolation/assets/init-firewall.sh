#!/usr/bin/env bash
set -euo pipefail

: "${CLAWVISOR_PROXY_PORT:?CLAWVISOR_PROXY_PORT is required}"
: "${CLAWVISOR_API_PORT:?CLAWVISOR_API_PORT is required}"

valid_port() {
    local port="$1"
    [[ "$port" =~ ^[0-9]+$ ]] && (( port >= 1 && port <= 65535 ))
}

valid_port "$CLAWVISOR_PROXY_PORT" || { echo "init-firewall: invalid CLAWVISOR_PROXY_PORT=$CLAWVISOR_PROXY_PORT" >&2; exit 1; }
valid_port "$CLAWVISOR_API_PORT" || { echo "init-firewall: invalid CLAWVISOR_API_PORT=$CLAWVISOR_API_PORT" >&2; exit 1; }

# Resolve the egress target host to its IPv4 address. The default is
# host.docker.internal (PR1 per-invocation flow): on Docker Desktop it points
# at the host via the VM's port-forwarding magic; on Linux Engine 20.10+ (with
# `--add-host host.docker.internal:host-gateway` set on the holder) it points
# at the bridge gateway IP. The host-side forwarders bound on 0.0.0.0 are
# reachable at this IP.
#
# The compose isolation flow (PR3) overrides this via CLAWVISOR_HOST_TARGET to
# point at the standalone `clawvisor proxy expose` host (which may be an IP
# literal or a DNS hostname).
HOST_TARGET="${CLAWVISOR_HOST_TARGET:-host.docker.internal}"
if [[ "$HOST_TARGET" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    HOST_IP="$HOST_TARGET"
else
    HOST_IP=$(getent ahostsv4 "$HOST_TARGET" | awk '{print $1; exit}' || true)
fi
if [[ -z "$HOST_IP" ]]; then
    echo "init-firewall: could not resolve $HOST_TARGET (IPv4)" >&2
    exit 1
fi
if [[ ! "$HOST_IP" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "init-firewall: resolved $HOST_TARGET to non-IPv4 address: $HOST_IP" >&2
    exit 1
fi
mkdir -p /run/clawvisor
printf '%s' "$HOST_IP" > /run/clawvisor/host.ip

iptables -F OUTPUT
iptables -P OUTPUT DROP

iptables -A OUTPUT -o lo -j ACCEPT
iptables -A OUTPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
iptables -A OUTPUT -p tcp -d "$HOST_IP" --dport "$CLAWVISOR_PROXY_PORT" -j ACCEPT
iptables -A OUTPUT -p tcp -d "$HOST_IP" --dport "$CLAWVISOR_API_PORT" -j ACCEPT

if [[ -n "${CLAWVISOR_TEST_ALLOW_PORT:-}" ]]; then
    if valid_port "$CLAWVISOR_TEST_ALLOW_PORT"; then
        iptables -A OUTPUT -p tcp -d "$HOST_IP" --dport "$CLAWVISOR_TEST_ALLOW_PORT" -j ACCEPT
        echo "init-firewall: allowing test port $HOST_IP:$CLAWVISOR_TEST_ALLOW_PORT" >&2
    else
        echo "init-firewall: invalid CLAWVISOR_TEST_ALLOW_PORT=$CLAWVISOR_TEST_ALLOW_PORT" >&2
        exit 1
    fi
fi

iptables -A OUTPUT -p tcp -j REJECT --reject-with tcp-reset

if command -v ip6tables >/dev/null 2>&1; then
    ip6tables -F OUTPUT || true
    ip6tables -P OUTPUT DROP || true
    ip6tables -A OUTPUT -o lo -j ACCEPT || true
    ip6tables -A OUTPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT || true
    ip6tables -A OUTPUT -p tcp -j REJECT --reject-with tcp-reset || true
fi

mkdir -p /run/clawvisor
touch /run/clawvisor/firewall.ready

echo "init-firewall: rules installed" >&2
iptables -S OUTPUT >&2
