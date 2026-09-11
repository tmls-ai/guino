#!/usr/bin/env bash
# Machine-checkable end-to-end network proof for Guino.
#
# Exercises the REAL guino binary + REAL Docker daemon over the HTTP API and
# asserts the security-critical network behavior. Any failed assertion exits
# non-zero with a diagnostic; a clean run prints "E2E NETWORK: ALL PASS".
#
# Legs:
#   A  bridge   serve → publish → host curl == 200 + body; same sandbox
#                resolves DNS and reaches HTTPS egress.
#   B  internal published host port is INERT (refused) and egress is closed;
#                a none-mode sandbox that requests ports is a 400.
#   C  bind guard: auth off + loopback bind + internal, NO platform_override
#                ⇒ Guino REFUSES to start: non-zero exit AND the committed
#                refusal message on stderr.
#   D  LOCAL-ONLY positive bind-guard leg (opt in with GUINO_E2E_LOCAL_NATIVE=1
#                ONLY on native co-resident Linux): same as C but WITH
#                runtime.platform_override ⇒ Guino STARTS and logs the committed
#                ERROR attestation. Skipped (not failed) by default because
#                the attestation is false-by-construction on proxied/remote/VM
#                Docker and the override is void there (see SECURITY.md §10/§11).
#
# Requires: bash, go, docker, curl, jq. Run from anywhere in the repo.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$(mktemp -d)/guino"
SUFFIX="$$-$(date +%s)"
IMAGE="busybox:latest"  # official busybox ships httpd/wget/nslookup; alpine's stripped busybox lacks httpd
KEY="e2e-secret-key"

for tool in go docker curl jq; do
  command -v "$tool" >/dev/null 2>&1 || { echo "FAIL: missing required tool: $tool" >&2; exit 1; }
done

echo ">> building guino binary"
( cd "$REPO" && go build -o "$BIN" ./cmd/guino )
echo ">> pulling $IMAGE"
docker pull -q "$IMAGE" >/dev/null

# netpolicy_anchor <CONST_NAME> — emit the longest quote/backslash-free
# string segment of a netpolicy message constant, read straight from the
# committed Go source. Leg C/D assert this verbatim instead of a loose
# substring, so the e2e proof tracks the exact committed literal with ZERO
# shell drift. Quote-bearing segments are excluded on purpose: slog's text
# handler escapes embedded quotes in the log, so only a quote-free slice of
# the constant is guaranteed to appear byte-identical on disk.
netpolicy_anchor() {
  awk -v name="$1" '
    $0 ~ "^[[:space:]]*" name " = " { grab=1 }
    grab {
      line=$0
      while (match(line, /"[^"\\]*"/)) {
        seg=substr(line, RSTART+1, RLENGTH-2)
        if (length(seg) > length(best)) best=seg
        line=substr(line, RSTART+RLENGTH)
      }
      if ($0 !~ /\+[[:space:]]*$/) { print best; exit }
    }
  ' "$REPO/internal/runtime/netpolicy/netpolicy.go"
}

EXPECT_REFUSAL="$(netpolicy_anchor MsgBindRefusal)"
EXPECT_OVERRIDE="$(netpolicy_anchor MsgPlatformOverrideAttested)"
[ -n "$EXPECT_REFUSAL" ]  || { echo "FAIL: could not extract MsgBindRefusal anchor" >&2; exit 1; }
[ -n "$EXPECT_OVERRIDE" ] || { echo "FAIL: could not extract MsgPlatformOverrideAttested anchor" >&2; exit 1; }

GUINO_PID=""
NETWORKS=()
WORKDIRS=()

cleanup() {
  [ -n "$GUINO_PID" ] && kill "$GUINO_PID" 2>/dev/null || true
  wait 2>/dev/null || true
  for n in "${NETWORKS[@]:-}"; do [ -n "$n" ] && docker network rm "$n" >/dev/null 2>&1 || true; done
  for d in "${WORKDIRS[@]:-}"; do [ -n "$d" ] && rm -rf "$d" || true; done
}
trap cleanup EXIT

# write_config <file> <port> <netid> <store> <auth> <mode> <unsafe_bridge> <override>
write_config() {
  cat > "$1" <<EOF
server:
  host: "127.0.0.1"
  port: $2
runtime:
  backend: "docker"
  network_id: "$3"
  default_network_mode: "$6"
  reconcile_network: true
  allow_unsafe_bridge: $7
  platform_override: "$8"
sandbox:
  default_image: "$IMAGE"
store:
  path: "$4"
auth:
  enabled: $5
  api_keys:
    - "$KEY"
log:
  level: "info"
  format: "json"
EOF
}

start_den() { # <config> <logfile>
  "$BIN" serve --config "$1" >"$2" 2>&1 &
  GUINO_PID=$!
}

wait_health() { # <port>
  for _ in $(seq 1 50); do
    if curl -fsS "http://127.0.0.1:$1/api/v1/health" >/dev/null 2>&1; then return 0; fi
    if ! kill -0 "$GUINO_PID" 2>/dev/null; then return 1; fi
    sleep 0.2
  done
  return 1
}

stop_den() {
  [ -n "$GUINO_PID" ] && kill "$GUINO_PID" 2>/dev/null || true
  wait "$GUINO_PID" 2>/dev/null || true
  GUINO_PID=""
}

api() { # <method> <port> <path> [json-body]
  local m=$1 p=$2 path=$3 body=${4:-}
  if [ -n "$body" ]; then
    curl -fsS -X "$m" -H "X-API-Key: $KEY" -H "Content-Type: application/json" \
      -d "$body" "http://127.0.0.1:$p/api/v1$path"
  else
    curl -fsS -X "$m" -H "X-API-Key: $KEY" "http://127.0.0.1:$p/api/v1$path"
  fi
}

exec_code() { # <port> <sbid> <json-cmd-array> -> echoes exit_code
  api POST "$1" "/sandboxes/$2/exec" "{\"cmd\":$3}" | jq -r '.exit_code'
}

pass() { echo "  PASS: $1"; }
die()  { echo "  FAIL: $1" >&2; exit 1; }

############################################
echo "== Leg A: bridge — egress open + host-published port reachable =="
PA=18080
NET_A="guino-e2e-a-$SUFFIX"; DB_A="$(mktemp -d)"; CFG_A="$DB_A/c.yaml"; LOG_A="$DB_A/guino.log"
NETWORKS+=("$NET_A"); WORKDIRS+=("$DB_A")
write_config "$CFG_A" "$PA" "$NET_A" "$DB_A/guino.db" true bridge true ""
start_den "$CFG_A" "$LOG_A"
wait_health "$PA" || { cat "$LOG_A"; die "guino (bridge) did not become healthy"; }

# /version must advertise the network_mode capability hint.
jq -e '.features | index("network_mode")' < <(api GET "$PA" "/version") >/dev/null \
  && pass "/version advertises network_mode" || die "network_mode not in /version features"

HOSTPORT=49240; BODY="guino-e2e-$SUFFIX"
SB=$(api POST "$PA" "/sandboxes" \
  "{\"image\":\"$IMAGE\",\"ports\":[{\"sandbox_port\":8080,\"host_port\":$HOSTPORT,\"protocol\":\"tcp\"}]}" \
  | jq -r '.id')
[ -n "$SB" ] && [ "$SB" != null ] || { cat "$LOG_A"; die "sandbox create failed"; }

api POST "$PA" "/sandboxes/$SB/exec" \
  "{\"cmd\":[\"sh\",\"-c\",\"mkdir -p /tmp/www && printf %s '$BODY' > /tmp/www/index.html && httpd -p 8080 -h /tmp/www\"]}" >/dev/null

ok=""
for _ in $(seq 1 40); do
  if out=$(curl -fsS "http://127.0.0.1:$HOSTPORT/" 2>/dev/null); then ok=1; break; fi
  sleep 0.3
done
[ -n "$ok" ] || die "published port $HOSTPORT never reachable from host loopback"
[ "$out" = "$BODY" ] && pass "host→sandbox: 200 + body round-trip on 127.0.0.1:$HOSTPORT" \
  || die "wrong body from published port: got '$out' want '$BODY'"

[ "$(exec_code "$PA" "$SB" '["nslookup","example.com"]')" = 0 ] \
  && pass "bridge sandbox: DNS resolves" || die "bridge sandbox: DNS should resolve"
[ "$(exec_code "$PA" "$SB" '["wget","-q","-T","10","-O","/dev/null","https://example.com"]')" = 0 ] \
  && pass "bridge sandbox: HTTPS egress works" || die "bridge sandbox: HTTPS egress should work"

stop_den

############################################
echo "== Leg B: internal — publish inert + egress closed; none+ports = 400 =="
PB=18081
NET_B="guino-e2e-b-$SUFFIX"; DB_B="$(mktemp -d)"; CFG_B="$DB_B/c.yaml"; LOG_B="$DB_B/guino.log"
NETWORKS+=("$NET_B"); WORKDIRS+=("$DB_B")
write_config "$CFG_B" "$PB" "$NET_B" "$DB_B/guino.db" true internal false ""
start_den "$CFG_B" "$LOG_B"
wait_health "$PB" || { cat "$LOG_B"; die "guino (internal) did not become healthy"; }

IPORT=49241
SBI=$(api POST "$PB" "/sandboxes" \
  "{\"image\":\"$IMAGE\",\"ports\":[{\"sandbox_port\":8080,\"host_port\":$IPORT,\"protocol\":\"tcp\"}]}" \
  | jq -r '.id')
api POST "$PB" "/sandboxes/$SBI/exec" \
  "{\"cmd\":[\"sh\",\"-c\",\"mkdir -p /tmp/www && echo hi > /tmp/www/index.html && httpd -p 8080 -h /tmp/www\"]}" >/dev/null

reachable=""
for _ in $(seq 1 10); do
  if curl -fsS --max-time 1 "http://127.0.0.1:$IPORT/" >/dev/null 2>&1; then reachable=1; break; fi
  sleep 0.3
done
[ -z "$reachable" ] && pass "internal: host port $IPORT is inert (publish refused)" \
  || die "internal: host port unexpectedly reachable — publish must be inert"

[ "$(exec_code "$PB" "$SBI" '["wget","-q","-T","5","-O","/dev/null","https://example.com"]')" != 0 ] \
  && pass "internal sandbox: HTTPS egress closed" || die "internal sandbox: egress must be closed"

code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "X-API-Key: $KEY" \
  -H "Content-Type: application/json" \
  -d "{\"image\":\"$IMAGE\",\"network_mode\":\"none\",\"ports\":[{\"sandbox_port\":8080,\"host_port\":49242,\"protocol\":\"tcp\"}]}" \
  "http://127.0.0.1:$PB/api/v1/sandboxes")
[ "$code" = 400 ] && pass "none + ports ⇒ HTTP 400 (not a silent no-op)" \
  || die "none + ports should be 400, got $code"

stop_den

############################################
echo "== Leg C: bind guard REFUSES (auth off + loopback + internal, no override) =="
PC=18082
NET_C="guino-e2e-c-$SUFFIX"; DB_C="$(mktemp -d)"; CFG_C="$DB_C/c.yaml"; LOG_C="$DB_C/guino.log"
NETWORKS+=("$NET_C"); WORKDIRS+=("$DB_C")
write_config "$CFG_C" "$PC" "$NET_C" "$DB_C/guino.db" false internal false ""
set +e
"$BIN" serve --config "$CFG_C" >"$LOG_C" 2>&1
RC=$?
set -e
[ "$RC" -ne 0 ] && pass "guino refused to start (exit $RC)" || die "Guino must NOT start with the bind guard tripped"
# Exact committed MsgBindRefusal literal (source-derived) — NOT the loose
# "guino refuses to start" substring, which also matches MsgBridgeRefusal.
grep -Fq "$EXPECT_REFUSAL" "$LOG_C" \
  && pass "exact committed MsgBindRefusal literal present on stderr" \
  || { cat "$LOG_C"; die "expected exact committed MsgBindRefusal literal"; }

############################################
echo "== Leg D: LOCAL-ONLY positive bind-guard leg =="
# Three independent gates, each SKIPs (never fails the script) with a logged
# reason — a false pass here would silently destroy the only positive-override
# coverage:
#   1. operator opt-in GUINO_E2E_LOCAL_NATIVE=1;
#   2. DOCKER_HOST must be a local unix socket (proxied/remote ⇒ override void);
#   3. the Go classifier itself (`guino debug classify-platform`, the SAME probe
#      production uses) must agree the host is linux-native-docker co-resident.
if [ "${GUINO_E2E_LOCAL_NATIVE:-0}" != "1" ]; then
  echo "  SKIP: set GUINO_E2E_LOCAL_NATIVE=1 ONLY on native co-resident Linux."
  echo "        On proxied/remote/VM Docker the platform_override is void"
  echo "        (SECURITY.md §10/§11) and this leg would be a false pass."
elif [ -n "${DOCKER_HOST:-}" ] && [ "${DOCKER_HOST#unix://}" = "${DOCKER_HOST}" ]; then
  echo "  SKIP: DOCKER_HOST='$DOCKER_HOST' is not a local unix:// socket —"
  echo "        the co-residency attestation would be false-by-construction."
elif ! "$BIN" debug classify-platform >/tmp/e2e-classify.out 2>&1; then
  echo "  SKIP: 'guino debug classify-platform' is non-zero on this host —"
  echo "        $(cat /tmp/e2e-classify.out)"
  echo "        the Go classifier (single source of truth) says NOT co-resident."
else
  PD=18083
  NET_D="guino-e2e-d-$SUFFIX"; DB_D="$(mktemp -d)"; CFG_D="$DB_D/c.yaml"; LOG_D="$DB_D/guino.log"
  NETWORKS+=("$NET_D"); WORKDIRS+=("$DB_D")
  write_config "$CFG_D" "$PD" "$NET_D" "$DB_D/guino.db" false internal false "linux-native-docker-co-resident"
  start_den "$CFG_D" "$LOG_D"
  if wait_health "$PD"; then
    pass "guino STARTED with attested platform_override"
    # Exact committed MsgPlatformOverrideAttested literal (source-derived),
    # same rigor as Leg C — not a loose 'platform_override'/'SECURITY' pair.
    grep -Fq "$EXPECT_OVERRIDE" "$LOG_D" \
      && pass "exact committed MsgPlatformOverrideAttested literal logged" \
      || { cat "$LOG_D"; die "expected exact committed MsgPlatformOverrideAttested literal"; }
  else
    cat "$LOG_D"; die "guino should START on native co-resident Linux with the override"
  fi
  stop_den
fi

echo
echo "E2E NETWORK: ALL PASS"
