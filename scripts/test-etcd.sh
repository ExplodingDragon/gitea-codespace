#!/usr/bin/env bash
set -euo pipefail

mode="${1:-single}"
go_cmd="${GO:-go}"
etcd_cmd="${ETCD:-etcd}"
base_dir=".tmp/etcd-${mode}"

if ! command -v "${etcd_cmd}" >/dev/null 2>&1; then
	echo "etcd binary not found: ${etcd_cmd}" >&2
	exit 127
fi

rm -rf "${base_dir}"
mkdir -p "${base_dir}"

pids=()
cleanup() {
	for pid in "${pids[@]:-}"; do
		kill "${pid}" >/dev/null 2>&1 || true
	done
	wait >/dev/null 2>&1 || true
	rm -rf "${base_dir}"
}
trap cleanup EXIT

start_node() {
	local name="$1"
	local client_port="$2"
	local peer_port="$3"
	local cluster="$4"

	"${etcd_cmd}" \
		--name "${name}" \
		--data-dir "${base_dir}/${name}" \
		--listen-client-urls "http://127.0.0.1:${client_port}" \
		--advertise-client-urls "http://127.0.0.1:${client_port}" \
		--listen-peer-urls "http://127.0.0.1:${peer_port}" \
		--initial-advertise-peer-urls "http://127.0.0.1:${peer_port}" \
		--initial-cluster "${cluster}" \
		--initial-cluster-state new \
		--logger zap \
		> "${base_dir}/${name}.log" 2>&1 &
	pids+=("$!")
}

wait_health() {
	local url="$1"
	for _ in $(seq 1 100); do
		if curl -fsS "${url}/health" >/dev/null 2>&1; then
			return 0
		fi
		sleep 0.1
	done
	echo "etcd did not become healthy at ${url}" >&2
	for log in "${base_dir}"/*.log; do
		echo "==== ${log} ====" >&2
		tail -100 "${log}" >&2 || true
	done
	return 1
}

case "${mode}" in
single)
	cluster="node1=http://127.0.0.1:12380"
	start_node node1 12379 12380 "${cluster}"
	wait_health "http://127.0.0.1:12379"
	CODESPACE_TEST_ETCD_ENDPOINTS="http://127.0.0.1:12379" \
		"${go_cmd}" test -count=1 -run '^TestEtcdInfrastructureStateSingleNode$' ./internal/app
	;;
cluster)
	cluster="node1=http://127.0.0.1:12380,node2=http://127.0.0.1:22380,node3=http://127.0.0.1:32380"
	start_node node1 12379 12380 "${cluster}"
	start_node node2 22379 22380 "${cluster}"
	start_node node3 32379 32380 "${cluster}"
	wait_health "http://127.0.0.1:12379"
	wait_health "http://127.0.0.1:22379"
	wait_health "http://127.0.0.1:32379"
	CODESPACE_TEST_ETCD_CLUSTER_ENDPOINTS="http://127.0.0.1:12379,http://127.0.0.1:22379,http://127.0.0.1:32379" \
		"${go_cmd}" test -count=1 -run '^TestEtcdInfrastructureStateClusterEndpoints$' ./internal/app
	;;
*)
	echo "usage: $0 single|cluster" >&2
	exit 2
	;;
esac
