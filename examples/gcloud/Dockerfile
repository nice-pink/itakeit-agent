# itakeit-agent with kubectl, gcloud and the GKE auth plugin, for tool entries
# against GKE clusters. Built on the published agent image, which carries only
# the Claude CLI; BASE picks another tag, the -mem image, or a local build
# (itakeit-agent:local).
ARG BASE=ghcr.io/nice-pink/itakeit-agent:latest
FROM ${BASE}

USER root
# From Google's apt repository (amd64 and arm64). gcloud brings an interpreter:
# on arm64 Debian's python3 on PATH, on amd64 a bundled Python (with pip3) off
# PATH. Read entries refuse interpreters, a plain Bash entry does not. The two
# directories are the mount points in docker-compose.yml: a new named volume
# takes their owner, so the agent (node) can write its login and kubeconfig.
RUN apt-get update && apt-get install -y --no-install-recommends curl gnupg \
 && curl -fsSL https://packages.cloud.google.com/apt/doc/apt-key.gpg | gpg --dearmor -o /usr/share/keyrings/cloud.google.gpg \
 && echo "deb [signed-by=/usr/share/keyrings/cloud.google.gpg] https://packages.cloud.google.com/apt cloud-sdk main" > /etc/apt/sources.list.d/google-cloud-sdk.list \
 && apt-get update && apt-get install -y --no-install-recommends google-cloud-cli google-cloud-cli-gke-gcloud-auth-plugin kubectl \
 && apt-get purge -y curl gnupg && apt-get autoremove -y && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /home/node/.kube /home/node/.config/gcloud && chown -R node:node /home/node/.kube /home/node/.config
USER node
