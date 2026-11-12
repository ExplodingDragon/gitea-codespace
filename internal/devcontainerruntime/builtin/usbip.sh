#!/bin/sh
set -eu
exec /usr/local/libexec/gitea-codespace-runtime runtime usbip "$@"
