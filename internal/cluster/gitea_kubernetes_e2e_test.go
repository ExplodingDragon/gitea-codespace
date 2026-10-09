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
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestKubernetesE2ERealGiteaHandshake(t *testing.T) {
	cluster := newE2ECluster(t, 30*time.Second)
	giteaURL := strings.TrimRight(requireE2EEnvironment(t, "CODESPACE_E2E_GITEA_URL"), "/")
	managerID, err := strconv.ParseInt(requireE2EEnvironment(t, "CODESPACE_E2E_GITEA_MANAGER_ID"), 10, 64)
	require.NoError(t, err)
	require.Positive(t, managerID)
	managerSecretData, err := os.ReadFile(requireE2EEnvironment(t, "CODESPACE_E2E_GITEA_MANAGER_SECRET_FILE"))
	require.NoError(t, err)
	managerSecret := strings.TrimSpace(string(managerSecretData))
	require.NotEmpty(t, managerSecret)

	siteUID := types.UID(uuid.NewString())
	credential := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "real-gitea-smoke-", Namespace: cluster.managementNamespace, Labels: map[string]string{SiteUIDLabel: string(siteUID), e2eRunLabel: cluster.runID}},
		Data:       map[string][]byte{"managerSecret": []byte(managerSecret)},
	}
	require.NoError(t, cluster.Create(cluster.ctx, credential))
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if err := client.IgnoreNotFound(cluster.Delete(cleanup, credential, client.Preconditions{UID: &credential.UID})); err != nil {
			t.Errorf("delete real Gitea E2E credential: %v", err)
		}
	})

	site := &api.GiteaSite{
		ObjectMeta: metav1.ObjectMeta{UID: siteUID},
		Spec: api.GiteaSiteSpec{
			ManagerID:  managerID,
			Credential: api.ResourceReference{Name: credential.Name, UID: credential.UID},
		},
		Status: api.GiteaSiteStatus{CanonicalURL: giteaURL},
	}
	remote, err := siteManagerClient(cluster.ctx, cluster.Client, cluster.managementNamespace, site)
	require.NoError(t, err)
	response, err := remote.CheckManager(cluster.ctx, connect.NewRequest(&codespacev1.CheckManagerRequest{ProtocolVersion: 1}))
	require.NoError(t, err)
	require.Equal(t, giteaURL, strings.TrimRight(response.Msg.GiteaWebUrl, "/"))
	require.NotEmpty(t, response.Msg.ManagerName)
}
