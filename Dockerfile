# Nag — §10. Multi-stage build, FROM scratch final image.
# The Go image follows ADR-002 (go.mod and the build on one version), not
# the spec sketch's literal 1.24.
# The builder runs on the build host's platform and cross-compiles, so a
# multi-arch build needs no emulation (the final stage has no RUN).
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" -o /nag ./cmd/nag
# /empty → / rather than /emptytmp → /tmp: COPY of a directory copies its
# contents, so only copying the parent makes tmp itself a copied entry that
# keeps its 1777 mode and takes --chown (§10.1). A root-owned 0755 /tmp
# fails SQLite's first temp file under uid 65534.
RUN mkdir -p /empty/tmp && chmod 1777 /empty/tmp

FROM scratch
COPY --from=build /nag /nag
# both Web Push and shoutrrr make outbound TLS calls
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build --chown=65534:65534 /empty /
ENV TMPDIR=/tmp
EXPOSE 8080
# no /etc/passwd: a bare numeric uid, against host bind mounts chown'd once
USER 65534:65534
HEALTHCHECK --interval=60s --timeout=5s --start-period=10s CMD ["/nag", "healthcheck"]
ENTRYPOINT ["/nag"]
CMD ["serve"]
