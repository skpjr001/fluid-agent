FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /fluid-agent .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /fluid-agent /fluid-agent
WORKDIR /app
USER nonroot
EXPOSE 8080 8081 9000
ENTRYPOINT ["/fluid-agent", "-config", "/app/config.json"]
