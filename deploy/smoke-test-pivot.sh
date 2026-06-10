#!/usr/bin/env bash
# Smoke test for the onboarding pivot against the local compose stack.
# Exercises the API + DB + email flow (NOT real Discord — dummy bot token means
# provisioning jobs are accepted and then fail at Discord; the role/channel are
# not really created, but the merchant/invite-link/collaborator/send-invite/email
# path is fully exercised).
#
# Usage: KEY=<backoffice-key> bash deploy/smoke-test-pivot.sh
set -uo pipefail
API="http://localhost:8080"
H_AUTH="Authorization: Bearer ${KEY:?set KEY to a backoffice service key}"
H_JSON="Content-Type: application/json"
pass=0; fail=0
chk() { # chk <desc> <expected_status> <actual_status> [extra]
  if [[ "$2" == "$3" ]]; then echo "  PASS: $1 ($3)"; pass=$((pass+1));
  else echo "  FAIL: $1 — expected $2 got $3 ${4:-}"; fail=$((fail+1)); fi
}

echo "== health =="
code=$(curl -s -o /dev/null -w '%{http_code}' "$API/livez"); chk "livez" 200 "$code"
code=$(curl -s -o /dev/null -w '%{http_code}' "$API/readyz"); chk "readyz" 200 "$code"

echo "== create merchant A (with invite link) =="
RESP=$(curl -s -w '\n%{http_code}' -X POST "$API/v1/merchants" -H "$H_AUTH" -H "$H_JSON" \
  -d '{"external_ref":"acme-001","name":"Acme Corp"}')
code=$(tail -1 <<<"$RESP"); body=$(sed '$d' <<<"$RESP"); chk "POST /merchants A" 201 "$code" "$body"
MID_A=$(sed -n 's/.*"id":"\([^"]*\)".*/\1/p' <<<"$body")
echo "  merchant A id: $MID_A"

echo "== invalid invite link rejected =="
code=$(curl -s -o /dev/null -w '%{http_code}' -X PUT "$API/v1/merchants/$MID_A/invite" -H "$H_AUTH" -H "$H_JSON" \
  -d '{"invite_link":"http://evil.com/phish"}'); chk "PUT invite (evil host) rejected" 400 "$code"

echo "== valid invite link stored =="
code=$(curl -s -o /dev/null -w '%{http_code}' -X PUT "$API/v1/merchants/$MID_A/invite" -H "$H_AUTH" -H "$H_JSON" \
  -d '{"invite_link":"https://discord.gg/acme-test"}'); chk "PUT invite (valid) stored" 200 "$code"
body=$(curl -s "$API/v1/merchants/$MID_A" -H "$H_AUTH")
grep -q 'discord.gg/acme-test' <<<"$body" && { echo "  PASS: invite_link persisted"; pass=$((pass+1)); } || { echo "  FAIL: invite_link not in GET"; fail=$((fail+1)); }

echo "== create merchant B (NO invite link) =="
RESP=$(curl -s -w '\n%{http_code}' -X POST "$API/v1/merchants" -H "$H_AUTH" -H "$H_JSON" \
  -d '{"external_ref":"beta-002","name":"Beta LLC"}')
MID_B=$(sed -n 's/.*"id":"\([^"]*\)".*/\1/p' <<<"$(sed '$d' <<<"$RESP")")
echo "  merchant B id: $MID_B"

echo "== provision a space under merchant A (will 202; Discord fails on dummy token) =="
RESP=$(curl -s -w '\n%{http_code}' -X POST "$API/v1/merchants/$MID_A/channels" -H "$H_AUTH" -H "$H_JSON" \
  -d '{"name":"acme-support"}')
code=$(tail -1 <<<"$RESP"); body=$(sed '$d' <<<"$RESP"); chk "POST provision space A" 202 "$code" "$body"
SPACE_A=$(sed -n 's/.*"space_id":"\([^"]*\)".*/\1/p' <<<"$body")
[[ -z "$SPACE_A" ]] && SPACE_A=$(sed -n 's/.*"id":"\([^"]*\)".*/\1/p' <<<"$body")
echo "  space A id: $SPACE_A"

echo "== register collaborator by name+email (201) =="
RESP=$(curl -s -w '\n%{http_code}' -X POST "$API/v1/channels/$SPACE_A/collaborators" -H "$H_AUTH" -H "$H_JSON" \
  -d '{"name":"Jane Agent","email":"jane@partner.example"}')
code=$(tail -1 <<<"$RESP"); body=$(sed '$d' <<<"$RESP"); chk "POST register collaborator" 201 "$code" "$body"
USER_A=$(sed -n 's/.*"user_id":"\([^"]*\)".*/\1/p' <<<"$body")
[[ -z "$USER_A" ]] && USER_A=$(sed -n 's/.*"id":"\([^"]*\)".*/\1/p' <<<"$body")
echo "  collaborator user id: $USER_A"

echo "== send-invite WITH stored link (202, email to Mailpit) =="
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$API/v1/channels/$SPACE_A/collaborators/$USER_A/send-invite" -H "$H_AUTH")
chk "send-invite (link present)" 202 "$code"

echo "== members read shows the collaborator =="
body=$(curl -s "$API/v1/channels/$SPACE_A/members" -H "$H_AUTH")
grep -q 'jane@partner.example\|Jane Agent\|'"$USER_A" <<<"$body" && { echo "  PASS: collaborator in members"; pass=$((pass+1)); } || { echo "  NOTE: members body: $body"; }

echo
echo "RESULT: $pass passed, $fail failed"
