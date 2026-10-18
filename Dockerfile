# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/gateway ./cmd/gateway \
 && CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/demo-upstream ./cmd/demo-upstream \
 && CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/orchestrator ./cmd/orchestrator \
 && CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/mock-reviewer ./cmd/mock-reviewer

# One image, four static binaries; the gateway is the default entrypoint and
# the orchestrator deployment overrides command to /app/orchestrator.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gateway /app/gateway
COPY --from=build /out/demo-upstream /app/demo-upstream
COPY --from=build /out/orchestrator /app/orchestrator
COPY --from=build /out/mock-reviewer /app/mock-reviewer
ENV GATEWAY_CONFIG=/etc/agent-gateway/config.json
ENV ORCHESTRATOR_CONFIG=/etc/orchestrator/config.json
EXPOSE 4817 4819
ENTRYPOINT ["/app/gateway"]
