# Meldgrove Updater: one static Go binary (README.md).
FROM golang:1.27-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY *.go host-setup.sh ./
RUN go vet ./... && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /updater .

# Alpine: the release workflow starts the image with uname, id and printenv.
FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
COPY --from=build /updater /usr/local/bin/updater
# The volume that the updater shares with the host (requests, results) and
# keeps its state in over deployments. A new named volume takes this owner.
RUN mkdir /trigger && chown 65534:65534 /trigger
# nobody: the updater needs no privileges and no Docker socket.
USER 65534:65534
# Set by the Release workflow; the updater logs it at start.
ARG APP_VERSION=""
ENV APP_VERSION=${APP_VERSION}
# The API the app calls; only on the Compose network, never published.
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s CMD ["/usr/local/bin/updater", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/updater"]
