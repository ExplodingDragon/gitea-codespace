#!/bin/sh

set -eu

source_bundle=/var/lib/gitea-codespace/ca/bundle.pem
[ -s "$source_bundle" ] || {
  echo "cannot update the Dev Container CA store: the platform CA bundle is unavailable" >&2
  exit 1
}
split_dir=$(mktemp -d /var/lib/gitea-codespace/.ca-XXXXXX)
trap 'rm -rf "$split_dir"' 0 HUP INT TERM
export TMPDIR="$split_dir"
certificate_count=0
certificate_active=false
certificate_path=
while IFS= read -r line || [ -n "$line" ]; do
  case $line in
  '-----BEGIN CERTIFICATE-----')
    [ "$certificate_active" = false ] || break
    certificate_count=$((certificate_count + 1))
    certificate_path=$(printf '%s/codespace-%03d.crt' "$split_dir" "$certificate_count")
    certificate_active=true
    ;;
  esac
  if [ "$certificate_active" = true ]; then
    printf '%s\n' "$line" >>"$certificate_path"
  fi
  case $line in
  '-----END CERTIFICATE-----') certificate_active=false ;;
  esac
done <"$source_bundle"
if [ "$certificate_count" -eq 0 ] || [ "$certificate_active" = true ]; then
  echo "cannot update the Dev Container CA store: the platform CA bundle is invalid" >&2
  exit 1
fi

for directory in \
  /usr/local/share/ca-certificates \
  /etc/pki/ca-trust/source/anchors \
  /etc/ca-certificates/trust-source/anchors \
  /etc/pki/trust/anchors; do
  [ -d "$directory" ] || continue
  for certificate in "$directory"/codespace-*.crt; do
    [ -e "$certificate" ] || continue
    rm -f "$certificate"
  done
done

if [ ! -r /etc/os-release ]; then
  echo "cannot update the Dev Container CA store: /etc/os-release is unavailable" >&2
  exit 1
fi
. /etc/os-release
family=" ${ID:-} ${ID_LIKE:-} "
case "$family" in
*" debian "* | *" ubuntu "*)
  target=/usr/local/share/ca-certificates
  set -- update-ca-certificates
  if ! command -v "$1" >/dev/null 2>&1; then
    command -v apt-get >/dev/null 2>&1 || {
      echo "cannot install the Dev Container CA tools: apt-get is unavailable" >&2
      exit 1
    }
    DEBIAN_FRONTEND=noninteractive apt-get update
    DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates
  fi
  ;;
*" alpine "*)
  target=/usr/local/share/ca-certificates
  set -- update-ca-certificates
  if ! command -v "$1" >/dev/null 2>&1; then
    command -v apk >/dev/null 2>&1 || {
      echo "cannot install the Dev Container CA tools: apk is unavailable" >&2
      exit 1
    }
    apk add --no-cache ca-certificates
  fi
  ;;
*" fedora "* | *" rhel "* | *" centos "*)
  target=/etc/pki/ca-trust/source/anchors
  set -- update-ca-trust extract
  if ! command -v "$1" >/dev/null 2>&1; then
    if command -v dnf >/dev/null 2>&1; then
      dnf install -y ca-certificates
    elif command -v microdnf >/dev/null 2>&1; then
      microdnf install -y ca-certificates
    elif command -v yum >/dev/null 2>&1; then
      yum install -y ca-certificates
    else
      echo "cannot install the Dev Container CA tools: no supported package manager is available" >&2
      exit 1
    fi
  fi
  ;;
*" arch "*)
  target=/etc/ca-certificates/trust-source/anchors
  set -- update-ca-trust
  if ! command -v "$1" >/dev/null 2>&1; then
    command -v pacman >/dev/null 2>&1 || {
      echo "cannot install the Dev Container CA tools: pacman is unavailable" >&2
      exit 1
    }
    pacman -Sy --noconfirm ca-certificates-utils
  fi
  ;;
*" suse "* | *" opensuse "*)
  target=/etc/pki/trust/anchors
  set -- update-ca-certificates
  if ! command -v "$1" >/dev/null 2>&1; then
    command -v zypper >/dev/null 2>&1 || {
      echo "cannot install the Dev Container CA tools: zypper is unavailable" >&2
      exit 1
    }
    zypper --non-interactive install ca-certificates
  fi
  ;;
*)
  echo "cannot update the Dev Container CA store for distribution ${ID:-unknown}" >&2
  exit 1
  ;;
esac
if ! command -v "$1" >/dev/null 2>&1; then
  echo "cannot update the Dev Container CA store: $1 is unavailable" >&2
  exit 1
fi
mkdir -p "$target"
for certificate in "$split_dir"/codespace-*.crt; do
  cp "$certificate" "$target/$(basename "$certificate")"
done
"$@"
