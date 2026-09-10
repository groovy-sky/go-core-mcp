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
is_debian_like=0
for token in $os_like; do
  case "$token" in
    debian|ubuntu)
      is_debian_like=1
      break
      ;;
  esac
done
if [[ "$os_id" != "ubuntu" && "$os_id" != "debian" && "$is_debian_like" != "1" ]]; then
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

if ! apt-get update; then
  fail "apt-get update failed; fix the base APT configuration before running this helper"
fi
apt-get install -y --no-install-recommends ca-certificates curl gpg

keyring='/usr/share/keyrings/google-linux-signing-keyring.gpg'
repo_file='/etc/apt/sources.list.d/google-chrome.list'
key_url='https://dl.google.com/linux/linux_signing_key.pub'
repo_url='https://dl.google.com/linux/chrome/deb/'
expected_fingerprint='EB4C1BFD4F042F6DDDCCEC917721F63BD38B4796'

install -d -m 0755 /usr/share/keyrings
tmp_key="$(mktemp)"
tmp_repo="$(mktemp "${repo_file}.tmp.XXXXXX")"
cleanup() {
  rm -f "$tmp_key" "$tmp_repo"
}
trap cleanup EXIT

if ! command -v gpg >/dev/null 2>&1; then
  fail "gpg is required to install the Google Chrome APT signing key"
fi

curl -fsSL --retry 5 --retry-delay 2 --retry-connrefused --retry-all-errors "$key_url" -o "$tmp_key"
key_info="$(gpg --batch --with-colons --import-options show-only --import "$tmp_key")"
pub_count="$(printf '%s\n' "$key_info" | awk -F: '$1 == "pub" { count++ } END { print count + 0 }')"
if [[ "$pub_count" != "1" ]]; then
  fail "expected exactly one public key in Google's signing key file"
fi
actual_fingerprint="$(printf '%s\n' "$key_info" | awk -F: '$1 == "pub" { seen_pub++; next } seen_pub == 1 && $1 == "fpr" { print $10; exit }')"
if [[ "$actual_fingerprint" != "$expected_fingerprint" ]]; then
  fail "unexpected Google Linux signing key fingerprint '$actual_fingerprint'"
fi
gpg --dearmor --yes --output "$keyring" "$tmp_key"
printf 'deb [arch=amd64 signed-by=%s] %s stable main\n' "$keyring" "$repo_url" > "$tmp_repo"
chmod 0644 "$tmp_repo"
mv "$tmp_repo" "$repo_file"

apt-get update
apt-get install -y --no-install-recommends google-chrome-stable

if [[ ! -x /usr/bin/google-chrome ]]; then
  fail "installation completed but /usr/bin/google-chrome was not found"
fi

echo "Installed google-chrome at /usr/bin/google-chrome"
/usr/bin/google-chrome --version
