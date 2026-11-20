// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/stretchr/testify/require"
)

type startAPIClient struct {
	client.APIClient
	started []string
}

func (c *startAPIClient) ContainerStart(_ context.Context, id string, _ container.StartOptions) error {
	c.started = append(c.started, id)
	return nil
}

func TestContainerStartPreparesFilesBeforeStartingProcess(t *testing.T) {
	t.Parallel()
	api := &startAPIClient{}
	preparationErr := errors.New("cannot prepare trust")
	engine := &Engine{BeforeContainerStart: func(_ context.Context, received client.APIClient, id string) error {
		require.Same(t, api, received)
		if id == "failed" {
			return preparationErr
		}
		require.Empty(t, api.started)
		return nil
	}}
	wrapped := &startingAPIClient{APIClient: api, engine: engine}
	// Docker CLI and Compose decorate this same client for container creation.
	decorated := &mutatingAPIClient{APIClient: wrapped}
	require.NoError(t, decorated.ContainerStart(t.Context(), "primary", container.StartOptions{}))
	require.ErrorIs(t, decorated.ContainerStart(t.Context(), "failed", container.StartOptions{}), preparationErr)
	require.Equal(t, []string{"primary"}, api.started)
}
