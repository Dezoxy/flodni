#!/usr/bin/env bash
# End-to-end stream test: a local seeder seeds a private torrent, the real Flodni
# image adds it through the library API, and the file is read back through FUSE
# and compared by sha256. Runs two Flodni configs in sequence (the default one
# has no source or mount path in config.json), then a TV
# replacement scenario (a second release replaces the first, a third that never
# delivers data is refused).
#
#   bash tests/e2e/run.sh [flodni-image] [seeder-image]
#
# Test hooks: E2E_CORRUPT_EXPECTED=1 flips the expected hash (the run must fail);
# E2E_KILL_SEEDER_AFTER_ADD=1 kills the seeder once the add is done (the read must
# fail within its timeout instead of hanging); E2E_REPLACEMENT_EXPECT_SUCCESS_FOR_C=1
# expects the undeliverable release to be accepted (the replacement variant must
# fail); E2E_PROM_DUMP=<file> saves the /metrics/prometheus body of each
# non-replacement variant (the last one wins; a scrape sample for the docs);
# E2E_ONLY=<variant> runs just that variant (default, slow-seeder,
# index-cache, warm-on-add, warm-lifecycle, catch-up, late-confirm, keep-alive,
# budget-refusal, replacement, removal). slow-seeder is default with a seeder that
# keeps the engine's 1 MiB per-peer request allowance. The index-cache variant runs
# with a random FLODNI_API_TOKEN: the seeder must see 401 without the bearer
# header before its authorised add, and the same for GoStorm's 8090 (401 on
# POST /torrents, 200 on /echo); the other variants run with auth off.
# The late-confirm variant (margin 8 MiB, auth off) reads half the fixture, waits
# for the over_margin [ReaderBudget] line, posts a play webhook for the file in the
# middle of the read, finishes the read and waits for the session to close.
# The keep-alive variant (pump off, auth off, the engine's 15 s idle timeout) reads
# one range and waits for the engine's idle close, posts a play webhook while the
# torrent is closed, and reads on the same open handle (a reopen). The keep-alive
# (playback_keepalive_minutes, default 15) now holds it: over two idle windows with no
# reader no idle close may be counted, and flodni_keepalive_held_torrents is 1. A
# media.stop webhook then releases it (releases{reason="stop"} 1) and the torrent
# idle-closes within the stop's 30 s grace. The ceiling is unit-tested only.
# The budget-refusal variant (margin 16 MiB, refuse_unconfirmed_reads on, auth off) runs
# the opt-in refusal against the real mount: not armed, a scan refused with EIO after
# the 15 s hold, no failed read, the windows served, the hold lifted by a late play, a
# fresh margin after a stop, a killed and a concurrent reader, and the seeder's upload
# bounded. It waits for the session close once, at the end.
# The removal variant (index-cache config, auth off) files four movies, reads A in the
# background, opens a descriptor on B, removes B and C through the library API back to
# back and D with rm through the mount, and asserts that A reads intact, that GoStorm's
# `get` answers 404 for B, C and D at once and after the idle timeout, that B's
# descriptor fails with EIO at an uncached offset (a refused wake, not a failed read) and
# that no index-cache file of the three remains.
# The warm-lifecycle variant (warm-on-add config, auth off) holds title A open with a
# confirmed playback, files B and C while the warmer is paused for it, removes B while it
# is queued, waits for C's torrent to be closed by the engine, ends the playback and
# asserts that C is warmed by a wake from the stored record and B never is. A public
# canary tracker (listed in trackers.txt, so only a public torrent may announce to it)
# must see a public control title and none of the private ones.
# E2E_WARM_NEGATIVE=1 runs the warm-on-add variant with the switch off: it must
# fail at step 2 (no fill). It is not part of make e2e.
set -euo pipefail

FLODNI_IMAGE=${1:-flodni:ci}
SEEDER_IMAGE=${2:-flodni-e2e-seeder:ci}
SOURCE_PATH=/mnt/flodni-real
MOUNT_PATH=/mnt/flodni-virtual
WAIT_SECONDS=180
READ_TIMEOUT=180

work=$(mktemp -d)
network=""
flodni=""
seeder=""

cleanup() {
  local rc=$?
  [ -n "$seeder" ] && docker rm -f "$seeder" >/dev/null 2>&1 || true
  [ -n "$flodni" ] && docker rm -f "$flodni" >/dev/null 2>&1 || true
  [ -n "$network" ] && docker network rm "$network" >/dev/null 2>&1 || true
  # State dirs were written by root inside the containers.
  docker run --rm -v "$work:/w" --entrypoint sh "$SEEDER_IMAGE" -c 'rm -rf /w/* /w/.[!.]*' >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
  return $rc
}
trap cleanup EXIT

dump_diagnostics() {
  echo "=== diagnostics: $flodni ===" >&2
  docker logs --tail 200 "$flodni" >&2 || true
  echo "=== fuse mounts in $flodni ===" >&2
  docker exec "$flodni" sh -c 'grep fuse /proc/mounts' >&2 || true
  echo "=== diagnostics: $seeder ===" >&2
  docker logs --tail 200 "$seeder" >&2 || true
}

# diag_dir is created on first use and, unlike $work, survives the run.
diag_dir=""

# slow_diagnostics variant seconds: print and save both containers' recent logs.
slow_diagnostics() {
  local variant=$1 seconds=$2 file
  [ -n "$diag_dir" ] || diag_dir=$(mktemp -d)
  file="$diag_dir/slow-$variant-$(date +%H%M%S).log"
  echo "SLOW ($variant): full read ${seconds}s" >&2
  {
    echo "=== flodni $flodni: last 300 lines ==="
    docker logs --tail 300 "$flodni" 2>&1 || true
    echo "=== seeder $seeder: last 300 lines ==="
    docker logs --tail 300 "$seeder" 2>&1 || true
  } >"$file"
  cat "$file" >&2
  echo "SLOW ($variant): diagnostics saved to $file" >&2
}

# report_first_byte variant: the first_byte_ms of the stub's sessions, read from
# Flodni's /api/sessions on its metrics port (no curl in the image, so bash's /dev/tcp).
report_first_byte() {
  local variant=$1 body
  body=$(docker exec "$flodni" bash -c 'exec 3<>/dev/tcp/127.0.0.1/9080 && printf "GET /api/sessions HTTP/1.0\r\nHost: x\r\n\r\n" >&3 && cat <&3' 2>/dev/null || true)
  echo "first_byte_ms ($variant): $(grep -o '"first_byte_ms":[0-9a-z]*' <<<"$body" | tr '\n' ' ')" >&2
}

# health_body: Flodni's /api/torrents/health. The container's FLODNI_API_TOKEN, when
# set, is read inside it and sent as the bearer header (the replacement variant sets none).
health_body() {
  docker exec "$flodni" bash -c 'h=""
    if [ -n "${FLODNI_API_TOKEN:-}" ]; then h="Authorization: Bearer $FLODNI_API_TOKEN\r\n"; fi
    exec 3<>/dev/tcp/127.0.0.1/9080 && printf "GET /api/torrents/health HTTP/1.0\r\nHost: x\r\n${h}\r\n" >&3 && cat <&3' 2>/dev/null || true
}

# check_files_record: after the full read through the mount, the per-file record
# (experimental `files` array) of the fixture is printed, not asserted: the mount path
# records a file once the recorder has run, so poll up to 30 s for a non-empty array.
# The expected measurement is readability warmed and cache cached.
check_files_record() {
  local body="" deadline=$((SECONDS + 30))
  while [ "$SECONDS" -lt "$deadline" ]; do
    body=$(health_body)
    grep -q '"files":\[{' <<<"$body" && break
    sleep 2
  done
  echo "FILES (index-cache): $(grep -o '"files":\[[^]]*\]' <<<"$body" | tr '\n' ' ')" >&2
}

# prom_raw [noauth]: Flodni's /metrics/prometheus with headers, over /dev/tcp. The
# container's FLODNI_API_TOKEN is read inside it, so this script never handles the
# token; the Authorization header goes only when one is set, and not at all with noauth.
prom_raw() {
  docker exec "$flodni" bash -c 'h=""
    if [ -z "$1" ] && [ -n "${FLODNI_API_TOKEN:-}" ]; then h="Authorization: Bearer $FLODNI_API_TOKEN\r\n"; fi
    exec 3<>/dev/tcp/127.0.0.1/9080 && printf "GET /metrics/prometheus HTTP/1.0\r\nHost: x\r\n${h}\r\n" >&3 && cat <&3' _ "${1:-}" 2>/dev/null || true
}

# check_prometheus variant: the exposition must answer 200 with the text format's
# content type, every line must fit the sample grammar, the first-byte histogram must
# be declared first-class, and the full read must show in the read counts and bytes.
# Per-source values are printed as a measurement, not asserted. In index-cache (token
# set) the request without the header must get 401.
check_prometheus() {
  local variant=$1 raw body status
  raw=$(prom_raw)
  status=$(head -n 1 <<<"$raw" | tr -d '\r')
  body=${raw#*$'\r\n\r\n'}
  if [[ "$status" != *" 200 "* ]]; then
    echo "FAIL ($variant): /metrics/prometheus: $status" >&2
    dump_diagnostics
    return 1
  fi
  if ! grep -qi '^content-type: text/plain; version=0.0.4; charset=utf-8' <<<"$raw"; then
    echo "FAIL ($variant): /metrics/prometheus content type is not text format 0.0.4" >&2
    dump_diagnostics
    return 1
  fi
  local bad
  bad=$(grep -vE '^(# (HELP|TYPE) [a-zA-Z_:][a-zA-Z0-9_:]* .+|[a-zA-Z_:][a-zA-Z0-9_:]*(\{[^{}]*\})? [-+0-9.eE]+|[a-zA-Z_:][a-zA-Z0-9_:]*(\{[^{}]*\})? (\+Inf|NaN))$' <<<"$body" | head -n 3 || true)
  if [ -n "$bad" ]; then
    echo "FAIL ($variant): /metrics/prometheus has lines outside the sample grammar: $bad" >&2
    dump_diagnostics
    return 1
  fi
  if ! grep -qx '# TYPE flodni_session_first_byte_seconds histogram' <<<"$body"; then
    echo "FAIL ($variant): /metrics/prometheus lacks the first-byte histogram TYPE line" >&2
    dump_diagnostics
    return 1
  fi
  local reads bytes
  reads=$(awk '/^flodni_read_duration_seconds_count\{/ { n += $2 } END { print n + 0 }' <<<"$body")
  bytes=$(awk '/^flodni_read_bytes_total\{/ { n += $2 } END { print n + 0 }' <<<"$body")
  if [ "$reads" -le 0 ] || [ "$bytes" -le 0 ]; then
    echo "FAIL ($variant): after a full read the read counts are $reads and the bytes $bytes" >&2
    dump_diagnostics
    return 1
  fi
  echo "PROM ($variant): reads=$reads bytes=$bytes" >&2
  echo "PROM ($variant): $(grep -E '^flodni_(read_duration_seconds_count|read_bytes_total)\{' <<<"$body" | tr '\n' ' ')" >&2
  # Measurement only: warm files and the readability split, as the scrape sees them.
  echo "PROM ($variant): $(grep -E '^flodni_(warmup_files|warmup_cache_bytes|readability_torrents)[{ ]' <<<"$body" | tr '\n' ' ')" >&2
  if [ "$variant" = index-cache ]; then
    status=$(prom_raw noauth | head -n 1 | tr -d '\r')
    if [[ "$status" != *" 401 "* ]]; then
      echo "FAIL ($variant): /metrics/prometheus without the token: $status, want 401" >&2
      dump_diagnostics
      return 1
    fi
    echo "ok ($variant): /metrics/prometheus without token refused with 401" >&2
  fi
  if [ -n "${E2E_PROM_DUMP:-}" ]; then printf '%s\n' "$body" >"$E2E_PROM_DUMP"; fi
}

# check_no_warning variant [matched]: /api/health (open, so no token) must carry an
# empty "warnings" array and flodni_warning{name="no_play_webhook"} must be 0. With
# "matched" the last matched webhook time must also be set (a webhook was posted).
check_no_warning() {
  local variant=$1 raw body ts
  raw=$(docker exec "$flodni" bash -c 'exec 3<>/dev/tcp/127.0.0.1/9080 && printf "GET /api/health HTTP/1.0\r\nHost: x\r\n\r\n" >&3 && cat <&3' 2>/dev/null || true)
  if ! grep -qF '"warnings":[]' <<<"$raw"; then
    echo "FAIL ($variant): /api/health has no empty warnings array: ${raw##*$'\r\n\r\n'}" >&2
    dump_diagnostics
    return 1
  fi
  body=$(prom_body)
  if ! grep -qxF 'flodni_warning{name="no_play_webhook"} 0' <<<"$body"; then
    echo "FAIL ($variant): flodni_warning no_play_webhook is not 0: $(grep -F 'flodni_warning{' <<<"$body")" >&2
    dump_diagnostics
    return 1
  fi
  if [ "${2:-}" = matched ]; then
    ts=$(awk '/^flodni_playback_webhook_last_timestamp_seconds\{matched="true"\} / { print $2 }' <<<"$body")
    if [ -z "$ts" ] || [ "$ts" = 0 ]; then
      echo "FAIL ($variant): the matched webhook time is ${ts:-absent} after a matched webhook" >&2
      dump_diagnostics
      return 1
    fi
  fi
  echo "ok ($variant): /api/health warnings [] and flodni_warning 0${2:+, matched webhook time set}" >&2
}

# check_reader_budget variant path: the full sha256sum read is a reader no webhook
# confirmed, so past the windows it must show in flodni_unconfirmed_read_bytes_total
# (printed in every variant, with the session and webhook series and the log line,
# whose swarm_bytes/cache_bytes split is a measurement). In
# index-cache (4 MB windows, 150 MiB fixture, default 64 MiB margin) the bytes must sum
# to at least 64 MiB and exactly one over_margin [ReaderBudget] line must exist,
# without the file's name, with a caller pid. The other variants only print.
check_reader_budget() {
  local variant=$1 path=$2 body logs lines
  body=$(prom_raw)
  body=${body#*$'\r\n\r\n'}
  echo "PROM ($variant): $(grep -E '^flodni_(unconfirmed_read_bytes_total|unconfirmed_read_sessions_total|playback_webhooks_received_total)[{ ]' <<<"$body" | tr '\n' ' ')" >&2
  logs=$(docker logs "$flodni" 2>&1 || true)
  lines=$(grep -F '[ReaderBudget] ' <<<"$logs" || true)
  echo "ReaderBudget ($variant): ${lines:-none}" >&2
  [ "$variant" = index-cache ] || return 0
  local total
  total=$(awk '/^flodni_unconfirmed_read_bytes_total / { printf "%d", $2 }' <<<"$body")
  total=${total:-0}
  if [ "$total" -lt $((64 * 1048576)) ]; then
    echo "FAIL ($variant): flodni_unconfirmed_read_bytes_total sums to $total, want at least 64 MiB" >&2
    dump_diagnostics
    return 1
  fi
  if [ "$(grep -c '"event":"over_margin"' <<<"$lines")" != 1 ]; then
    echo "FAIL ($variant): want exactly one over_margin [ReaderBudget] line" >&2
    dump_diagnostics
    return 1
  fi
  if grep -qF "$(basename "$path")" <<<"$lines" || grep -qF "$(basename "$path" .mkv)" <<<"$lines"; then
    echo "FAIL ($variant): the [ReaderBudget] line carries the file name" >&2
    dump_diagnostics
    return 1
  fi
  if ! grep -Eq '"pid":[1-9]' <<<"$lines"; then
    echo "FAIL ($variant): the [ReaderBudget] line has no caller pid" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($variant): $total unconfirmed bytes counted, one [ReaderBudget] line, no file name" >&2
}

# check_sessions_closed: after B's session closed, the counter of served sessions must be
# at least 1 (check_session_line has just found the line, so this is deterministic).
check_sessions_closed() {
  local body n
  body=$(prom_raw)
  n=$(grep -E '^flodni_sessions_closed_total\{outcome="served"\} ' <<<"$body" | awk '{ print $2 }')
  if [ -z "$n" ] || [ "$n" -lt 1 ]; then
    echo "FAIL (replacement): flodni_sessions_closed_total{outcome=\"served\"} is '${n:-absent}', want >= 1" >&2
    dump_diagnostics
    return 1
  fi
  echo "PROM (replacement): sessions_closed served=$n" >&2
}

# check_readability_gauge: the gauge refreshes every 60 s, so poll up to 70 s
# for the library to show in it (inode map count and ticker, end to end).
check_readability_gauge() {
  local body="" sum=0 deadline=$((SECONDS + 70))
  while [ "$SECONDS" -lt "$deadline" ]; do
    body=$(prom_raw)
    sum=$(awk '/^flodni_readability_torrents\{/ { n += $2 } END { print n + 0 }' <<<"$body")
    [ "$sum" -ge 1 ] && break
    sleep 5
  done
  echo "PROM (replacement): $(grep -E '^flodni_readability_torrents\{' <<<"$body" | tr '\n' ' ')" >&2
  if [ "$sum" -lt 1 ]; then
    echo "FAIL (replacement): flodni_readability_torrents still sums to 0 after 70 s" >&2
    dump_diagnostics
    return 1
  fi
}

# check_readability out: B's session closes up to two minutes after its last read, so
# poll up to 130 s. The endpoint must answer with a record list and C must have no
# record. B stamped is reported, not asserted: sessions fed by the read-ahead pump
# may not stamp (README), so B not stamped is the measurement, not a failure.
check_readability() {
  local out=$1 a_hash b_hash c_hash body="" deadline=$((SECONDS + 130))
  a_hash=$(cat "$out/a.hash") b_hash=$(cat "$out/b.hash") c_hash=$(cat "$out/c.hash")
  while [ "$SECONDS" -lt "$deadline" ]; do
    body=$(health_body)
    grep -q "\"hash\":\"$b_hash\"[^}]*\"last_swarm_read_at\":\"" <<<"$body" && break
    sleep 5
  done
  if ! grep -q '"torrents":' <<<"$body"; then
    echo "FAIL (replacement): /api/torrents/health returned no record list: $body" >&2
    dump_diagnostics
    return 1
  fi
  if ! grep -q "\"hash\":\"$b_hash\"[^}]*\"last_swarm_read_at\":\"" <<<"$body"; then
    echo "READABILITY (replacement): B not stamped" >&2
    echo "READABILITY (replacement): endpoint: $body" >&2
    return 0
  fi
  echo "READABILITY (replacement): B stamped: $(grep -o "{\"hash\":\"$b_hash\"[^}]*}" <<<"$body")" >&2
  # C never read, so it must have no record. A was read before B replaced it and its
  # session can close after the library dropped the row, which leaves an orphan until
  # the next restart's prune: reported, not judged.
  if grep -q "\"hash\":\"$c_hash\"" <<<"$body"; then
    echo "FAIL (replacement): readability record of C ($c_hash) exists: $body" >&2
    dump_diagnostics
    return 1
  fi
  if grep -q "\"hash\":\"$a_hash\"" <<<"$body"; then
    echo "READABILITY (replacement): A has a record after replacement: $(grep -o "{\"hash\":\"$a_hash\"[^}]*}" <<<"$body")" >&2
  else
    echo "READABILITY (replacement): A has no record" >&2
  fi
}

# check_session_line out: after B's session closed, its [Session] log line must exist
# with outcome served, a file index and a byte split that sums to bytes_served. The
# split is printed as a measurement; swarm_served false is reported, not failed
# (a pump-fed session may not set it, README).
check_session_line() {
  local out=$1 b_hash logs="" line="" deadline=$((SECONDS + 3))
  b_hash=$(cat "$out/b.hash")
  # Read the log once per try: grep -q on a live docker logs pipe fails under pipefail.
  while :; do
    logs=$(docker logs "$flodni" 2>&1 || true)
    line=$(grep -F '[Session] ' <<<"$logs" | grep -F "\"hash\":\"$b_hash\"" | grep -F '"outcome":"served"' | tail -n 1 || true)
    [ -n "$line" ] || [ "$SECONDS" -ge "$deadline" ] && break
    sleep 1
  done
  if [ -z "$line" ]; then
    echo "FAIL (replacement): no [Session] line with outcome served for B ($b_hash)" >&2
    dump_diagnostics
    return 1
  fi
  local json=${line#*\[Session\] } idx served sum f h t r
  idx=$(grep -o '"file_index":[0-9]*' <<<"$json" | cut -d: -f2)
  served=$(grep -o '"bytes_served":[0-9]*' <<<"$json" | cut -d: -f2)
  f=$(grep -o '"bytes_fetch_block":[0-9]*' <<<"$json" | cut -d: -f2)
  r=$(grep -o '"bytes_racache_hit":[0-9]*' <<<"$json" | cut -d: -f2)
  h=$(grep -o '"bytes_warmup_head":[0-9]*' <<<"$json" | cut -d: -f2)
  t=$(grep -o '"bytes_warmup_tail":[0-9]*' <<<"$json" | cut -d: -f2)
  if [ -z "$idx" ] || [ "$idx" -lt 1 ] || [ -z "$served" ] || [ -z "$f" ] || [ -z "$r" ] || [ -z "$h" ] || [ -z "$t" ]; then
    echo "FAIL (replacement): [Session] line of B lacks file_index >= 1 or the byte fields: $json" >&2
    dump_diagnostics
    return 1
  fi
  sum=$((f + r + h + t))
  if [ "$sum" -ne "$served" ]; then
    echo "FAIL (replacement): [Session] byte fields sum to $sum, bytes_served is $served: $json" >&2
    dump_diagnostics
    return 1
  fi
  echo "SESSION (replacement): B file_index=$idx bytes_served=$served fetch_block=$f racache_hit=$r warmup_head=$h warmup_tail=$t" >&2
  echo "SESSION (replacement): B line: $json" >&2
  if grep -q '"swarm_served":true' <<<"$json"; then
    echo "SESSION (replacement): B swarm_served true" >&2
  else
    echo "SESSION (replacement): B swarm_served false" >&2
  fi
}

is_running() {
  [ "$(docker inspect -f '{{.State.Running}}' "$1" 2>/dev/null)" = "true" ]
}

# catchup_config warm_on_add file: the catch-up variant's config. Written with `cat >` so
# a rewrite keeps the inode (the container bind-mounts the single file).
catchup_config() {
  cat >"$2" <<JSON
{"telemetry": false, "telemetry_url": "", "scheduler": {"enabled": false},
 "physical_source_path": "$SOURCE_PATH", "fuse_mount_path": "$MOUNT_PATH",
 "pump_on_open": false, "warmup_dir": "/cache/index",
 "warmup_head_size_mb": 16, "warmup_tail_size_mb": 8, "disk_warmup_quota_gb": 1,
 "warm_on_add": $1, "warm_catchup_per_hour": 3600}
JSON
}

write_config() { # variant file
  local variant=$1 file=$2
  case "$variant" in
    default)
      # No physical_source_path or fuse_mount_path: the entrypoint's
      # command-line paths alone decide where stubs go and what FUSE serves.
      cat >"$file" <<JSON
{"telemetry": false, "telemetry_url": "", "scheduler": {"enabled": false}}
JSON
      ;;
    replacement)
      cat >"$file" <<JSON
{"telemetry": false, "telemetry_url": "", "scheduler": {"enabled": false},
 "physical_source_path": "$SOURCE_PATH", "fuse_mount_path": "$MOUNT_PATH"}
JSON
      ;;
    index-cache)
      cat >"$file" <<JSON
{"telemetry": false, "telemetry_url": "", "scheduler": {"enabled": false},
 "physical_source_path": "$SOURCE_PATH", "fuse_mount_path": "$MOUNT_PATH",
 "pump_on_open": false, "warmup_dir": "/cache/index",
 "warmup_head_size_mb": 4, "warmup_tail_size_mb": 4, "disk_warmup_quota_gb": 1}
JSON
      ;;
    warm-on-add)
      # The index-cache config with the warmer on and a 16 MiB head window for the
      # 8 MiB head read (the kernel reads ahead: the head read runs up to 4 MiB past
      # its end). The 4 MiB tail window is the size of the tail read: its start is
      # rounded down to a page, so the kernel's read of the last 4 MiB, which starts on
      # the page before the file's size minus 4 MiB (the size is not page aligned),
      # stays inside the window.
      # E2E_WARM_NEGATIVE=1 switches the warmer off, so step 2 must fail.
      local warm=true
      if [ "${E2E_WARM_NEGATIVE:-}" = "1" ]; then warm=false; fi
      cat >"$file" <<JSON
{"telemetry": false, "telemetry_url": "", "scheduler": {"enabled": false},
 "physical_source_path": "$SOURCE_PATH", "fuse_mount_path": "$MOUNT_PATH",
 "pump_on_open": false, "warmup_dir": "/cache/index",
 "warmup_head_size_mb": 16, "warmup_tail_size_mb": 4, "disk_warmup_quota_gb": 1,
 "warm_on_add": $warm}
JSON
      ;;
    late-confirm)
      # The index-cache windows with a small margin, so the read crosses it after
      # 4 MiB of head and 8 MiB counted. No API token: the webhook route sits behind
      # the same bearer middleware as the rest, and auth is covered by index-cache.
      cat >"$file" <<JSON
{"telemetry": false, "telemetry_url": "", "scheduler": {"enabled": false},
 "physical_source_path": "$SOURCE_PATH", "fuse_mount_path": "$MOUNT_PATH",
 "pump_on_open": false, "warmup_dir": "/cache/index",
 "warmup_head_size_mb": 4, "warmup_tail_size_mb": 4, "disk_warmup_quota_gb": 1,
 "unconfirmed_read_margin_mb": 8}
JSON
      ;;
    keep-alive)
      # The late-confirm windows without the margin: the reads here are not budgeted.
      cat >"$file" <<JSON
{"telemetry": false, "telemetry_url": "", "scheduler": {"enabled": false},
 "physical_source_path": "$SOURCE_PATH", "fuse_mount_path": "$MOUNT_PATH",
 "pump_on_open": false, "warmup_dir": "/cache/index",
 "warmup_head_size_mb": 4, "warmup_tail_size_mb": 4, "disk_warmup_quota_gb": 1}
JSON
      ;;
    budget-refusal)
      # The late-confirm windows with a 16 MiB margin and the opt-in refusal on: a
      # scan is refused after 4 MiB of head and 16 MiB counted.
      cat >"$file" <<JSON
{"telemetry": false, "telemetry_url": "", "scheduler": {"enabled": false},
 "physical_source_path": "$SOURCE_PATH", "fuse_mount_path": "$MOUNT_PATH",
 "pump_on_open": false, "warmup_dir": "/cache/index",
 "warmup_head_size_mb": 4, "warmup_tail_size_mb": 4, "disk_warmup_quota_gb": 1,
 "unconfirmed_read_margin_mb": 16, "refuse_unconfirmed_reads": true}
JSON
      ;;
    catch-up)
      # The warm-on-add windows with the warmer off and catch-up at its fastest pace:
      # the first pass runs 1 s after a start. The variant rewrites this file in place
      # with warm_on_add true.
      catchup_config false "$file"
      ;;
    removal) write_config index-cache "$file" ;;
    warm-lifecycle) write_config warm-on-add "$file" ;;
    slow-seeder) write_config default "$file" ;;
    *) echo "unknown variant $variant" >&2; return 1 ;;
  esac
}

# start_stack variant [seeder args...]: network, Flodni and seeder for one variant.
start_stack() {
  local variant=$1
  shift
  dir="$work/$variant"
  mkdir -p "$dir/state" "$dir/out" "$dir/cache"
  chmod 777 "$dir/out" "$dir/cache"
  write_config "$variant" "$dir/config.json"
  local -a envargs=()
  if [ "$variant" = index-cache ]; then
    envargs=(-e "FLODNI_API_TOKEN=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')")
  fi
  # Test-only: shorten the session idle close (default 60 s) and the cleanup tick (60 s)
  # for the variants that wait for a session to close. late-confirm pauses a read for a
  # few seconds before the webhook; budget-refusal has gaps of about 20 s with no read
  # (step 7), and a session closed inside a gap would start the next one with a fresh tally.
  case "$variant" in
    late-confirm) envargs+=(-e FLODNI_TEST_SESSION_IDLE_CLOSE=10s -e FLODNI_TEST_SESSION_TICK=2s) ;;
    budget-refusal) envargs+=(-e FLODNI_TEST_SESSION_IDLE_CLOSE=30s -e FLODNI_TEST_SESSION_TICK=5s) ;;
  esac

  echo "=== variant: $variant ===" >&2
  network="e2e-$variant-$$"
  flodni="e2e-flodni-$variant"
  seeder="e2e-seeder-$variant"
  docker network create --internal "$network" >/dev/null

  docker run -d --name "$flodni" --network "$network" --network-alias flodni \
    --device /dev/fuse --cap-add SYS_ADMIN --security-opt apparmor:unconfined \
    -v "$dir/config.json:/config.json:ro" -v "$dir/state:/state" \
    -v "$dir/cache:/cache" ${envargs[@]+"${envargs[@]}"} "$FLODNI_IMAGE" >/dev/null
  local -a seederenv=()
  if [ -n "${E2E_GOSTORM_CACHE_MB:-}" ]; then
    seederenv=(-e "E2E_GOSTORM_CACHE_MB=$E2E_GOSTORM_CACHE_MB")
  fi
  docker run -d --name "$seeder" --network "$network" --network-alias seeder \
    -v "$dir/out:/e2e" ${envargs[@]+"${envargs[@]}"} ${seederenv[@]+"${seederenv[@]}"} "$SEEDER_IMAGE" -out /e2e "$@" >/dev/null
}

stop_stack() {
  docker rm -f "$seeder" "$flodni" >/dev/null
  docker network rm "$network" >/dev/null
  seeder="" flodni="" network=""
}

# wait_for_file path seconds label: wait for a seeder output, failing fast if a
# container exits.
wait_for_file() {
  local file=$1 limit=$2 label=$3 waited=0
  until [ -s "$file" ]; do
    if ! is_running "$flodni" || ! is_running "$seeder"; then
      echo "FAIL ($label): a container exited before $(basename "$file") appeared" >&2
      dump_diagnostics
      return 1
    fi
    if [ "$waited" -ge "$limit" ]; then
      echo "FAIL ($label): no $(basename "$file") after ${limit}s" >&2
      dump_diagnostics
      return 1
    fi
    sleep 1
    waited=$((waited + 1))
  done
}

run_variant() {
  local variant=$1 dir t0=$SECONDS
  local -a seeder_args=()
  # The seeder keeps the engine's default per-peer request allowance, like a
  # real peer that is not tuned for one fast leecher.
  if [ "$variant" = slow-seeder ]; then seeder_args=(-engine-default-alloc); fi
  start_stack "$variant" ${seeder_args[@]+"${seeder_args[@]}"}

  local waited=0
  until [ -s "$dir/out/expected.sha256" ] && [ -s "$dir/out/fuse_path" ]; do
    if ! is_running "$flodni" || ! is_running "$seeder"; then
      echo "FAIL ($variant): a container exited before the add finished" >&2
      dump_diagnostics
      return 1
    fi
    if [ "$waited" -ge "$WAIT_SECONDS" ]; then
      echo "FAIL ($variant): no expected.sha256/fuse_path after ${WAIT_SECONDS}s" >&2
      dump_diagnostics
      return 1
    fi
    sleep 2
    waited=$((waited + 2))
  done

  local expected fuse_path path
  expected=$(cat "$dir/out/expected.sha256")
  fuse_path=$(cat "$dir/out/fuse_path")
  path="$MOUNT_PATH/$fuse_path"
  if [ "${E2E_CORRUPT_EXPECTED:-}" = "1" ]; then
    expected=$(printf '%s' "$expected" | tr '0-9a-f' '1-9a-f0')
    echo "E2E_CORRUPT_EXPECTED: expecting $expected" >&2
  fi
  if [ "${E2E_KILL_SEEDER_AFTER_ADD:-}" = "1" ]; then
    echo "E2E_KILL_SEEDER_AFTER_ADD: killing $seeder" >&2
    docker kill "$seeder" >/dev/null
  fi

  check_no_warning "$variant" || return 1

  echo "tail read of $path" >&2
  if ! docker exec "$flodni" timeout -k 10 "$READ_TIMEOUT" \
    sh -c "size=\$(stat -c %s '$path') && dd if='$path' of=/dev/null bs=1048576 skip=\$(( (size - 1) / 1048576 - 1 )) status=none"; then
    echo "FAIL ($variant): tail read failed" >&2
    dump_diagnostics
    return 1
  fi

  echo "full read of $path" >&2
  local actual full_t0=$SECONDS
  if ! actual=$(docker exec "$flodni" timeout -k 10 "$READ_TIMEOUT" sha256sum "$path"); then
    echo "FAIL ($variant): full read failed or timed out" >&2
    dump_diagnostics
    return 1
  fi
  actual=${actual%% *}
  local full_s=$((SECONDS - full_t0))
  echo "full read of $variant took ${full_s}s" >&2
  report_first_byte "$variant"
  check_prometheus "$variant" || return 1
  check_reader_budget "$variant" "$path" || return 1
  # Only reported for now: the bound comes once the stall is fixed.
  if [ "$full_s" -gt 20 ]; then slow_diagnostics "$variant" "$full_s"; fi
  if [ "$actual" != "$expected" ]; then
    echo "FAIL ($variant): sha256 mismatch: got $actual, want $expected" >&2
    dump_diagnostics
    return 1
  fi
  if [ "$variant" = index-cache ]; then
    # Read the log once: grep -q exits on the first match, and under pipefail the
    # SIGPIPE it gives a still-writing docker logs would fail the whole check.
    local seeder_log
    seeder_log=$(docker logs "$seeder" 2>&1)
    if ! grep -q 'unauthenticated library add refused: status 401' <<<"$seeder_log"; then
      echo "FAIL ($variant): the seeder did not see a 401 for the add without a token" >&2
      dump_diagnostics
      return 1
    fi
    echo "ok ($variant): add without token refused with 401, with token accepted" >&2
    if ! grep -q 'unauthenticated POST :8090/torrents refused: status 401' <<<"$seeder_log" ||
      ! grep -q 'unauthenticated GET :8090/echo open: status 200' <<<"$seeder_log"; then
      echo "FAIL ($variant): GoStorm on 8090 did not refuse torrent control without a token, or closed /echo" >&2
      dump_diagnostics
      return 1
    fi
    echo "ok ($variant): GoStorm 8090 refused POST /torrents with 401, /echo open" >&2
    check_files_record
  fi
  echo "PASS ($variant): sha256 $actual, $((SECONDS - t0))s" >&2

  # A graceful stop makes the seeder log its upload stats (piece data sent / video size).
  docker stop -t 10 "$seeder" >/dev/null 2>&1 || true
  docker logs "$seeder" 2>&1 | grep 'upload stats' | sed "s/^/($variant) /" >&2 || true
  stop_stack
}

# check_sha label path want: full read through FUSE, compared by sha256.
check_sha() {
  local label=$1 path=$2 want=$3 got
  if ! got=$(docker exec "$flodni" timeout -k 10 "$READ_TIMEOUT" sha256sum "$path"); then
    echo "FAIL (replacement): $label: read failed or timed out" >&2
    dump_diagnostics
    return 1
  fi
  got=${got%% *}
  if [ "$got" != "$want" ]; then
    echo "FAIL (replacement): $label: sha256 mismatch: got $got, want $want" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($label): sha256 $got" >&2
}

# stub_gone path: the old stub must disappear from the mount (polled briefly, as
# FUSE may cache the directory).
stub_gone() {
  local tries=0
  while [ "$tries" -lt 10 ]; do
    tries=$((tries + 1))
    docker exec "$flodni" test ! -e "$1" && return 0
    sleep 1
  done
  return 1
}

run_replacement() {
  local dir t0
  t0=$SECONDS
  start_stack replacement -mode replacement
  local out="$dir/out"

  wait_for_file "$out/a.path" "$WAIT_SECONDS" replacement
  echo "step add A: $((SECONDS - t0))s since start" >&2
  local path_a path_b
  path_a="$MOUNT_PATH/$(cat "$out/a.path")"
  check_sha "A read" "$path_a" "$(cat "$out/a.sha256")"
  : >"$out/go-b"

  t0=$SECONDS
  wait_for_file "$out/b.path" "$WAIT_SECONDS" replacement
  echo "step add B (replaces A): $((SECONDS - t0))s" >&2
  path_b="$MOUNT_PATH/$(cat "$out/b.path")"
  if [ "$path_a" = "$path_b" ]; then
    echo "FAIL (replacement): B filed at A's path $path_a" >&2
    dump_diagnostics
    return 1
  fi
  if ! stub_gone "$path_a"; then
    echo "FAIL (replacement): A's stub $path_a still exists after B replaced it" >&2
    dump_diagnostics
    return 1
  fi
  check_sha "B read" "$path_b" "$(cat "$out/b.sha256")"
  : >"$out/go-c"

  t0=$SECONDS
  wait_for_file "$out/c.status" "$WAIT_SECONDS" replacement
  echo "step add C (refused): $((SECONDS - t0))s, seeder measured $(cat "$out/c.seconds")s" >&2
  local want_status=422 status
  if [ "${E2E_REPLACEMENT_EXPECT_SUCCESS_FOR_C:-}" = "1" ]; then
    want_status=200
    echo "E2E_REPLACEMENT_EXPECT_SUCCESS_FOR_C: expecting status $want_status" >&2
  fi
  status=$(cat "$out/c.status")
  if [ "$status" != "$want_status" ]; then
    echo "FAIL (replacement): add C: status $status, want $want_status: $(cat "$out/c.body")" >&2
    dump_diagnostics
    return 1
  fi
  if ! grep -q 'existing files kept' "$out/c.body"; then
    echo "FAIL (replacement): add C: body does not say existing files were kept: $(cat "$out/c.body")" >&2
    dump_diagnostics
    return 1
  fi
  if ! docker exec "$flodni" test -e "$path_b"; then
    echo "FAIL (replacement): B's stub $path_b is gone after the refused add" >&2
    dump_diagnostics
    return 1
  fi
  check_readability "$out" || return 1
  check_readability_gauge || return 1
  check_session_line "$out" || return 1
  check_sessions_closed || return 1
  check_sha "B read after refused C" "$path_b" "$(cat "$out/b.sha256")"
  echo "PASS (replacement): A replaced by B, C refused with $status" >&2

  stop_stack
}

# prom_body: the /metrics/prometheus body without the HTTP headers.
prom_body() {
  local raw
  raw=$(prom_raw)
  printf '%s\n' "${raw#*$'\r\n\r\n'}"
}

# read_source body source: flodni_read_bytes_total of one serving path, 0 if absent.
read_source() {
  awk -v s="source=\"$2\"" 'index($0, "flodni_read_bytes_total{") == 1 && index($0, s) { printf "%d", $2; f = 1 } END { if (!f) printf "0" }' <<<"$1"
}

# check_readback v path head_want tail_want: the seeder dies, the head (8 MiB) and the
# tail (4 MiB) are read back through the mount and compared with the seeder's hashes, and
# every byte must come from the SSD windows. The read counters must be 0 before (the
# variant has not read through the mount since the process started).
check_readback() {
  local v=$1 path=$2 head_want=$3 tail_want=$4
  echo "killing $seeder" >&2
  docker kill "$seeder" >/dev/null
  local first got ms size body
  size=$(docker exec "$flodni" stat -c %s "$path")
  # Measurement only: the first read of the head after the kill, then the whole head.
  first=$(docker exec "$flodni" timeout -k 10 "$READ_TIMEOUT" sh -c "s=\$(date +%s%N); dd if='$path' of=/dev/null bs=4096 count=1 status=none && e=\$(date +%s%N) && echo \$(( (e - s) / 1000000 ))") || first=failed
  echo "LATENCY ($v): first 4 KiB of the head after the kill: ${first} ms" >&2
  if ! got=$(docker exec "$flodni" timeout -k 10 "$READ_TIMEOUT" sh -c "s=\$(date +%s%N); h=\$(head -c 8388608 '$path' | sha256sum) && e=\$(date +%s%N) && echo \"\${h%% *} \$(( (e - s) / 1000000 ))\""); then
    echo "FAIL ($v): head read of 8 MiB failed or timed out after the seeder was killed" >&2
    dump_diagnostics
    return 1
  fi
  ms=${got#* } got=${got%% *}
  echo "LATENCY ($v): head 8 MiB read and hash: ${ms} ms" >&2
  if [ "$got" != "$head_want" ]; then
    echo "FAIL ($v): head sha256 mismatch: got $got, want $head_want" >&2
    dump_diagnostics
    return 1
  fi
  if ! got=$(docker exec "$flodni" timeout -k 10 "$READ_TIMEOUT" sh -c "s=\$(date +%s%N); h=\$(tail -c 4194304 '$path' | sha256sum) && e=\$(date +%s%N) && echo \"\${h%% *} \$(( (e - s) / 1000000 ))\""); then
    echo "FAIL ($v): tail read of 4 MiB failed or timed out after the seeder was killed" >&2
    dump_diagnostics
    return 1
  fi
  ms=${got#* } got=${got%% *}
  echo "LATENCY ($v): tail 4 MiB read and hash: ${ms} ms (file size $size)" >&2
  if [ "$got" != "$tail_want" ]; then
    echo "FAIL ($v): tail sha256 mismatch: got $got, want $tail_want" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): head 8 MiB and tail 4 MiB match the seeder's sha256 with the seeder dead" >&2

  # Reads by source: every byte from the SSD windows (the counters were 0 before).
  body=$(prom_body)
  local fb rc wh wt
  fb=$(read_source "$body" fetch_block) rc=$(read_source "$body" racache_hit)
  wh=$(read_source "$body" warmup_head) wt=$(read_source "$body" warmup_tail)
  echo "READS ($v): fetch_block=$fb racache_hit=$rc warmup_head=$wh warmup_tail=$wt" >&2
  if [ "$wh" -lt $((8 * 1048576)) ] || [ "$wt" -lt $((4 * 1048576)) ] || [ "$fb" -ne 0 ] || [ "$rc" -ne 0 ]; then
    echo "FAIL ($v): the reads did not come from the SSD windows: want warmup_head >= 8 MiB, warmup_tail >= 4 MiB, fetch_block 0, racache_hit 0" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): reads served from warmup_head and warmup_tail only" >&2
}

# run_warm_on_add: the warmer against the real engine. Add the fixture with warm_on_add
# on and read nothing through the mount; wait for the fill; check the evidence (metric,
# [Warm] line, per-file row, torrent record); kill the seeder; read the head and the
# tail back through the mount and compare them with the seeder's hashes, and show that
# the bytes came from the SSD windows. Pause, wake and removal while queued are the
# warm-lifecycle variant.
run_warm_on_add() {
  local dir t0=$SECONDS
  start_stack warm-on-add
  local out="$dir/out" v=warm-on-add
  wait_for_file "$out/expected.sha256" "$WAIT_SECONDS" "$v"
  wait_for_file "$out/fuse_path" "$WAIT_SECONDS" "$v"
  local hash head_want tail_want path
  hash=$(cat "$out/hash") head_want=$(cat "$out/head8m.sha256") tail_want=$(cat "$out/tail4m.sha256")
  path="$MOUNT_PATH/$(cat "$out/fuse_path")"
  echo "step add: $((SECONDS - t0))s since start" >&2

  # Step 2: wait for the warmer, with no read through the mount.
  local body="" warmed=0 deadline=$((SECONDS + 120)) w0=$SECONDS
  while [ "$SECONDS" -lt "$deadline" ]; do
    body=$(prom_body)
    warmed=$(awk '/^flodni_warm_fills_total\{trigger="add",outcome="warmed"\} / { printf "%d", $2 }' <<<"$body")
    [ "${warmed:-0}" -ge 1 ] && break
    sleep 1
  done
  if [ "${warmed:-0}" -lt 1 ]; then
    echo "FAIL ($v): no warm-on-add fill reached outcome warmed in 120 s: $(grep -E '^flodni_warm_fills_total\{trigger="add"' <<<"$body" | tr '\n' ' ')" >&2
    dump_diagnostics
    return 1
  fi
  echo "step warm: $((SECONDS - w0))s from the add to a warmed fill" >&2
  echo "ok ($v): $(grep -E '^flodni_warm_fills_total\{trigger="add",outcome="warmed"\}' <<<"$body")" >&2
  local src before_total=0 n
  for src in fetch_block racache_hit warmup_head warmup_tail; do
    n=$(read_source "$body" "$src")
    before_total=$((before_total + n))
  done
  if [ "$before_total" -ne 0 ]; then
    echo "FAIL ($v): flodni_read_bytes_total sums to $before_total before any mount read, want 0: $(grep -E '^flodni_read_bytes_total\{' <<<"$body" | tr '\n' ' ')" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): flodni_read_bytes_total is 0 before any mount read; the warmer filled the windows" >&2
  echo "PROM ($v): $(grep -E '^flodni_warm_(fill_bytes_total|queue_files|paused|cache_failures_total)[{ ]' <<<"$body" | tr '\n' ' ')" >&2

  # The [Warm] line: full windows, swarm-delivered pieces, no file name.
  local logs line json pieces hb tb idx
  logs=$(docker logs "$flodni" 2>&1 || true)
  line=$(grep -F '[Warm] ' <<<"$logs" | grep -F "\"hash\":\"$hash\"" | grep -F '"outcome":"warmed"' | head -n 1 || true)
  if [ -z "$line" ]; then
    echo "FAIL ($v): no [Warm] line with outcome warmed for $hash" >&2
    dump_diagnostics
    return 1
  fi
  json=${line#*\[Warm\] }
  pieces=$(grep -o '"delivered_pieces":[0-9]*' <<<"$json" | cut -d: -f2)
  hb=$(grep -o '"head_bytes":[0-9]*' <<<"$json" | cut -d: -f2)
  tb=$(grep -o '"tail_bytes":[0-9]*' <<<"$json" | cut -d: -f2)
  idx=$(grep -o '"file":[0-9]*' <<<"$json" | cut -d: -f2)
  # The tail window is 4 MiB plus the part of a page its start was rounded down by.
  if [ "${pieces:-0}" -le 0 ] || [ "${hb:-0}" -ne $((16 * 1048576)) ] ||
    [ "${tb:-0}" -lt $((4 * 1048576)) ] || [ "${tb:-0}" -ge $((4 * 1048576 + 4096)) ]; then
    echo "FAIL ($v): [Warm] line wants delivered_pieces > 0, head_bytes 16 MiB, tail_bytes 4 MiB to 4 MiB + 4095: $json" >&2
    dump_diagnostics
    return 1
  fi
  if grep -qE 'Flodni\.E2E|\.mkv|/mnt/' <<<"$json"; then
    echo "FAIL ($v): the [Warm] line carries a path or file name: $json" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): [Warm] $json" >&2

  # The per-file row and the torrent record.
  local row="" trow="" deadline2=$((SECONDS + 30))
  while [ "$SECONDS" -lt "$deadline2" ]; do
    body=$(health_body)
    row=$(grep -o "{\"file_index\":$idx,[^}]*}" <<<"$body" || true)
    trow=$(grep -o "{\"hash\":\"$hash\"[^}]*" <<<"$body" || true)
    [ -n "$row" ] && break
    sleep 1
  done
  if [[ "$row" != *'"readability":"warmed"'* || "$row" != *'"cache":"cached"'* ||
    "$row" != *'"last_swarm_delivery_at":"'* || "$row" != *'"last_attempt_at":"'* ]]; then
    echo "FAIL ($v): files row of file $idx wants readability warmed, cache cached, last_swarm_delivery_at and last_attempt_at set: ${row:-none}" >&2
    dump_diagnostics
    return 1
  fi
  if [[ "$trow" != *'"last_swarm_read_at":"'* ]]; then
    echo "FAIL ($v): the torrent record has no last_swarm_read_at: ${trow:-none}" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): files row $row" >&2
  echo "ok ($v): torrent record ${trow}" >&2

  check_readback "$v" "$path" "$head_want" "$tail_want" || return 1
  echo "NOTE ($v): pause, wake and removal while queued are the warm-lifecycle variant" >&2
  echo "PASS ($v): windows filled by the warmer, read back with the seeder dead, $((SECONDS - t0))s" >&2

  stop_stack
}

# warm_line hash outcome: the first [Warm] line of the hash with that outcome ("" any).
warm_line() {
  local logs
  logs=$(docker logs "$flodni" 2>&1 || true)
  grep -F '[Warm] ' <<<"$logs" | grep -F "\"hash\":\"$1\"" | grep -F "${2:+\"outcome\":\"$2\"}" | head -n 1 || true
}

# lc_wait v label seconds series value: poll a Prometheus series until it equals value.
lc_wait() {
  local v=$1 label=$2 limit=$3 series=$4 want=$5 deadline got
  deadline=$((SECONDS + limit))
  while :; do
    got=$(prom_value "$(prom_body)" "$series")
    [ "$got" = "$want" ] && return 0
    if [ "$SECONDS" -ge "$deadline" ]; then
      echo "FAIL ($v): $label: $series is $got after ${limit}s, want $want" >&2
      return 1
    fi
    sleep 1
  done
}

# library_remove hash: POST /api/library/remove; prints the status line.
library_remove() {
  docker exec "$flodni" bash -c 'b="{\"hash\":\"$1\"}"
    exec 3<>/dev/tcp/127.0.0.1/9080
    printf "POST /api/library/remove HTTP/1.0\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: ${#b}\r\n\r\n%s" "$b" >&3
    head -n 1 <&3' _ "$1" 2>/dev/null || true
}

# lc_stop v reader: a failed step. Ends the playback hold, prints the diagnostics.
lc_stop() {
  docker exec "$flodni" touch /tmp/lc.go >/dev/null 2>&1 || true
  [ -n "${2:-}" ] && kill "$2" 2>/dev/null || true
  dump_diagnostics
}

# run_warm_lifecycle: warm on add against the real engine, the parts the warm-on-add
# variant leaves out. A is warmed, then held open by a descriptor and a confirmed play
# webhook, so the warmer pauses for it. B is filed and removed while queued; C is filed and
# waits until the engine closes its torrent. Then the playback ends: the pause lifts, C
# is woken from the stored record and warmed, B is never warmed. A public canary tracker
# in trackers.txt gets a public control title and no private one (hard rule 3). DHT and
# PEX are not observable on the internal network and stay with the unit pins.
run_warm_lifecycle() {
  local dir t0=$SECONDS
  start_stack warm-lifecycle -mode warm-lifecycle
  local out="$dir/out" v=warm-lifecycle
  printf 'http://seeder:%s/announce\n' 6970 >"$dir/state/trackers.txt"
  wait_for_file "$out/a.path" "$WAIT_SECONDS" "$v"
  local hash_a path_a name
  hash_a=$(cat "$out/a.hash") path_a="$MOUNT_PATH/$(cat "$out/a.path")" name=$(basename "$path_a")
  echo "step add A: $((SECONDS - t0))s since start" >&2

  # Step 1: A is warmed with nothing in the way.
  local deadline=$((SECONDS + 120)) line=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    line=$(warm_line "$hash_a" warmed)
    [ -n "$line" ] && break
    sleep 1
  done
  if [ -z "$line" ]; then
    echo "FAIL ($v): A was not warmed in 120 s: $(warm_line "$hash_a")" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): A warmed: ${line#*\[Warm\] }" >&2

  # Step 2: hold A open and confirm its playback, so the warmer has a reason to pause.
  docker exec "$flodni" rm -f /tmp/lc.go
  docker exec "$flodni" timeout -k 10 600 sh -c \
    '( dd bs=1048576 count=1 iflag=fullblock of=/dev/null status=none; \
       until [ -e /tmp/lc.go ]; do sleep 0.1; done ) < "$1"' _ "$path_a" >"$out/hold.out" 2>"$out/hold.err" &
  local reader=$!
  sleep 2
  local payload resp
  payload='{"event":"media.play","Metadata":{"title":"E2E Fixture","year":2026,"librarySectionType":"movie","Media":[{"Part":[{"file":"/data/movies/'"$name"'"}]}]}}'
  resp=$(post_webhook "$payload")
  if ! head -n 1 <<<"$resp" | grep -q ' 200 '; then
    echo "FAIL ($v): play webhook post: $(head -n 1 <<<"$resp")" >&2
    lc_stop "$v" "$reader"
    return 1
  fi

  # Step 3: B is filed while A plays: queued, the warmer paused, nothing warmed.
  touch "$out/add-b"
  wait_for_file "$out/b.path" "$WAIT_SECONDS" "$v" || { lc_stop "$v" "$reader"; return 1; }
  local hash_b
  hash_b=$(cat "$out/b.hash")
  lc_wait "$v" "B queued" 30 flodni_warm_queue_files 1 || { lc_stop "$v" "$reader"; return 1; }
  lc_wait "$v" "B queued" 30 flodni_warm_paused 1 || { lc_stop "$v" "$reader"; return 1; }
  sleep 8
  if [ -n "$(warm_line "$hash_b" "")" ]; then
    echo "FAIL ($v): B has a [Warm] line while the warmer is paused: $(warm_line "$hash_b" "")" >&2
    lc_stop "$v" "$reader"
    return 1
  fi
  echo "ok ($v): B queued behind A's playback: flodni_warm_queue_files 1, flodni_warm_paused 1, no [Warm] line in 8 s" >&2

  # Step 4: B is removed while queued: the queue empties, the pause stays.
  resp=$(library_remove "$hash_b")
  if ! grep -q ' 200 ' <<<"$resp"; then
    echo "FAIL ($v): library remove of B: $resp" >&2
    lc_stop "$v" "$reader"
    return 1
  fi
  lc_wait "$v" "B removed" 30 flodni_warm_queue_files 0 || { lc_stop "$v" "$reader"; return 1; }
  echo "ok ($v): B removed while queued: flodni_warm_queue_files 0" >&2

  # Step 5: C is filed and queued; the engine closes its idle torrent during the pause.
  touch "$out/add-c"
  wait_for_file "$out/c.path" "$WAIT_SECONDS" "$v" || { lc_stop "$v" "$reader"; return 1; }
  local hash_c stat="" list
  hash_c=$(cat "$out/c.hash")
  lc_wait "$v" "C queued" 30 flodni_warm_queue_files 1 || { lc_stop "$v" "$reader"; return 1; }
  deadline=$((SECONDS + 90))
  while [ "$SECONDS" -lt "$deadline" ]; do
    list=$(gostorm_list)
    stat=$(grep -o "\"hash\":\"$hash_c\"[^}]*" <<<"$list" | grep -o '"stat_string":"[^"]*"' | head -n 1 | cut -d'"' -f4 || true)
    if grep -qiE 'closed|in db' <<<"$stat"; then break; fi
    sleep 1
  done
  if ! grep -qiE 'closed|in db' <<<"$stat"; then
    echo "FAIL ($v): C's torrent is not closed after 90 s: ${stat:-no entry}" >&2
    lc_stop "$v" "$reader"
    return 1
  fi
  if [ "$(prom_value "$(prom_body)" flodni_warm_paused)" != 1 ] || [ -n "$(warm_line "$hash_c" "")" ]; then
    echo "FAIL ($v): C was warmed or the pause lifted before the playback ended: $(warm_line "$hash_c" "")" >&2
    lc_stop "$v" "$reader"
    return 1
  fi
  echo "ok ($v): C queued, its torrent closed ($stat), warmer still paused, no [Warm] line for B or C" >&2

  # Step 6: the playback ends. Pause lifts, C is woken from the stored record and warmed.
  docker exec "$flodni" touch /tmp/lc.go
  wait "$reader" || true
  resp=$(post_webhook "${payload/media.play/media.stop}")
  if ! head -n 1 <<<"$resp" | grep -q ' 200 '; then
    echo "FAIL ($v): stop webhook post: $(head -n 1 <<<"$resp")" >&2
    dump_diagnostics
    return 1
  fi
  lc_wait "$v" "the playback ended" 90 flodni_warm_paused 0 || { dump_diagnostics; return 1; }
  deadline=$((SECONDS + 120))
  line=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    line=$(warm_line "$hash_c" "")
    [ -n "$line" ] && break
    sleep 1
  done
  if [[ "$line" != *'"outcome":"warmed"'* || "$line" != *'"trigger":"add"'* ]]; then
    echo "FAIL ($v): C wants a [Warm] line trigger add, outcome warmed: ${line:-none}" >&2
    dump_diagnostics
    return 1
  fi
  local hb tb
  hb=$(json_num "$line" head_bytes) tb=$(json_num "$line" tail_bytes)
  if [ "${hb:-0}" -ne $((16 * 1048576)) ] || [ "${tb:-0}" -lt $((4 * 1048576)) ]; then
    echo "FAIL ($v): C's windows are not full: $line" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): C woken from the stored record and warmed after the pause: ${line#*\[Warm\] }" >&2

  # Step 7: B was never warmed, left nothing behind and the totals add up.
  sleep 5
  local body
  body=$(prom_body)
  if [ -n "$(warm_line "$hash_b" "")" ] || [ "$(cache_files "$hash_b")" != 0 ] ||
    [ "$(gostorm_get_status "$hash_b")" != 404 ]; then
    echo "FAIL ($v): B left a trace: [Warm] '$(warm_line "$hash_b" "")', cache files $(cache_files "$hash_b"), get $(gostorm_get_status "$hash_b")" >&2
    dump_diagnostics
    return 1
  fi
  local warmed bad
  warmed=$(prom_value "$body" 'flodni_warm_fills_total{trigger="add",outcome="warmed"}')
  bad=$(grep -E '^flodni_warm_fills_total\{trigger="add",outcome="(wake_error|skipped)"\} ' <<<"$body" | awk '$2 != 0' || true)
  if [ "$warmed" != 2 ] || [ -n "$bad" ] || [ "$(prom_value "$body" flodni_wake_refused_total)" != 0 ]; then
    echo "FAIL ($v): want 2 warmed fills (A and C), no wake_error or skipped, no refused wake: warmed=$warmed bad='$bad' refused=$(prom_value "$body" flodni_wake_refused_total)" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): B never warmed, no cache file, get 404; fills add warmed=$warmed, no wake_error, no refused wake" >&2

  # Step 8: the canary tracker. The public control title must reach it, the private
  # titles (A, B and C, one woken) must not.
  touch "$out/add-p"
  wait_for_file "$out/p.path" "$WAIT_SECONDS" "$v"
  local hash_p canary=""
  hash_p=$(cat "$out/p.hash")
  deadline=$((SECONDS + 45))
  while [ "$SECONDS" -lt "$deadline" ]; do
    canary=$(cat "$out/canary.log")
    grep -F "$hash_p" <<<"$canary" >/dev/null && break
    sleep 1
  done
  if ! grep -F "$hash_p" <<<"$canary" >/dev/null; then
    echo "FAIL ($v): the canary tracker never saw the public control title: the canary proves nothing" >&2
    dump_diagnostics
    return 1
  fi
  local h
  for h in "$hash_a" "$hash_b" "$hash_c"; do
    if grep -F "$h" <<<"$canary" >/dev/null; then
      echo "FAIL ($v): a private torrent announced to the public canary tracker: $h" >&2
      dump_diagnostics
      return 1
    fi
  done
  echo "ok ($v): canary tracker saw the public control title ($(grep -c . <<<"$canary") announce(s)) and none of the private A, B, C" >&2
  echo "NOTE ($v): DHT and PEX are not observable on the internal network; the unit pins cover them" >&2
  echo "PASS ($v): B queued, removed and never warmed; C queued behind a playback, woken from the stored record and warmed after it; no private torrent reached the canary, $((SECONDS - t0))s" >&2

  stop_stack
}

# catchup_lines: the [WarmCatchup] lines of the container's whole log (a restart keeps it).
catchup_lines() { docker logs "$flodni" 2>&1 | grep -F '[WarmCatchup] ' || true; }

# json_num line key: the integer value of "key" in a JSON log line, empty if absent.
json_num() { grep -o "\"$2\":[0-9]*" <<<"$1" | head -n 1 | cut -d: -f2; }

# catchup_zero body label: every catch-up and add fill series, the catch-up fill bytes
# and every backlog gauge state must be 0. Prints the series it checked.
catchup_zero() {
  local body=$1 label=$2 bad
  bad=$(grep -E '^(flodni_warm_fills_total\{trigger="(add|catchup)"|flodni_warm_fill_bytes_total\{trigger="catchup"|flodni_warm_catchup_files\{)' <<<"$body" | awk '$2 != 0' || true)
  if [ -n "$bad" ] || ! grep -q '^flodni_warm_catchup_files{state="pending"} 0$' <<<"$body"; then
    echo "FAIL ($label): want every add/catch-up fill series, the catch-up bytes and the gauge at 0: ${bad:-gauge missing}" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($label): add and catch-up fills, catch-up bytes and the backlog gauge are all 0" >&2
}

# catchup_pass label want_passes: after a restart, wait up to 120 s for the want_passes-th
# [WarmCatchup] pass_end line. Fails fast, with the [Warm] and [WarmCatchup] lines, on a
# wake_error outcome or a container that exited. A first pass that feeds nothing is not
# retried for 6 hours, so waiting longer would only hide it. Sets pass_start_line and
# pass_end_line.
catchup_pass() {
  local label=$1 want=$2 deadline=$((SECONDS + 120)) lines n
  while :; do
    lines=$(catchup_lines)
    n=$(grep -c '"event":"pass_end"' <<<"$lines" || true)
    [ "$n" -ge "$want" ] && break
    if ! is_running "$flodni" || docker logs "$flodni" 2>&1 | grep -F '[Warm] ' | grep -qF '"outcome":"wake_error"'; then
      echo "FAIL ($label): a container exited or a wake_error was logged before pass $want ended" >&2
      docker logs "$flodni" 2>&1 | grep -E '\[Warm(Catchup)?\] ' >&2 || true
      dump_diagnostics
      return 1
    fi
    if [ "$SECONDS" -ge "$deadline" ]; then
      echo "FAIL ($label): no pass_end number $want in 120 s" >&2
      docker logs "$flodni" 2>&1 | grep -E '\[Warm(Catchup)?\] |WARNING' >&2 || true
      dump_diagnostics
      return 1
    fi
    sleep 1
  done
  pass_start_line=$(grep -F '"event":"pass_start"' <<<"$lines" | sed -n "${want}p")
  pass_end_line=$(grep -F '"event":"pass_end"' <<<"$lines" | sed -n "${want}p")
  echo "ok ($label): $pass_start_line" >&2
  echo "ok ($label): $pass_end_line" >&2
}

# gostorm_list: the engine's torrent list (POST /torrents list on 8090; the variant sets
# no token), over /dev/tcp, body only. Empty until the engine's API is up.
gostorm_list() {
  docker exec "$flodni" bash -c 'b="{\"action\":\"list\"}"
    exec 3<>/dev/tcp/127.0.0.1/8090 && printf "POST /torrents HTTP/1.0\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: ${#b}\r\n\r\n%s" "$b" >&3 && cat <&3' 2>/dev/null | sed '1,/^\r$/d' || true
}

# run_catchup: catch-up (#124, #125) against the real engine across daemon restarts.
# Off phase: the fixture is added with warm_on_add false and nothing reads it, so catch-up
# must stay silent. Then config.json is rewritten in place with warm_on_add true and the
# container restarted (state and cache are volumes, the stub stays in the container's
# layer): the first pass, 1 s after the start, must warm the title with trigger catchup.
# A second restart must find nothing to do. Last, the seeder dies and the windows are read
# back from the SSD. The quota stop, the eviction exclusion, the stale-evicted overwrite
# and the 20 h rule are unit tests only (warm_catchup_test.go, internal/warmer).
run_catchup() {
  local dir t0=$SECONDS
  start_stack catch-up
  local out="$dir/out" v=catch-up
  wait_for_file "$out/expected.sha256" "$WAIT_SECONDS" "$v"
  wait_for_file "$out/fuse_path" "$WAIT_SECONDS" "$v"
  local hash head_want tail_want path
  hash=$(cat "$out/hash") head_want=$(cat "$out/head8m.sha256") tail_want=$(cat "$out/tail4m.sha256")
  path="$MOUNT_PATH/$(cat "$out/fuse_path")"
  echo "step add: $((SECONDS - t0))s since start" >&2

  # Step 1: off phase. The first pass (1 s after the start, long done) was skipped.
  local body lines
  sleep 5
  lines=$(catchup_lines)
  if [ -n "$lines" ] || docker logs "$flodni" 2>&1 | grep -F 'WARNING: [WarmCatchup]' >/dev/null; then
    echo "FAIL ($v): off phase: a [WarmCatchup] line or warning was logged: $lines" >&2
    dump_diagnostics
    return 1
  fi
  body=$(prom_body)
  catchup_zero "$body" "$v off" || return 1
  echo "ok ($v): off phase: no [WarmCatchup] line, no catch-up warning" >&2

  # Step 2: warm_on_add on, in place, restart. The torrent is only a record in the engine's
  # database after the restart; poll the engine's list to see its state before the pass.
  echo "step restart 1: warm_on_add true, warm_catchup_per_hour 3600" >&2
  catchup_config true "$dir/config.json"
  local r0=$SECONDS list="" stat=""
  docker restart "$flodni" >/dev/null
  while [ "$((SECONDS - r0))" -lt 20 ]; do
    list=$(gostorm_list)
    stat=$(grep -o "\"hash\":\"$hash\"[^}]*" <<<"$list" | grep -o '"stat_string":"[^"]*"' | head -n 1 | cut -d'"' -f4 || true)
    [ -n "$stat" ] && break
    sleep 0.2
  done
  echo "WAKE ($v): engine list right after the restart: ${stat:-no entry}" >&2
  local wake_seen=${stat:-none}
  catchup_pass "$v restart 1" 1 || return 1
  local start1=$pass_start_line end1=$pass_end_line
  echo "step catch-up: $((SECONDS - r0))s from the restart to pass_end" >&2
  if [ "$(json_num "$start1" pending)" != 1 ] || [ "$(json_num "$start1" candidate_hashes)" != 1 ]; then
    echo "FAIL ($v): pass_start wants candidate_hashes 1 and pending 1: $start1" >&2
    dump_diagnostics
    return 1
  fi
  if [ "$(json_num "$end1" fed)" != 1 ] || [ "$(json_num "$end1" fed_hashes)" != 1 ]; then
    echo "FAIL ($v): pass_end wants fed_hashes 1 and fed 1: $end1" >&2
    docker logs "$flodni" 2>&1 | grep -E '\[Warm(Catchup)?\] ' >&2 || true
    dump_diagnostics
    return 1
  fi
  body=$(prom_body)
  local cu add_bad bytes rt
  cu=$(awk '/^flodni_warm_fills_total\{trigger="catchup",outcome="warmed"\} / { printf "%d", $2 }' <<<"$body")
  add_bad=$(grep -E '^flodni_warm_fills_total\{trigger="add"' <<<"$body" | awk '$2 != 0' || true)
  bytes=$(awk '/^flodni_warm_fill_bytes_total\{trigger="catchup"/ { n += $2 } END { printf "%d", n }' <<<"$body")
  rt=0
  local src n
  for src in fetch_block racache_hit warmup_head warmup_tail; do
    n=$(read_source "$body" "$src")
    rt=$((rt + n))
  done
  if [ "${cu:-0}" -ne 1 ] || [ -n "$add_bad" ] || [ "${bytes:-0}" -le 0 ] || [ "$rt" -ne 0 ]; then
    echo "FAIL ($v): want catchup warmed 1, every add outcome 0, catch-up bytes > 0, read bytes 0: warmed=${cu:-absent} add_nonzero='$add_bad' bytes=$bytes read=$rt" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): $(grep -E '^flodni_warm_fills_total\{trigger="catchup",outcome="warmed"\}' <<<"$body"), every trigger=add outcome 0, catch-up fill bytes $bytes, flodni_read_bytes_total 0" >&2
  echo "PROM ($v): $(grep -E '^flodni_warm_fill_bytes_total\{trigger="catchup"' <<<"$body" | tr '\n' ' ')" >&2
  # Every gauge state is 0 once the pass has ended (the gauge is set before the line).
  if [ -n "$(grep -E '^flodni_warm_catchup_files\{' <<<"$body" | awk '$2 != 0')" ]; then
    echo "FAIL ($v): a backlog gauge state is not 0 after pass_end: $(grep -E '^flodni_warm_catchup_files\{' <<<"$body" | tr '\n' ' ')" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): after pass_end every flodni_warm_catchup_files state is 0" >&2

  local line json idx
  line=$(docker logs "$flodni" 2>&1 | grep -F '[Warm] ' | grep -F '"trigger":"catchup"' || true)
  if [ "$(wc -l <<<"$line" | tr -d ' ')" != 1 ] || [ -z "$line" ]; then
    echo "FAIL ($v): want exactly one [Warm] line with trigger catchup: ${line:-none}" >&2
    dump_diagnostics
    return 1
  fi
  json=${line#*\[Warm\] }
  idx=$(json_num "$json" file)
  if ! grep -qF "\"hash\":\"$hash\"" <<<"$json" || ! grep -qF '"outcome":"warmed"' <<<"$json" ||
    grep -qE 'Flodni\.E2E|\.mkv|/mnt/' <<<"$json"; then
    echo "FAIL ($v): the catch-up [Warm] line is not warmed for $hash or carries a path: $json" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): [Warm] $json" >&2

  local row="" trow="" deadline=$((SECONDS + 30))
  while [ "$SECONDS" -lt "$deadline" ]; do
    body=$(health_body)
    row=$(grep -o "{\"file_index\":$idx,[^}]*}" <<<"$body" || true)
    trow=$(grep -o "{\"hash\":\"$hash\"[^}]*" <<<"$body" || true)
    [ -n "$row" ] && break
    sleep 1
  done
  if [[ "$row" != *'"readability":"warmed"'* || "$row" != *'"cache":"cached"'* ||
    "$row" != *'"last_swarm_delivery_at":"'* || "$row" != *'"last_attempt_at":"'* ]]; then
    echo "FAIL ($v): files row of file $idx wants readability warmed, cache cached, last_swarm_delivery_at and last_attempt_at set: ${row:-none}" >&2
    dump_diagnostics
    return 1
  fi
  if [[ "$trow" != *'"last_swarm_read_at":"'* ]]; then
    echo "FAIL ($v): the torrent record has no last_swarm_read_at: ${trow:-none}" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): files row $row" >&2
  echo "ok ($v): torrent record ${trow}" >&2
  local attempt1
  attempt1=$(grep -o '"last_attempt_at":"[^"]*"' <<<"$row")

  # The WakeStored claim: a woken torrent was closed. Only the engine's list right after
  # the start can show it, and the pass runs 1 s after the start, so say what was seen.
  if [[ "$wake_seen" == *"in db"* ]]; then
    echo "ok ($v): WakeStored on a closed private torrent shown: the engine listed it as \"${wake_seen}\" when its API first answered, before the pass fed it" >&2
  else
    echo "NOTE ($v): the WakeStored-on-a-closed-private-torrent claim is unverified by this run: the engine list when its API first answered showed \"${wake_seen}\"" >&2
  fi

  # Step 3: a second restart with the same config finds nothing to do.
  echo "step restart 2: no config change" >&2
  docker restart "$flodni" >/dev/null
  catchup_pass "$v restart 2" 2 || return 1
  local start2=$pass_start_line end2=$pass_end_line
  if [ "$(json_num "$start2" pending)" != 0 ] || [ "$(json_num "$end2" fed)" != 0 ]; then
    echo "FAIL ($v): second pass wants pending 0 and fed 0: $start2 / $end2" >&2
    dump_diagnostics
    return 1
  fi
  body=$(prom_body)
  catchup_zero "$body" "$v restart 2" || return 1
  if [ "$(docker logs "$flodni" 2>&1 | grep -F '[Warm] ' | grep -cF '"trigger":"catchup"')" != 1 ]; then
    echo "FAIL ($v): a second catch-up [Warm] line appeared after the second restart" >&2
    dump_diagnostics
    return 1
  fi
  local row2 attempt2
  row2=$(health_body | grep -o "{\"file_index\":$idx,[^}]*}" || true)
  attempt2=$(grep -o '"last_attempt_at":"[^"]*"' <<<"$row2")
  if [ -z "$attempt1" ] || [ "$attempt1" != "$attempt2" ] || [[ "$row2" != *'"cache":"cached"'* ]]; then
    echo "FAIL ($v): the files row changed or is gone after the second restart: before $attempt1, after: ${row2:-none}" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): second restart: pending 0, fed 0, no catch-up fill or bytes, no new [Warm] line, $attempt2 unchanged" >&2

  # Step 4: the windows survive the restarts: read them back with the seeder dead.
  check_readback "$v" "$path" "$head_want" "$tail_want" || return 1
  echo "PASS ($v): off phase silent, one catch-up fill after a restart, none after the next, read back with the seeder dead, $((SECONDS - t0))s" >&2

  stop_stack
}

# post_webhook body: POST a JSON webhook to the control port from inside the container
# (no curl in the image); prints the response, status line first.
post_webhook() {
  docker exec "$flodni" bash -c 'body=$1
    exec 3<>/dev/tcp/127.0.0.1/9080 && printf "POST /plex/webhook HTTP/1.0\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s" "${#body}" "$body" >&3 && cat <&3' _ "$1" 2>/dev/null || true
}

# run_late_confirm: the entry condition for the reader budget's phase b. A full read
# of the fixture is held half way, after the session has crossed the 8 MiB margin
# (the over_margin [ReaderBudget] line is the proof, polled for). A play webhook for
# the file is then posted in the middle of the read, and the read is let go on. The
# read must complete with the right sha256, the webhook must count matched="true",
# and the session, which resolves when it closes (10 s idle plus up to 2 s to the
# next cleanup tick, both shortened by test-only env vars), must count as
# confirmed_late and not as unconfirmed.
run_late_confirm() {
  local dir t0=$SECONDS v=late-confirm
  start_stack "$v"
  local out="$dir/out"
  wait_for_file "$out/expected.sha256" "$WAIT_SECONDS" "$v"
  wait_for_file "$out/fuse_path" "$WAIT_SECONDS" "$v"
  local expected path name
  expected=$(cat "$out/expected.sha256")
  path="$MOUNT_PATH/$(cat "$out/fuse_path")"
  name=$(basename "$path")
  echo "step add: $((SECONDS - t0))s since start" >&2

  # One open file description: the second dd continues where the first stopped, and
  # the file stays open through the pause. iflag=fullblock makes count=24 mean 24 MiB.
  docker exec "$flodni" rm -f /tmp/late.go
  docker exec "$flodni" timeout -k 10 "$READ_TIMEOUT" sh -c \
    '( dd bs=1048576 count=24 iflag=fullblock status=none; \
       until [ -e /tmp/late.go ]; do sleep 0.1; done; \
       dd bs=1048576 status=none ) < "$1" | sha256sum' _ "$path" >"$out/late.sha" 2>"$out/late.err" &
  local reader=$!

  local deadline=$((SECONDS + 120)) lines=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    lines=$(docker logs "$flodni" 2>&1 | grep -F '[ReaderBudget] ' || true)
    grep -qF '"event":"over_margin"' <<<"$lines" && break
    kill -0 "$reader" 2>/dev/null || break
    sleep 0.5
  done
  if ! grep -qF '"event":"over_margin"' <<<"$lines"; then
    echo "FAIL ($v): no over_margin [ReaderBudget] line before the webhook: ${lines:-none}" >&2
    kill "$reader" 2>/dev/null || true
    dump_diagnostics
    return 1
  fi
  echo "step crossed: $((SECONDS - t0))s since start, $(grep -F '"event":"over_margin"' <<<"$lines")" >&2

  # The payload is what Plex sends, reduced to what the handler reads: the event, the
  # library type and the file name of the part, which is the exact-match key.
  local payload resp
  payload='{"event":"media.play","Metadata":{"title":"E2E Fixture","year":2026,"librarySectionType":"movie","Media":[{"Part":[{"file":"/data/movies/'"$name"'"}]}]}}'
  resp=$(post_webhook "$payload")
  if ! head -n 1 <<<"$resp" | grep -q ' 200 '; then
    echo "FAIL ($v): webhook post: $(head -n 1 <<<"$resp")" >&2
    kill "$reader" 2>/dev/null || true
    dump_diagnostics
    return 1
  fi
  echo "step webhook: $((SECONDS - t0))s since start, $(head -n 1 <<<"$resp" | tr -d '\r')" >&2
  docker exec "$flodni" touch /tmp/late.go

  if ! wait "$reader"; then
    echo "FAIL ($v): the held read failed or timed out: $(cat "$out/late.err")" >&2
    dump_diagnostics
    return 1
  fi
  local actual
  actual=$(awk '{ print $1 }' "$out/late.sha")
  if [ "$actual" != "$expected" ]; then
    echo "FAIL ($v): sha256 mismatch after the webhook: got ${actual:-none}, want $expected" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): the read completed across the webhook, sha256 $actual" >&2

  local body matched unmatched late unconf
  body=$(prom_body)
  matched=$(awk '/^flodni_playback_webhooks_received_total\{matched="true"\} / { printf "%d", $2 }' <<<"$body")
  unmatched=$(awk '/^flodni_playback_webhooks_received_total\{matched="false"\} / { printf "%d", $2 }' <<<"$body")
  if [ "${matched:-0}" != 1 ] || [ "${unmatched:-0}" != 0 ]; then
    echo "FAIL ($v): flodni_playback_webhooks_received_total matched=true ${matched:-absent}, matched=false ${unmatched:-absent}, want 1 and 0" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): $(grep -E '^flodni_playback_webhooks_received_total\{matched="true"\}' <<<"$body")" >&2

  # The session resolves at close: 10 s idle, then the cleanup tick (2 s). Poll 45 s.
  local c0=$SECONDS
  deadline=$((SECONDS + 45))
  late="" unconf=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    body=$(prom_body)
    late=$(awk '/^flodni_unconfirmed_read_sessions_total\{outcome="confirmed_late"\} / { printf "%d", $2 }' <<<"$body")
    unconf=$(awk '/^flodni_unconfirmed_read_sessions_total\{outcome="unconfirmed"\} / { printf "%d", $2 }' <<<"$body")
    [ "${late:-0}" -ge 1 ] || [ "${unconf:-0}" -ge 1 ] && break
    sleep 2
  done
  if [ "${late:-0}" != 1 ] || [ "${unconf:-0}" != 0 ]; then
    echo "FAIL ($v): after $((SECONDS - c0)) s the sessions are confirmed_late ${late:-absent}, unconfirmed ${unconf:-absent}, want 1 and 0" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): $(grep -E '^flodni_unconfirmed_read_sessions_total\{' <<<"$body" | tr '\n' ' ')(session closed $((SECONDS - c0)) s after the read)" >&2
  check_no_warning "$v" matched || return 1

  lines=$(docker logs "$flodni" 2>&1 | grep -F '[ReaderBudget] ' || true)
  if [ "$(sed -n 's/.*"event":"\([a-z_]*\)".*/\1/p' <<<"$lines" | tr '\n' ' ')" != "over_margin confirmed_late " ]; then
    echo "FAIL ($v): want [ReaderBudget] lines over_margin then confirmed_late, got: ${lines:-none}" >&2
    dump_diagnostics
    return 1
  fi
  if ! grep -Eq '"event":"confirmed_late".*"late_ms":[0-9]+' <<<"$lines" || grep -qF "$name" <<<"$lines"; then
    echo "FAIL ($v): the confirmed_late line lacks late_ms or carries the file name: $lines" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): $(grep -F '"event":"confirmed_late"' <<<"$lines")" >&2
  echo "PASS ($v): a play webhook in the middle of a read past the margin is confirmed_late and counted, read intact, $((SECONDS - t0))s" >&2
  stop_stack
}

# prom_value body series: the value of one series, "absent" if the body lacks it.
prom_value() {
  awk -v s="$2" 'index($0, s " ") == 1 { printf "%d", $2; f = 1 } END { if (!f) printf "absent" }' <<<"$1"
}

# run_keepalive: reliability step 8, phase a counters and the phase b hold. One open
# handle reads 1 MiB at 60 MiB (outside the 4 MiB windows, so the engine serves it).
# With no reader and no confirmed playback the engine closes the torrent after its 15 s
# idle timeout: the unconfirmed flodni_torrent_idle_closes_total must reach 1. A play
# webhook is then posted while the torrent is closed (the old per-object mark would be
# skipped here, the leak phase a measured); the same handle reads on (a reopen). The
# hold derives from the playback state, so the reopened torrent must not idle-close
# over two idle windows. A media.stop webhook then releases it and the torrent closes.
run_keepalive() {
  local dir t0=$SECONDS v=keep-alive
  start_stack "$v"
  local out="$dir/out"
  wait_for_file "$out/expected.sha256" "$WAIT_SECONDS" "$v"
  wait_for_file "$out/fuse_path" "$WAIT_SECONDS" "$v"
  local path name
  path="$MOUNT_PATH/$(cat "$out/fuse_path")"
  name=$(basename "$path")

  # One open file description: the second dd continues on the same handle, 20 MiB on.
  docker exec "$flodni" rm -f /tmp/ka.go
  docker exec "$flodni" timeout -k 10 "$READ_TIMEOUT" sh -c \
    '( dd bs=1048576 skip=60 count=1 iflag=fullblock of=/dev/null status=none; \
       until [ -e /tmp/ka.go ]; do sleep 0.1; done; \
       dd bs=1048576 skip=20 count=1 iflag=fullblock of=/dev/null status=none ) < "$1"' _ "$path" >"$out/ka.out" 2>"$out/ka.err" &
  local reader=$!

  # Past the timeout with no reader: poll up to 45 s for the unconfirmed idle close.
  local body ic0 deadline=$((SECONDS + 45))
  ic0=absent
  while [ "$SECONDS" -lt "$deadline" ]; do
    body=$(prom_body)
    ic0=$(prom_value "$body" 'flodni_torrent_idle_closes_total{confirmed="false"}')
    [ "$ic0" != absent ] && [ "$ic0" -ge 1 ] && break
    sleep 1
  done
  if [ "$ic0" = absent ] || [ "$ic0" -lt 1 ]; then
    echo "FAIL ($v): no unconfirmed idle close within 45 s: idle_closes{confirmed=\"false\"} ${ic0}" >&2
    kill "$reader" 2>/dev/null || true
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): the engine closed the idle torrent, $(prom_value "$body" 'flodni_torrent_idle_closes_total{confirmed="false"}') unconfirmed close(s), $((SECONDS - t0))s since start" >&2

  local payload resp
  payload='{"event":"media.play","Metadata":{"title":"E2E Fixture","year":2026,"librarySectionType":"movie","Media":[{"Part":[{"file":"/data/movies/'"$name"'"}]}]}}'
  resp=$(post_webhook "$payload")
  if ! head -n 1 <<<"$resp" | grep -q ' 200 '; then
    echo "FAIL ($v): webhook post: $(head -n 1 <<<"$resp")" >&2
    kill "$reader" 2>/dev/null || true
    dump_diagnostics
    return 1
  fi
  echo "step webhook (torrent closed): $((SECONDS - t0))s since start" >&2
  # The confirmed label comes from a set rebuilt once a second, last built at the idle
  # close: without this pause the reopen below is labelled with the state before the
  # webhook.
  sleep 2
  local ic1 total0
  body=$(prom_body)
  ic1=$(prom_value "$body" 'flodni_torrent_idle_closes_total{confirmed="true"}')
  total0=$(( $(prom_value "$body" 'flodni_torrent_idle_closes_total{confirmed="false"}') + ${ic1/absent/0} ))
  docker exec "$flodni" touch /tmp/ka.go
  if ! wait "$reader"; then
    echo "FAIL ($v): the handle's reads failed: $(cat "$out/ka.err")" >&2
    dump_diagnostics
    return 1
  fi

  # The torrent was closed when the webhook arrived: the second read must have woken it.
  body=$(prom_body)
  local ro
  ro=$(( $(prom_value "$body" 'flodni_torrent_reopens_total{confirmed="true"}') + $(prom_value "$body" 'flodni_torrent_reopens_total{confirmed="false"}') ))
  if [ "$ro" -lt 1 ]; then
    echo "FAIL ($v): the read after the webhook did not reopen the torrent (reopens $ro): the premise of the leg does not hold" >&2
    dump_diagnostics
    return 1
  fi

  # Two idle windows (2 x (15 s timeout + 1 s ticker) + 2 s) with no reader: the hold
  # must keep the reopened torrent open, so no idle close of either label may appear.
  local total=$total0 ict deadline=$((SECONDS + 34))
  while [ "$SECONDS" -lt "$deadline" ]; do
    body=$(prom_body)
    ict=$(prom_value "$body" 'flodni_torrent_idle_closes_total{confirmed="true"}')
    total=$(( $(prom_value "$body" 'flodni_torrent_idle_closes_total{confirmed="false"}') + ${ict/absent/0} ))
    if [ "$total" -gt "$total0" ]; then
      echo "FAIL ($v): a confirmed torrent idle-closed under the keep-alive: idle_closes{confirmed=\"true\"}=$ict, total $total (was $total0)" >&2
      dump_diagnostics
      return 1
    fi
    sleep 1
  done
  if [ "$ict" != 0 ]; then
    echo "FAIL ($v): idle_closes{confirmed=\"true\"} is $ict, must stay 0 under the keep-alive" >&2
    dump_diagnostics
    return 1
  fi
  local held
  held=$(prom_value "$body" 'flodni_keepalive_held_torrents')
  if [ "$held" != 1 ]; then
    echo "FAIL ($v): flodni_keepalive_held_torrents is $held after two idle windows, expected 1" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): held through two idle windows: idle_closes{confirmed=\"true\"}=$ict, held_torrents=$held, $((SECONDS - t0))s since start" >&2

  # The stop webhook releases the hold at once; the torrent then idle-closes within the
  # stop's 30 s grace (plus the ticker).
  resp=$(post_webhook "${payload/media.play/media.stop}")
  if ! head -n 1 <<<"$resp" | grep -q ' 200 '; then
    echo "FAIL ($v): stop webhook post: $(head -n 1 <<<"$resp")" >&2
    dump_diagnostics
    return 1
  fi
  deadline=$((SECONDS + 60))
  local rel
  while [ "$SECONDS" -lt "$deadline" ]; do
    body=$(prom_body)
    total=$(( $(prom_value "$body" 'flodni_torrent_idle_closes_total{confirmed="false"}') + $(prom_value "$body" 'flodni_torrent_idle_closes_total{confirmed="true"}') ))
    [ "$total" -gt "$total0" ] && break
    sleep 1
  done
  rel=$(prom_value "$body" 'flodni_keepalive_releases_total{reason="stop"}')
  if [ "$total" -le "$total0" ] || [ "$rel" != 1 ]; then
    echo "FAIL ($v): after media.stop: idle closes $total (was $total0), releases{reason=\"stop\"}=$rel, expected one close and 1 release" >&2
    dump_diagnostics
    return 1
  fi
  local s c missing=""
  for s in flodni_torrent_idle_closes_total flodni_torrent_reopens_total flodni_read_stalls_total; do
    for c in true false; do
      [ "$(prom_value "$body" "$s{confirmed=\"$c\"}")" = absent ] && missing="$missing $s{$c}"
    done
  done
  for s in 'flodni_keepalive_releases_total{reason="stop"}' 'flodni_keepalive_releases_total{reason="ceiling"}' flodni_keepalive_evicted_total flodni_keepalive_held_torrents; do
    [ "$(prom_value "$body" "$s")" = absent ] && missing="$missing $s"
  done
  if [ -n "$missing" ]; then
    echo "FAIL ($v): series missing:$missing" >&2
    dump_diagnostics
    return 1
  fi
  echo "MEASURE ($v): $(grep -E '^flodni_(torrent_idle_closes_total|torrent_reopens_total|read_stalls_total|keepalive_[a-z_]+)[{ ]' <<<"$body" | tr '\n' ' ')" >&2
  echo "ok ($v): media.stop released the hold, releases{reason=\"stop\"}=$rel, the torrent idle-closed, $((SECONDS - t0))s since start" >&2
  docker logs "$flodni" 2>&1 | grep -E 'Playback confirmed by webhook|High Priority|Re-activated|Torrent close by timeout' | sed 's/^/  log: /' >&2 || true
  check_no_warning "$v" matched || return 1
  echo "PASS ($v): the unconfirmed idle close counted, the confirmed torrent held through two idle windows, released by stop, $((SECONDS - t0))s" >&2
  stop_stack
}
# prom_num body series: the integer value of one exposition series, 0 if absent.
prom_num() {
  awk -v k="$2" 'index($0, k " ") == 1 { printf "%d", $2; f = 1 } END { if (!f) printf "0" }' <<<"$1"
}

# net_rx: bytes received on Flodni's non-loopback interfaces. The only peer on the
# internal network is the seeder, so growth is the swarm bytes pulled (piece data
# plus a little protocol overhead).
net_rx() {
  docker exec "$flodni" cat /proc/net/dev | sed 's/:/ /' | awk 'NR > 2 && $1 != "lo" { n += $2 } END { printf "%d", n }'
}

# sessions_body: Flodni's /api/sessions, headers included (auth off in this variant).
sessions_body() {
  docker exec "$flodni" bash -c 'exec 3<>/dev/tcp/127.0.0.1/9080 && printf "GET /api/sessions HTTP/1.0\r\nHost: x\r\n\r\n" >&3 && cat <&3' 2>/dev/null || true
}

rb_lines() { docker logs "$flodni" 2>&1 | grep -F '[ReaderBudget] ' || true; }

# scan_dd path [skip_bytes]: a dd of the file (from skip_bytes, default the start) in 1 MiB
# blocks inside the container, which a refusal stops with EIO. Sets sd_rc, sd_bytes (what
# dd got, exact), sd_secs and sd_out.
scan_dd() {
  local t=$SECONDS
  sd_out=$(docker exec "$flodni" timeout -k 10 90 sh -c 'dd if="$1" of=/dev/null bs=1048576 iflag=skip_bytes skip="$2" 2>&1; echo "rc=$?"' _ "$1" "${2:-0}" || true)
  sd_secs=$((SECONDS - t))
  sd_rc=$(sed -n 's/^rc=//p' <<<"$sd_out" | tail -n 1)
  sd_bytes=$(sed -n 's/^\([0-9][0-9]*\) bytes.*/\1/p' <<<"$sd_out" | tail -n 1)
}

# window_read path skip_pages pages seconds: a page-aligned dd of `pages` 4 KiB pages
# starting at page skip_pages; prints the bytes read, fails after `seconds`.
window_read() {
  docker exec "$flodni" timeout -k 5 "$4" sh -c 'dd if="$1" of=/dev/null bs=4096 skip="$2" count="$3" 2>&1 | sed -n "s/^\([0-9]*\) bytes.*/\1/p"' _ "$1" "$2" "$3"
}

# timed_read path skip_bytes iflags: dd of 16 pages at skip_bytes (iflags like ",direct"),
# timed in the container. Prints "bytes ms"; "-" for bytes when dd printed none.
timed_read() {
  docker exec "$flodni" bash -c 's=$(date +%s%N); o=$(timeout -k 5 20 dd if="$1" of=/dev/null bs=4096 count=16 iflag=skip_bytes"$3" skip="$2" 2>&1); n=$(sed -n "s/^\([0-9][0-9]*\) bytes.*/\1/p" <<<"$o"); echo "${n:--} $(( ($(date +%s%N) - s) / 1000000 ))"' _ "$1" "$2" "$3"
}

# budget_not_failure v hash fail0: the refusal is not a failed read: no session counts
# one, the short-read "failed" result has not moved, the fixture has no failure record
# in the readability endpoint.
budget_not_failure() {
  local v=$1 hash=$2 fail0=$3 b bad n health
  b=$(sessions_body)
  bad=$(grep -o '"failed_reads":[0-9]*' <<<"$b" | grep -v ':0$' || true)
  if ! grep -q '"failed_reads":0' <<<"$b" || [ -n "$bad" ]; then
    echo "FAIL ($v): /api/sessions has a failed_reads other than 0 (or no session): ${bad:-none seen}: $b" >&2
    dump_diagnostics
    return 1
  fi
  n=$(prom_num "$(prom_body)" 'flodni_fuse_short_read_results_total{result="failed"}')
  if [ "$n" != "$fail0" ]; then
    echo "FAIL ($v): flodni_fuse_short_read_results_total{result=\"failed\"} moved from $fail0 to $n" >&2
    dump_diagnostics
    return 1
  fi
  health=$(health_body)
  if grep -Eq "\"hash\":\"$hash\"[^}]*\"consecutive_failures\":[1-9]" <<<"$health" || grep -q '"failed_attempts":[1-9]' <<<"$health"; then
    echo "FAIL ($v): a failure in /api/torrents/health for the fixture: $health" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): not a failure: every failed_reads 0, short-read failed $n (was $fail0), health clean" >&2
}

# run_budget_refusal: the opt-in refusal (refuse_unconfirmed_reads, margin 16 MiB,
# windows 4 MiB head and 4 MiB tail) against the real mount and engine. See the header.
run_budget_refusal() {
  local dir t0=$SECONDS v=budget-refusal
  start_stack "$v"
  local out="$dir/out"
  wait_for_file "$out/expected.sha256" "$WAIT_SECONDS" "$v"
  wait_for_file "$out/fuse_path" "$WAIT_SECONDS" "$v"
  local expected hash path name size
  expected=$(cat "$out/expected.sha256") hash=$(cat "$out/hash")
  path="$MOUNT_PATH/$(cat "$out/fuse_path")"
  name=$(basename "$path")
  size=$(docker exec "$flodni" stat -c %s "$path")
  local mib=1048576 head=$((4 * 1048576)) margin=$((16 * 1048576))
  local want_min=$((head + margin)) want_max=$((100 * 1048576))
  echo "step add: $((SECONDS - t0))s since start, file $size bytes, a scan is refused after at least $want_min" >&2

  local play_payload stop_payload payload ev resp body actual n
  play_payload='{"event":"media.play","Metadata":{"title":"E2E Fixture","year":2026,"librarySectionType":"movie","Media":[{"Part":[{"file":"/data/movies/'"$name"'"}]}]}}'
  stop_payload=${play_payload/media.play/media.stop}

  # 1. Not armed: no matched webhook yet, so a read far past the margin succeeds. It is 40
  # MiB from the middle of the file, not the whole file: a full read would push the head
  # out of the engine's 128 MiB cache, and re-fetching evicted pieces stalls the engine
  # (the stall the other variants report), which would turn the scan below into a timeout
  # instead of a refusal. The whole file is read, and hashed, in step 6.
  body=$(prom_body)
  if [ "$(prom_num "$body" 'flodni_playback_webhooks_received_total{matched="true"}')" != 0 ]; then
    echo "FAIL ($v): a matched webhook was counted before the first read" >&2
    return 1
  fi
  local s1=$SECONDS
  actual=$(docker exec "$flodni" timeout -k 10 "$READ_TIMEOUT" sh -c 'dd if="$1" of=/dev/null bs=1048576 skip=100 count=40 iflag=fullblock 2>&1' _ "$path" | sed -n 's/^\([0-9][0-9]*\) bytes.*/\1/p') || actual=""
  if [ "$actual" != $((40 * mib)) ]; then
    echo "FAIL ($v): the 40 MiB read before any webhook returned '${actual:-nothing}' bytes (not armed, it must succeed)" >&2
    dump_diagnostics
    return 1
  fi
  body=$(prom_body)
  if [ "$(prom_num "$body" flodni_unconfirmed_read_refused_reads_total)" != 0 ] || grep -qF '"event":"refused"' <<<"$(rb_lines)"; then
    echo "FAIL ($v): a read was refused before any webhook matched" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): 1 not armed: switch on, no matched webhook, 40 MiB past the margin read in $((SECONDS - s1)) s, refused_reads 0, $(grep -F '"event":"over_margin"' <<<"$(rb_lines)" | grep -o '"enforced":[a-z]*')" >&2

  # Arm: a matched play, then a stop. The stop makes the file unconfirmed again; the
  # play started the session's tally afresh, so the next reader gets a fresh margin.
  for ev in play stop; do
    payload=$play_payload
    [ "$ev" = stop ] && payload=$stop_payload
    resp=$(post_webhook "$payload")
    if ! head -n 1 <<<"$resp" | grep -q ' 200 '; then
      echo "FAIL ($v): $ev webhook post: $(head -n 1 <<<"$resp")" >&2
      dump_diagnostics
      return 1
    fi
  done
  for _ in $(seq 1 20); do
    docker logs "$flodni" 2>&1 | grep -F '[PLEX] STOP applied' >/dev/null && break
    sleep 0.5
  done
  if ! docker logs "$flodni" 2>&1 | grep -F '[PLEX] STOP applied' >/dev/null; then
    echo "FAIL ($v): the stop webhook was not applied" >&2
    dump_diagnostics
    return 1
  fi
  body=$(prom_body)
  echo "ok ($v): armed: $(grep -E '^flodni_playback_webhooks_received_total\{' <<<"$body" | tr '\n' ' ')play then stop applied" >&2
  sleep 3 # the stop pulls the rest of the tail window; let it finish before measuring
  local fail0
  fail0=$(prom_num "$(prom_body)" 'flodni_fuse_short_read_results_total{result="failed"}')

  # 2 and 3. A scan: dd of the whole file.
  local rx0 rx1 rx2
  scan_dd "$path"
  rx0=$(net_rx)
  echo "SCAN ($v): dd rc=$sd_rc bytes=$sd_bytes ($((sd_bytes / mib)) MiB of $((size / mib))) in ${sd_secs}s: $(grep -i 'error' <<<"$sd_out" | head -n 1)" >&2
  if [ "$sd_rc" = 0 ] || [ -z "$sd_rc" ] || ! grep -q 'Input/output error' <<<"$sd_out"; then
    echo "FAIL ($v): the scan must fail with EIO, got rc=${sd_rc:-none}: $sd_out" >&2
    dump_diagnostics
    return 1
  fi
  if [ -z "$sd_bytes" ] || [ "$sd_bytes" -lt "$want_min" ] || [ "$sd_bytes" -gt "$want_max" ]; then
    echo "FAIL ($v): the scan read ${sd_bytes:-no} bytes, want between $want_min (windows plus margin) and $want_max" >&2
    dump_diagnostics
    return 1
  fi
  local lines refused held off
  for _ in $(seq 1 10); do
    lines=$(rb_lines)
    grep -qF '"event":"refused"' <<<"$lines" && break
    sleep 0.5
  done
  refused=$(grep -F '"event":"refused"' <<<"$lines" || true)
  if [ "$(grep -c . <<<"$refused")" != 1 ] || ! grep -q '"enforced":true' <<<"$refused" || grep -qF "$name" <<<"$lines"; then
    echo "FAIL ($v): want one refused line with enforced true and no file name, got: ${lines:-none}" >&2
    dump_diagnostics
    return 1
  fi
  held=$(json_num "$refused" held_ms) off=$(json_num "$refused" offset)
  if [ -z "$held" ] || [ "$held" -lt 10000 ] || [ "$held" -gt 17000 ]; then
    echo "FAIL ($v): held_ms ${held:-absent}, want near the 15 s hold: $refused" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): 2 scan refused with EIO after $sd_bytes bytes, one refused line held_ms=$held offset=$off: $refused" >&2
  echo "ITEM3 ($v): dd got $sd_bytes bytes; windows plus margin $want_min; first refused read at offset $off; dd got $((sd_bytes - off)) bytes past it (negative: it stopped short)" >&2
  echo "ok ($v): 3 cached bytes up to the cut: dd got $sd_bytes >= $want_min (windows plus margin)" >&2
  body=$(prom_body)
  echo "PROM ($v): $(grep -E '^flodni_unconfirmed_read_(refused_reads_total|holds_total|confirmed_after_refusal_total)[{ ]' <<<"$body" | tr '\n' ' ')" >&2
  if [ "$(prom_num "$body" flodni_unconfirmed_read_refused_reads_total)" -lt 1 ] ||
    [ "$(prom_num "$body" 'flodni_unconfirmed_read_holds_total{result="refused"}')" != 1 ]; then
    echo "FAIL ($v): want refused_reads_total >= 1 and holds_total{result=\"refused\"} 1" >&2
    dump_diagnostics
    return 1
  fi
  # 4. Not a failure (the dead-swarm verdict is a close-time check, at the end).
  budget_not_failure "$v" "$hash" "$fail0" || return 1

  # 7. The seeder's upload stops growing: the engine's in-flight work settles in a few
  # seconds, then nothing is fetched while the file stays refused.
  sleep 4
  rx1=$(net_rx)
  sleep 16
  rx2=$(net_rx)
  echo "UPLOAD ($v): swarm bytes received after the scan: $((rx1 - rx0)) in the 4 s after it, then $((rx2 - rx1)) in the next 16 s (file $size)" >&2
  if [ $((rx2 - rx1)) -gt $((1 * mib)) ]; then
    echo "FAIL ($v): $((rx2 - rx1)) bytes pulled from the seeder in 16 s while the file was refused, want under 1 MiB" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): 7 seeder upload stopped: $((rx2 - rx1)) bytes in 16 s, bound 1 MiB" >&2

  # 3b. A request that straddles the cut: a new reader starts 512 KiB before the cut, so its
  # first 1 MiB request holds 512 KiB the read-ahead cache may still have and 512 KiB it
  # does not. The gate answers such a request with EIO as a whole; the kernel must then retry
  # page by page and hand dd the cached half. Measured only (a new open, held 15 s): what the
  # read-ahead cache holds at that offset varies from run to run, so the cache hits say
  # whether the straddle happened. A request that does not straddle gets 0 bytes.
  local rc0 rc1
  rc0=$(read_source "$(prom_body)" racache_hit)
  scan_dd "$path" $((want_min - 512 * 1024))
  rc1=$(read_source "$(prom_body)" racache_hit)
  echo "ITEM3b ($v): dd from 512 KiB before the cut got $sd_bytes bytes (rc=$sd_rc, ${sd_secs}s), read-ahead cache hits grew by $((rc1 - rc0)) bytes; 524288 would be the cached half served" >&2

  # 5. The windows still work: page-aligned reads strictly inside the head and the
  # tail, each a new open (allow 15 s).
  local got tailpage
  got=$(window_read "$path" 16 256 20) || got=""
  if [ "$got" != "$mib" ]; then
    echo "FAIL ($v): head window read (1 MiB at 64 KiB) after the refusal returned '${got:-nothing}' bytes" >&2
    dump_diagnostics
    return 1
  fi
  tailpage=$(((size - head) / 4096 + 17))
  got=$(window_read "$path" "$tailpage" 256 20) || got=""
  if [ "$got" != "$mib" ]; then
    echo "FAIL ($v): tail window read (1 MiB inside the tail) after the refusal returned '${got:-nothing}' bytes" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): 5 windows after the refusal: 1 MiB inside the head and 1 MiB inside the tail read" >&2

  # 9. The mount while a reader is held (a new open, a new 15 s hold). Nothing may wait for
  # the hold: stat, the reads of a reader that opened the file before the hold, and fresh
  # opens reading a head and a tail window, buffered and O_DIRECT. A fresh open used to
  # wait for the hold: without FOPEN_KEEP_CACHE the kernel invalidated the file's pages on
  # open and blocked on the ones the held reader's requests had locked.
  docker exec "$flodni" rm -f /tmp/fd9.go /tmp/fd9.ready /tmp/fd9.out /tmp/scan2.pid
  docker exec "$flodni" bash -c 'exec 9<"$1"; touch /tmp/fd9.ready
    while [ ! -e /tmp/fd9.go ]; do sleep 0.05; done
    t() { s=$(date +%s%N); n=$(dd bs=65536 skip="$2" count=1 <&9 2>&1 | sed -n "s/^\([0-9][0-9]*\) bytes.*/\1/p"); echo "$1 ${n:--} $(( ($(date +%s%N) - s) / 1000000 ))"; }
    { t pre-opened-head 1; t pre-opened-tail "$(( ($2 - 2097152) / 65536 ))"; } >/tmp/fd9.out' _ "$path" "$size" &
  local fd9=$!
  for _ in $(seq 1 50); do
    docker exec "$flodni" test -e /tmp/fd9.ready && break
    sleep 0.2
  done
  docker exec "$flodni" sh -c 'echo $$ >/tmp/scan2.pid; exec dd if="$1" of=/dev/null bs=1048576 2>/tmp/scan2.err' _ "$path" &
  local scan2=$! pid2="" t st pl got lat
  sleep 5
  pid2=$(docker exec "$flodni" cat /tmp/scan2.pid 2>/dev/null || true)
  if [ -z "$pid2" ] || ! docker exec "$flodni" sh -c 'kill -0 "$1"' _ "$pid2" 2>/dev/null; then
    echo "FAIL ($v): the second scanner is not held 5 s after its start (pid '${pid2:-none}'): $(docker exec "$flodni" cat /tmp/scan2.err 2>&1)" >&2
    dump_diagnostics
    return 1
  fi
  st=$(docker exec "$flodni" sh -c 's=$(date +%s%N); timeout -k 2 5 stat -c %s "$1" >/dev/null && echo $(( ($(date +%s%N) - s) / 1000 / 1000 ))' _ "$path") || st=""
  docker exec "$flodni" touch /tmp/fd9.go
  local report="stat ${st:-none} ms"
  for pl in "fresh-head:65536:" "fresh-head-direct:65536:,direct" \
    "fresh-tail:$((size - 2 * mib)):" "fresh-tail-direct:$((size - 2 * mib)):,direct"; do
    t=$(timed_read "$path" "$(cut -d: -f2 <<<"$pl")" "$(cut -d: -f3 <<<"$pl")") || t="- 99999"
    report="$report; ${pl%%:*} ${t% *} bytes ${t#* } ms"
    got=${t% *} lat=${t#* }
    if [ "$got" != 65536 ] || [ "$lat" -ge 1000 ]; then
      echo "FAIL ($v): ${pl%%:*} during a hold: $got bytes in $lat ms, want 65536 under 1000 ms" >&2
      dump_diagnostics
      return 1
    fi
  done
  wait "$fd9" 2>/dev/null || true
  while read -r pl got lat; do
    report="$report; $pl $got bytes $lat ms"
    if [ "$got" != 65536 ] || [ "$lat" -ge 1000 ]; then
      echo "FAIL ($v): $pl during a hold: $got bytes in $lat ms, want 65536 under 1000 ms" >&2
      dump_diagnostics
      return 1
    fi
  done < <(docker exec "$flodni" cat /tmp/fd9.out)
  if [ -z "$st" ] || [ "$st" -ge 1000 ]; then
    echo "FAIL ($v): stat took '${st:-no answer}' ms during a hold, want under 1000" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): 9 mount responsive during a hold, every read under 1 s: $report" >&2
  wait "$scan2" 2>/dev/null || true

  # 10. A reader killed mid-hold is not a failed read.
  docker exec "$flodni" rm -f /tmp/scan3.pid
  docker exec "$flodni" sh -c 'echo $$ >/tmp/scan3.pid; exec dd if="$1" of=/dev/null bs=1048576 2>/tmp/scan3.err' _ "$path" &
  local scan3=$! pid3=""
  sleep 5
  pid3=$(docker exec "$flodni" cat /tmp/scan3.pid 2>/dev/null || true)
  if [ -z "$pid3" ] || ! docker exec "$flodni" sh -c 'kill -0 "$1"' _ "$pid3" 2>/dev/null; then
    echo "FAIL ($v): the third scanner is not held 5 s after its start (pid '${pid3:-none}'): $(docker exec "$flodni" cat /tmp/scan3.err 2>&1)" >&2
    dump_diagnostics
    return 1
  fi
  docker exec "$flodni" sh -c 'kill -9 "$1"' _ "$pid3" || true
  wait "$scan3" 2>/dev/null || true
  sleep 2
  budget_not_failure "$v" "$hash" "$fail0" || return 1
  echo "ok ($v): 10 reader killed mid-hold: no failed read" >&2

  # 11. A fresh margin after a stop. Control: with the tally past the margin a reader that
  # starts in the middle of the file (64 MiB, outside the windows and not yet in the
  # read-ahead cache) is refused at its first read. Then a play and a stop: the play starts
  # the tally afresh, the stop makes the file unconfirmed again, and a new reader gets the
  # margin before it is refused. This comes before step 6, whose full read leaves the whole
  # file in the read-ahead cache, where nothing is left to refuse.
  scan_dd "$path" $((64 * mib))
  echo "SCAN ($v): control, dd from 64 MiB with the tally past the margin: rc=$sd_rc bytes=${sd_bytes:-0} in ${sd_secs}s" >&2
  if [ "$sd_rc" = 0 ] || [ "${sd_bytes:-0}" -gt $((1 * mib)) ]; then
    echo "FAIL ($v): a new reader at 64 MiB past the margin must be refused at once, got rc=${sd_rc:-none} bytes=${sd_bytes:-none}: $sd_out" >&2
    dump_diagnostics
    return 1
  fi
  for ev in play stop; do
    payload=$play_payload
    [ "$ev" = stop ] && payload=$stop_payload
    resp=$(post_webhook "$payload")
    if ! head -n 1 <<<"$resp" | grep -q ' 200 '; then
      echo "FAIL ($v): $ev webhook post: $(head -n 1 <<<"$resp")" >&2
      dump_diagnostics
      return 1
    fi
  done
  sleep 2
  scan_dd "$path" $((64 * mib))
  echo "SCAN ($v): after a play and a stop, dd from 64 MiB: rc=$sd_rc bytes=$sd_bytes ($((sd_bytes / mib)) MiB) in ${sd_secs}s" >&2
  if [ "$sd_rc" = 0 ] || [ -z "$sd_bytes" ] || [ "$sd_bytes" -lt "$margin" ] || [ "$sd_bytes" -gt "$want_max" ]; then
    echo "FAIL ($v): after play and stop a new reader must get the margin ($margin) and then be refused: rc=${sd_rc:-none} bytes=${sd_bytes:-none}: $sd_out" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): 11 fresh margin after stop: refused at once before the play, $sd_bytes bytes (>= $margin) after it, then refused again" >&2
  budget_not_failure "$v" "$hash" "$fail0" || return 1

  # 6. The hold is lifted by a late play. The reader is held 5 s after its start (nothing
  # signals the start of a hold), then a matched play arrives.
  local lifted0
  lifted0=$(prom_num "$(prom_body)" 'flodni_unconfirmed_read_holds_total{result="lifted"}')
  docker exec "$flodni" timeout -k 10 "$READ_TIMEOUT" sha256sum "$path" >"$out/hold.sha" 2>"$out/hold.err" &
  local reader=$!
  sleep 5
  if ! kill -0 "$reader" 2>/dev/null; then
    echo "FAIL ($v): the hashing reader ended before the play (it should be held): $(cat "$out/hold.err")" >&2
    dump_diagnostics
    return 1
  fi
  resp=$(post_webhook "$play_payload")
  if ! head -n 1 <<<"$resp" | grep -q ' 200 '; then
    echo "FAIL ($v): late play webhook post: $(head -n 1 <<<"$resp")" >&2
    kill "$reader" 2>/dev/null || true
    dump_diagnostics
    return 1
  fi
  if ! wait "$reader"; then
    echo "FAIL ($v): the held read failed after the play: $(cat "$out/hold.err")" >&2
    dump_diagnostics
    return 1
  fi
  actual=$(awk '{ print $1 }' "$out/hold.sha")
  if [ "$actual" != "$expected" ]; then
    echo "FAIL ($v): sha256 mismatch after the hold was lifted: got ${actual:-none}, want $expected" >&2
    dump_diagnostics
    return 1
  fi
  body=$(prom_body)
  n=$(prom_num "$body" 'flodni_unconfirmed_read_holds_total{result="lifted"}')
  if [ "$n" -lt $((lifted0 + 1)) ] || [ "$(prom_num "$body" flodni_unconfirmed_read_confirmed_after_refusal_total)" != 1 ]; then
    echo "FAIL ($v): want holds_total{result=\"lifted\"} up from $lifted0 and confirmed_after_refusal_total 1: $(grep -E '^flodni_unconfirmed_read_(holds|confirmed_after_refusal)_total' <<<"$body" | tr '\n' ' ')" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): 6 hold lifted: the late play let the held read through, sha256 $actual, $(grep -E '^flodni_unconfirmed_read_(holds|confirmed_after_refusal)_total' <<<"$body" | tr '\n' ' ')" >&2
  budget_not_failure "$v" "$hash" "$fail0" || return 1

  # The one wait for the session close: the dead-swarm verdict is logged at close, and a
  # refusal must not produce one.
  local c0=$SECONDS closed="" deadline=$((SECONDS + 60))
  while [ "$SECONDS" -lt "$deadline" ]; do
    closed=$(sessions_body | grep -o '"closed":.*' || true)
    grep -q "\"hash\":\"$hash\"" <<<"$closed" && break
    sleep 3
  done
  if ! grep -q "\"hash\":\"$hash\"" <<<"$closed"; then
    echo "FAIL ($v): the session did not close within 60 s" >&2
    dump_diagnostics
    return 1
  fi
  if docker logs "$flodni" 2>&1 | grep -F '[DeadSwarm]' >/dev/null || grep -q '"outcome":"dead_swarm"' <<<"$closed"; then
    echo "FAIL ($v): a dead-swarm verdict after refusals: $(docker logs "$flodni" 2>&1 | grep -F '[DeadSwarm]')" >&2
    dump_diagnostics
    return 1
  fi
  budget_not_failure "$v" "$hash" "$fail0" || return 1
  echo "ok ($v): 4 at close ($((SECONDS - c0)) s waited): no [DeadSwarm] line, no dead_swarm outcome, failed_reads 0: $(grep -o '"outcome":"[a-z_]*"' <<<"$closed" | tr '\n' ' ')" >&2
  echo "ok ($v): $(grep -E '^flodni_unconfirmed_read_sessions_total\{' <<<"$(prom_body)" | tr '\n' ' ')" >&2
  echo "ReaderBudget ($v): $(rb_lines | tr '\n' ' ')" >&2
  echo "PASS ($v): not armed passes, a scan is refused with EIO after the hold, windows and a late play get through, no failed read, $((SECONDS - t0))s" >&2

  docker stop -t 10 "$seeder" >/dev/null 2>&1 || true
  docker logs "$seeder" 2>&1 | grep 'upload stats' | sed "s/^/($v) /" >&2 || true
  stop_stack
}

# gostorm_get_status hash: the HTTP status of GoStorm's `get` for one hash (404 when the
# engine neither runs nor stores it), over 8090 (auth is off in the removal variant).
gostorm_get_status() {
  docker exec "$flodni" bash -c 'b="{\"action\":\"get\",\"hash\":\"$1\"}"
    exec 3<>/dev/tcp/127.0.0.1/8090 && printf "POST /torrents HTTP/1.0\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: ${#b}\r\n\r\n%s" "$b" >&3 && head -n 1 <&3' _ "$1" 2>/dev/null | awk '{ printf "%s", $2 }' || true
}

# cache_files hash: how many files of the index cache belong to the hash.
cache_files() {
  docker exec "$flodni" ls /cache/index 2>/dev/null | grep -c "^$1-" || true
}

# expect_get v status label hash...: every hash answers `get` with the status.
expect_get() {
  local v=$1 want=$2 label=$3 h got
  shift 3
  for h in "$@"; do
    got=$(gostorm_get_status "$h")
    if [ "$got" != "$want" ]; then
      echo "FAIL ($v): $label: get $h answers '${got:-nothing}', want $want" >&2
      dump_diagnostics
      return 1
    fi
  done
  echo "ok ($v): $label: get answers $want for $#" >&2
}

# run_removal: roadmap "Before a deployment relies on it", step 1, end to end. Four movies
# are filed. A is read in the background; a descriptor is open on B; B and C are removed
# through POST /api/library/remove back to back and D with rm through the mount. Asserted:
# A's sha256, `get` answers 404 for B, C and D at once and again after the engine's idle
# timeout (15 s) plus a margin, a read on B's descriptor at an uncached offset fails with
# EIO, the wake is counted as refused and is not a failure (failed_reads 0, no dead-swarm
# verdict), and no index-cache file of B, C or D remains.
run_removal() {
  local dir t0=$SECONDS v=removal
  start_stack "$v" -mode removal
  local out="$dir/out"
  wait_for_file "$out/d.path" "$WAIT_SECONDS" "$v"
  echo "step add four titles: $((SECONDS - t0))s since start" >&2
  local l path_a path_b path_c path_d hash_a hash_b hash_c hash_d
  for l in a b c d; do
    printf -v "path_$l" '%s' "$MOUNT_PATH/$(cat "$out/$l.path")"
    printf -v "hash_$l" '%s' "$(cat "$out/$l.hash")"
  done
  check_no_warning "$v" || return 1
  expect_get "$v" 200 "before the removals" "$hash_a" "$hash_b" "$hash_c" "$hash_d" || return 1

  # One descriptor on B: 1 MiB at the start, then it waits for the go file and reads
  # 1 MiB at 40 MiB, an offset nothing cached.
  docker exec "$flodni" rm -f /tmp/rm.go
  docker exec "$flodni" timeout -k 10 "$READ_TIMEOUT" sh -c \
    '( dd bs=1048576 count=1 iflag=fullblock of=/dev/null status=none; \
       until [ -e /tmp/rm.go ]; do sleep 0.1; done; \
       dd bs=1048576 skip=40 count=1 iflag=fullblock of=/dev/null status=none ) < "$1"' _ "$path_b" >"$out/b.out" 2>"$out/b.err" &
  local reader=$!
  # C and D are read at the start too, so each has index-cache files to lose.
  local p
  for p in "$path_c" "$path_d"; do
    if ! docker exec "$flodni" timeout -k 10 "$READ_TIMEOUT" dd if="$p" of=/dev/null bs=1048576 count=1 status=none; then
      echo "FAIL ($v): head read of $p failed" >&2
      dump_diagnostics
      return 1
    fi
  done
  local nb=0 nc=0 nd=0 deadline=$((SECONDS + 30))
  while [ "$SECONDS" -lt "$deadline" ]; do
    nb=$(cache_files "$hash_b") nc=$(cache_files "$hash_c") nd=$(cache_files "$hash_d")
    [ "$nb" -ge 1 ] && [ "$nc" -ge 1 ] && [ "$nd" -ge 1 ] && break
    sleep 1
  done
  if [ "$nb" -lt 1 ] || [ "$nc" -lt 1 ] || [ "$nd" -lt 1 ]; then
    echo "FAIL ($v): premise: index-cache files before the removals: B $nb, C $nc, D $nd, want each at least 1" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): index-cache files before the removals: B $nb, C $nc, D $nd" >&2
  local fail0 refused0
  fail0=$(prom_num "$(prom_body)" 'flodni_fuse_short_read_results_total{result="failed"}')
  refused0=$(prom_num "$(prom_body)" 'flodni_wake_refused_total')

  # A is read to the end in the background while the removals run: 8 MiB at a time with a
  # pause (about 8 s for the 192 MiB), then the rest, so the read outlasts the removals.
  docker exec "$flodni" timeout -k 10 "$READ_TIMEOUT" sh -c \
    '{ i=0; while [ "$i" -lt 192 ]; do dd if="$1" bs=1048576 skip="$i" count=8 status=none || exit 1; i=$((i + 8)); sleep 0.3; done
       dd if="$1" bs=1048576 skip=192 status=none; } | sha256sum' _ "$path_a" >"$out/a.read" 2>"$out/a.err" &
  local reader_a=$!
  sleep 1
  local rm_t0=$SECONDS resp
  resp=$(docker exec "$flodni" bash -c 'post() { local b="{\"hash\":\"$1\"}"
      exec 3<>/dev/tcp/127.0.0.1/9080
      printf "POST /api/library/remove HTTP/1.0\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: ${#b}\r\n\r\n%s" "$b" >&3
      head -n 1 <&3; exec 3<&-; }
    post "$1"; post "$2"' _ "$hash_b" "$hash_c")
  if [ "$(grep -c ' 200 ' <<<"$resp")" != 2 ]; then
    echo "FAIL ($v): library remove of B and C: $resp" >&2
    dump_diagnostics
    return 1
  fi
  if ! docker exec "$flodni" rm "$path_d"; then
    echo "FAIL ($v): rm of D through the mount failed" >&2
    dump_diagnostics
    return 1
  fi
  echo "step removals: B and C through the API, D with rm, $((SECONDS - rm_t0))s" >&2
  if kill -0 "$reader_a" 2>/dev/null; then
    echo "ok ($v): A's read was still running when the removals returned" >&2
  else
    echo "FAIL ($v): A's read had finished before the removals returned: the overlap is not tested" >&2
    dump_diagnostics
    return 1
  fi
  expect_get "$v" 404 "right after the removals" "$hash_b" "$hash_c" "$hash_d" || return 1
  local x
  for x in "$path_b" "$path_c" "$path_d"; do
    if ! stub_gone "$x"; then
      echo "FAIL ($v): the stub $x is still there" >&2
      dump_diagnostics
      return 1
    fi
  done
  echo "ok ($v): the stubs of B, C and D are gone" >&2

  # B's descriptor reads at an uncached offset: the engine no longer stores B.
  docker exec "$flodni" touch /tmp/rm.go
  if wait "$reader"; then
    echo "FAIL ($v): the read on B's descriptor succeeded after the removal" >&2
    dump_diagnostics
    return 1
  fi
  if ! grep -q 'Input/output error' "$out/b.err"; then
    echo "FAIL ($v): the read on B's descriptor did not fail with EIO: $(cat "$out/b.err")" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): the read on B's descriptor at 40 MiB failed: $(head -n 1 "$out/b.err")" >&2

  local a_got
  if ! wait "$reader_a"; then
    echo "FAIL ($v): the background read of A failed: $(cat "$out/a.err")" >&2
    dump_diagnostics
    return 1
  fi
  a_got=$(cut -d' ' -f1 "$out/a.read")
  if [ "$a_got" != "$(cat "$out/a.sha256")" ]; then
    echo "FAIL ($v): A's sha256 is $a_got, want $(cat "$out/a.sha256")" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): A read in the background while the others were removed: sha256 $a_got" >&2

  # The refusal is counted and is not a failure.
  local refused
  refused=$(prom_num "$(prom_body)" 'flodni_wake_refused_total')
  if [ "$refused" -le "$refused0" ]; then
    echo "FAIL ($v): flodni_wake_refused_total did not move: $refused0 -> $refused" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): flodni_wake_refused_total $refused0 -> $refused" >&2
  budget_not_failure "$v" "$hash_b" "$fail0" || return 1
  local rec
  rec=$(grep -o "{\"hash\":\"$hash_b\"[^}]*}" <<<"$(health_body)" || true)
  if ! grep -q '"last_failure_reason":"not_stored"' <<<"$rec"; then
    echo "FAIL ($v): B's readability record does not say not_stored: ${rec:-no record}" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): B's readability record: $rec" >&2

  # Past the engine's idle timeout (15 s) with a margin: nothing wakes or saves a removed
  # torrent, and A, which was not removed, is still stored.
  sleep 30
  expect_get "$v" 404 "after the idle timeout" "$hash_b" "$hash_c" "$hash_d" || return 1
  expect_get "$v" 200 "control: A is not removed" "$hash_a" || return 1
  if docker logs "$flodni" 2>&1 | grep -F '[DeadSwarm]' >/dev/null; then
    echo "FAIL ($v): a dead-swarm verdict: $(docker logs "$flodni" 2>&1 | grep -F '[DeadSwarm]')" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): no [DeadSwarm] line" >&2
  nb=$(cache_files "$hash_b") nc=$(cache_files "$hash_c") nd=$(cache_files "$hash_d")
  if [ "$nb" != 0 ] || [ "$nc" != 0 ] || [ "$nd" != 0 ]; then
    echo "FAIL ($v): index-cache files left: B $nb, C $nc, D $nd: $(docker exec "$flodni" ls /cache/index)" >&2
    dump_diagnostics
    return 1
  fi
  echo "ok ($v): no index-cache file of B, C or D remains" >&2
  echo "PASS ($v): A sha256 intact, B, C and D answer 404 at once and after the idle timeout, B's read fails with EIO, refusals counted, no failure, no cache file left, $((SECONDS - t0))s" >&2

  stop_stack
}

want_variant() { [ -z "${E2E_ONLY:-}" ] || [ "$E2E_ONLY" = "$1" ]; }

if want_variant default; then run_variant default; fi
if want_variant slow-seeder; then run_variant slow-seeder; fi
if want_variant index-cache; then run_variant index-cache; fi
if want_variant warm-on-add; then run_warm_on_add; fi
if want_variant warm-lifecycle; then run_warm_lifecycle; fi
if want_variant catch-up; then run_catchup; fi
if want_variant late-confirm; then run_late_confirm; fi
if want_variant keep-alive; then run_keepalive; fi
if want_variant budget-refusal; then run_budget_refusal; fi
if want_variant replacement; then run_replacement; fi
if want_variant removal; then run_removal; fi
echo "e2e: all variants passed" >&2
