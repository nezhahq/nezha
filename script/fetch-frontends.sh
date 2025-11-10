#!/bin/bash

set -euo pipefail

ROOT_DIR="$(realpath "$(dirname "${BASH_SOURCE[0]}")/..")"
TEMPLATES_FILE="$ROOT_DIR/service/singleton/frontend-templates.yaml"

download_and_extract() {
  local repository="$1"
  local version="$2"
  local targetDir="$3"
  local TMP_DIR

  TMP_DIR="$(mktemp -d)"

  echo "Downloading from repository: $repository, version: $version"

  pushd "$TMP_DIR" || exit

  curl -L -o "dist.zip" "$repository/releases/download/$version/dist.zip"

  [ -e "$targetDir" ] && rm -r "$targetDir"
  unzip -q dist.zip
  mv dist "$targetDir"

  rm "dist.zip"
  popd || exit
}

count=$(yq eval '. | length' "$TEMPLATES_FILE")

for i in $(seq 0 $(("$count"-1))); do
  path=$(yq -r ".[$i].path" "$TEMPLATES_FILE")
  repository=$(yq -r ".[$i].repository" "$TEMPLATES_FILE")
  version=$(yq -r ".[$i].version" "$TEMPLATES_FILE")

  if [[ -n $path && -n $repository && -n $version ]]; then
    download_and_extract "$repository" "$version" "$ROOT_DIR/cmd/dashboard/$path"
  fi
done
