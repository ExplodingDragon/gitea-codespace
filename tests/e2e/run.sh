#!/usr/bin/env bash

set -euo pipefail

GO_BIN=${GO:-go}
HELM_BIN=${HELM:-helm}
KUBECTL_BIN=${KUBECTL:-kubectl}
S3MOCK_IMAGE=${CODESPACE_E2E_S3MOCK_IMAGE:-adobe/s3mock:5.2.2}
RUN_ID=${CODESPACE_E2E_RUN_ID:-"$(date -u +%Y%m%d%H%M%S)-$$"}
MANAGEMENT_NAMESPACE=${CODESPACE_E2E_MANAGEMENT_NAMESPACE:-"codespace-e2e-${RUN_ID}"}
RELEASE=${CODESPACE_E2E_RELEASE:-codespace-e2e}
MANAGER_NAME=${CODESPACE_E2E_MANAGER_NAME:-"codespace-manager-${RUN_ID}"}
ISSUER=${CODESPACE_E2E_IDENTITY_ISSUER:-codespace-identity}
ARTIFACTS=${CODESPACE_E2E_ARTIFACTS:-".tmp/e2e/${RUN_ID}"}
RUN_LABEL=codespace.gitea.dev/e2e-run
PORT_FORWARD_PID=

if [[ $ARTIFACTS != /* ]]; then
	ARTIFACTS="$PWD/$ARTIFACTS"
fi

required_commands=("$GO_BIN" "$HELM_BIN" "$KUBECTL_BIN" sed)
for command_name in "${required_commands[@]}"; do
	if ! command -v "$command_name" >/dev/null 2>&1; then
		echo "required command is unavailable: $command_name" >&2
		exit 1
	fi
done

required_variables=(
	CODESPACE_E2E_PLATFORM_IMAGE
	CODESPACE_E2E_RUNTIME_ISOLATION
	CODESPACE_E2E_RUNTIME_CLASS
	CODESPACE_E2E_STORAGE_CLASS
	CODESPACE_E2E_GITEA_URL
	CODESPACE_E2E_GITEA_MANAGER_ID
	CODESPACE_E2E_GITEA_MANAGER_SECRET_FILE
	CODESPACE_E2E_GITEA_LISTEN
	CODESPACE_E2E_NODE_ADDRESS
)
for variable_name in "${required_variables[@]}"; do
	if [[ -z ${!variable_name:-} ]]; then
		echo "$variable_name is required" >&2
		exit 1
	fi
done

if [[ ! -r $CODESPACE_E2E_GITEA_MANAGER_SECRET_FILE ]]; then
	echo "CODESPACE_E2E_GITEA_MANAGER_SECRET_FILE must be a readable file" >&2
	exit 1
fi
if [[ $CODESPACE_E2E_PLATFORM_IMAGE != *@sha256:* ]]; then
	echo "CODESPACE_E2E_PLATFORM_IMAGE must be an immutable repository@sha256:digest reference" >&2
	exit 1
fi
if [[ $CODESPACE_E2E_RUNTIME_ISOLATION != sysbox && $CODESPACE_E2E_RUNTIME_ISOLATION != kata ]]; then
	echo "CODESPACE_E2E_RUNTIME_ISOLATION must be sysbox or kata" >&2
	exit 1
fi
if ((${#MANAGER_NAME} > 63)); then
	echo "CODESPACE_E2E_MANAGER_NAME must fit the Kubernetes 63-character name limit" >&2
	exit 1
fi

mkdir -p "$ARTIFACTS/bin" "$ARTIFACTS/diagnostics"

collect_diagnostics() {
	set +e
	"$KUBECTL_BIN" get giteasites -l "$RUN_LABEL=$RUN_ID" -o yaml >"$ARTIFACTS/diagnostics/giteasites.yaml" 2>&1
	"$KUBECTL_BIN" get environmenttemplates -l "$RUN_LABEL=$RUN_ID" -o yaml >"$ARTIFACTS/diagnostics/environmenttemplates.yaml" 2>&1
	for namespace in "$@"; do
		[[ -n $namespace ]] || continue
		directory="$ARTIFACTS/diagnostics/$namespace"
		mkdir -p "$directory"
		"$KUBECTL_BIN" get namespace "$namespace" -o yaml >"$directory/namespace.yaml" 2>&1
		"$KUBECTL_BIN" get all,pvc,networkpolicy,certificate -n "$namespace" -o wide >"$directory/resources.txt" 2>&1
		"$KUBECTL_BIN" get events -n "$namespace" --sort-by=.lastTimestamp >"$directory/events.txt" 2>&1
		while IFS= read -r pod; do
			[[ -n $pod ]] || continue
			"$KUBECTL_BIN" logs -n "$namespace" "$pod" --all-containers --tail=200 >"$directory/${pod}.log" 2>&1
			"$KUBECTL_BIN" logs -n "$namespace" "$pod" --all-containers --previous --tail=200 >"$directory/${pod}.previous.log" 2>&1
		done < <("$KUBECTL_BIN" get pods -n "$namespace" -o name 2>/dev/null | sed 's#^pod/##')
	done
}

cleanup() {
	status=$?
	trap - EXIT INT TERM
	set +e
	cleanup_failed=0
	mapfile -t namespaces < <("$KUBECTL_BIN" get namespace -l "$RUN_LABEL=$RUN_ID" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null)
	site_namespace="codespace-e2e-site-$RUN_ID"
	if "$KUBECTL_BIN" get namespace "$site_namespace" >/dev/null 2>&1; then
		namespaces+=("$site_namespace")
	fi
	if ((status != 0)); then
		collect_diagnostics "${namespaces[@]}"
	fi
	if [[ -n $PORT_FORWARD_PID ]]; then
		kill "$PORT_FORWARD_PID" >/dev/null 2>&1
		wait "$PORT_FORWARD_PID" >/dev/null 2>&1
	fi
	if ! "$KUBECTL_BIN" delete giteasites,environmenttemplates -l "$RUN_LABEL=$RUN_ID" --wait=false >/dev/null 2>&1; then
		cleanup_failed=1
	fi
	if ! "$KUBECTL_BIN" wait --for=delete giteasites,environmenttemplates -l "$RUN_LABEL=$RUN_ID" --timeout=2m >/dev/null 2>&1; then
		cleanup_failed=1
	fi
	if ! "$HELM_BIN" uninstall "$RELEASE" -n "$MANAGEMENT_NAMESPACE" --wait >/dev/null 2>&1; then
		cleanup_failed=1
	fi
	for namespace in "${namespaces[@]}"; do
		if ! "$KUBECTL_BIN" delete namespace "$namespace" --wait=false >/dev/null 2>&1; then
			cleanup_failed=1
		fi
	done
	for namespace in "${namespaces[@]}"; do
		if ! "$KUBECTL_BIN" wait --for=delete "namespace/$namespace" --timeout=3m >/dev/null 2>&1; then
			cleanup_failed=1
		fi
	done
	if ((status == 0 && cleanup_failed != 0)); then
		status=1
		echo "E2E resources were not completely removed" >"$ARTIFACTS/diagnostics/cleanup.txt"
	fi
	if ((status == 0)); then
		rm -rf "$ARTIFACTS"
		echo "Codespace E2E completed successfully"
	else
		echo "Codespace E2E failed; diagnostics are available in $ARTIFACTS/diagnostics" >&2
	fi
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

echo "Checking Kubernetes prerequisites"
"$KUBECTL_BIN" version >/dev/null
for resource in \
	customresourcedefinition/giteasites.codespace.gitea.dev \
	customresourcedefinition/environmenttemplates.codespace.gitea.dev \
	customresourcedefinition/codespaces.codespace.gitea.dev \
	"clusterissuer/$ISSUER" \
	"runtimeclass/$CODESPACE_E2E_RUNTIME_CLASS" \
	"storageclass/$CODESPACE_E2E_STORAGE_CLASS"; do
	"$KUBECTL_BIN" get "$resource" >/dev/null
done

echo "Building Runtime test binaries"
"$GO_BIN" test -c -tags netgo,osusergo -o "$ARTIFACTS/bin/agent.test" ./internal/agent
CGO_ENABLED=0 "$GO_BIN" test -c -o "$ARTIFACTS/bin/devcontainer.test" ./devcontainer/docker

"$KUBECTL_BIN" create namespace "$MANAGEMENT_NAMESPACE"
"$KUBECTL_BIN" label namespace "$MANAGEMENT_NAMESPACE" "$RUN_LABEL=$RUN_ID"
"$KUBECTL_BIN" create secret generic codespace-admin -n "$MANAGEMENT_NAMESPACE" \
	--from-literal=token="e2e-admin-$RUN_ID-isolated-test-token"

image_repository=${CODESPACE_E2E_PLATFORM_IMAGE%@sha256:*}
image_digest=sha256:${CODESPACE_E2E_PLATFORM_IMAGE##*@sha256:}
echo "Deploying isolated Manager release"
"$HELM_BIN" upgrade --install "$RELEASE" charts/gitea-codespace \
	--namespace "$MANAGEMENT_NAMESPACE" \
	--set-string fullnameOverride="$MANAGER_NAME" \
	--set-string image.repository="$image_repository" \
	--set-string image.digest="$image_digest" \
	--set-string image.pullPolicy=IfNotPresent \
	--set-string identity.clusterIssuer="$ISSUER" \
	--set-string admin.tokenSecret.name=codespace-admin \
	--set replicaCount="${CODESPACE_E2E_MANAGER_REPLICAS:-2}" \
	--wait --timeout 5m
"$KUBECTL_BIN" rollout status -n "$MANAGEMENT_NAMESPACE" "deployment/$MANAGER_NAME" --timeout=5m

export CODESPACE_E2E=1
export CODESPACE_E2E_RUN_ID="$RUN_ID"
export CODESPACE_E2E_MANAGEMENT_NAMESPACE="$MANAGEMENT_NAMESPACE"
export CODESPACE_E2E_MANAGER_NAME="$MANAGER_NAME"
export CODESPACE_E2E_AGENT_TEST_BINARY="$ARTIFACTS/bin/agent.test"
export CODESPACE_E2E_DEVCONTAINER_TEST_BINARY="$ARTIFACTS/bin/devcontainer.test"

run_cluster_tests() {
	stage=$1
	pattern=$2
	timeout=$3
	echo "Running $stage"
	"$GO_BIN" test -p 1 -count=1 -v -timeout "$timeout" -run "$pattern" ./internal/cluster
}

run_cluster_tests "Kubernetes API integration" \
	'^(TestKubernetesE2EResourceOwnership|TestKubernetesE2EAgentCertificate|TestKubernetesE2EManagerServing)$' 8m
run_cluster_tests "component lifecycle" '^TestKubernetesE2EComponentWorkloads$' 5m
run_cluster_tests "Manager leader failover" '^TestKubernetesE2EManagerFailover$' 5m
run_cluster_tests "real Gitea handshake" '^TestKubernetesE2ERealGiteaHandshake$' 2m
run_cluster_tests "Runtime Agent process model" '^TestKubernetesE2EAgentProcesses$' 6m
run_cluster_tests "Runtime, Docker persistence, and Dev Container" '^TestKubernetesE2EDockerPersistence$' 40m

echo "Deploying S3Mock"
"$KUBECTL_BIN" create deployment s3mock -n "$MANAGEMENT_NAMESPACE" --image="$S3MOCK_IMAGE" --dry-run=client -o yaml | \
	"$KUBECTL_BIN" set env --local -f - COM_ADOBE_TESTING_S3MOCK_STORE_INITIAL_BUCKETS=codespace-e2e -o yaml | \
	"$KUBECTL_BIN" set resources --local -f - --requests=cpu=50m,memory=128Mi --limits=cpu=1,memory=512Mi -o yaml | \
	"$KUBECTL_BIN" patch --local -f - --type=strategic --patch \
		'{"spec":{"template":{"spec":{"containers":[{"name":"s3mock","readinessProbe":{"tcpSocket":{"port":9090},"periodSeconds":1,"failureThreshold":120}}]}}}}' -o yaml | \
	"$KUBECTL_BIN" apply -f -
"$KUBECTL_BIN" expose deployment s3mock -n "$MANAGEMENT_NAMESPACE" --name=s3mock --port=9090 --target-port=9090
"$KUBECTL_BIN" rollout status deployment/s3mock -n "$MANAGEMENT_NAMESPACE" --timeout=5m

port_forward_log="$ARTIFACTS/s3mock-port-forward.log"
"$KUBECTL_BIN" port-forward -n "$MANAGEMENT_NAMESPACE" service/s3mock :9090 >"$port_forward_log" 2>&1 &
PORT_FORWARD_PID=$!
for ((attempt = 0; attempt < 100; attempt++)); do
	s3_port=$(sed -n 's/.*127\.0\.0\.1:\([0-9][0-9]*\) -> 9090.*/\1/p' "$port_forward_log" | head -n 1)
	if [[ -n $s3_port ]]; then
		break
	fi
	if ! kill -0 "$PORT_FORWARD_PID" 2>/dev/null; then
		cat "$port_forward_log" >&2
		exit 1
	fi
	sleep 0.1
done
if [[ -z ${s3_port:-} ]]; then
	echo "S3Mock port-forward did not become ready" >&2
	exit 1
fi
export CODESPACE_E2E_S3_BUCKET=codespace-e2e
export CODESPACE_E2E_S3_REGION=us-east-1
export CODESPACE_E2E_S3_ENDPOINT="http://127.0.0.1:$s3_port"
export CODESPACE_E2E_S3_ACCESS_KEY=e2e-access-key
export CODESPACE_E2E_S3_SECRET_KEY=e2e-secret-key
echo "Running S3 cache integration"
"$GO_BIN" test -p 1 -count=1 -v -timeout 5m -run '^TestCacheS3E2E$' ./internal/cache
kill "$PORT_FORWARD_PID" >/dev/null 2>&1 || true
wait "$PORT_FORWARD_PID" >/dev/null 2>&1 || true
PORT_FORWARD_PID=

run_cluster_tests "complete Codespace lifecycle" '^TestKubernetesE2EProductLifecycle$' 115m
