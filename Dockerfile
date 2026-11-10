# syntax=docker/dockerfile:1

FROM golang:1.27.0-bookworm AS build

ARG REVISION=unknown

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

# Only the source of cmd/factortrail is copied: no config, key or env file can
# enter the build, and .dockerignore keeps them out of the context as well.
COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -buildid= -X main.revision=${REVISION}" -o /out/factortrail ./cmd/factortrail

FROM gcr.io/distroless/static-debian12:nonroot

ARG REVISION=unknown
LABEL org.opencontainers.image.source="https://github.com/mgh3326/factortrail" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.revision="${REVISION}"

COPY --from=build /out/factortrail /usr/local/bin/factortrail

USER nonroot:nonroot

# No configuration is embedded. The profile and the Access settings come from
# the environment at run time; without them the process refuses to start.
ENTRYPOINT ["/usr/local/bin/factortrail"]
CMD ["serve"]
