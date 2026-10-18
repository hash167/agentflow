# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/gateway ./cmd/gateway \
 && CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/demo-upstream ./cmd/demo-upstream

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gateway /app/gateway
COPY --from=build /out/demo-upstream /app/demo-upstream
ENV GATEWAY_CONFIG=/etc/agent-gateway/config.json
EXPOSE 4817
ENTRYPOINT ["/app/gateway"]
