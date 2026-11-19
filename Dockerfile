ARG GO_VERSION=1.26.0
FROM golang:${GO_VERSION}-alpine AS build
ARG GO_VERSION
WORKDIR /src
ENV GOTOOLCHAIN=local CGO_ENABLED=0
COPY go.mod go.sum ./
RUN test "$(awk '/^go / {print $2}' go.mod)" = "$GO_VERSION" && go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN go build -mod=readonly -trimpath -buildvcs=false -ldflags="-s -w -buildid=" -o /out/typhon ./cmd/typhon

FROM alpine:3.23
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /out/typhon /usr/local/bin/typhon
COPY examples/compose/ ./
USER 10001:10001
EXPOSE 8080
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/typhon"]
CMD ["run", "/app/service.ty"]
