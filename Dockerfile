# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X agent-gateway/internal/telemetry.ServiceVersion=${VERSION}" -o /out/gateway ./cmd/gateway \
 && CGO_ENABLED=0 go build -ldflags="-s -w -X agent-gateway/internal/telemetry.ServiceVersion=${VERSION}" -o /out/demo-upstream ./cmd/demo-upstream \
 && CGO_ENABLED=0 go build -ldflags="-s -w -X agent-gateway/internal/telemetry.ServiceVersion=${VERSION}" -o /out/orchestrator ./cmd/orchestrator \
 && CGO_ENABLED=0 go build -ldflags="-s -w -X agent-gateway/internal/telemetry.ServiceVersion=${VERSION}" -o /out/mock-reviewer ./cmd/mock-reviewer \
 && CGO_ENABLED=0 go build -ldflags="-s -w -X agent-gateway/internal/telemetry.ServiceVersion=${VERSION}" -o /out/cursor-reviewer ./cmd/cursor-reviewer

# Cursor CLI via the official installer (https://cursor.com/docs/cli/installation).
# It installs into $HOME/.local/share/cursor-agent/versions/<version>; we
# relocate that directory to /opt/cursor-agent.
FROM debian:bookworm-slim AS cursor-cli
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl bash \
 && rm -rf /var/lib/apt/lists/*
ENV HOME=/root
RUN curl https://cursor.com/install -fsS | bash \
 && mv "$(dirname "$(readlink -f /root/.local/bin/agent)")" /opt/cursor-agent \
 && /opt/cursor-agent/cursor-agent --version > /opt/cursor-agent/VERSION

# Runtime: glibc base (the Cursor CLI bundles a dynamically linked node).
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates bash \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd -g 65532 nonroot && useradd -u 65532 -g 65532 -m -d /home/nonroot nonroot
COPY --from=cursor-cli /opt/cursor-agent /opt/cursor-agent
RUN ln -s /opt/cursor-agent/cursor-agent /usr/local/bin/agent \
 && ln -s /opt/cursor-agent/cursor-agent /usr/local/bin/cursor-agent
COPY --from=build /out/gateway /app/gateway
COPY --from=build /out/demo-upstream /app/demo-upstream
COPY --from=build /out/orchestrator /app/orchestrator
COPY --from=build /out/mock-reviewer /app/mock-reviewer
COPY --from=build /out/cursor-reviewer /app/cursor-reviewer
ENV GATEWAY_CONFIG=/etc/agent-gateway/config.json
ENV ORCHESTRATOR_CONFIG=/etc/orchestrator/config.json
ENV CURSOR_AGENT_BIN=/usr/local/bin/agent
USER 65532:65532
WORKDIR /home/nonroot
EXPOSE 4817 4819
ENTRYPOINT ["/app/gateway"]
