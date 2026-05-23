# artist-alley — PHP-FPM image for ResourceSpace
#
# Clean-room: no proprietary corporate certs, no Matomo, no hardcoded user names.
# Mirrors the system dependencies ResourceSpace needs for its preview pipeline
# (ImageMagick, FFmpeg, ExifTool, LibreOffice, etc.) plus the PHP extensions
# RS uses (gd, intl, ldap, exif, pdo_mysql, mysqli, zip, apcu).

FROM php:8.3-fpm-bookworm

ARG UID=1000
ARG GID=1000
ARG TZ=UTC

ENV DEBIAN_FRONTEND=noninteractive

# System dependencies: RS preview pipeline + PHP build deps
RUN apt-get update && apt-get install -y --no-install-recommends \
        antiword \
        ca-certificates \
        cron \
        default-mysql-client \
        exiftool \
        ffmpeg \
        ghostscript \
        git \
        imagemagick \
        libfreetype6-dev \
        libicu-dev \
        libjpeg62-turbo-dev \
        libldap2-dev \
        libpng-dev \
        libpq-dev \
        libzip-dev \
        libreoffice \
        poppler-utils \
        unoconv \
        wget \
        zip \
    && rm -rf /var/lib/apt/lists/*

# PHP extensions used by ResourceSpace + pdo_pgsql for future Postgres sidecars
RUN docker-php-ext-configure gd --with-freetype --with-jpeg \
    && docker-php-ext-install -j"$(nproc)" \
        gd \
        intl \
        ldap \
        exif \
        pdo_mysql \
        pdo_pgsql \
        mysqli \
        zip \
    && docker-php-ext-enable mysqli

# APCu (RS uses it for user-cache fast paths)
RUN pecl install apcu \
    && docker-php-ext-enable apcu

# php.ini — production baseline with RS-friendly upload limits
RUN cp /usr/local/etc/php/php.ini-production /usr/local/etc/php/php.ini \
    && sed -i \
        -e 's/memory_limit = .*/memory_limit = 512M/' \
        -e 's/post_max_size = .*/post_max_size = 5000M/' \
        -e 's/upload_max_filesize = .*/upload_max_filesize = 5000M/' \
        -e "s|;date.timezone =.*|date.timezone = ${TZ}|" \
        -e 's/max_input_vars = .*/max_input_vars = 10000/' \
        /usr/local/etc/php/php.ini

# Match container user to host UID/GID so bind-mounted files stay sane
RUN groupadd -og "${GID}" app \
    && useradd  -ou "${UID}" -g "${GID}" -d /home/app -m app \
    && sed -i "s|^user = .*|user = ${UID}|" /usr/local/etc/php-fpm.d/www.conf \
    && sed -i "s|^group = .*|group = ${GID}|" /usr/local/etc/php-fpm.d/www.conf

WORKDIR /var/www/html

# RS expects these dirs present and writable
RUN mkdir -p include filestore gfx/homeanim /var/log/artist-alley \
    && chown -R "${UID}:${GID}" /var/www/html /var/log/artist-alley

# RS offline-jobs runner. Triggered every minute via cron.
RUN echo "* * * * * ${UID} /usr/local/bin/php /var/www/html/pages/tools/offline_jobs.php > /dev/null 2>&1" \
    > /etc/cron.d/artist-alley \
    && chmod 0644 /etc/cron.d/artist-alley

COPY infra/docker/php-fpm/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["php-fpm"]
