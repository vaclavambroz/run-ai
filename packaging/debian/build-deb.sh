#!/bin/sh
set -eu

usage() {
  echo "usage: $0 VERSION ARCH BINARY OUTPUT_DIR" >&2
  exit 64
}

[ "$#" -eq 4 ] || usage
version=$1
input_arch=$2
binary=$3
output_dir=$4
epoch=${SOURCE_DATE_EPOCH:-}
case "$epoch" in
  ''|*[!0-9]*|0) echo "SOURCE_DATE_EPOCH must be a positive integer" >&2; exit 64 ;;
esac

printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' || {
  echo "invalid release version: $version" >&2
  exit 64
}

case "$input_arch" in
  amd64|x86_64) deb_arch=amd64 ;;
  arm64|aarch64) deb_arch=arm64 ;;
  *) echo "unsupported Debian architecture: $input_arch" >&2; exit 64 ;;
esac

[ -f "$binary" ] && [ -x "$binary" ] && [ ! -L "$binary" ] || {
  echo "release binary must be a regular executable non-symlink: $binary" >&2
  exit 66
}
command -v dpkg-deb >/dev/null 2>&1 || {
  echo "dpkg-deb is required" >&2
  exit 69
}

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repository=$(CDPATH= cd -- "$script_dir/../.." && pwd)
stage=$(mktemp -d "${TMPDIR:-/tmp}/run-ai-deb.XXXXXX")
trap 'rm -rf -- "$stage"' EXIT HUP INT TERM

install -d -m 0755 \
  "$stage/DEBIAN" \
  "$stage/usr/bin" \
  "$stage/etc/run-ai" \
  "$stage/usr/lib/run-ai" \
  "$stage/usr/share/run-ai/profiles.d" \
  "$stage/usr/share/run-ai/schemas" \
  "$stage/usr/share/doc/run-ai"
install -m 0755 "$binary" "$stage/usr/bin/run_ai"
install -m 0644 "$script_dir/config.json" "$stage/etc/run-ai/config.json"
install -m 0644 "$script_dir/copyright" "$stage/usr/share/doc/run-ai/copyright"
install -m 0644 "$repository/LICENSE" "$stage/usr/share/doc/run-ai/LICENSE"
install -m 0755 "$script_dir/../wsl/detect-wsl.sh" "$stage/usr/lib/run-ai/detect-wsl"
install -m 0644 "$script_dir/../wsl/wsl.json" "$stage/usr/share/run-ai/profiles.d/wsl.json"
install -m 0644 "$script_dir/../wsl/profile.schema.json" "$stage/usr/share/run-ai/schemas/run-ai-platform-profile-v1.schema.json"
install -m 0644 "$script_dir/conffiles" "$stage/DEBIAN/conffiles"
for document in distribution install upgrade-rollback release persistent-codex persistent-candidate-cutover; do
  install -m 0644 "$repository/docs/$document.md" "$stage/usr/share/doc/run-ai/$document.md"
done
for maintainer_script in preinst postinst prerm postrm; do
  install -m 0755 "$script_dir/$maintainer_script" "$stage/DEBIAN/$maintainer_script"
done
sed -e "s/@VERSION@/$version/g" -e "s/@DEB_ARCH@/$deb_arch/g" \
  "$script_dir/control.in" >"$stage/DEBIAN/control"
chmod 0644 "$stage/DEBIAN/control"

LC_ALL=C date -u -d "@$epoch" -R | {
  IFS= read -r release_date
  printf 'run-ai (%s) stable; urgency=medium\n\n  * Reproducible upstream release.\n\n -- Václav Ambrož <vaclav@ambroz.email>  %s\n' \
    "$version" "$release_date"
} | gzip -n -9 >"$stage/usr/share/doc/run-ai/changelog.Debian.gz"
chmod 0644 "$stage/usr/share/doc/run-ai/changelog.Debian.gz"

find "$stage" -print0 | xargs -0 touch -h -d "@$epoch"

mkdir -p -- "$output_dir"
package="$output_dir/run-ai_${version}_${deb_arch}.deb"
dpkg-deb --root-owner-group --build "$stage" "$package" >/dev/null
printf '%s\n' "$package"
