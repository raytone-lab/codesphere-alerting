#!/bin/sh
# Runtime config overlay. Secrets come from the environment, not from the image.
set -eu

rm -rf /tmp/etc
cp -a /app/etc /tmp/etc
CFG=/tmp/etc/config.toml
mkdir -p /tmp/logs /app/data/tsdb

if [ -z "${DATABASE_DSN:-}" ]; then
  echo "DATABASE_DSN is required" >&2
  exit 1
fi

awk -v dsn="$DATABASE_DSN" -v addr="${REDIS_ADDRESS:-}" -v pass="${REDIS_PASSWORD:-}" '
  /^DSN = / { print "DSN = \"" dsn "\""; next }
  /^Address = / && addr != "" { print "Address = \"" addr "\""; next }
  /^# Password = / && pass != "" { print "Password = \"" pass "\""; next }
  /^RedisType = / {
    print "RedisType = \"standalone\""
    print "UseTLS = true"
    print "InsecureSkipVerify = true"
    next
  }
  /^Dir = "logs"/ { print "Dir = \"/tmp/logs\""; next }
  /^Dir = "data\/tsdb"/ { print "Dir = \"/app/data/tsdb\""; next }
  { print }
' "$CFG" > "$CFG.tmp"
mv "$CFG.tmp" "$CFG"

exec /app/n9e -configs /tmp/etc
