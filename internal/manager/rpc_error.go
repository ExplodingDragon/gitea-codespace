// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package manager

import (
	"errors"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
)

const (
	failureProtocolMismatch     = "protocol_mismatch"
	failureStateHistoryConflict = "state_history_conflict"
	failureManagerUnregistered  = "manager_unregistered"
	failureOperationRegression  = "operation_version_regression"
	failureLocalStateCommit     = "local_state_commit_failed"
)

type categorizedError struct {
	category string
	message  string
}

func (e *categorizedError) Error() string {
	return e.message
}

func failureCategory(err error) string {
	var categorized *categorizedError
	if errors.As(err, &categorized) {
		return categorized.category
	}
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		return ""
	}
	for _, detail := range connectErr.Details() {
		value, detailErr := detail.Value()
		if detailErr != nil {
			continue
		}
		if failure, ok := value.(*codespacev1.FailureDetail); ok {
			return failure.GetCategory()
		}
	}
	return ""
}

func isManagerCriticalError(err error) bool {
	switch failureCategory(err) {
	case failureProtocolMismatch, failureStateHistoryConflict, failureManagerUnregistered, failureOperationRegression, failureLocalStateCommit:
		return true
	default:
		return false
	}
}
