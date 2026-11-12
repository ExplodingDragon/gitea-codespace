# Kubernetes E2E tests

The suite runs real Manager, Gateway, Cache, Agent, storage, and Dev Container
workloads against a small Gitea protocol fixture. A deterministic Web IDE image
tests the provider contract independently of editor releases. CI runs Kata,
Sysbox, and Native variants on fresh single-node k3s workers.

Use the [development guide](../../DEVELOPMENT.md) for ordinary non-E2E checks.
Run the commands below from the repository root.

## Prepare a worker

Use a dedicated Linux k3s test worker with the selected runtime and StorageClass
installed. The harness imports images through local k3s containerd and, for
Native tests, registers a local Device Plugin. `KUBECONFIG` selects Kubernetes
API access; those host operations still run on the machine invoking the harness.

Complete the repository's development setup and install Docker, kubectl, and
Helm. The test process needs sudo access to k3s and, for Native tests, the
worker's device-plugin directories. Prepare runtime and storage according to
the [operator guide](../../charts/gitea-codespace/README.md#choose-a-workspace-runtime).
Kata also needs the USB/IP preparation below.

## Build and run

Build the platform and fake Web IDE. Import the platform image and select its
digest so test Pods run the exact build under review:

```sh
make images e2e-web-ide-image

docker save gitea-codespace:local -o .tmp/platform-image.tar
sudo k3s ctr --namespace k8s.io images import .tmp/platform-image.tar
image_tag=docker.io/library/gitea-codespace:local
image_digest=$(sudo k3s ctr --namespace k8s.io images list | awk -v ref="$image_tag" '$1 == ref {print $3}')
image_reference="${image_tag%:*}@$image_digest"
sudo k3s ctr --namespace k8s.io images tag --local --force "$image_tag" "$image_reference"
```

Set the kubeconfig and addresses, replacing the placeholders for this worker:

```sh
export KUBECONFIG=/path/to/test-kubeconfig
export CODESPACE_E2E_GITEA_LISTEN='<host-and-port-reachable-from-the-cluster>'
export CODESPACE_E2E_NODE_ADDRESS='<worker-address-reachable-from-the-test-process>'

make test-e2e \
  CODESPACE_IMAGE="$image_reference" \
  KUBERNETES_RUNTIME_ISOLATION=sysbox \
  KUBERNETES_RUNTIME_CLASS=sysbox-runc \
  KUBERNETES_STORAGE_CLASS='<storage-class>'
```

The example selects Sysbox. For Kata, use `KUBERNETES_RUNTIME_ISOLATION=kata`
and the installed Kata RuntimeClass. For Native, use
`KUBERNETES_RUNTIME_ISOLATION=native` and `KUBERNETES_RUNTIME_CLASS=` because
that mode uses the node's default CRI runtime. All variants need a compatible
StorageClass. The harness imports the fake Web IDE image built by the first
command.

## Prepare Kata USB/IP tests

Use a Kata guest kernel with the [required USB/IP capabilities](../../charts/gitea-codespace/README.md#enable-kata-usbip).
The test uses a virtual CDC ACM serial device, so the guest also needs that
driver. Install Rust with Cargo and the libusb development package on the test
worker, then set:

```sh
export CODESPACE_E2E_USBIP_RUNTIME_CLASS='<usbip-runtime-class>'
export CODESPACE_E2E_USBIP_KERNEL_RELEASE='<guest-kernel-release>'
```

The harness builds the pinned virtual-device fixture with its lockfile and
starts and stops it for the run. Set `CODESPACE_E2E_USBIP_FIXTURE` to an existing
fixture executable to reuse a local build.

On an amd64 worker with the kernel build dependencies installed, build and
verify the same guest kernel used by CI:

```sh
tests/e2e/build-kata-kernel.sh .tmp/kata-kernel
tests/e2e/build-kata-kernel.sh --verify .tmp/kata-kernel
```

See [`ci.yml`](../../.github/workflows/ci.yml) for the dependency installation
commands and cache configuration, and [`setup-k3s.sh`](setup-k3s.sh) for the
separate USB/IP RuntimeClass on disposable workers. Existing clusters need the
resulting kernel installed and selected by their Kata handler before running
the test.

## Coverage and validation

USB/IP tests use two servers exporting the same bus ID. The real Gateway and
Agent handle source selection, SSH cancellation, reconnection, and stop/start;
the Dev Container verifies hotplug, udev permissions, and non-root serial I/O.
All runtime variants also exercise ordinary SSH reverse forwarding into the
Dev Container. These checks distinguish device visibility from usable access.

The kernel cache key includes the runner OS, architecture, version file,
USB/IP configuration, and build script. Fresh and cached artifacts pass the
same checksum and configuration checks, while the product tests validate the
running guest. Artifact integrity and runtime behavior are separate checks.

### Implementation acceptance criteria

- Reusing an imported image succeeds and Pods run the selected digest.
- Ordinary reverse forwarding reaches the Dev Container, and independent
  USB/IP sources can list, attach, transfer data, and detach.
- A wrong kernel, ineffective device permissions, or broken forwarding fails
  the Kata product test with diagnostics.
- Fresh kernel builds and cache hits pass the same artifact validation and
  real device tests.
- Test resources carry the run label from creation so cleanup can find them
  after a partial test failure.

## Diagnostics and cleanup

The harness writes artifacts under `.tmp/e2e/<run-id>` by default; set
`CODESPACE_E2E_ARTIFACTS` to choose another location. Inspect the test output,
Pod events and logs, and worker diagnostics before attributing a failure to
product behavior. Registry connectivity and runtime installation are test
prerequisites.

On exit, the harness removes the resources it created and stops its local
fixture and port-forward processes. The cluster and its runtime/storage
installation remain available for another run. After an interrupted process,
identify residual resources by the `codespace.gitea.dev/e2e-run` label and
the run ID before cleaning them up.
