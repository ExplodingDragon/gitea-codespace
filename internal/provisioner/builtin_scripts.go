// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package provisioner

import _ "embed"

//go:embed builtin/init.sh
var builtinInitScript string

//go:embed builtin/start.sh
var builtinStartScript string

//go:embed builtin/resume.sh
var builtinResumeScript string
