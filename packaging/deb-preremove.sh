#!/bin/sh
set -eu

# dpkg passes "upgrade" here while replacing an old version. Keep the service
# running on upgrade; the new package restarts it from postinst.
case "${1:-}" in
    remove|deconfigure)
        if [ -d /run/systemd/system ]; then
            systemctl disable --now orbitron.service || true
        fi
        ;;
esac
