# Multi-stage build for the backoffice POC frontend.
# Stage 1 (node): runs `npm ci && npm run build` to produce dist/.
# Stage 2 (nginx): serves dist/ as a static SPA and reverse-proxies the hub API
# so the whole app is single-origin (no CORS).

# ── Stage 1: build ────────────────────────────────────────────────────────────
FROM node:22-alpine AS node

WORKDIR /app

# Install dependencies first (layer-cached separately from source).
COPY package.json package-lock.json ./
RUN npm ci

# Copy source and produce the static build.
COPY . .
RUN npm run build

# ── Stage 2: serve ────────────────────────────────────────────────────────────
FROM nginx:alpine AS final

# Remove the default nginx config; the entrypoint regenerates it from the template.
RUN rm -f /etc/nginx/conf.d/default.conf

# Install the nginx config template. The official nginx:alpine entrypoint runs
# envsubst over /etc/nginx/templates/*.template and writes to /etc/nginx/conf.d/.
# NGINX_ENVSUBST_FILTER restricts substitution to only ${BACKOFFICE_API_KEY},
# which leaves all native nginx variables ($host, $remote_addr, etc.) untouched.
COPY nginx.conf.template /etc/nginx/templates/default.conf.template

ENV NGINX_ENVSUBST_FILTER="BACKOFFICE_API_KEY"

# Copy the Vite build output into the nginx document root.
COPY --from=node /app/dist /usr/share/nginx/html

EXPOSE 80
