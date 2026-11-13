#!/bin/sh
set -eu

[ "$#" -ge 2 ] && [ "$#" -le 3 ] || {
  echo "usage: $0 PUBLIC_KEY manifest.json [ARTIFACT_ROOT]" >&2
  exit 64
}
public_key=$1
manifest=$2
artifact_root=${3:-}
[ -f "$public_key" ] && [ ! -L "$public_key" ] || {
  echo "public key must be a regular non-symlink file" >&2
  exit 66
}
[ -f "$manifest" ] && [ ! -L "$manifest" ] && [ -f "$manifest.minisig" ] && [ ! -L "$manifest.minisig" ] || {
  echo "manifest and detached signature must be regular non-symlink files" >&2
  exit 66
}
command -v minisign >/dev/null 2>&1 || {
  echo "minisign is required" >&2
  exit 69
}
minisign -Vm "$manifest" -p "$public_key" -x "$manifest.minisig"
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
if [ -n "$artifact_root" ]; then
  exec python3 "$script_dir/validate-release.py" "$manifest" --artifact-root "$artifact_root"
fi
exec python3 "$script_dir/validate-release.py" "$manifest"
