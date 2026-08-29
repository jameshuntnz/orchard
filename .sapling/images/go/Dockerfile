# Go toolchain for the Sapling node (Apple silicon, arm64).
#
# Selected by `runs-on: [self-hosted, linux, arm64, image:go]`.
#
# Deliberately small — this repo builds one static binary. Two things the bare
# runner agent lacks, both of which ubuntu-latest ships preinstalled, so nothing
# in a normal workflow ever has to ask for them:
#
#   - Go itself. Baked in so `setup-go` has nothing to download: a Linux job is
#     ephemeral and starts with an empty tool cache every single run.
#   - A C compiler. `go test -race` links the race runtime through cgo, so on
#     arm64 the race detector does not run at all without one — and the failure
#     names cgo rather than the missing compiler.
#
# python3 is here for the release workflow, which reads one field out of a GitHub
# API response. Depending on it being present in the base image and finding out
# during a release is worse than one more package.
FROM --platform=linux/arm64 ghcr.io/actions/actions-runner:latest

USER root

RUN set -eux; \
    apt-get update -qq; \
    apt-get install -y --no-install-recommends \
        gcc libc6-dev git ca-certificates curl python3; \
    rm -rf /var/lib/apt/lists/*

# Must track the `go` directive in go.mod. GOTOOLCHAIN=local below is what keeps
# that honest: if go.mod moves ahead of this, the build fails naming both
# versions instead of quietly downloading a second toolchain per run and
# throwing away the reason this image exists.
ARG GO_VERSION=1.27.0
RUN set -eux; \
    curl -fsSL -o /tmp/go.tar.gz "https://go.dev/dl/go${GO_VERSION}.linux-arm64.tar.gz"; \
    tar -C /usr/local -xzf /tmp/go.tar.gz; \
    rm /tmp/go.tar.gz; \
    /usr/local/go/bin/go version

ENV PATH=/usr/local/go/bin:/home/runner/go/bin:$PATH
ENV GOTOOLCHAIN=local

USER runner
