// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const e2eRunLabel = "codespace.gitea.dev/e2e-run"

type e2eCluster struct {
	client.Client
	config              *rest.Config
	ctx                 context.Context
	managementNamespace string
	runID               string
}

type e2eRuntime struct {
	isolation     string
	runtimeClass  string
	storageClass  string
	platformImage string
}

func requireE2E(t *testing.T) {
	t.Helper()
	if os.Getenv("CODESPACE_E2E") != "1" {
		t.Skip("run make test-e2e against a prepared Kubernetes test cluster")
	}
}

func requireE2EEnvironment(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	require.NotEmpty(t, value, "%s must be set by the E2E runner", name)
	return value
}

func newE2ECluster(t *testing.T, timeout time.Duration) *e2eCluster {
	t.Helper()
	requireE2E(t)
	config, err := ctrl.GetConfig()
	require.NoError(t, err)
	kubernetes, err := client.New(config, client.Options{Scheme: testScheme(t)})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	t.Cleanup(cancel)
	return &e2eCluster{
		Client:              kubernetes,
		config:              config,
		ctx:                 ctx,
		managementNamespace: requireE2EEnvironment(t, "CODESPACE_E2E_MANAGEMENT_NAMESPACE"),
		runID:               requireE2EEnvironment(t, "CODESPACE_E2E_RUN_ID"),
	}
}

func requireE2ERuntime(t *testing.T) e2eRuntime {
	t.Helper()
	runtime := e2eRuntime{
		isolation:     requireE2EEnvironment(t, "CODESPACE_E2E_RUNTIME_ISOLATION"),
		runtimeClass:  requireE2EEnvironment(t, "CODESPACE_E2E_RUNTIME_CLASS"),
		storageClass:  requireE2EEnvironment(t, "CODESPACE_E2E_STORAGE_CLASS"),
		platformImage: requireE2EEnvironment(t, "CODESPACE_E2E_PLATFORM_IMAGE"),
	}
	require.Contains(t, []string{"kata", "sysbox"}, runtime.isolation)
	return runtime
}

func (e *e2eCluster) createNamespace(t *testing.T, prefix string) *corev1.Namespace {
	t.Helper()
	const uuidSuffixLength = 1 + 36
	if len(prefix) > 63-uuidSuffixLength {
		prefix = prefix[:63-uuidSuffixLength]
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   prefix + "-" + uuid.NewString(),
		Labels: map[string]string{e2eRunLabel: e.runID},
	}}
	require.NoError(t, e.Create(e.ctx, namespace))
	t.Cleanup(func() {
		if t.Failed() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := client.IgnoreNotFound(e.Delete(ctx, namespace, client.Preconditions{UID: &namespace.UID})); err != nil {
			t.Errorf("delete E2E namespace %s: %v", namespace.Name, err)
			return
		}
		assert.Eventually(t, func() bool {
			return apierrors.IsNotFound(e.Get(ctx, client.ObjectKeyFromObject(namespace), &corev1.Namespace{}))
		}, time.Minute, 200*time.Millisecond, "E2E namespace %s was not deleted", namespace.Name)
	})
	return namespace
}
