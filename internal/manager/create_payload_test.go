// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package manager

import (
	"testing"

	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
)

func TestUserFeaturesFromCreatePayload(t *testing.T) {
	t.Parallel()

	payload := &codespacev1.CreateOperationPayload{DevContainer: &codespacev1.DevContainerConfiguration{UserFeatures: []*codespacev1.DevContainerFeature{{
		Reference: "ghcr.io/example/features/tool:1",
		Options: []*codespacev1.DevContainerFeatureOption{
			{Name: "version", Value: &codespacev1.DevContainerFeatureOption_StringValue{StringValue: "1.2.3"}},
			{Name: "enabled", Value: &codespacev1.DevContainerFeatureOption_BoolValue{BoolValue: true}},
		},
	}}}}
	features, err := userFeaturesFromCreatePayload(payload)
	if err != nil {
		t.Fatalf("convert user Features: %v", err)
	}
	if len(features) != 1 || string(features[0].Options["version"]) != `"1.2.3"` || string(features[0].Options["enabled"]) != "true" {
		t.Fatalf("converted user Features = %#v", features)
	}
}
