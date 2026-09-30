# itakeit-agent with kubectl

The agent image carries only the Claude Code CLI: which tools a deployment needs, and which credentials they run with, is each operator's decision. This example adds kubectl on top of the published image and mounts a kubeconfig, so tool entries such as `Bash(kubectl get *)` and `Bash(kubectl rollout restart *)` work. `examples/gcloud` does the same for GKE with gcloud, and `examples/in-cluster` runs the agent as a pod, where kubectl needs no kubeconfig.

## The kubeconfig

The file is read inside the container, so it has to work there on its own:

- No auth plugin the image lacks. A kubeconfig from `gcloud`, `aws eks` or `az aks` runs `gke-gcloud-auth-plugin`, `aws` or `kubelogin` on every call, and this image has none of them (see `examples/gcloud`). Use a user with a token or a client certificate.
- No paths into your machine. Embed certificates with `kubectl config view --minify --flatten --context NAME > agent.kubeconfig`, which also drops every other context.
- The narrowest identity that covers the tool entries, since every tool session runs as it. A ServiceAccount with the roles from `examples/in-cluster/rbac.yaml` and `restart-binding.yaml` is a good fit: `kubectl create token itakeit-agent -n itakeit --duration 24h` gives a short-lived token for its `users[].user.token`.

Keep the file outside the repo and mode 600. A Bash read entry that can read files (`Bash(cat *)`) can read it, so keep read entries narrow.

## Config

- Add `KUBECONFIG` to `agent.env`: `env: [KUBECONFIG]`. The compose file points it at the mounted file. In fix mode the agent copies it into each session's fresh `HOME` (README "Tools"); with `allow_real_home: true` or in propose mode kubectl reads it in place.
- Keep read entries narrow: `Bash(kubectl get *)`, `Bash(kubectl describe *)`. Entries that reach `kubectl config`, `kubectl cp` or `kubectl create token` are refused.

## Setup

Run everything from the repo root. `c` below stands for `docker compose -f examples/kubectl/docker-compose.yml --project-directory .`.

1. Set `AGENT_KUBECONFIG=/path/to/agent.kubeconfig` in `./.env`, or export it.
2. Build the image: `c build`. `ITAKEIT_AGENT_BASE=itakeit-agent:local` builds on a local agent image instead of the published one.
3. Check the kubeconfig inside the container: `c run --rm --entrypoint kubectl agent get namespaces`.
4. Start both apps: `c up -d`. Logs: `c logs -f agent`, which should show `connected to slack`.

Stop `scripts/tmux.sh` and the other compose files first: two instances in one channel answer every event twice.
