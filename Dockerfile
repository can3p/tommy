# tommy's image: one static binary on a distroless base.
#
# This file is written for GoReleaser's `dockers_v2` build context, which places
# each prebuilt binary under a directory named after its platform - so the COPY
# below is the only way to pick the right one, and there is no `go build` here.
# `docs/docker.md` explains how to build the same file locally from a binary you
# compiled yourself; that path stages a context in the same shape, so this stays
# the one Dockerfile that ships.
#
# Two things GoReleaser must put in the context besides the binaries
# (.goreleaser.yaml `extra_files`): LICENSE and tommy.toml.

FROM gcr.io/distroless/static:nonroot

# distroless/static rather than scratch or alpine:
#   - scratch has no CA certificates and no tzdata. tommy is a Go binary that
#     makes outbound TLS calls (the update check) and stamps events with a
#     timestamp, so both matter and neither is worth vendoring by hand.
#   - alpine's only real advantage is a shell for `docker exec`. tommy is one
#     static binary with an HTTP API and a UI, so its debugging surface is
#     already reachable over the network; a shell would add a package manager
#     and a libc to an image that needs neither.
#   - :nonroot means the image already runs as uid 65532 with no capability
#     grants, which every default port being >= 1024 makes possible.
# Recorded here and in docs/docker.md so the choice is not re-litigated.

ARG TARGETPLATFORM
COPY $TARGETPLATFORM/tommy /usr/bin/tommy

# MIT requires the notice to travel with every copy, an image included.
COPY LICENSE /LICENSE

# The repository's own example configuration, which is default-equivalent (a
# test in core/config holds it to that), so shipping it changes no behaviour.
# It is here to be *replaced*: narrowing tommy to one plugin is then a single
# read-only bind mount over this path, with no flags to remember.
COPY tommy.toml /etc/tommy/tommy.toml

# A writable directory for everything tommy generates rather than captures: the
# AS2 identity today, and whatever persistence grows later. Taken from the base
# image's own nonroot home so it arrives owned by uid 65532 - a distroless image
# has no shell to mkdir with, and a /data that Docker creates for a volume would
# be owned by root and unwritable by the user this image runs as.
COPY --from=gcr.io/distroless/static:nonroot --chown=65532:65532 /home/nonroot /data
VOLUME /data

# Derived from `tommy providers --json`, plus the two core listeners. Keep one
# port per line, `EXPOSE <port>/<proto>`: the test that holds this list to the
# binary's own answer parses it.
EXPOSE 8811/tcp
EXPOSE 8822/tcp
EXPOSE 1025/tcp
EXPOSE 2121/tcp
EXPOSE 2222/tcp
EXPOSE 6969/udp
EXPOSE 2049/tcp
EXPOSE 2575/tcp
EXPOSE 1162/udp

# A container must not phone GitHub on boot. There is a sharper reason than
# manners: the update check in can3p/kleiner dereferences a nil version when
# GitHub is unreachable, so an image started on an air-gapped network would
# panic before it served anything.
ENV TOMMY_NO_UPDATE_CHECK=1

LABEL org.opencontainers.image.title="tommy" \
      org.opencontainers.image.description="A fake for the services your application talks to - mail, SMS, file transfer, chat, HL7, SNMP, push and AS2 - that shows you exactly what your code sent." \
      org.opencontainers.image.source="https://github.com/can3p/tommy" \
      org.opencontainers.image.url="https://github.com/can3p/tommy" \
      org.opencontainers.image.documentation="https://github.com/can3p/tommy/blob/main/docs/docker.md" \
      org.opencontainers.image.licenses="MIT"

USER 65532:65532
ENTRYPOINT ["/usr/bin/tommy"]

# --bind 0.0.0.0 because config.DefaultBind is 127.0.0.1: right for a binary on
# a laptop, useless in a container, where a published port never reaches a
# loopback listener. It is a flag rather than a changed default because the flag
# is applied *over* whatever --config loaded, so a mounted config still carrying
# `bind = "127.0.0.1"` cannot quietly make the container unreachable.
#
# --as2-cert-dir because as2 mints a key pair on first use and would otherwise
# put it beside the config file, in a directory this user cannot write.
#
# The entrypoint is the binary, so every subcommand still works:
#   docker run --rm can3p/tommy providers --config /etc/tommy/tommy.toml
#   docker run --rm -p 8811:8811 -p 8822:8822 can3p/tommy mail --bind 0.0.0.0
CMD ["serve", "--bind", "0.0.0.0", "--config", "/etc/tommy/tommy.toml", "--as2-cert-dir", "/data/as2"]
