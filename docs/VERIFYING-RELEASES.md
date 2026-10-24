# Verifying releases

[Product overview](../README.md) · [Security](../SECURITY.md) · [Operations](OPERATIONS.md)

A KyPost release is the container image `ghcr.io/busnes-app/kypost-server:X.Y.Z` (or `X.Y.Z.N`
for a dependency-only release), built for `linux/amd64`. This page shows how to check that an
image was built by this repository's release workflow from the tagged commit, how to read its
software bill of materials, and how to rebuild its server binary yourself and get the same bytes.

The push relays (`worker/`, `worker-apns/`) and the receiving Worker are not released as
artifacts: operators deploy them from source with `wrangler`, so there is nothing to verify
beyond the commit you deploy.

## What is signed, and by whom

- `release-image.yml` builds each release from its tag and publishes a SLSA build-provenance
  attestation for the image digest through GitHub's Sigstore integration. It verifies that
  attestation before it moves `:stable`, so `:stable` never points at an unattested image. It
  also refuses to build a tag whose commit did not pass every `ci.yml` job.
- `:main` is built from main's tip by `publish-main.yml` and attested with that workflow's
  identity; [Operations](OPERATIONS.md#verifying-an-image-by-hand) has the command for it.
- There is no long-lived signing key to download. The signature is tied to a workflow identity,
  and that identity is the thing you check:
  `https://github.com/Busnes-app/KyPost-Server/.github/workflows/release-image.yml@refs/tags/v<version>`.
  `gh` fetches Sigstore's public trust root itself.
- Each image also carries an SPDX software bill of materials made by BuildKit. The SBOM sits in
  the image index and is **not** separately signed; trust it as far as you trust the digest you
  verified.

`scripts/update-host.sh` runs an attestation check of its own before it deploys anything.

## Verify a release image

You need `gh` (logged in) and `docker` with buildx. Name the version yourself, and verify the
version tag, not `:stable`: tags can move, so the check resolves the digest first and then requires
the attestation to name the commit the release tag points at.

```bash
v=<release version, e.g. 0.4.3>
sha=$(gh api repos/Busnes-app/KyPost-Server/commits/refs/tags/v$v --jq .sha) \
  && d=$(docker buildx imagetools inspect ghcr.io/busnes-app/kypost-server:$v --format '{{.Manifest.Digest}}') \
  && gh attestation verify "oci://ghcr.io/busnes-app/kypost-server@$d" --repo Busnes-app/KyPost-Server \
       --cert-identity https://github.com/Busnes-app/KyPost-Server/.github/workflows/release-image.yml@refs/tags/v$v \
  && [ "$(gh attestation verify "oci://ghcr.io/busnes-app/kypost-server@$d" --repo Busnes-app/KyPost-Server \
       --cert-identity https://github.com/Busnes-app/KyPost-Server/.github/workflows/release-image.yml@refs/tags/v$v \
       --format json --jq '.[0].verificationResult.statement.predicate.buildDefinition.resolvedDependencies[0].digest.gitCommit')" = "$sha" ] \
  && echo "OK: ghcr.io/busnes-app/kypost-server@$d is release v$v, built from $sha"
```

Then run that digest, not the tag. [Operations](OPERATIONS.md#verifying-an-image-by-hand) shows
how to start a verified digest with Compose without `KYPOST_IMAGE` or `KYPOST_VERSION` steering it
somewhere else.

Any failure means: do not run that image. A different workflow, a branch instead of the tag, or
another repository all fail the `--cert-identity` or `--repo` check.

To read the SBOM:

```bash
docker buildx imagetools inspect ghcr.io/busnes-app/kypost-server@$d --format '{{ json .SBOM }}'
```

## Rebuild the binary and compare

The Dockerfile builds `kypost-server` with `-trimpath` and `CGO_ENABLED=0`, and the build context
has no `.git`, so the same commit, Go version and version string give the same binary on any
machine and in any directory. CI checks this on every push (`release binary is repeatable` in
`ci-backend-api`). This was checked byte for byte against the 0.4.3 image; earlier releases
were not checked.

```bash
v=<release version>
git clone --branch v$v https://github.com/Busnes-app/KyPost-Server.git kypost-v$v && cd kypost-v$v
go_version=$(sed -n 's/^FROM golang:\([0-9.]*\)@.*/\1/p' Dockerfile)
(cd backend && GOTOOLCHAIN=go$go_version CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -buildvcs=false -trimpath \
  -ldflags="-X github.com/Busnes-app/kypost-server/backend/internal/appversion.release=$v" \
  -o ../kypost-from-source ./cmd/main.go)
cid=$(docker create ghcr.io/busnes-app/kypost-server@$d) \
  && docker cp "$cid:/usr/local/bin/kypost-server" kypost-from-image && docker rm "$cid" >/dev/null
sha256sum kypost-from-source kypost-from-image
```

The two hashes must match. `docker create` pulls the whole image (about 1.9 GB with Ollama);
`podman create` and `podman cp` work the same way. Only the server binary is rebuilt bit for bit:
the frontend bundle, the Debian base with its `apt-get` packages, and the Ollama tarball (pinned
by SHA-256 in the Dockerfile) are not.

`-buildvcs=false` is needed only because a clone has a `.git` directory; the image build context
excludes `.git` (`.dockerignore`), so Go stamps nothing there.
