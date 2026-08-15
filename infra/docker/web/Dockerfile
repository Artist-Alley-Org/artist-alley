# Dev image for the SvelteKit frontend (Vite HMR).
#
# Production builds run `npm run build` in CI and pipe the static
# output into the Go binary via //go:embed — there is no prod image
# for this service.

FROM node:24-alpine

ARG UID=1000
ARG GID=1000

# Match the host UID/GID so HMR can write into bind-mounted node_modules
# without sudo. Alpine ships `node` as UID 1000 already; only remap when
# the host differs.
RUN if [ "$UID" != "1000" ] || [ "$GID" != "1000" ]; then \
      deluser node && \
      addgroup -g "$GID" node && \
      adduser -D -u "$UID" -G node node; \
    fi

WORKDIR /app

# npm 11 comes from the BASE IMAGE now — there is no explicit pin here
# any more, and that is deliberate (#1039). Keep node on an Active LTS
# line that ships npm 11+; do not drop back to a base whose bundled npm
# is 10.
#
# The constraint that used to need a pin (#1004): web/package-lock.json
# is written by npm 11+, which records a `libc` field on the 10 optional
# platform bindings (@rolldown/*, lightningcss-*). npm 10 does not know
# that field and silently strips all 10 on any write, so the obvious dev
# command — `docker compose exec web npm update <pkg>` — came back with
# 30 phantom deletions on top of the real change, and the next npm 11
# writer put them all back. That round-trip bit #1001. node:22-alpine
# shipped npm 10.9.8, hence `RUN npm install -g npm@11`.
#
# node:24-alpine ships npm 11.17.0 in the image, so the pin was a layer
# that reinstalled npm 11 over npm 11. CI never surfaced the original
# problem because CI only ever runs `npm ci`, which is read-only against
# the lockfile and safe at either version — the WRITE path is what needs
# npm 11, and it is the base image that guarantees it now.

# Install deps first so we get a cached layer that doesn't bust on
# source edits. node_modules ownership has to match the runtime user
# (`node`) because docker-compose masks /app/node_modules with an
# anonymous volume that inherits the image's perms — root-owned
# modules would EACCES on .vite cache writes at startup.
COPY --chown=node:node web/package.json web/package-lock.json* ./
RUN if [ -f package-lock.json ]; then npm ci; else npm install; fi \
 && chown -R node:node /app

USER node
EXPOSE 5173
CMD ["npm", "run", "dev"]
