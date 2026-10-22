#!/bin/sh
set -eu

# Package installation is also used in image builds/chroots without systemd.
if [ -d /run/systemd/system ]; then
    systemctl daemon-reload
    systemctl enable orbitron.service
    if systemctl is-active --quiet orbitron.service; then
        systemctl restart orbitron.service
    else
        systemctl start orbitron.service
    fi
fi
