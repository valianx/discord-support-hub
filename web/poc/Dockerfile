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

# Remove the default nginx config and install ours.
RUN rm /etc/nginx/conf.d/default.conf
COPY nginx.conf /etc/nginx/conf.d/default.conf

# Copy the Vite build output into the nginx document root.
COPY --from=node /app/dist /usr/share/nginx/html

EXPOSE 80
CMD ["nginx", "-g", "daemon off;"]
