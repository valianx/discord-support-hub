# Discord Support Hub — Backoffice POC

A single-view backoffice simulator for the Discord Support Hub control-plane API. Built with Vite + React + TypeScript + Tailwind CSS + shadcn/ui.

**Security caveat:** This is a local operator tool. You supply your own backoffice service API key at runtime; it is stored only in your browser session (`sessionStorage`, cleared on tab close) and is never sent anywhere except the hub. Do not deploy this publicly.

---

## Run the whole stack (test/demo)

The fastest way to try the full system — no local Go, Postgres, or Valkey required.

```bash
# From the repo root:
docker compose -f deploy/docker-compose.test.yml up --build
```

This brings up: postgres + valkey + migrate (one-shot) + api + worker + frontend (nginx).
Open http://localhost:3000 when the stack is healthy.

**Mint an API key (first time, or whenever you need a fresh one):**

```bash
docker compose -f deploy/docker-compose.test.yml --profile tools run --rm keygen
```

Copy the raw key printed to stdout. Then in the browser:
1. Click **Settings** (top-right).
2. Paste the key into **Service API Key**.
3. Leave **Hub Base URL** as `/` (nginx proxies to the api — no CORS needed).
4. Click **Save**.

> **Caveat about real Discord provisioning:** the test stack ships with dummy
> Discord credentials. The UI, authentication, and all listing/read endpoints
> work normally. Provisioning jobs (`POST /v1/merchants/{id}/channels`) are
> accepted (`202`) and then fail at Discord because the bot token is fake.
> To provision for real, override the `DISCORD_*` env vars in
> `deploy/docker-compose.test.yml` (or pass them with `--env-file`) with your
> actual bot token, guild ID, agent role ID, and category ID.

---

## Prerequisites

- Node.js 18+ / npm 9+
- A running Discord Support Hub API (`http://localhost:8080` by default)
- A service API key (generate one with `go run ./cmd/keygen` from the hub repo)

---

## Running in development

```bash
# From the web/poc directory:
npm install
npm run dev
# Open http://localhost:5173
```

The Vite dev server proxies `/v1`, `/livez`, and `/readyz` to `http://localhost:8080`, so the browser calls same-origin `/v1/...` and CORS is not an issue in development.

### Entering the API key

1. Click **Settings** (top-right of the page).
2. Paste your service API key into the **Service API Key** field.
3. Leave **Hub Base URL** as `/` when using the dev proxy.
4. Click **Save**.

> **Security note:** your API key is sent to whatever URL is configured as the Hub Base URL — only point this at your own trusted hub.

The key is stored in `sessionStorage` and is cleared when you close the tab. A **Clear Key** button is available in the Settings dialog.

---

## What the UI does

| Feature | Description |
|---|---|
| Connection status | Pings `/readyz` and shows Connected / Unreachable in the header |
| Provision a Space | Form: merchant UUID + space name → `POST /v1/merchants/{id}/channels` → polls job until completion |
| Spaces table | `GET /v1/channels` → table with lifecycle state, ACL state, merchant, dates |
| View Members | `GET /v1/channels/{id}/members` → dialog with ACL status per member |
| Invite Collaborator | `POST /v1/channels/{id}/collaborators` → shows OAuth2 `connect_url` if present |
| Expel Collaborator | `DELETE /v1/channels/{id}/collaborators/{userId}?scope=channel|server` |
| Change Lifecycle | `POST /v1/channels/{id}/lifecycle` → archive / resolve / reopen |

All mutating operations return `202 Accepted` with a job handle; the UI polls `GET /v1/jobs/{id}` and shows job state via a Badge and Sonner toast.

---

## Building for production (static assets)

```bash
npm run build
# Output: dist/
```

To serve the built assets against a remote hub (not the dev proxy), set the CORS_ALLOWED_ORIGINS variable in the hub's environment:

```
CORS_ALLOWED_ORIGINS=https://your-backoffice-host.example.com
```

The Vite proxy is a dev-only convenience; the production build is plain static assets that call the hub API directly from the browser. In that case the hub must include `Access-Control-Allow-Origin` headers for your origin.

---

## Running tests

```bash
npm test
# vitest run — 3 test files, 21 tests
```

Test coverage:
- API client: Authorization header construction, error shape `{code, message}`, URL path building
- Settings: sessionStorage persist / clear for API key and base URL
- Dashboard: smoke render — title, POC badge, Settings button, security banner, API key prompt

---

## Environment variables

| Variable | Required | Description |
|---|---|---|
| `VITE_HUB_BASE_URL` | No | Pre-configures the default Hub Base URL at build time (e.g. `https://hub.example.com`). When set, the Settings dialog pre-fills with this value. Falls back to `/` (Vite dev proxy). Runtime key is never an env var. |

See `.env.example` for documentation. The API key is **runtime-only** — never in env files.

> **Trust caveat:** the API key is sent to whatever Hub Base URL is active (env default or operator-entered). Only point the app at your own trusted hub.

---

## File structure

```
web/poc/
  src/
    lib/
      api.ts          — typed fetch wrapper + all endpoint functions
      settings.ts     — sessionStorage key/URL helpers
      utils.ts        — cn() utility for Tailwind class merging
    components/
      ui/             — shadcn/ui components (Button, Input, Label, Card,
                        Table, Dialog, Select, Badge, Separator,
                        ScrollArea, DropdownMenu)
      Dashboard.tsx   — main single-view dashboard
      SettingsDialog.tsx
      ProvisionSpaceDialog.tsx
      InviteCollaboratorDialog.tsx
      MembersDialog.tsx
      LifecycleDialog.tsx
      SpacesTable.tsx
    test/
      api.test.ts
      settings.test.ts
      Dashboard.test.tsx
      setup.ts
  vite.config.ts      — Tailwind v4 plugin, path alias, dev proxy, vitest config
  .env.example        — documents VITE_HUB_BASE_URL (no real values)
  .gitignore          — excludes node_modules, dist, .env* (keeps .env.example)
```
