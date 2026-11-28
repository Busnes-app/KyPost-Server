#!/usr/bin/env bash
# Starts the disposable S3-compatible fixture (SeaweedFS, pinned as KyDrive pins it) on
# loopback, creates its bucket and exports KY_TEST_S3_* for later CI steps. Its credentials
# are not real.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
url=http://127.0.0.1:8333/kypost-ci
auth=(--aws-sigv4 aws:amz:us-east-1:s3 --user kypost-ci:kypost-ci-secret-not-a-real-credential)
docker run -d --name kypost-s3 -p 127.0.0.1:8333:8333 -v "$PWD/.github/ci/seaweedfs-s3.json:/etc/s3.json:ro" \
  chrislusf/seaweedfs:4.52@sha256:57877e32bb90073b37dc109b438ba66e84fd5ed9ced5f5c04f56444ef243ce59 \
  server -dir=/data -s3 -s3.port=8333 -s3.config=/etc/s3.json >/dev/null
# Create the bucket through the S3 API (never rely on SeaweedFS creating it on first write),
# and be ready only once a signed HeadBucket answers 200.
for _ in $(seq 60); do
  curl -s -o /dev/null -X PUT "${auth[@]}" "$url" || true
  if [ "$(curl -s -o /dev/null -w '%{http_code}' -I "${auth[@]}" "$url")" = 200 ]; then
    {
      echo KY_TEST_S3_ENDPOINT=http://127.0.0.1:8333
      echo KY_TEST_S3_BUCKET=kypost-ci
      echo KY_TEST_S3_ACCESS_KEY=kypost-ci
      echo KY_TEST_S3_SECRET_KEY=kypost-ci-secret-not-a-real-credential
    } >> "${GITHUB_ENV:?}"
    exit 0
  fi
  sleep 1
done
docker logs kypost-s3
exit 1
