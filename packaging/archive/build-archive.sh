#!/bin/sh
set -eu

[ "$#" -eq 5 ] || {
  echo "usage: $0 VERSION OS ARCH BINARY OUTPUT_DIR" >&2
  exit 64
}
version=$1
operating_system=$2
architecture=$3
binary=$4
output_dir=$5
epoch=${SOURCE_DATE_EPOCH:-}
case "$epoch" in
  ''|*[!0-9]*|0) echo "SOURCE_DATE_EPOCH must be a positive integer" >&2; exit 64 ;;
esac
printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' || {
  echo "invalid release version: $version" >&2
  exit 64
}
case "$operating_system/$architecture" in
  linux/amd64|linux/arm64|darwin/amd64|darwin/arm64) ;;
  *) echo "unsupported archive platform: $operating_system/$architecture" >&2; exit 64 ;;
esac
[ -f "$binary" ] && [ -x "$binary" ] && [ ! -L "$binary" ] || {
  echo "release binary must be a regular executable non-symlink" >&2
  exit 66
}
command -v tar >/dev/null 2>&1 || exit 69
command -v gzip >/dev/null 2>&1 || exit 69

stage=$(mktemp -d "${TMPDIR:-/tmp}/run-ai-archive.XXXXXX")
trap 'rm -rf -- "$stage"' EXIT HUP INT TERM
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repository=$(CDPATH= cd -- "$script_dir/../.." && pwd)
install -m 0755 "$binary" "$stage/run_ai"
install -m 0644 "$repository/LICENSE" "$stage/LICENSE"
touch -h -d "@$epoch" "$stage/run_ai" "$stage/LICENSE"
mkdir -p "$output_dir"
archive="$output_dir/run-ai_${version}_${operating_system}_${architecture}.tar.gz"
tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$epoch" \
  -C "$stage" -cf - LICENSE run_ai | gzip -n -9 >"$archive"
printf '%s\n' "$archive"
