# An SSH client that holds one port-forward open and reconnects when it drops.
# The worker uses two of these to reach Postgres and Temporal on other servers.
FROM alpine:3.22
RUN apk add --no-cache openssh-client autossh
# Keep retrying even when the very first connection fails.
ENV AUTOSSH_GATETIME=0
ENTRYPOINT ["autossh", "-M", "0", "-N", \
  "-o", "ExitOnForwardFailure=yes", \
  "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", \
  "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=/keys/known_hosts", \
  "-o", "IdentitiesOnly=yes", "-i", "/keys/tunnel_ed25519"]
