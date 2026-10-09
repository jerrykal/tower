# A simulated host for the scenario suite: sshd and tmux on Ubuntu, with
# the tools to shape its link (iproute2's tc, iptables). The harness
# reaches it over real ssh as the user tt, whose uid is the test user's,
# so the run's directories bind-mounted at their own paths are shared.

FROM ubuntu:24.04 AS tmux
ARG TMUX_VERSION=3.8
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates curl build-essential pkg-config bison libevent-dev libncurses-dev \
 && curl -fsSL https://github.com/tmux/tmux/releases/download/${TMUX_VERSION}/tmux-${TMUX_VERSION}.tar.gz | tar xz \
 && cd tmux-${TMUX_VERSION} && ./configure --prefix=/usr/local && make -j"$(nproc)" && make install

FROM ubuntu:24.04
ARG UID=1000
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      openssh-server openssh-client iproute2 iptables procps git libevent-core-2.1-7t64 libncurses6 \
 && rm -rf /var/lib/apt/lists/* \
 && (userdel -r ubuntu 2>/dev/null || true) \
 && useradd -m -u "$UID" -s /bin/sh tt && usermod -p '*' tt \
 && mkdir -p /run/sshd
COPY --from=tmux /usr/local/bin/tmux /usr/local/bin/tmux
COPY sshd_config /etc/ssh/sshd_config.d/tt.conf
COPY entrypoint.sh tt-run /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
