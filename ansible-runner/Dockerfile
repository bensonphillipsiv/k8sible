FROM python:3.12-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    git \
    openssh-client \
    && rm -rf /var/lib/apt/lists/* \
    && pip install --no-cache-dir ansible

WORKDIR /ansible

ENTRYPOINT ["/bin/bash", "-c"]
