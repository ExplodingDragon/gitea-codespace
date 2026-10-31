// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	configpkg "gitea.dev/codespace/internal/config"
	corev1 "k8s.io/api/core/v1"
)

func decodeComponentConfig(component *corev1.ConfigMap, role string, target any) error {
	if component.Labels[ComponentLabel] != role {
		return fmt.Errorf("component %s is not a %s", component.Name, role)
	}
	decoder := json.NewDecoder(strings.NewReader(component.Data["config"]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode %s configuration: %w", role, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("decode %s configuration: expected one JSON object", role)
	}
	return nil
}

func gatewayComponentConfig(component *corev1.ConfigMap) (configpkg.GatewayConfig, error) {
	var config configpkg.GatewayConfig
	if err := decodeComponentConfig(component, "gateway", &config); err != nil {
		return config, err
	}
	if err := config.Validate(); err != nil {
		return config, fmt.Errorf("invalid gateway configuration: %w", err)
	}
	return config, nil
}

func cacheComponentConfig(component *corev1.ConfigMap) (configpkg.CacheConfig, error) {
	var config configpkg.CacheConfig
	if err := decodeComponentConfig(component, "cache", &config); err != nil {
		return config, err
	}
	config.ID = component.Name
	if err := config.Validate(); err != nil {
		return config, fmt.Errorf("invalid cache configuration: %w", err)
	}
	return config, nil
}
