#!/bin/sh
# Package a locally verified binary for a manually authorized cutover.
# This deliberately does not make a release and refuses the live binary path.
set -eu

usage() {
  echo "usage: $0 BINARY NEW_CANDIDATE_DIRECTORY" >&2
  exit 64
}

[ "$#" -eq 2 ] || usage
binary=$1
output=$2

[ -f "$binary" ] && [ -x "$binary" ] && [ ! -L "$binary" ] || {
  echo "candidate binary must be a regular executable non-symlink: $binary" >&2
  exit 66
}
case "$output" in
  /*) ;;
  *) echo "candidate output must be an absolute path" >&2; exit 64 ;;
esac

output_parent=$(CDPATH= cd -- "$(dirname -- "$output")" && pwd -P) || {
  echo "candidate output parent must already exist: $(dirname -- "$output")" >&2
  exit 66
}
output_name=$(basename -- "$output")
case "$output_name" in
  ''|.|..) echo "invalid candidate output name" >&2; exit 64 ;;
esac
output="$output_parent/$output_name"
case "$output" in
  /usr/bin|/usr/bin/*)
    echo "candidate packaging never writes under /usr/bin" >&2
    exit 77
    ;;
esac
[ ! -e "$output" ] && [ ! -L "$output" ] || {
  echo "candidate output already exists: $output" >&2
  exit 73
}

command -v sha256sum >/dev/null 2>&1 || {
  echo "sha256sum is required" >&2
  exit 69
}
command -v git >/dev/null 2>&1 || {
  echo "git is required" >&2
  exit 69
}
command -v python3 >/dev/null 2>&1 || {
  echo "python3 is required" >&2
  exit 69
}
command -v tar >/dev/null 2>&1 || {
  echo "tar is required" >&2
  exit 69
}
command -v go >/dev/null 2>&1 || {
  echo "go is required" >&2
  exit 69
}

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repository=$(CDPATH= cd -- "$script_dir/../.." && pwd)
revision=$(git -C "$repository" rev-parse HEAD 2>/dev/null || printf '%s' unknown)
case "$revision" in
  *[!0-9a-f]*|'') revision=unknown ;;
esac
if [ "$revision" != unknown ] && [ -z "$(git -C "$repository" status --porcelain --untracked-files=all)" ]; then
  source_tree=clean
else
  source_tree=dirty_or_unknown
fi

stage=$(mktemp -d "$output_parent/.run-ai-candidate.XXXXXX")
trap 'rm -rf -- "$stage"' EXIT HUP INT TERM
install -m 0755 "$binary" "$stage/run_ai"
python3 "$script_dir/source-state-manifest.py" --repository "$repository" \
  --output "$stage/source-state.json" --archive "$stage/source-state.tar.gz"
input_digest=$(sha256sum "$binary" | awk '{print $1}')
candidate_digest=$(sha256sum "$stage/run_ai" | awk '{print $1}')
source_state_digest=$(sha256sum "$stage/source-state.json" | awk '{print $1}')
source_archive_digest=$(sha256sum "$stage/source-state.tar.gz" | awk '{print $1}')
[ "$input_digest" = "$candidate_digest" ] || {
  echo "candidate copy digest mismatch" >&2
  exit 74
}
mkdir "$stage/.source-verify"
tar -xzf "$stage/source-state.tar.gz" -C "$stage/.source-verify"
(
  cd "$stage/.source-verify"
  go build -trimpath -buildvcs=false -o "$stage/.rebuilt_run_ai" ./cmd/run_ai
)
rebuilt_digest=$(sha256sum "$stage/.rebuilt_run_ai" | awk '{print $1}')
[ "$candidate_digest" = "$rebuilt_digest" ] || {
  echo "candidate binary does not reproduce from captured source state" >&2
  exit 74
}
rm -rf -- "$stage/.source-verify" "$stage/.rebuilt_run_ai"
printf '%s  %s\n' "$candidate_digest" run_ai >"$stage/SHA256SUMS"
cat >"$stage/candidate.json" <<EOF
{
  "schema": "run-ai-local-candidate/v1",
  "candidate_sha256": "$candidate_digest",
  "source_revision": "$revision",
  "source_tree": "$source_tree",
  "source_state_sha256": "$source_state_digest",
  "source_archive_sha256": "$source_archive_digest",
  "source_rebuild_sha256": "$rebuilt_digest",
  "release_eligible": false,
  "purpose": "locally verified cutover candidate; not a signed release"
}
EOF
chmod 0644 "$stage/SHA256SUMS" "$stage/candidate.json" "$stage/source-state.json" "$stage/source-state.tar.gz"
chmod 0755 "$stage"
mv -T -- "$stage" "$output"
trap - EXIT HUP INT TERM
printf '%s\n' "$output"
