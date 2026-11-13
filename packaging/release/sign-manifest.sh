#!/bin/sh
set -eu

[ "$#" -eq 2 ] || {
  echo "usage: $0 SECRET_KEY manifest.json" >&2
  exit 64
}
secret_key=$1
manifest=$2
[ -f "$secret_key" ] && [ ! -L "$secret_key" ] || {
  echo "secret key must be a regular non-symlink file" >&2
  exit 66
}
[ -f "$manifest" ] && [ ! -L "$manifest" ] || {
  echo "manifest must be a regular non-symlink file" >&2
  exit 66
}
command -v minisign >/dev/null 2>&1 || {
  echo "minisign is required" >&2
  exit 69
}

umask 077
minisign -S -s "$secret_key" -m "$manifest" -x "$manifest.minisig"
