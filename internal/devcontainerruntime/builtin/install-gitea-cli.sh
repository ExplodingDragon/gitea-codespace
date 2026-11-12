#!/bin/sh
# Copyright 2026 The Gitea Authors. All rights reserved.
# SPDX-License-Identifier: MIT

set -eu

gt=/usr/local/libexec/gitea-codespace/gt
"$gt" version
mkdir -p /usr/local/share/bash-completion/completions \
  /usr/local/share/zsh/site-functions \
  /usr/local/share/fish/vendor_completions.d
"$gt" completion bash >/usr/local/share/bash-completion/completions/gt
"$gt" completion zsh >/usr/local/share/zsh/site-functions/_gt
"$gt" completion fish >/usr/local/share/fish/vendor_completions.d/gt.fish
rm -f /usr/local/bin/gt
