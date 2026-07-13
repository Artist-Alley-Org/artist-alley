#!/bin/sh
# Post-install (deb + rpm) — fix ownership on data + config dirs, then
# print the next-steps banner. Idempotent.

set -e

# Storage volume — service writes binary blobs + thumbnails here.
chown -R artist-alley:artist-alley /var/lib/artist-alley
chmod 0750 /var/lib/artist-alley

# Config — readable by service user only.
if [ -d /etc/artist-alley ]; then
    chown -R root:artist-alley /etc/artist-alley
    chmod 0750 /etc/artist-alley
    if [ -f /etc/artist-alley/aa.env ]; then
        chmod 0640 /etc/artist-alley/aa.env
    fi
fi

# Reload systemd so the freshly-shipped unit is picked up.
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
fi

cat <<'EOF'

artist-alley installed.

Next steps:
  1. Edit /etc/artist-alley/aa.env to point at your Postgres + storage.
  2. Create the database:
       sudo -u postgres createuser artist_alley
       sudo -u postgres createdb -O artist_alley artist_alley
  3. Start the service:
       sudo systemctl enable --now artist-alley
  4. Open http://localhost:8080 to finish setup.

Docs: https://github.com/Artist-Alley-Org/artist-alley/blob/main/docs/install/README.md

EOF

exit 0
