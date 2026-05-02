#!/usr/bin/env bash
set -euo pipefail
/usr/local/bin/init-firewall.sh
exec tail -f /dev/null
