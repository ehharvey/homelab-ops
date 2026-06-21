# Dev-only fixture: a tiny git remote, reachable from the web container over
# docker-compose's network, for exercising internal/configsync's real clone
# path. Not part of the app's own image/binary — installing git here is fine.
FROM alpine:3.20
RUN apk add --no-cache git git-daemon

WORKDIR /work
COPY fleet.yaml ./fleet.yaml
RUN git init -b main . \
    && git config user.email "dev@homelab-ops.local" \
    && git config user.name "homelab-ops dev fixture" \
    && git add fleet.yaml \
    && git commit -m "seed dev fixture fleet" \
    && git clone --bare /work /srv/git/fleet.git \
    && rm -rf /work

EXPOSE 9418
CMD ["git", "daemon", "--base-path=/srv/git", "--export-all", "--reuseaddr", "/srv/git"]
