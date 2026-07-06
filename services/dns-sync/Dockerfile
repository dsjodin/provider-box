FROM docker.io/library/golang:1.24-alpine AS build
WORKDIR /src
COPY . .
RUN go mod download \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/dns-sync ./cmd/dns-sync \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/dns-seed ./cmd/dns-seed \
 && echo 'dns-sync:x:1000:1000::/home/dns-sync:/sbin/nologin' > /tmp/passwd \
 && echo 'dns-sync:x:1000:' > /tmp/group

FROM scratch
ENV PATH=/usr/local/bin
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /tmp/passwd /etc/passwd
COPY --from=build /tmp/group /etc/group
COPY --from=build /out/dns-sync /usr/local/bin/dns-sync
COPY --from=build /out/dns-seed /usr/local/bin/dns-seed
USER 1000:1000
CMD ["/usr/local/bin/dns-sync"]
