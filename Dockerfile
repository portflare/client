FROM golang:1.23-bookworm AS build
WORKDIR /src
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
COPY . .
RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w -X github.com/portflare/client/internal/buildinfo.Version=${VERSION} -X github.com/portflare/client/internal/buildinfo.Commit=${COMMIT} -X github.com/portflare/client/internal/buildinfo.Date=${BUILD_DATE}" \
    -o /out/portflare ./cmd/portflare

FROM gcr.io/distroless/base-debian12
COPY --from=build /out/portflare /usr/local/bin/portflare
ENTRYPOINT ["/usr/local/bin/portflare","daemon"]
