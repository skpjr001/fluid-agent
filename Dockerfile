FROM docker.io/library/golang:1.27.4-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY ui ./ui
COPY schema ./schema
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /fluid-agent . \
 && mkdir -p /out/app/data /out/app/dynamic

FROM gcr.io/distroless/static:nonroot
COPY --from=build /fluid-agent /fluid-agent
COPY --from=build --chown=65532:65532 /out/app /app
WORKDIR /app
USER nonroot
EXPOSE 8080 8081 9000
VOLUME ["/app/data"]
HEALTHCHECK --interval=15s --timeout=5s --start-period=10s CMD ["/fluid-agent", "healthcheck", "-config", "/app/config.json"]
ENTRYPOINT ["/fluid-agent", "-config", "/app/config.json"]
