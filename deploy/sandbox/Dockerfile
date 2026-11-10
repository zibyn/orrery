# Orrery's platform Snapshot images (#235): Ubuntu 24.04 plus the shell tools
# agents commonly reach for. CI builds it twice, as orrery-sandbox on the
# default BASE and as orrery-sandbox-cuda on the CUDA runtime image.
ARG BASE=ubuntu:24.04
FROM ghcr.io/astral-sh/uv:0.9.30 AS uv

FROM ${BASE}
ENV DEBIAN_FRONTEND=noninteractive LANG=C.UTF-8
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      bash coreutils ca-certificates curl wget git openssh-client \
      jq ripgrep less procps unzip zip file tree xz-utils \
      build-essential pkg-config \
      python3 python3-venv python3-pip python3-dev \
      nodejs npm \
 && rm -rf /var/lib/apt/lists/*
COPY --from=uv /uv /uvx /usr/local/bin/
CMD ["bash"]
