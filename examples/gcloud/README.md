# itakeit-agent with gcloud

The agent image carries only the Claude Code CLI: which tools a deployment needs, and which identity they run as, is each operator's decision. This example adds kubectl, gcloud and `gke-gcloud-auth-plugin` on top of the published image, all from Google's apt repository, so tool entries such as `Bash(kubectl get *)` can reach GKE clusters. Use it as a pattern for other clouds: the Dockerfile adds the CLI, the compose file keeps its login in a volume.

## Config

- `mode: propose` works as is, since propose mode keeps the real `HOME`.
- `mode: fix` needs `agent.allow_real_home: true` (with an `approver`), or every session gets an empty `HOME` without the login and kubeconfig. See README "Tools" for what that option allows and refuses.
- Read entries that reach `gcloud auth`, `gcloud config`, `kubectl config` and similar are refused (they print credentials or change them), so list narrow ones: `Bash(kubectl get *)`, `Bash(gcloud container clusters list *)`.

## Setup

Run everything from the repo root. `c` below stands for `docker compose -f examples/gcloud/docker-compose.yml --project-directory .`.

1. Build the image: `c build`. `ITAKEIT_AGENT_BASE=itakeit-agent:local` builds on a local agent image instead of the published one.
2. Log in once. The login lands in the `gcloud` volume. Prefer a service account with read access to the clusters over a personal login, since every tool session runs as it:
   - Service account: with its key outside the repo, run `c run --rm -v /path/to/key.json:/tmp/sa.json:ro --entrypoint gcloud agent auth activate-service-account --key-file /tmp/sa.json`. The volume keeps the credential, so the key file is not needed afterwards. That credential is a long-lived private key under the agent's `HOME`: a Bash read entry that can read files (`Bash(cat *)`) reaches it, so keep read entries narrow, and to reach only the cluster it runs in, consider running the agent there with its ServiceAccount (`examples/in-cluster`), which needs no gcloud login.
   - Personal login: `c run --rm --entrypoint gcloud agent auth login --no-launch-browser`.
3. Write the kubeconfig into the `kube` volume: `c run --rm --entrypoint gcloud agent container clusters get-credentials CLUSTER --region REGION --project PROJECT`. It calls `gke-gcloud-auth-plugin` by name, which resolves inside the container, unlike a kubeconfig copied from a Mac.
4. Check it: `c run --rm --entrypoint kubectl agent get namespaces`.
5. Start both apps: `c up -d`. Logs: `c logs -f agent`.

Stop `scripts/tmux.sh` and the other compose files first: two instances in one channel answer every event twice.
