#!/usr/bin/env bash
#
# net-throughput-test.sh — find what caps Nimbo's download throughput.
#
# The sync sits at ~5 MB/s whether we run 24 or 64 parallel downloads, while the
# server reports spare CPU/RAM. That flat response to concurrency means the limit
# is somewhere on the *path* — raw link bandwidth, or an nginx/proxy rate or
# connection cap — not the client and not server compute. This script pinpoints
# which, by measuring one stream vs many over the exact WebDAV path the app uses.
#
# Run it FROM THE CLIENT MACHINE (the one running Nimbo) so it measures the same
# network path. The server-side checks at the bottom are run ON the server.
#
# Usage:
#   1. Fill in the four CONFIG values below (APP_PASSWORD = a Nimbo/Nextcloud app
#      password, not your login password — Settings ▸ Security ▸ Devices & sessions).
#   2. Pick REMOTE_FILE = any existing file in your Nextcloud that is >= ~100 MB
#      (a video, an ISO, a big archive). Path is relative to your files root.
#   3. bash tools/net-throughput-test.sh
#
# It downloads that one file to /dev/null at several concurrency levels and prints
# the aggregate MB/s. Nothing is written to disk; it only reads from the server.

set -u

# ---------------------------------------------------------------------------
# CONFIG — edit these four, OR pass them as env vars without editing the file:
#   APP_PASSWORD='xxxx' REMOTE_FILE='Videos/clip.mkv' bash net-throughput-test.sh
# ---------------------------------------------------------------------------
SERVER="${SERVER:-https://cloud.example.com}"
USER="${NC_USER:-alice}"
APP_PASSWORD="${APP_PASSWORD:-}"                       # required — pass as an env var
REMOTE_FILE="${REMOTE_FILE:-path/to/big-file.bin}"    # >= ~100 MB, relative to your files root
# ---------------------------------------------------------------------------

[ -n "$APP_PASSWORD" ] || { echo "APP_PASSWORD is required (set it as an env var)" >&2; exit 1; }

# URL-encode the path so spaces and special/non-ASCII characters in the file name
# don't turn into a wrong path (a 404). Encodes per byte; keeps "/" as separators.
urlencode_path() {
  local LC_ALL=C s="$1" out="" i c
  for ((i=0; i<${#s}; i++)); do
    c="${s:i:1}"
    case "$c" in
      [a-zA-Z0-9._~/-]) out+="$c" ;;
      *) printf -v c '%%%02X' "'$c"; out+="$c" ;;
    esac
  done
  printf '%s' "$out"
}

REMOTE_FILE="${REMOTE_FILE#/}"   # tolerate a leading slash
ENC_PATH=$(urlencode_path "$REMOTE_FILE")
URL="$SERVER/remote.php/dav/files/$USER/$ENC_PATH"
AUTH="$USER:$APP_PASSWORD"

# LAN_IP: force the hostname to resolve to this IP (TLS/SNI/cert stay valid) so we
# can compare the LAN path against the public/hairpinned path without editing DNS
# or hosts:  LAN_IP=192.168.1.50 APP_PASSWORD=... bash net-throughput-test.sh
RESOLVE=()
if [ -n "${LAN_IP:-}" ]; then
  _host=${SERVER#*://}; _host=${_host%%/*}; _host=${_host%%:*}
  _port=443; case "$SERVER" in http://*) _port=80;; esac
  RESOLVE=(--resolve "$_host:$_port:$LAN_IP")
  echo "Forcing $_host -> $LAN_IP (bypassing public DNS)"
fi

# Match the client: HTTP/1.1, keep-alive, no progress meter.
CURL=(curl -s --http1.1 "${RESOLVE[@]}" -u "$AUTH" -o /dev/null)

hr() { printf '%s\n' "------------------------------------------------------------"; }

# Confirm the file exists and grab its size (so parallel math is exact).
echo "Probing: $URL"
HEAD=$(curl -sI --http1.1 "${RESOLVE[@]}" -u "$AUTH" "$URL")
CODE=$(printf '%s' "$HEAD" | awk 'NR==1{print $2}')
SIZE=$(printf '%s' "$HEAD" | awk 'tolower($1)=="content-length:"{gsub(/\r/,"",$2); print $2}')
if [ "${CODE:-}" != "200" ] || [ -z "${SIZE:-}" ]; then
  echo "!! HTTP $CODE — could not read the file."
  case "$CODE" in
    401) echo "   -> Auth failed. APP_PASSWORD wrong, or use an *app* password (Settings > Security), not your login password." ;;
    404) echo "   -> Not found. The path is relative to your files root, case-sensitive, with NO leading 'files/'." ;;
    *)   echo "   -> Unexpected status." ;;
  esac
  echo "   Raw path : $REMOTE_FILE"
  echo "   Encoded  : $ENC_PATH"
  echo "   Tried URL: $URL"
  echo "   HEAD response:"; printf '%s\n' "$HEAD" | sed 's/^/     /'
  exit 1
fi
SIZE_MB=$(awk -v s="$SIZE" 'BEGIN{printf "%.1f", s/1048576}')
echo "File size: ${SIZE_MB} MB"
hr

# Each stream downloads for DUR seconds then is cut off (--max-time), so the test
# is bounded no matter how big the file or how fast the link — we measure the
# bytes that actually arrived, not the whole file. Aggregate = total bytes / wall.
DUR="${DUR:-15}"   # seconds per concurrency level

# single_stream: one timed download; report its speed + time-to-first-byte.
single_stream() {
  local out spd ttfb bytes
  out=$("${CURL[@]}" --max-time "$DUR" -w '%{speed_download} %{time_starttransfer} %{size_download}' "$URL")
  spd=$(echo "$out" | awk '{print $1}')
  ttfb=$(echo "$out" | awk '{print $2}')
  bytes=$(echo "$out" | awk '{print $3}')
  awk -v s="$spd" -v t="$ttfb" -v b="$bytes" \
    'BEGIN{printf "  1 stream : %6.2f MB/s   (TTFB %.0f ms, %.0f MB in ~'"$DUR"'s)\n", s/1048576, t*1000, b/1048576}'
}

# par_test N: N timed concurrent streams; aggregate = sum(bytes arrived) / wall.
par_test() {
  local n="$1" i tmp start end wall total=0 b
  tmp=$(mktemp -d)
  start=$(date +%s.%3N)
  for ((i=0; i<n; i++)); do ( "${CURL[@]}" --max-time "$DUR" -w '%{size_download}' "$URL" > "$tmp/$i" ) & done
  wait
  end=$(date +%s.%3N)
  wall=$(awk -v a="$start" -v b="$end" 'BEGIN{printf "%.3f", b-a}')
  for ((i=0; i<n; i++)); do b=$(cat "$tmp/$i" 2>/dev/null); total=$(( total + ${b:-0} )); done
  rm -rf "$tmp"
  awk -v t="$total" -v w="$wall" -v n="$n" \
    'BEGIN{printf "  %2d streams: %6.2f MB/s   (%.0f MB in %.1fs)\n", n, t/1048576/w, t/1048576, w}'
}

echo "Throughput by concurrency (same file, to /dev/null):"
single_stream
par_test 4
par_test 8
par_test 32
par_test 64
hr

cat <<'NOTES'
How to read this:

  * Single stream already ~= the 64-stream aggregate (e.g. 1≈5 MB/s, 64≈5 MB/s)
      -> a PER-IP BANDWIDTH / RATE cap (or the physical link). More streams can't
         help; a server-side bulk endpoint won't help either. Fix is on the pipe:
         remove an nginx `limit_rate`, raise a shaper, or get more bandwidth.

  * Single stream slow, but aggregate climbs with N then plateaus
      -> a CONNECTION-COUNT cap (nginx `limit_conn`, php-fpm pm.max_children, or a
         proxy worker limit). Raising that cap — or a bulk endpoint that needs far
         fewer connections — is the fix.

  * Aggregate keeps scaling past 32–64
      -> we were simply under-parallelising; raise Nimbo's worker count.

Then confirm on the SERVER (run these there):

  # nginx rate / connection limits in front of Nextcloud
  grep -RInE 'limit_rate|limit_conn|limit_req' /etc/nginx/

  # proxy buffering / proxy_max_temp_file_size can also throttle large bodies
  grep -RInE 'proxy_buffering|proxy_max_temp_file_size|proxy_request_buffering' /etc/nginx/

  # php-fpm concurrency ceiling (and watch active children during a sync)
  grep -RInE 'pm\.max_children|^pm ' /etc/php/*/fpm/pool.d/ 2>/dev/null
  # live: how many FPM workers are actually busy?  (needs pm.status_path set)
  # curl -s http://127.0.0.1/fpm-status

  # per-request server time: add $upstream_response_time $request_time to the
  # nginx log_format, reload, then watch a few WebDAV GETs:
  # tail -f /var/log/nginx/access.log | grep 'remote.php/dav'

  # raw pipe ceiling, independent of Nextcloud (run server first, then client):
  #   server:  iperf3 -s
  #   client:  iperf3 -c cloud.example.com -P 8
NOTES
