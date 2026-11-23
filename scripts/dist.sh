#!/usr/bin/env bash
# Build release archives into dist/: scripts/dist.sh <version without v>
#
# Two programs, one archive each per target:
#   gnss_ptp_exporter-<ver>.linux-<arch>.tar.gz    (layout unchanged since 0.1.0;
#     installers extract gnss_ptp_exporter-<ver>.linux-<arch>/gnss_ptp_exporter)
#   gnss_ptp_clockclass-<ver>.linux-<arch>.tar.gz
set -euo pipefail
version="${1:?version}"
cd "$(dirname "$0")/.."
rm -rf dist && mkdir dist
export CGO_ENABLED=0
# Reproducible: no paths, no VCS stamping beyond the version.
ldflags="-s -w -X main.version=${version}"
mtime="@$(git log -1 --format=%ct)"

# build <program> <package> <os> <arch> <out dir>
build() {
  local prog="$1" pkg="$2" os="$3" arch="$4" out="$5"
  if [[ "$arch" == armv7 ]]; then
    GOOS="$os" GOARCH=arm GOARM=7 go build -trimpath -ldflags "$ldflags" -o "${out}/${prog}" "$pkg"
  else
    GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "$ldflags" -o "${out}/${prog}" "$pkg"
  fi
}

pack() {
  tar -C dist --owner=0 --group=0 --numeric-owner --sort=name \
    --mtime="$mtime" -czf "dist/$1.tar.gz" "$1"
  rm -rf "dist/$1"
}

for target in linux/amd64 linux/arm64 linux/armv7; do
  os="${target%%/*}"; arch="${target#*/}"

  name="gnss_ptp_exporter-${version}.${os}-${arch}"
  mkdir -p "dist/${name}/contrib"
  build gnss_ptp_exporter . "$os" "$arch" "dist/${name}"
  cp LICENSE README.md CHANGELOG.md "dist/${name}/"
  cp -r contrib/systemd "dist/${name}/contrib/"
  pack "$name"

  name="gnss_ptp_clockclass-${version}.${os}-${arch}"
  mkdir -p "dist/${name}/contrib"
  build gnss_ptp_clockclass ./cmd/gnss_ptp_clockclass "$os" "$arch" "dist/${name}"
  cp LICENSE CHANGELOG.md "dist/${name}/"
  cp docs/gnss_ptp_clockclass.md "dist/${name}/README.md"
  cp -r contrib/gnss_ptp_clockclass "dist/${name}/contrib/"
  pack "$name"
done
ls -l dist
