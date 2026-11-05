# The hosted spov server. One image serves the API, runs migrations and
# the built web app; which of those it does is the argument.
#
# @decision(DL-010)

# The web app, on Bun. Bun appears at build time only: the runtime image
# carries the built bytes inside the Go binary and no JS runtime at all.
#
# @decision(DL-055)
FROM oven/bun:1.4.0 AS web
WORKDIR /web
COPY web/package.json web/bun.lock ./
# Frozen installs are not automatic in CI the way npm's are.
RUN bun install --frozen-lockfile
COPY web/ ./
RUN bun run build

FROM golang:1.26 AS build
WORKDIR /src

# Dependencies first, so a source-only change does not re-download them.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Over the placeholder the repository tracks, so the binary embeds the
# real dashboard.
#
# @decision(DL-055)
COPY --from=web /web/dist/ ./internal/webui/dist/
# The credential proxy the binary embeds; a hosted server refuses to
# start without it.
#
# @decision(DL-034)
RUN bash scripts/build-proxy.sh
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w \
      -X github.com/jimutt/spov/internal/version.version=${VERSION} \
      -X github.com/jimutt/spov/internal/version.commit=${COMMIT} \
      -X github.com/jimutt/spov/internal/version.date=${DATE}" \
    -o /spov ./cmd/spov

# static-debian13 carries CA certificates and tzdata, which scratch does
# not and the Modal and forge clients need. Pin by digest when deploying.
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /spov /spov

# The Modal SDK resolves its config path through os.UserHomeDir before
# it reads the credentials it was handed, and errors when HOME is unset.
# The path need not exist: a missing config file is treated as empty.
#
# @decision(DL-010)
ENV HOME=/home/nonroot

EXPOSE 8080
# The entrypoint is the binary alone and the subcommand is the command,
# so a Kubernetes `args:` (which replaces CMD, not ENTRYPOINT) reads as
# the whole invocation rather than appending to a fixed "serve".
ENTRYPOINT ["/spov"]
CMD ["serve"]
