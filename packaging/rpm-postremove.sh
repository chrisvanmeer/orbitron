#!/bin/sh
set -eu

# On erase, reload systemd after the unit file has been removed. During an
# upgrade the replacement package's postinstall already reloaded systemd.
if [ "${1:-0}" -eq 0 ] && [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
fi
