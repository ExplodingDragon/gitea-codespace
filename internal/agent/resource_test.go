// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseResourceLimits(t *testing.T) {
	tests := []struct {
		name  string
		parse func([]byte) (int64, error)
		input string
		want  int64
		err   bool
	}{
		{name: "unlimited cpu", parse: parseCPUMax, input: "max 100000\n"},
		{name: "limited cpu", parse: parseCPUMax, input: "200000 100000\n", want: 2000},
		{name: "invalid cpu", parse: parseCPUMax, input: "invalid", err: true},
		{name: "unlimited memory", parse: parseMemoryMax, input: "max\n"},
		{name: "limited memory", parse: parseMemoryMax, input: "1073741824\n", want: 1073741824},
		{name: "invalid memory", parse: parseMemoryMax, input: "invalid", err: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.parse([]byte(test.input))
			if test.err {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.want, got)
		})
	}
}
