// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package manager

import (
	"errors"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
)

const (
	failureProtocolMismatch       = "protocol_mismatch"
	failureStateHistoryConflict   = "state_history_conflict"
	failureManagerUnregistered    = "manager_unregistered"
	failureInvalidDeclaration     = "invalid_declaration"
	failureGatewayURLConflict     = "gateway_url_conflict"
	failureGatewaySSHAddrConflict = "gateway_ssh_addr_conflict"
	failureOperationRegression    = "operation_version_regression"
	failureLocalStateCommit       = "local_state_commit_failed"
	failureGenerationConflict     = "generation_conflict"
	failureVersionExhausted       = "version_exhausted"
	failureLogOffsetConflict      = "offset_conflict"
	failureLogOffsetGap           = "offset_gap"
	failureLogSizeExceeded        = "log_size_exceeded"
)

type categorizedError struct {
	category string
	message  string
}

func logCurrentOffset(err error) (int64, bool) {
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		return 0, false
	}
	for _, detail := range connectErr.Details() {
		value, detailErr := detail.Value()
		if detailErr != nil {
			continue
		}
		if offset, ok := value.(*codespacev1.LogOffsetDetail); ok {
			return offset.GetCurrentOffset(), true
		}
	}
	return 0, false
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
	case failureProtocolMismatch,
		failureStateHistoryConflict,
		failureManagerUnregistered,
		failureInvalidDeclaration,
		failureGatewayURLConflict,
		failureGatewaySSHAddrConflict,
		failureOperationRegression,
		failureLocalStateCommit:
		return true
	default:
		return false
	}
}
