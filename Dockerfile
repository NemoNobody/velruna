# syntax=docker/dockerfile:1

# velruna is a static Go binary: the runtime image needs the file and nothing
# else. The build stage pins the Go version to the go.mod directive so an image
# and a local build cannot drift.
#
# BUILDPLATFORM keeps the compiler on the build host and cross-compiles for the
# target: the arm64 image then costs seconds instead of an emulated full build.
# TARGETOS/TARGETARCH are set by BuildKit; with the legacy builder they are empty
# and Go falls back to the host platform, which matches that builder's base image.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src

# Dependency layer first: source edits then rebuild without re-downloading.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Version and commit are stamped exactly like `make build` does: the binary
# reports them in `velruna status`, so an image can be traced to a tag.
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=0.0.0-dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
        -ldflags "-s -w \
            -X github.com/NemoNobody/velruna/internal/version.Version=${VERSION} \
            -X github.com/NemoNobody/velruna/internal/version.Commit=${COMMIT}" \
        -o /out/velruna .

# distroless/static carries CA certificates (HTTPS to OpenSearch) and nothing
# else: no shell, no package manager, so code execution has nothing to run.
# nonroot = uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/velruna /velruna

# Ownership label for the official MCP Registry: it is compared byte-for-byte
# against the server.json `name` when the registry verifies the OCI package.
LABEL io.modelcontextprotocol.server.name="io.github.NemoNobody/velruna"

# No VELRUNA_CONF default on purpose: the binary's own lookup (--conf,
# VELRUNA_CONF, ./velruna.conf, next to the binary, ~/.velruna.conf) then works
# unchanged, and `docker run -e OPENSEARCH_URL -e OPENSEARCH_PASSWORD` needs no
# config file at all. A mounted config is still the normal setup — see
# docs/setup.md. A container has no session bus, so the OS keyring is
# unreachable: the password comes from the environment or the config file.

# A bare `docker run <image>` is the MCP server over stdio; the dev compose
# stack and `docker run <image> status` override the command.
CMD ["mcp"]

ENTRYPOINT ["/velruna"]
