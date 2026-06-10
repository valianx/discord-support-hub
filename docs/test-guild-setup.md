# Test Guild Setup Guide

This guide walks through creating a throwaway Discord server, a bot application,
the bot permissions, the SMTP relay, and the per-merchant invite-with-role link
needed before M2/M6 can run against real Discord APIs and before M5's integration
tests execute. Access is role-based: each merchant gets one Discord role, and
collaborators join through a native invite-with-role link the hub stores and emails.

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
   - Enable **GUILDS INTENT** under **Privileged Gateway Intents**. The hub is REST-only:
     it does **not** open a gateway connection and does **not** need the Server Members
     Intent (the merchant role is bound natively by the invite-with-role link, not by a
     join-event listener).
4. Note the **Application ID** (visible in the **General Information** tab) — you will need it
   when constructing the bot invite URL in step 4.

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

The bot needs specific permissions to operate. Construct the bot invite URL:

```
https://discord.com/api/oauth2/authorize
  ?client_id=YOUR_APPLICATION_ID
  &permissions=YOUR_PERMISSION_INTEGER
  &scope=bot%20applications.commands
```

(The `bot` scope here authorizes the bot into the guild — this is the standard bot
install flow, not a per-user OAuth2 authorize. The hub performs no per-user OAuth2.)

Required permissions (compute the integer at https://discordapi.com/permissions.html):
- **Manage Roles** — create the merchant role and assign/remove the Agent role (reserved to the bot, NFR-13).
- **Manage Channels** — create and configure channels.
- **View Channels** — read channel state for reconciliation.
- **Send Messages** — post the welcome pin (FR-15 static) and the `#bienvenida` message.
- **Manage Messages** — pin the welcome message.
- **Create Instant Invite** — reserved to the bot only (NFR-14; deny for everyone else).
- **Kick Members** — for scope=server expulsion (FR-19).
- **Manage Permissions** — set the channel role allows (deny @everyone, allow merchant role + Agent role).

Visit the URL, select your test server, and click **Authorize**.

## 5. Configure the SMTP relay

The hub emails the merchant invite-with-role link itself (Discord does not). Set the
relay in `.env` (config-by-env; the password is never persisted in Postgres or logged):

```
SMTP_HOST=smtp.example.com
SMTP_PORT=587
SMTP_USERNAME=your-relay-user
SMTP_PASSWORD=your-relay-password   # do not commit
SMTP_FROM=support@example.com
```

A configurable welcome message template (e.g. "Welcome to Support ...") is set in the
hub config; the agent suffix and welcome text carry no hard-coded brand name.

## 6. Create a per-merchant invite-with-role link (per merchant, in the Discord client)

This is the one non-automatable step: the bot REST API can create an invite but
**cannot attach a role to it** (the `roles` field is silently ignored — it is a
Discord-client-only feature). For each merchant, after the hub has provisioned its
space (which auto-creates the merchant role), create the invite by hand:

1. In the Discord client, right-click the **merchant's channel** (or the server) →
   **Invite People** → **Edit invite link**.
2. In the **"Roles (opcional)"** / **"Assign a role"** dropdown, select that merchant's
   role (named per the hub's role template, e.g. `Merchant: Acme`).
3. Set the link to not expire and to unlimited uses (it is reused for every collaborator
   of that merchant).
4. Copy the link and store it via the API:
   `PUT /v1/merchants/{merchantId}/invite` with body `{"invite_link": "https://discord.gg/..."}`.

The hub emails this stored link to each registered collaborator on `:send-invite`.
Until a link is stored, `:send-invite` for that merchant returns `409`.

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
- [ ] Bot application created; `DISCORD_BOT_TOKEN` saved (never committed). GUILDS INTENT enabled; Server Members Intent NOT required.
- [ ] Agent role created; `DISCORD_AGENT_ROLE_ID` saved.
- [ ] Support category created with Agent-role VIEW_CHANNEL overwrite; `DISCORD_CATEGORY_ID` saved.
- [ ] Bot invited to the server with all required permissions.
- [ ] SMTP relay configured; `SMTP_HOST`, `SMTP_PORT`, `SMTP_USERNAME`, `SMTP_PASSWORD`, `SMTP_FROM` saved (password never committed).
- [ ] For each test merchant: space provisioned (merchant role auto-created), invite-with-role link created in the client and stored via `PUT /merchants/{id}/invite`.
- [ ] `make up` succeeds; `GET /readyz` returns green.

Once this checklist is complete, the stack is ready for M2's real provisioning run,
M6's role-per-merchant + invite-email flow, and M5's integration tests against the test guild.
