// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package controlplane

import (
	"context"
	"fmt"
	"math"
	"strconv"

	"connectrpc.com/connect"
	"gitea.dev/codespace-proto-go/codespace/v1/codespacev1connect"
	"google.golang.org/protobuf/proto"
)

const (
	// ProtocolVersion is the ManagerService protocol implemented by this binary.
	ProtocolVersion int32 = 1
	// ManagerIDHeader carries the Gitea-issued Manager identity.
	ManagerIDHeader = "x-codespace-manager-id"
	// ManagerSecretHeader authenticates the Gitea-issued Manager identity.
	ManagerSecretHeader = "x-codespace-manager-secret"
)

// NewManagerServiceClient returns an authenticated ManagerService client.
func NewManagerServiceClient(httpClient connect.HTTPClient, baseURL string, managerID int64, managerSecret string, maxBytes int64) codespacev1connect.ManagerServiceClient {
	opts := []connect.ClientOption{
		connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(ctx context.Context, request connect.AnyRequest) (connect.AnyResponse, error) {
				request.Header().Set(ManagerIDHeader, strconv.FormatInt(managerID, 10))
				request.Header().Set(ManagerSecretHeader, managerSecret)
				return next(ctx, request)
			}
		})),
	}
	if maxBytes > 0 {
		maxSize := int(maxBytes)
		if int64(maxSize) != maxBytes {
			maxSize = math.MaxInt
		}
		opts = append(opts, connect.WithReadMaxBytes(maxSize), connect.WithSendMaxBytes(maxSize))
	}
	return codespacev1connect.NewManagerServiceClient(httpClient, baseURL, opts...)
}

// CheckMessageSize verifies a protobuf message before a size-limited RPC.
func CheckMessageSize(message proto.Message, maxBytes int64) error {
	if maxBytes <= 0 || message == nil {
		return nil
	}
	size := proto.Size(message)
	if int64(size) <= maxBytes {
		return nil
	}
	return fmt.Errorf("control plane message size %d exceeds limit %d", size, maxBytes)
}
