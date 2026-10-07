#!/usr/bin/env bash
# Writes the krew manifest for a release to stdout, from the plugin archives
# in the current directory, which the release workflow builds.
#
#   hack/krew-manifest.sh v0.2.0 okedeji/hybernate > hybernate.yaml
set -euo pipefail

tag=$1
repo=$2
platforms=(linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64)

cat <<EOF
apiVersion: krew.googlecontainertools.github.com/v1alpha2
kind: Plugin
metadata:
  name: hybernate
spec:
  version: ${tag}
  homepage: https://github.com/${repo}
  shortDescription: Find idle workloads, and pause and wake them
  description: |
    Hybernate pauses idle Kubernetes workloads and wakes them when they're
    used. This plugin works with it from the command line:

      scan     finds idle Deployments and StatefulSets and what they cost,
               without Hybernate installed
      status   shows what Hybernate is pausing, waking and saving
      enable   ends dry-run, so Hybernate pauses a workload it's measuring
      pause    pauses a workload now, to wake on its next request
      wake     wakes a paused workload and waits until it's ready
      deps     shows what a workload depends on and what depends on it
  platforms:
EOF

for platform in "${platforms[@]}"; do
  os=${platform%/*}
  arch=${platform#*/}
  ext=""
  if [ "$os" = windows ]; then
    ext=.exe
  fi
  archive=kubectl-hybernate-${os}-${arch}.tar.gz
  sha=$(sha256sum "$archive" | cut -d' ' -f1)
  cat <<EOF
    - selector:
        matchLabels:
          os: ${os}
          arch: ${arch}
      uri: https://github.com/${repo}/releases/download/${tag}/${archive}
      sha256: "${sha}"
      files:
        - from: kubectl-hybernate-${os}-${arch}${ext}
          to: kubectl-hybernate${ext}
        - from: LICENSE
          to: .
      bin: kubectl-hybernate${ext}
EOF
done
