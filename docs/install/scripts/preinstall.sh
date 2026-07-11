#!/bin/sh
# Pre-install (deb + rpm) — create the system user/group so the
# postinstall step can chown the data dir. Idempotent.

set -e

if ! getent group artist-alley >/dev/null; then
    groupadd --system artist-alley
fi

if ! getent passwd artist-alley >/dev/null; then
    useradd --system \
        --gid artist-alley \
        --home-dir /var/lib/artist-alley \
        --no-create-home \
        --shell /usr/sbin/nologin \
        --comment "artist-alley service user" \
        artist-alley
fi

exit 0
