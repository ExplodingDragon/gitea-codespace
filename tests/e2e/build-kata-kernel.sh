#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

verify_kernel_config() {
	local config=$1 setting expected
	while IFS= read -r setting; do
		[[ -z "$setting" || $setting == \#* ]] && continue
		expected=$setting
		if [[ $setting == *=n ]]; then
			expected="# ${setting%=n} is not set"
		fi
		if ! grep -Fxq -- "$expected" "$config"; then
			echo "required kernel config is missing: $setting" >&2
			exit 1
		fi
	done <"$SCRIPT_DIR/kata-usbip.config"
}

verify_kernel_bundle() {
	local output=$1 file
	for file in vmlinuz config release metadata sha256sums; do
		if [[ ! -s $output/$file ]]; then
			echo "required kernel artifact is missing: $output/$file" >&2
			exit 1
		fi
	done
	(cd "$output" && sha256sum --check sha256sums)
	verify_kernel_config "$output/config"
}

if [[ ${1:-} == --verify ]]; then
	verify_kernel_bundle "${2:?kernel output directory is required}"
	exit 0
fi

ARCH=${ARCH:-x86_64}
if [[ $ARCH != x86_64 ]]; then
	echo "only the amd64 Kata guest kernel is supported" >&2
	exit 1
fi
KERNEL_ARCH=x86
OUTPUT_DIR=${1:?output directory is required}
source "$SCRIPT_DIR/kata-kernel.version"
KATA_VERSION=${KATA_VERSION:?KATA_VERSION is required}
KERNEL_VERSION=${KERNEL_VERSION:?KERNEL_VERSION is required}
BUILD_CACHE_DIR=${KATA_BUILD_DIR:-$HOME/.cache/codespace-kata-kernel}
SOURCE_ARCHIVE="$BUILD_CACHE_DIR/kata-containers-${KATA_VERSION}.tar.gz"
WORK_DIR="$BUILD_CACHE_DIR/kata-kernel-${KATA_VERSION}-${ARCH}"
kernel_source="$WORK_DIR/linux-source-${KERNEL_VERSION}"
source_archive_tmp=

for command_name in curl tar make sha256sum; do
	if ! command -v "$command_name" >/dev/null 2>&1; then
		echo "required command is unavailable: $command_name" >&2
		exit 1
	fi
done

if [[ ! $KERNEL_VERSION =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
	echo "invalid Kata kernel version: $KERNEL_VERSION" >&2
	exit 1
fi

mkdir -p "$BUILD_CACHE_DIR" "$OUTPUT_DIR" "$WORK_DIR"
if [[ ! -s "$SOURCE_ARCHIVE" ]]; then
	source_archive_tmp=$(mktemp "$SOURCE_ARCHIVE.XXXXXX")
	trap 'if [[ -n ${source_archive_tmp:-} ]]; then rm -f "$source_archive_tmp"; fi' EXIT
	curl -fsSL "https://github.com/kata-containers/kata-containers/archive/refs/tags/${KATA_VERSION}.tar.gz" -o "$source_archive_tmp"
	mv "$source_archive_tmp" "$SOURCE_ARCHIVE"
	source_archive_tmp=
	trap - EXIT
fi

source_root="$WORK_DIR/kata-containers-${KATA_VERSION}"
if [[ ! -s "$source_root/versions.yaml" || ! -x "$source_root/tools/packaging/kernel/build-kernel.sh" ]]; then
	rm -rf "$source_root"
	tar -xzf "$SOURCE_ARCHIVE" -C "$WORK_DIR"
fi

versions_file="$source_root/versions.yaml"
expected_kernel_version=$(awk '
	$0 == "  kernel:" { in_kernel = 1; next }
	in_kernel && /^[^ ]/ { exit }
	in_kernel && $1 == "version:" { gsub(/"/, "", $2); print $2; exit }
' "$versions_file")
expected_kernel_version=${expected_kernel_version#v}
if [[ $expected_kernel_version != "$KERNEL_VERSION" ]]; then
	echo "pinned kernel version $KERNEL_VERSION differs from Kata $KATA_VERSION versions.yaml ($expected_kernel_version)" >&2
	exit 1
fi

if [[ ! -s "$kernel_source/.config" ]]; then
	rm -rf "$kernel_source" "$WORK_DIR/linux-${KERNEL_VERSION}"
	pushd "$WORK_DIR" >/dev/null
	"$source_root/tools/packaging/kernel/build-kernel.sh" \
		-a "$ARCH" -t qemu -v "$KERNEL_VERSION" -f -k "$kernel_source" setup
	popd >/dev/null
fi

if [[ ! -x "$kernel_source/scripts/config" ]]; then
	make -C "$kernel_source" scripts
fi
config_args=()
while IFS= read -r config_line; do
	[[ -z "$config_line" || $config_line == \#* ]] && continue
	symbol=${config_line#CONFIG_}
	symbol=${symbol%%=*}
	want=${config_line#*=}
	case "$want" in
	y) config_args+=(--enable "$symbol") ;;
	m) config_args+=(--module "$symbol") ;;
	n) config_args+=(--disable "$symbol") ;;
	*)
		echo "unsupported kernel config value: $config_line" >&2
		exit 1
		;;
	esac
done <"$SCRIPT_DIR/kata-usbip.config"
"$kernel_source/scripts/config" --file "$kernel_source/.config" "${config_args[@]}"
make -C "$kernel_source" ARCH="$KERNEL_ARCH" olddefconfig

verify_kernel_config "$kernel_source/.config"

make -C "$kernel_source" -j "${KATA_BUILD_JOBS:-$(nproc)}" ARCH="$KERNEL_ARCH" vmlinux bzImage

install -m 0644 "$kernel_source/arch/x86/boot/bzImage" "$OUTPUT_DIR/vmlinuz"
install -m 0644 "$kernel_source/.config" "$OUTPUT_DIR/config"
kernel_release=$(make -s -C "$kernel_source" ARCH="$KERNEL_ARCH" kernelrelease)
printf '%s\n' "$kernel_release" >"$OUTPUT_DIR/release"
printf 'kata_version=%s\nkernel_version=%s\narch=%s\nkernel_release=%s\n' \
	"$KATA_VERSION" "$KERNEL_VERSION" "$ARCH" "$kernel_release" >"$OUTPUT_DIR/metadata"
(cd "$OUTPUT_DIR" && sha256sum vmlinuz config release metadata >sha256sums)
verify_kernel_bundle "$OUTPUT_DIR"
