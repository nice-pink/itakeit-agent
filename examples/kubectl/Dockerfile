# itakeit-agent with kubectl, for tool entries such as Bash(kubectl get *).
# BASE picks another tag, the -mem image, or a local build
# (itakeit-agent:local). examples/gcloud and examples/in-cluster build on this
# pattern.
ARG BASE=ghcr.io/nice-pink/itakeit-agent:latest
FROM ${BASE}

USER root
# From dl.k8s.io, checked against its published sha256. TARGETARCH is set by
# BuildKit. curl is only for the download and is removed. /home/node/.kube is
# where docker-compose.yml mounts the kubeconfig; kubectl writes its cache
# next to it, so node owns the directory.
ARG KUBECTL_VERSION=v1.37.1
ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends curl \
 && curl -fsSLo /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" \
 && echo "$(curl -fsSL "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl.sha256")  /usr/local/bin/kubectl" | sha256sum -c - \
 && chmod 755 /usr/local/bin/kubectl \
 && apt-get purge -y curl && apt-get autoremove -y && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /home/node/.kube && chown node:node /home/node/.kube
USER node
