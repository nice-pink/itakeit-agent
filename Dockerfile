FROM cgr.dev/chainguard/go:latest-dev AS builder

LABEL org.opencontainers.image.source="https://github.com/nice-pink/itakeit-agent"

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 ./build

# The claude-code backend runs the Claude Code CLI, so the runtime image carries it.
FROM node:22-slim AS runner

LABEL org.opencontainers.image.source="https://github.com/nice-pink/itakeit-agent"

# The CLI version the agent's flags and JSON parsing were verified against.
ARG CLAUDE_CODE_VERSION=2.1.285
# ca-certificates: the Go binary verifies Slack's TLS with the system store.
# No init such as tini: as PID 1 it would hold the Slack tokens in a readable
# /proc/1/environ, which the agent hides for itself (hideEnviron).
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/* \
 && npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} && npm cache clean --force
COPY --from=builder /app/bin/itakeit-agent /app/itakeit-agent
# This image has no poma-memory: with agent.memory enabled the agent stops and
# names the image built from Dockerfile.mem.
ENV ITAKEIT_AGENT_IMAGE=itakeit-agent
USER node
ENTRYPOINT [ "/app/itakeit-agent", "-config", "/config/config.yaml" ]
