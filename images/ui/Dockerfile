# agentdev UI (#48). Published as ghcr.io/valentinalexeev/agentdev-ui (#28).
# The build output (static files) is the same on every platform, so it is built once on the builder's own platform,
# not under QEMU per target. Lint, type check and build also run in CI (lint job).
FROM --platform=$BUILDPLATFORM node:24.11.1-bookworm-slim AS build
WORKDIR /src
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
RUN npm run build

# nginx-unprivileged: non-root (uid 101), port 8080. Its entrypoint renders /etc/nginx/templates/*.template with
# envsubst at start: the API upstream and /config.json (OIDC issuer, client id) come from the pod's environment.
FROM nginxinc/nginx-unprivileged:1.29.3-alpine
COPY nginx/default.conf.template /etc/nginx/templates/default.conf.template
COPY --chmod=0755 nginx/15-env.envsh /docker-entrypoint.d/15-env.envsh
COPY nginx/security.conf /etc/nginx/snippets/security.conf
COPY --from=build /src/dist /usr/share/nginx/html
ENV API_UPSTREAM=http://agentdev-api:8080 OIDC_CLIENT_ID=agentdev
EXPOSE 8080
