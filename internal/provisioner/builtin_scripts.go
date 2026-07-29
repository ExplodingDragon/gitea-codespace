// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package provisioner

import _ "embed"

//go:embed builtin/bootstrap.sh
var builtinBootstrapScript string
