# Dev image for the SvelteKit frontend (Vite HMR).
#
# Production builds run `npm run build` in CI and pipe the static
# output into the Go binary via //go:embed — there is no prod image
# for this service.

FROM node:22-alpine

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
