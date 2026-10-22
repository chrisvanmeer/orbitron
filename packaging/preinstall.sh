#!/bin/sh
set -eu

if ! getent group orbitron >/dev/null 2>&1; then
    groupadd --system orbitron
fi

if ! id -u orbitron >/dev/null 2>&1; then
    useradd --system --gid orbitron --home-dir /var/lib/orbitron \
        --no-create-home --shell /bin/false orbitron
fi
