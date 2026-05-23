#!/bin/sh
# artist-alley php-fpm entrypoint.
# - Starts cron in the background for RS's offline_jobs.php runner.
# - Hands off to the original CMD (php-fpm by default).

set -e

service cron start >/dev/null 2>&1 || cron

exec "$@"
