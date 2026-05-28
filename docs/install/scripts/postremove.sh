#!/bin/sh
# Post-remove (deb + rpm) — clean up systemd state on full uninstall.
# Data directory (/var/lib/artist-alley) and the service user are
# left in place so an upgrade-via-uninstall doesn't lose state.
# Purge (deb) or explicit `userdel artist-alley` is the manual path
# for a hard wipe.

set -e

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
fi

exit 0
