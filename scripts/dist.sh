#!/usr/bin/env bash
# Build release archives into dist/: scripts/dist.sh <version without v>
set -euo pipefail
version="${1:?version}"
cd "$(dirname "$0")/.."
rm -rf dist && mkdir dist
export CGO_ENABLED=0
# Reproducible: no paths, no VCS stamping beyond the version.
ldflags="-s -w -X main.version=${version}"
for target in linux/amd64 linux/arm64 linux/armv7; do
  os="${target%%/*}"; arch="${target#*/}"
  name="gnss_ptp_exporter-${version}.${os}-${arch}"
  mkdir -p "dist/${name}"
  if [[ "$arch" == armv7 ]]; then
    GOOS="$os" GOARCH=arm GOARM=7 go build -trimpath -ldflags "$ldflags" -o "dist/${name}/gnss_ptp_exporter" .
  else
    GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "$ldflags" -o "dist/${name}/gnss_ptp_exporter" .
  fi
  cp LICENSE README.md CHANGELOG.md "dist/${name}/"
  cp -r contrib "dist/${name}/"
  tar -C dist --owner=0 --group=0 --numeric-owner --sort=name \
    --mtime="@$(git log -1 --format=%ct)" -czf "dist/${name}.tar.gz" "${name}"
  rm -rf "dist/${name}"
done
ls -l dist
