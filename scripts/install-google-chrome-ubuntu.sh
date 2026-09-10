#!/usr/bin/env bash
set -euo pipefail

fail() {
  echo "ERROR: $*" >&2
  exit 1
}

if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
  fail "run this helper with root privileges (for example: sudo ./scripts/install-google-chrome-ubuntu.sh)"
fi

if [[ ! -x /usr/bin/apt-get || ! -r /etc/os-release ]]; then
  fail "this helper only supports Ubuntu/Debian-style APT environments"
fi

. /etc/os-release
os_id="${ID:-}"
os_like="${ID_LIKE:-}"
if [[ "$os_id" != "ubuntu" && "$os_id" != "debian" && "$os_like" != *debian* && "$os_like" != *ubuntu* ]]; then
  fail "unsupported operating system '${PRETTY_NAME:-unknown}'; expected Ubuntu/Debian-style APT"
fi

if [[ -x /usr/bin/google-chrome ]]; then
  echo "google-chrome is already installed at /usr/bin/google-chrome"
  /usr/bin/google-chrome --version || true
  exit 0
fi

if ! command -v dpkg >/dev/null 2>&1; then
  fail "dpkg is required to detect the system architecture"
fi

arch="$(dpkg --print-architecture)"
case "$arch" in
  amd64) ;;
  *)
    fail "unsupported architecture '$arch'; this helper only installs google-chrome-stable for amd64"
    ;;
esac

export DEBIAN_FRONTEND=noninteractive

apt-get update
apt-get install -y --no-install-recommends ca-certificates curl gnupg

keyring='/usr/share/keyrings/google-linux-signing-keyring.gpg'
repo_file='/etc/apt/sources.list.d/google-chrome.list'
key_url='https://dl.google.com/linux/linux_signing_key.pub'
repo_url='https://dl.google.com/linux/chrome/deb/'

install -d -m 0755 /usr/share/keyrings
tmp_key="$(mktemp)"
trap 'rm -f "$tmp_key"' EXIT
curl -fsSL "$key_url" -o "$tmp_key"
gpg --dearmor --yes --output "$keyring" "$tmp_key"
printf 'deb [arch=amd64 signed-by=%s] %s stable main\n' "$keyring" "$repo_url" > "$repo_file"

apt-get update
apt-get install -y --no-install-recommends google-chrome-stable

if [[ ! -x /usr/bin/google-chrome ]]; then
  fail "installation completed but /usr/bin/google-chrome was not found"
fi

echo "Installed google-chrome at /usr/bin/google-chrome"
/usr/bin/google-chrome --version
