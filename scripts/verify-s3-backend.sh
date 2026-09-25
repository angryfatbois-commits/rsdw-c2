#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

if [[ -z "${RSDW_S3_TEST_ENDPOINT:-}" ]]; then
  printf '%s\n' 'RSDW_S3_TEST_ENDPOINT is not set; skipping the live S3 backend smoke test.'
  printf '%s\n' 'Set RSDW_S3_TEST_ENDPOINT, RSDW_S3_TEST_BUCKET, RSDW_S3_TEST_ACCESS_KEY_ID, and RSDW_S3_TEST_SECRET_ACCESS_KEY to run it against a real bucket.'
  exit 0
fi

go test -run '^TestS3BackupRepositoryLiveSmoke$' -v ./...
printf '%s\n' 'Live S3 backend smoke test passed'
