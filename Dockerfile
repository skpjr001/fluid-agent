FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /proxy-agent .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /proxy-agent /proxy-agent
WORKDIR /app
USER nonroot
EXPOSE 8080 8081 9000
ENTRYPOINT ["/proxy-agent", "-config", "/app/config.json"]
