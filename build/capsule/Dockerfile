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
FROM golang:1.26.6-alpine@sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
# Copied by allowlist: the supervisor is built from the module
# definition and the source trees, nothing else.
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
      -o /out/capsule-supervisor ./cmd/capsule-supervisor

FROM docker:29.7.2-dind@sha256:12e683a161823b2a839aeea999b9d960e6e1f9a97b1679ad6b441982e2d9cf07 AS engine

FROM ghcr.io/actions/actions-runner:2.336.0@sha256:0cfdcc701ce933c6d243c6b0b2da767366dc9f2e99961d4c3754b0b78084cdda

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
