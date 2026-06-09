# Test Guild Setup Guide

This guide walks through creating a throwaway Discord server, a bot application,
and the OAuth2 configuration needed before M2 can run against real Discord APIs
and before M5's integration tests execute.

---

## 1. Create a throwaway Discord server

1. Open Discord (desktop or browser).
2. Click the **+** icon in the left sidebar.
3. Select **Create My Own** → **For me and my friends** (or any server type).
4. Name it something like `hub-test-YYYY-MM-DD` so it is obviously ephemeral.
5. After creation, note the **Server ID**:
   - Enable Developer Mode in Discord: **User Settings** → **Advanced** → **Developer Mode**.
   - Right-click the server name in the sidebar → **Copy Server ID**.
   - Save this as `DISCORD_GUILD_ID` in your `.env`.

## 2. Create a Bot Application

1. Go to https://discord.com/developers/applications.
2. Click **New Application**, name it `discord-support-hub-test`.
3. In the **Bot** tab:
   - Click **Add Bot**.
   - Under **TOKEN**, click **Reset Token** → copy it.
   - Save this as `DISCORD_BOT_TOKEN` in your `.env`. **Do not commit this value.**
   - Enable **SERVER MEMBERS INTENT** and **GUILDS INTENT** under **Privileged Gateway Intents**.
4. Note the **Application ID** (visible in the **General Information** tab) — you will need it
   when constructing the OAuth2 authorization URL in step 4.

## 3. Create the Agent role and category

Before inviting the bot, set up the Discord server structure the hub expects:

1. **Create the Agent role:**
   - In your test server, go to **Server Settings** → **Roles** → **Create Role**.
   - Name it `Agent` (or any name — the hub uses the role ID, not the name).
   - Right-click the role → **Copy Role ID**.
   - Save this as `DISCORD_AGENT_ROLE_ID` in your `.env`.

2. **Create the support category:**
   - In your test server, right-click on the channel list area → **Create Category**.
   - Name it `Support Spaces` (the name is cosmetic; the hub uses the category ID).
   - Right-click the category → **Copy ID**.
   - Save this as `DISCORD_CATEGORY_ID` in your `.env`.

3. **Set the category-level Agent-role permission overwrite** (VIEW_CHANNEL = allow for the Agent role):
   - Right-click the category → **Edit Category** → **Permissions**.
   - Click **+** next to **Roles/Members**, select the Agent role.
   - Enable **View Channel** for the Agent role. This is what grants agents access to all spaces.

## 4. Invite the bot to the test server

The bot needs specific permissions to operate. Construct the OAuth2 invite URL:

```
https://discord.com/api/oauth2/authorize
  ?client_id=YOUR_APPLICATION_ID
  &permissions=YOUR_PERMISSION_INTEGER
  &scope=bot%20applications.commands
```

Required permissions (compute the integer at https://discordapi.com/permissions.html):
- **Manage Roles** — assign/remove the Agent role (reserved to the bot, NFR-13).
- **Manage Channels** — create and configure channels.
- **View Channels** — read channel state for reconciliation.
- **Send Messages** — post the welcome pin (FR-15 static).
- **Manage Messages** — pin the welcome message.
- **Create Instant Invite** — reserved to the bot only (NFR-14; deny for everyone else).
- **Kick Members** — for scope=server expulsion (FR-19).
- **Manage Permissions** — set per-user permission overwrites.

Visit the URL, select your test server, and click **Authorize**.

## 5. Configure OAuth2 for "Connect with Discord"

The hub uses Discord OAuth2 (`guilds.join` + `identify`) to add users to the guild
without invite links (NFR-14). Configure the redirect URL:

1. In https://discord.com/developers/applications → your app → **OAuth2** → **General**.
2. Under **Redirects**, add your callback URL, e.g.:
   - Local dev: `http://localhost:8080/v1/oauth/discord/callback`
   - Staging/production: `https://your-hub-domain.example.com/v1/oauth/discord/callback`
3. Save the **Client ID** as `DISCORD_OAUTH_CLIENT_ID` in `.env`.
4. Generate a **Client Secret** → save as `DISCORD_OAUTH_CLIENT_SECRET` in `.env`.
5. Set `DISCORD_OAUTH_REDIRECT_URL` to the redirect URL you registered.

The OAuth2 scopes the hub requests are:
- `identify` — to link the Discord user ID to the hub user row.
- `guilds.join` — to add the user to the guild via the bot's `Add Guild Member` endpoint.

## 6. Generate the encryption key

OAuth2 tokens are encrypted at rest with AES-256-GCM (NFR-6). Generate a 32-byte key:

```bash
openssl rand -base64 32
```

Save the output as `ENCRYPTION_KEY` in your `.env`.

## 7. Verify connectivity

With `.env` populated, start the stack:

```bash
make up
```

Check the readiness probe — it pings Postgres and Valkey:

```bash
curl http://localhost:8080/readyz
# Expected: {"postgres":"ok","valkey":"ok"}
```

The bot session is created at worker startup. Confirm the worker logs show
`discord: session created` without error.

---

## Checklist

- [ ] Test server created; `DISCORD_GUILD_ID` saved.
- [ ] Bot application created; `DISCORD_BOT_TOKEN` saved (never committed).
- [ ] Agent role created; `DISCORD_AGENT_ROLE_ID` saved.
- [ ] Support category created with Agent-role VIEW_CHANNEL overwrite; `DISCORD_CATEGORY_ID` saved.
- [ ] Bot invited to the server with all required permissions.
- [ ] OAuth2 redirect URL registered; `DISCORD_OAUTH_CLIENT_ID`, `DISCORD_OAUTH_CLIENT_SECRET`, `DISCORD_OAUTH_REDIRECT_URL` saved.
- [ ] `ENCRYPTION_KEY` generated and saved.
- [ ] `make up` succeeds; `GET /readyz` returns green.

Once this checklist is complete, the stack is ready for M2's real provisioning run
and for M5's integration tests against the test guild.
