# The outer capsule image: one container holding the runner, dockerd and
# every inner container a job launches, so one cgroup carries the whole
# job. The base is the pinned actions runner — the runner's externals
# tree and .NET dependencies are the fragile half — and the engine
# arrives as static binaries copied from the pinned dind image, the same
# bytes the two-container capsule ran.
#
# Both sources are digest-pinned to the entries in build/images.lock.json;
# a moving tag here would put unreviewed bytes inside a privileged
# container.
FROM golang:1.26.7-alpine@sha256:28d89ee9cc0ff9fec75c82ca201e6bf7fdf9a679d4b7b24dfa04f2bb766bb468 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
# Copied by allowlist: the supervisor is built from the module
# definition and the source trees, nothing else.
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
      -o /out/capsule-supervisor ./cmd/capsule-supervisor

FROM docker:29.7.2-dind@sha256:3ef33f2e220b79ed3ef3b99d81746f06f306cd6340e2cb7331d17ae996e74cb6 AS engine

FROM ghcr.io/actions/actions-runner:2.337.0@sha256:e5496277be5d09bc968b3d64911b74e219ac4a3f2edce956a3ecf9271bea1ef4

# Root both to install the engine below and to keep it at runtime: the
# supervisor is PID 1 and needs root to boot the inner dockerd. It drops
# the runner to uid 1001 itself, which the image's last USER cannot say.
#
# Numeric, because a name has to resolve against the image's passwd and
# uid 0 never does.
# hadolint ignore=DL3002
USER 0

# The engine, containerd and their runtime dependencies, verbatim from
# the pinned dind image. iptables comes from the base distribution: the
# static bundle's xtables links against Alpine paths.
COPY --from=engine /usr/local/bin/dockerd /usr/local/bin/docker \
     /usr/local/bin/containerd /usr/local/bin/containerd-shim-runc-v2 \
     /usr/local/bin/runc /usr/local/bin/ctr \
     /usr/local/bin/docker-init /usr/local/bin/docker-proxy \
     /usr/local/bin/
# iptables serves both the inner dockerd and this image's gateway mode;
# iproute2 serves the route initializer and the host discovery probe.
RUN apt-get update \
 && apt-get install -y --no-install-recommends iptables uidmap iproute2 \
 && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/capsule-supervisor /usr/local/bin/capsule-supervisor

# dockerd's data root must be a volume: overlayfs cannot stack on the
# container's own overlayfs. The controller mounts a fresh volume per
# job, which is also what guarantees no image, layer, build cache or
# runner bootstrap state survives to the next job.
VOLUME /var/lib/docker

# The control surface, delivered JIT bundle, and runner configuration files
# live on tmpfs declared by the controller (--tmpfs /run/runpool).

# The supervisor is PID 1: it boots dockerd, proves readiness, waits for
# the start authorization, runs the runner as uid 1001, propagates
# signals and reaps everything.
ENTRYPOINT ["/usr/local/bin/capsule-supervisor"]
