# The controller image. CGo is off because the SQLite driver is pure Go
# (see docs/adrs/2026-08-11-sqlite-driver.md), so the binary is static and
# the runtime layer is distroless: no shell, no package manager, nothing
# for a compromised process to reach for beyond the base image's CA
# certificates, timezone data and passwd.
FROM golang:1.26.7-alpine@sha256:28d89ee9cc0ff9fec75c82ca201e6bf7fdf9a679d4b7b24dfa04f2bb766bb468 AS build
WORKDIR /src

# Dependencies resolve in their own layer so source edits do not refetch.
COPY go.mod go.sum ./
RUN go mod download

# Copied by allowlist: the binary is built from the module definition
# and the two source trees, and nothing else can reach a builder layer.
# .dockerignore stays as a second defense, not as the authority.
COPY cmd/ cmd/
COPY internal/ internal/
ARG VERSION=dev
ARG CAPSULE_IMAGE=runpool-capsule:dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.capsuleImage=${CAPSULE_IMAGE}" \
      -o /out/runpool ./cmd/runpool

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS runtime
COPY --from=build /out/runpool /usr/local/bin/runpool

# The controller runs as root because the Docker socket's group is not
# portable across hosts, and the socket already confers host-root
# authority: dropping to a non-root uid would obscure that without
# reducing it. The root filesystem is read-only and capabilities are
# dropped in the deployment manifest, where those choices are visible to
# whoever deploys it.
#
# Numeric, because a name has to resolve against the image's passwd and
# uid 0 never does.
# hadolint ignore=DL3002
USER 0
ENTRYPOINT ["/usr/local/bin/runpool"]
CMD ["serve"]
