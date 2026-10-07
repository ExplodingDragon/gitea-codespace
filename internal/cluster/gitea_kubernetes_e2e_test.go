// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cluster

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	codespacev1 "gitea.dev/codespace-proto-go/codespace/v1"
	api "gitea.dev/codespace/internal/cluster/api/v1alpha1"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestKubernetesE2ERealGiteaHandshake(t *testing.T) {
	if os.Getenv("CODESPACE_TEST_KUBERNETES_GITEA") != "1" {
		t.Skip("requires a Kubernetes cluster and a real Gitea Codespace Manager credential")
	}
	giteaURL := strings.TrimRight(os.Getenv("CODESPACE_TEST_GITEA_URL"), "/")
	managerID, err := strconv.ParseInt(os.Getenv("CODESPACE_TEST_GITEA_MANAGER_ID"), 10, 64)
	require.NoError(t, err)
	require.NotEmpty(t, giteaURL)
	require.Positive(t, managerID)
	managerSecret := os.Getenv("CODESPACE_TEST_GITEA_MANAGER_SECRET")
	require.NotEmpty(t, managerSecret)
	managementNamespace := os.Getenv("CODESPACE_TEST_MANAGEMENT_NAMESPACE")
	if managementNamespace == "" {
		managementNamespace = "codespace-system"
	}

	config, err := ctrl.GetConfig()
	require.NoError(t, err)
	kubernetes, err := client.New(config, client.Options{Scheme: testScheme(t)})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	siteUID := types.UID(uuid.NewString())
	credential := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "real-gitea-smoke-", Namespace: managementNamespace, Labels: map[string]string{SiteUIDLabel: string(siteUID)}},
		Data:       map[string][]byte{"managerSecret": []byte(managerSecret)},
	}
	require.NoError(t, kubernetes.Create(ctx, credential))
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		_ = kubernetes.Delete(cleanup, credential, client.Preconditions{UID: &credential.UID})
	})

	site := &api.GiteaSite{
		ObjectMeta: metav1.ObjectMeta{UID: siteUID},
		Spec: api.GiteaSiteSpec{
			ManagerID:  managerID,
			Credential: api.ResourceReference{Name: credential.Name, UID: credential.UID},
		},
		Status: api.GiteaSiteStatus{CanonicalURL: giteaURL},
	}
	remote, err := siteManagerClient(ctx, kubernetes, managementNamespace, site)
	require.NoError(t, err)
	response, err := remote.CheckManager(ctx, connect.NewRequest(&codespacev1.CheckManagerRequest{ProtocolVersion: 1}))
	require.NoError(t, err)
	require.Equal(t, giteaURL, strings.TrimRight(response.Msg.GiteaWebUrl, "/"))
	require.NotEmpty(t, response.Msg.ManagerName)
}
