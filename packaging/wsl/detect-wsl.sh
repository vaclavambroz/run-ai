#!/bin/sh
set -eu

# RUN_AI_OSRELEASE_FILE exists only to make the pure detector testable. The
# packaged invocation uses the kernel-owned default below.
osrelease_file=${RUN_AI_OSRELEASE_FILE:-/proc/sys/kernel/osrelease}
release=
if [ -r "$osrelease_file" ] && [ ! -L "$osrelease_file" ]; then
  IFS= read -r release <"$osrelease_file" || true
fi

case "$release" in
  *[Mm]icrosoft*[Ss]tandard*|*[Mm]icrosoft*WSL2*) printf '%s\n' wsl2 ;;
  *[Mm]icrosoft*) printf '%s\n' wsl1 ;;
  *)
    if [ -n "${WSL_INTEROP:-}" ] || [ -n "${WSL_DISTRO_NAME:-}" ]; then
      printf '%s\n' wsl
    else
      printf '%s\n' not-wsl
    fi
    ;;
esac
