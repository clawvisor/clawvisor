FROM debian:12-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates iptables iproute2 gosu tini coreutils bash curl \
 && rm -rf /var/lib/apt/lists/*
COPY init-firewall.sh /usr/local/bin/init-firewall.sh
COPY entrypoint-holder.sh /usr/local/bin/clawvisor-firewall-and-hold
RUN chmod +x /usr/local/bin/init-firewall.sh /usr/local/bin/clawvisor-firewall-and-hold
ENTRYPOINT ["/usr/bin/tini","--"]
CMD ["/usr/local/bin/clawvisor-firewall-and-hold"]
