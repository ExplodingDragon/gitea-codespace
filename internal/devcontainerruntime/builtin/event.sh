#!/bin/sh
# Copyright 2026 The Gitea Authors. All rights reserved.
# SPDX-License-Identifier: MIT
set -eu
exec /usr/local/libexec/gitea-codespace-runtime runtime event "$@"
