FROM docker.io/library/golang:1.24-alpine AS build
WORKDIR /src
COPY . .
RUN go mod download \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/dashboard ./cmd/dashboard \
 && echo 'dashboard:x:1000:1000::/home/dashboard:/sbin/nologin' > /tmp/passwd \
 && echo 'dashboard:x:1000:' > /tmp/group

FROM scratch
ENV PATH=/usr/local/bin
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /tmp/passwd /etc/passwd
COPY --from=build /tmp/group /etc/group
COPY --from=build /out/dashboard /usr/local/bin/dashboard
# Runs as uid 1000. The step-ca DB snapshot dir and the docker socket group
# access are provided by the compose file's mounts and group mapping.
USER 1000:1000
CMD ["/usr/local/bin/dashboard"]
