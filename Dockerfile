ARG GO_VERSION=1.26.11
FROM golang:${GO_VERSION}-alpine@sha256:ecb67f987aa1a57794c91d19ade7ae0209f2f0f5496aaef9e6ae85ddf92d02ab AS build
ARG GO_VERSION
WORKDIR /src
ENV GOTOOLCHAIN=local CGO_ENABLED=0
COPY go.mod go.sum ./
RUN test "$(awk '/^toolchain / {sub(/^go/, "", $2); print $2}' go.mod)" = "$GO_VERSION" && go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN go build -mod=readonly -trimpath -buildvcs=false -ldflags="-s -w -buildid=" -o /out/typhon ./cmd/typhon

FROM alpine:3.23@sha256:469c9ea410b9215eaee7976ba6c0639039b444be8e5d638f7abbada189971eb2
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /out/typhon /usr/local/bin/typhon
COPY examples/compose/ ./
USER 10001:10001
EXPOSE 8080
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/typhon"]
CMD ["run", "/app/service.ty"]
