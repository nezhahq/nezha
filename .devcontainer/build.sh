#!/bin/bash
goreleaser build --snapshot --clean
mv dist/**/* dist/
docker buildx build --platform linux/amd64,linux/arm64,linux/s390x -t nezha .
