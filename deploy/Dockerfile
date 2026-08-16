# dsh-fleet control plane.
#
# Docker is the supported deployment path, so this image is the reference
# build: everything the binary needs is here, and nothing else is.

# ── build ────────────────────────────────────────────────────────────────────
FROM golang:1.23-alpine AS build

WORKDIR /src

# Dependency layer first: source edits then rebuild without re-downloading.
COPY go.mod go.sum* ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY pkg/ ./pkg/

ARG VERSION=dev
# CGO off keeps the binary static so the runtime stage needs no libc.
# The version is stamped rather than derived: the build has no git context.
RUN CGO_ENABLED=0 go build \
      -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/dshf ./cmd/dshf

# ── runtime ──────────────────────────────────────────────────────────────────
# distroless/static carries CA certificates and nothing executable besides the
# binary. `docker compose exec dshf dshf node add …` still works: exec runs the
# binary directly and needs no shell.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/dshf /usr/local/bin/dshf
COPY deploy/migrations/ /usr/share/dsh-fleet/migrations/

USER nonroot:nonroot
EXPOSE 8080

ENV DSHF_LISTEN=":8080" \
    DSHF_MIGRATIONS_DIR="/usr/share/dsh-fleet/migrations"

ENTRYPOINT ["/usr/local/bin/dshf"]
CMD ["serve"]
