# Build context is the REPO ROOT (install.sh and the compose file pass it)
# so the image can carry the shipped example config for the wizard.
FROM docker.io/library/golang:1.24-alpine AS build
WORKDIR /src
COPY services/control-plane/ .
RUN go mod download \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/control-plane ./cmd/control-plane

FROM docker.io/library/docker:27.5.1-cli AS dockercli

FROM docker.io/library/alpine:3.22
RUN apk add --no-cache ca-certificates
# The deploy engine execs the docker CLI (compose v2, build) against the
# mounted host socket; the static CLI and plugins come from the pinned
# docker-cli image.
COPY --from=dockercli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=dockercli /usr/local/libexec/docker/cli-plugins/docker-compose /usr/local/lib/docker/cli-plugins/docker-compose
COPY --from=dockercli /usr/local/libexec/docker/cli-plugins/docker-buildx /usr/local/lib/docker/cli-plugins/docker-buildx
COPY --from=build /out/control-plane /usr/local/bin/control-plane
COPY config/provider-box.env.example /usr/local/share/provider-box/provider-box.env.example
# dns-sync is built locally by the deploy engine; its source ships in the
# image so no repo checkout is needed on the host.
COPY services/dns-sync /usr/local/share/provider-box/services/dns-sync
# No USER here: the legacy read-only compose deployment pins user 1000:1000
# itself; install.sh runs the engine-enabled container as root so it can
# create and chown service data dirs under /opt/provider-box.
CMD ["/usr/local/bin/control-plane"]
