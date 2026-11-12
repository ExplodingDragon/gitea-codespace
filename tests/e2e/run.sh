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
ARTIFACTS=${CODESPACE_E2E_ARTIFACTS:-".tmp/e2e/${RUN_ID}"}
RUN_LABEL=codespace.gitea.dev/e2e-run
PORT_FORWARD_PID=
DEVICE_PLUGIN_PID=
USBIP_FIXTURE_PID=
DEVICE_PLUGIN_SOCKET=/var/lib/kubelet/device-plugins/codespace-e2e-fuse.sock
DEVICE_PLUGIN_CDI_SPEC=/var/run/cdi/codespace-e2e-fuse.yaml

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
	KUBECONFIG
	CODESPACE_E2E_PLATFORM_IMAGE
	CODESPACE_E2E_WEB_IDE_IMAGE
	CODESPACE_E2E_WEB_IDE_LAYOUT
	CODESPACE_E2E_RUNTIME_ISOLATION
	CODESPACE_E2E_STORAGE_CLASS
	CODESPACE_E2E_GITEA_LISTEN
	CODESPACE_E2E_NODE_ADDRESS
)
for variable_name in "${required_variables[@]}"; do
	if [[ -z ${!variable_name:-} ]]; then
		echo "$variable_name is required" >&2
		exit 1
	fi
done

if [[ $CODESPACE_E2E_PLATFORM_IMAGE != *@sha256:* ]]; then
	echo "CODESPACE_E2E_PLATFORM_IMAGE must be an immutable repository@sha256:digest reference" >&2
	exit 1
fi
if [[ $CODESPACE_E2E_WEB_IDE_IMAGE != *:* || ! -r $CODESPACE_E2E_WEB_IDE_LAYOUT/index.json ]]; then
	echo "fake Web IDE image tag and readable OCI layout are required" >&2
	exit 1
fi
if [[ $CODESPACE_E2E_RUNTIME_ISOLATION != sysbox && $CODESPACE_E2E_RUNTIME_ISOLATION != kata && $CODESPACE_E2E_RUNTIME_ISOLATION != native ]]; then
	echo "CODESPACE_E2E_RUNTIME_ISOLATION must be sysbox, kata or native" >&2
	exit 1
fi
if [[ $CODESPACE_E2E_RUNTIME_ISOLATION == native ]]; then
	if [[ -n ${CODESPACE_E2E_RUNTIME_CLASS:-} ]]; then
		echo "Native E2E uses the cluster default runtime and must not set CODESPACE_E2E_RUNTIME_CLASS" >&2
		exit 1
	fi
elif [[ -z ${CODESPACE_E2E_RUNTIME_CLASS:-} ]]; then
	echo "CODESPACE_E2E_RUNTIME_CLASS is required for $CODESPACE_E2E_RUNTIME_ISOLATION" >&2
	exit 1
fi
if [[ $CODESPACE_E2E_RUNTIME_ISOLATION == kata ]]; then
	if [[ -z ${CODESPACE_E2E_USBIP_RUNTIME_CLASS:-} || -z ${CODESPACE_E2E_USBIP_KERNEL_RELEASE:-} ]]; then
		echo "CODESPACE_E2E_USBIP_RUNTIME_CLASS and CODESPACE_E2E_USBIP_KERNEL_RELEASE are required for the Kata USB/IP product E2E" >&2
		exit 1
	fi
fi

if ((${#MANAGER_NAME} > 63)); then
	echo "CODESPACE_E2E_MANAGER_NAME must fit the Kubernetes 63-character name limit" >&2
	exit 1
fi

mkdir -p "$ARTIFACTS/bin" "$ARTIFACTS/diagnostics"

collect_diagnostics() {
	set +e
	"$KUBECTL_BIN" get nodes -o wide >"$ARTIFACTS/diagnostics/nodes.txt" 2>&1
	"$KUBECTL_BIN" get nodes -o yaml >"$ARTIFACTS/diagnostics/nodes.yaml" 2>&1
	"$KUBECTL_BIN" describe nodes >"$ARTIFACTS/diagnostics/nodes.describe.txt" 2>&1
	"$KUBECTL_BIN" get runtimeclasses -o yaml >"$ARTIFACTS/diagnostics/runtimeclasses.yaml" 2>&1
	"$KUBECTL_BIN" get storageclasses -o yaml >"$ARTIFACTS/diagnostics/storageclasses.yaml" 2>&1
	"$HELM_BIN" status "$RELEASE" -n "$MANAGEMENT_NAMESPACE" >"$ARTIFACTS/diagnostics/helm.txt" 2>&1
	"$KUBECTL_BIN" get giteasites -l "$RUN_LABEL=$RUN_ID" -o yaml >"$ARTIFACTS/diagnostics/giteasites.yaml" 2>&1
	"$KUBECTL_BIN" get environmenttemplates -l "$RUN_LABEL=$RUN_ID" -o yaml >"$ARTIFACTS/diagnostics/environmenttemplates.yaml" 2>&1
	"$KUBECTL_BIN" get codespacecomponents -l "$RUN_LABEL=$RUN_ID" -o yaml >"$ARTIFACTS/diagnostics/codespacecomponents.yaml" 2>&1
	for namespace in "$@"; do
		[[ -n $namespace ]] || continue
		directory="$ARTIFACTS/diagnostics/$namespace"
		mkdir -p "$directory"
		"$KUBECTL_BIN" get namespace "$namespace" -o yaml >"$directory/namespace.yaml" 2>&1
		"$KUBECTL_BIN" get all,configmap,pvc,networkpolicy -n "$namespace" -o wide >"$directory/resources.txt" 2>&1
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
	if [[ -n $DEVICE_PLUGIN_PID ]]; then
		sudo kill "$DEVICE_PLUGIN_PID" >/dev/null 2>&1 || true
		wait "$DEVICE_PLUGIN_PID" >/dev/null 2>&1 || true
		sudo rm -f "$DEVICE_PLUGIN_SOCKET" "$DEVICE_PLUGIN_CDI_SPEC"
	fi
	if [[ -n $USBIP_FIXTURE_PID ]]; then
		kill "$USBIP_FIXTURE_PID" >/dev/null 2>&1 || true
		wait "$USBIP_FIXTURE_PID" >/dev/null 2>&1 || true
	fi
	if ! "$KUBECTL_BIN" delete codespacecomponents,giteasites,environmenttemplates -l "$RUN_LABEL=$RUN_ID" --wait=false >/dev/null 2>&1; then
		cleanup_failed=1
	fi
	if ! "$KUBECTL_BIN" wait --for=delete codespacecomponents,giteasites,environmenttemplates -l "$RUN_LABEL=$RUN_ID" --timeout=2m >/dev/null 2>&1; then
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
		collect_diagnostics "${namespaces[@]}"
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

if [[ $CODESPACE_E2E_RUNTIME_ISOLATION == kata ]]; then
	if [[ -n ${CODESPACE_E2E_USBIP_FIXTURE:-} ]]; then
		usbip_fixture=$CODESPACE_E2E_USBIP_FIXTURE
		if [[ ! -x $usbip_fixture ]]; then
			echo "CODESPACE_E2E_USBIP_FIXTURE must point to an executable fixture" >&2
			exit 1
		fi
	else
		if ! command -v cargo >/dev/null 2>&1; then
			echo "cargo is required to build the Kata USB/IP product fixture" >&2
			exit 1
		fi
		usbip_fixture="$ARTIFACTS/bin/codespace-usbip-echo"
		echo "Building the pinned virtual CDC ACM USB/IP fixture"
		CARGO_TARGET_DIR="$ARTIFACTS/usbip-target" cargo build --manifest-path tests/e2e/usbip/Cargo.toml --locked --release --bin codespace-usbip-echo
		cp "$ARTIFACTS/usbip-target/release/codespace-usbip-echo" "$usbip_fixture"
		chmod 0555 "$usbip_fixture"
	fi
	"$usbip_fixture" >"$ARTIFACTS/diagnostics/usbip-fixture.log" 2>&1 &
	USBIP_FIXTURE_PID=$!
	fixture_ready=0
	for ((attempt = 0; attempt < 100; attempt++)); do
		if ! kill -0 "$USBIP_FIXTURE_PID" 2>/dev/null; then
			cat "$ARTIFACTS/diagnostics/usbip-fixture.log" >&2
			exit 1
		fi
		if (exec 3<>/dev/tcp/127.0.0.1/3240) 2>/dev/null && (exec 3<>/dev/tcp/127.0.0.1/3241) 2>/dev/null; then
			fixture_ready=1
			break
		fi
		sleep 0.1
	done
	if ! kill -0 "$USBIP_FIXTURE_PID" 2>/dev/null; then
		cat "$ARTIFACTS/diagnostics/usbip-fixture.log" >&2
		exit 1
	fi
	if ((fixture_ready == 0)); then
		echo "USB/IP fixture did not listen on 127.0.0.1 ports 3240 and 3241" >&2
		exit 1
	fi
	export CODESPACE_E2E_USBIP=1
fi

echo "Checking Kubernetes prerequisites"
"$KUBECTL_BIN" version >/dev/null
"$KUBECTL_BIN" get "storageclass/$CODESPACE_E2E_STORAGE_CLASS" >/dev/null
if [[ $CODESPACE_E2E_RUNTIME_ISOLATION != native ]]; then
	"$KUBECTL_BIN" get "runtimeclass/$CODESPACE_E2E_RUNTIME_CLASS" >/dev/null
	"$KUBECTL_BIN" label "runtimeclass/$CODESPACE_E2E_RUNTIME_CLASS" \
		"codespace.gitea.dev/isolation=$CODESPACE_E2E_RUNTIME_ISOLATION" --overwrite >/dev/null
	if [[ $CODESPACE_E2E_RUNTIME_ISOLATION == kata && $CODESPACE_E2E_USBIP_RUNTIME_CLASS != "$CODESPACE_E2E_RUNTIME_CLASS" ]]; then
		"$KUBECTL_BIN" get "runtimeclass/$CODESPACE_E2E_USBIP_RUNTIME_CLASS" >/dev/null
		"$KUBECTL_BIN" label "runtimeclass/$CODESPACE_E2E_USBIP_RUNTIME_CLASS" \
			codespace.gitea.dev/isolation=kata --overwrite >/dev/null
	fi
fi

if [[ -n ${CODESPACE_E2E_WEB_IDE_ARCHIVE:-} ]]; then
	echo "Importing the fake Web IDE image"
	sudo k3s ctr --namespace k8s.io images import "$CODESPACE_E2E_WEB_IDE_ARCHIVE"
	web_ide_digest=$(sudo k3s ctr --namespace k8s.io images list | awk -v ref="$CODESPACE_E2E_WEB_IDE_IMAGE" '$1 == ref {print $3}')
	case "$web_ide_digest" in
	sha256:????????????????????????????????????????????????????????????????) ;;
	*)
		echo "imported fake Web IDE image has no sha256 digest" >&2
		exit 1
		;;
	esac
	web_ide_repository=${CODESPACE_E2E_WEB_IDE_IMAGE%:*}
	sudo k3s ctr --namespace k8s.io images tag --local --force "$CODESPACE_E2E_WEB_IDE_IMAGE" "$web_ide_repository@$web_ide_digest"
fi

echo "Building Runtime test binaries"
"$GO_BIN" test -c -tags netgo,osusergo -o "$ARTIFACTS/bin/agent.test" ./internal/agent
CGO_ENABLED=0 "$GO_BIN" test -c -o "$ARTIFACTS/bin/devcontainer.test" ./devcontainer/docker

if [[ $CODESPACE_E2E_RUNTIME_ISOLATION == native ]]; then
	echo "Registering the E2E FUSE Device Plugin"
	sudo modprobe fuse
	test -c /dev/fuse
	CGO_ENABLED=0 "$GO_BIN" build -o "$ARTIFACTS/bin/device-plugin" ./tests/e2e/device-plugin
	sudo install -d -m 0755 /var/run/cdi
	sudo tee "$DEVICE_PLUGIN_CDI_SPEC" >/dev/null <<'EOF'
cdiVersion: 0.8.0
kind: e2e.codespace.gitea.dev/fuse
devices:
  - name: fuse0
    containerEdits:
      env:
        - CODESPACE_E2E_CDI=fuse0
      deviceNodes:
        - path: /dev/fuse
          hostPath: /dev/fuse
          permissions: rwm
  - name: fuse1
    containerEdits:
      env:
        - CODESPACE_E2E_CDI=fuse1
      deviceNodes:
        - path: /dev/fuse
          hostPath: /dev/fuse
          permissions: rwm
EOF
	sudo "$ARTIFACTS/bin/device-plugin" >"$ARTIFACTS/diagnostics/device-plugin.log" 2>&1 &
	DEVICE_PLUGIN_PID=$!
	for ((attempt = 0; attempt < 120; attempt++)); do
		allocatable=$("$KUBECTL_BIN" get node -o jsonpath='{.items[0].status.allocatable.e2e\.codespace\.gitea\.dev/fuse}' 2>/dev/null || true)
		if [[ $allocatable == 2 ]]; then
			break
		fi
		if ! sudo kill -0 "$DEVICE_PLUGIN_PID" 2>/dev/null; then
			cat "$ARTIFACTS/diagnostics/device-plugin.log" >&2
			exit 1
		fi
		sleep 1
	done
	if [[ ${allocatable:-} != 2 ]]; then
		echo "E2E FUSE Device Plugin did not publish its resource" >&2
		exit 1
	fi
fi

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
	--set-string admin.tokenSecret.name=codespace-admin \
	--set replicaCount="${CODESPACE_E2E_MANAGER_REPLICAS:-2}" \
	--wait --timeout 5m
"$KUBECTL_BIN" rollout status -n "$MANAGEMENT_NAMESPACE" "deployment/$MANAGER_NAME" --timeout=5m
for resource in \
	customresourcedefinition/giteasites.codespace.gitea.dev \
	customresourcedefinition/environmenttemplates.codespace.gitea.dev \
	customresourcedefinition/codespacecomponents.codespace.gitea.dev \
	customresourcedefinition/codespaces.codespace.gitea.dev; do
	"$KUBECTL_BIN" get "$resource" >/dev/null
done

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
run_cluster_tests "Runtime Agent process model" '^TestKubernetesE2EAgentProcesses$' 6m
run_cluster_tests "Runtime, Docker persistence, and Dev Container" '^TestKubernetesE2EDockerPersistence$' 80m

echo "Deploying S3Mock"
"$KUBECTL_BIN" create deployment s3mock -n "$MANAGEMENT_NAMESPACE" --image="$S3MOCK_IMAGE" --dry-run=client -o yaml |
	"$KUBECTL_BIN" set env --local -f - COM_ADOBE_TESTING_S3MOCK_STORE_INITIAL_BUCKETS=codespace-e2e -o yaml |
	"$KUBECTL_BIN" set resources --local -f - --requests=cpu=50m,memory=128Mi --limits=cpu=1,memory=512Mi -o yaml |
	"$KUBECTL_BIN" patch --local -f - --type=strategic --patch \
		'{"spec":{"template":{"spec":{"containers":[{"name":"s3mock","readinessProbe":{"tcpSocket":{"port":9090},"periodSeconds":1,"failureThreshold":120}}]}}}}' -o yaml |
	"$KUBECTL_BIN" apply -f -
"$KUBECTL_BIN" expose deployment s3mock -n "$MANAGEMENT_NAMESPACE" --name=s3mock --port=9090 --target-port=9090
"$KUBECTL_BIN" rollout status deployment/s3mock -n "$MANAGEMENT_NAMESPACE" --timeout=5m

port_forward_log="$ARTIFACTS/diagnostics/s3mock-port-forward.log"
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
