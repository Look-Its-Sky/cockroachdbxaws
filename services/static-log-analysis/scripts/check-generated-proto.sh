#!/bin/sh
set -eu

# Tool versions are part of the reproducibility contract. The checked archive
# name pins protoc and go install pins protoc-gen-go.
protoc_version=21.12
protoc_gen_go_version=v1.36.11
module=github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis

service_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT HUP INT TERM

case "$(uname -s)-$(uname -m)" in
  Linux-x86_64)
    archive=protoc-${protoc_version}-linux-x86_64.zip
    archive_sha256=3a4c1e5f2516c639d3079b1586e703fc7bcfa2136d58bda24d1d54f949c315e8
    ;;
  *) echo "unsupported proto-check platform" >&2; exit 2 ;;
esac

curl --fail --location --silent --show-error \
  "https://github.com/protocolbuffers/protobuf/releases/download/v${protoc_version}/${archive}" \
  --output "$work_dir/protoc.zip"
printf '%s  %s\n' "$archive_sha256" "$work_dir/protoc.zip" | sha256sum -c - >/dev/null
python3 -m zipfile -e "$work_dir/protoc.zip" "$work_dir/protoc"
chmod +x "$work_dir/protoc/bin/protoc"
GOBIN="$work_dir/bin" go install "google.golang.org/protobuf/cmd/protoc-gen-go@${protoc_gen_go_version}"

mkdir -p "$work_dir/generated"
cd "$service_dir"
PATH="$work_dir/bin:$PATH" "$work_dir/protoc/bin/protoc" \
  -I . -I "$work_dir/protoc/include" \
  --go_out="$work_dir/generated" --go_opt="module=${module}" \
  api/proto/internal/v1/normalized_log.proto

generated="$work_dir/generated/internal/gen/internalv1/normalized_log.pb.go"
checked_in=internal/gen/internalv1/normalized_log.pb.go
if [ "${UPDATE_GENERATED:-}" = "1" ]; then
  cp "$generated" "$checked_in"
else
  cmp "$generated" "$checked_in"
fi
