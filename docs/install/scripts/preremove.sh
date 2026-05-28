#!/bin/sh
# Pre-remove (deb + rpm) — stop the service so the binary isn't in
# use when the package files are deleted. Idempotent.

set -e

if command -v systemctl >/dev/null 2>&1; then
    systemctl stop artist-alley 2>/dev/null || true
    systemctl disable artist-alley 2>/dev/null || true
fi

exit 0
