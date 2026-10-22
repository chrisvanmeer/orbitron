#!/bin/sh
set -eu

# RPM passes 1 for upgrades and 0 for a final erase.
if [ "${1:-0}" -eq 0 ] && [ -d /run/systemd/system ]; then
    systemctl disable --now orbitron.service || true
fi
