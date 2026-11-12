# Meldgrove Updater: a POSIX shell script with curl and jq (README.md).
FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
RUN apk add --no-cache curl jq
COPY --chmod=0755 updater.sh /usr/local/bin/updater
# nobody: the updater needs no files and no privileges.
USER 65534:65534
# Set by the Release workflow; the updater logs it at start.
ARG APP_VERSION=""
ENV APP_VERSION=${APP_VERSION}
# The updater has nothing to check but its running process. A health check lets
# platforms such as Coolify count the container as healthy instead of unknown.
HEALTHCHECK --interval=10s --timeout=3s CMD ["true"]
ENTRYPOINT ["/usr/local/bin/updater"]
