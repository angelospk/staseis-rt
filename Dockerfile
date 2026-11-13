# Static atrt binary on distroless (CA certificates, non-root user 65532). The Athens time
# zone is embedded (timetzdata), so the image needs no tzdata.
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
ARG TARGETOS TARGETARCH
# The git commit of the build, for /v1/status `rev`. .git is in .dockerignore, so this is the only
# source of it in the image (image.yml passes github.sha).
ARG REVISION
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -tags timetzdata \
    -ldflags="-s -w -X github.com/angelospk/athens-transit-rt/internal/buildinfo.Revision=${REVISION}" -o /out/atrt ./cmd/atrt && mkdir -p /out/state

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/atrt /usr/local/bin/atrt
COPY --from=build --chown=65532:65532 /out/state /var/lib/atrt
VOLUME /var/lib/atrt
ENV GOMEMLIMIT=100MiB
ENTRYPOINT ["/usr/local/bin/atrt"]
# Default: API on all container interfaces (for `docker run -p 127.0.0.1:8095:8095`),
# metrics on the container's loopback only. compose.yaml uses host networking instead.
CMD ["serve", "--listen", "0.0.0.0:8095", "--metrics-listen", "127.0.0.1:8096", "--state", "/var/lib/atrt"]
