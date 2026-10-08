// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package serve

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"gitea.dev/codespace/internal/cluster"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// NewCommand creates the manager service command.
func NewCommand() *cobra.Command {
	var options cluster.ServerOptions
	var kubeconfig string
	command := &cobra.Command{
		Use:   "serve",
		Short: "Run the Codespace manager and administration server",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			ctrl.SetLogger(zap.New(zap.WriteTo(command.ErrOrStderr())))
			rules := clientcmd.NewDefaultClientConfigLoadingRules()
			rules.ExplicitPath = kubeconfig
			config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
			if err != nil {
				return err
			}
			ctx, cancel := signal.NotifyContext(command.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			return cluster.Run(ctx, config, options)
		},
	}
	flags := command.Flags()
	flags.StringVar(&kubeconfig, "kubeconfig", "", "Kubernetes client configuration (defaults to in-cluster credentials)")
	flags.StringVar(&options.Namespace, "namespace", "codespace-system", "Management namespace")
	flags.StringVar(&options.ManagerURL, "manager-url", "https://codespace-manager.codespace-system.svc:8443", "Manager Service URL used by Agents")
	flags.StringVar(&options.ComponentURL, "component-url", "https://codespace-manager.codespace-system.svc:8445", "Manager Service URL used by Gateway and Cache")
	flags.StringVar(&options.PlatformImage, "platform-image", "", "Digest-pinned image used by all managed Codespace workloads")
	flags.StringSliceVar(&options.ImagePullSecrets, "image-pull-secret", nil, "Management-namespace image pull Secret copied to managed site namespaces")
	flags.StringVar(&options.IdentityIssuer, "identity-issuer", "codespace-identity", "cert-manager ClusterIssuer for component identities")
	flags.StringVar(&options.HealthAddress, "health-address", ":8081", "Health and readiness listen address")
	flags.StringVar(&options.AdminAddress, "admin-address", ":18080", "Administration HTTP listen address")
	flags.StringVar(&options.AdminPublicURL, "admin-public-url", "http://127.0.0.1:18080", "Browser-facing administration origin")
	flags.StringVar(&options.AdminTokenFile, "admin-token-file", "/var/run/codespace/admin/token", "Projected administrator token file")
	flags.StringVar(&options.AgentAddress, "agent-address", ":8443", "Agent mTLS listen address")
	flags.StringVar(&options.ComponentAddress, "component-address", ":8445", "Gateway and Cache mTLS listen address")
	flags.StringVar(&options.CertificateDirectory, "certificate-directory", "/var/run/codespace/identity", "Projected tls.crt, tls.key and ca.crt directory")
	flags.StringVar(&options.GatewayParentName, "gateway-parent-name", "", "existing Gateway API parent used for managed public routes")
	flags.StringVar(&options.GatewayParentNamespace, "gateway-parent-namespace", "", "namespace of the parent Gateway (defaults to the management namespace)")
	flags.StringVar(&options.GatewayHTTPSectionName, "gateway-http-listener", "", "parent Gateway HTTPS listener name")
	flags.StringVar(&options.GatewaySSHSectionName, "gateway-ssh-listener", "", "parent Gateway TCP listener name; empty leaves SSH exposure to the operator")
	flags.StringVar(&options.PodName, "pod-name", os.Getenv("POD_NAME"), "current Manager Pod name used for leader Service routing")
	return command
}
