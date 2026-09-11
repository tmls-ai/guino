#!/bin/sh
# Install a published Guino release. No Guino domain or package manager required.
set -eu

fail() { printf '%s\n' "$*" >&2; exit 1; }
command -v curl >/dev/null 2>&1 || fail 'curl is required'
command -v tar >/dev/null 2>&1 || fail 'tar is required'

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) fail 'Guino release binaries support Linux and macOS' ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) fail 'Guino release binaries support amd64 and arm64' ;;
esac

version=${GUINO_VERSION:-}
if [ -z "$version" ]; then
  metadata=$(curl --proto '=https' --tlsv1.2 -fsSL https://api.github.com/repos/tmls-ai/guino/releases/latest) ||
    fail 'No published Guino release is available. Build from source using the README.'
  version=$(printf '%s\n' "$metadata" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)
fi
version=${version#v}
printf '%s\n' "$version" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' ||
  fail 'GUINO_VERSION must be a stable version such as v0.1.0'
case "$version" in 0.0.*) fail 'Guino binaries require version 0.1.0 or newer' ;; esac

archive="guino_${version}_${os}_${arch}.tar.gz"
url="https://github.com/tmls-ai/guino/releases/download/v${version}"
workdir=$(mktemp -d "${TMPDIR:-/tmp}/guino-install.XXXXXX")
staged=''
trap 'rm -rf "$workdir"; if [ -n "$staged" ]; then rm -f "$staged"; fi' EXIT HUP INT TERM
curl --proto '=https' --tlsv1.2 -fsSL "$url/$archive" -o "$workdir/$archive" ||
  fail 'Guino archive unavailable. Check the version or build from source.'
curl --proto '=https' --tlsv1.2 -fsSL "$url/checksums.txt" -o "$workdir/checksums.txt" ||
  fail 'Cannot verify archive: checksums unavailable'
expected=$(awk -v name="$archive" '$2 == name { print $1 }' "$workdir/checksums.txt")
printf '%s\n' "$expected" | grep -Eq '^[0-9a-fA-F]{64}$' || fail 'Archive checksum missing or ambiguous'
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$workdir/$archive" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "$workdir/$archive" | awk '{print $1}')
else
  fail 'sha256sum or shasum is required'
fi
[ "$expected" = "$actual" ] || fail 'Checksum mismatch; existing installation unchanged'
tar -xzf "$workdir/$archive" -C "$workdir" guino
[ -f "$workdir/guino" ] && [ ! -L "$workdir/guino" ] || fail 'Archive must contain a regular guino binary'
install_dir=${GUINO_INSTALL_DIR:-"$HOME/.local/bin"}
mkdir -p "$install_dir"
[ ! -d "$install_dir/guino" ] || fail 'Install destination is a directory; existing contents unchanged'
staged=$(mktemp "$install_dir/.guino.XXXXXX")
cp "$workdir/guino" "$staged"
chmod 755 "$staged"
mv -f "$staged" "$install_dir/guino"
staged=''
printf 'Installed Guino %s to %s/guino\n' "$version" "$install_dir"
printf 'Add %s to PATH if needed. Docker and container images are required for sandbox execution.\n' "$install_dir"
