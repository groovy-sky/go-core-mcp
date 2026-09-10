#!/usr/bin/env sh
set -eu

bundle_root="/opt/chromium-libs"
browser_bin="/usr/lib/chromium/chromium"

if [ ! -x "$browser_bin" ]; then
  echo "Chromium bundle missing executable: $browser_bin" >&2
  exit 1
fi

ld_library_path=""
for dir in \
  "${bundle_root}"/lib/*-linux-gnu \
  "${bundle_root}"/usr/lib/*-linux-gnu \
  "${bundle_root}"/usr/lib/*-linux-gnu/* \
  /usr/lib/chromium
do
  if [ -d "$dir" ]; then
    if [ -n "$ld_library_path" ]; then
      ld_library_path="${ld_library_path}:"
    fi
    ld_library_path="${ld_library_path}${dir}"
  fi
done

if [ -n "$ld_library_path" ]; then
  if [ -n "${LD_LIBRARY_PATH:-}" ]; then
    export LD_LIBRARY_PATH="${ld_library_path}:${LD_LIBRARY_PATH}"
  else
    export LD_LIBRARY_PATH="${ld_library_path}"
  fi
fi

exec "$browser_bin" "$@"
