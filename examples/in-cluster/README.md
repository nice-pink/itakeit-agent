# itakeit-agent in a cluster, with in-cluster kubectl

The agent runs as a pod, and its kubectl uses the pod's ServiceAccount: no kubeconfig, no cloud CLI and no credentials in `HOME`. kubectl finds no kubeconfig and falls back to the in-cluster config, the token at `/var/run/secrets/kubernetes.io/serviceaccount/` and the `KUBERNETES_SERVICE_HOST` and `KUBERNETES_SERVICE_PORT` variables. It reaches the cluster it runs in; for other clusters, give it a kubeconfig as in `examples/gcloud`.

What kubectl may do is what the ServiceAccount may do (`rbac.yaml`, `restart-binding.yaml`). RBAC holds even when a tool entry is broader than meant, but only as far as the grants are narrow, so keep them to what the entries need:

- Read entries such as `Bash(kubectl get *)` use the built-in `view` role in every namespace. `view` does not read Secret objects, but it reads ConfigMaps and pod specs with their env, so anything kept there can be quoted into Slack. Bind `view` per namespace with RoleBindings to narrow it.
- The write entry `Bash(kubectl rollout restart *)` uses `itakeit-agent-restart` (`get`, `list` and `patch` on Deployments, StatefulSets and DaemonSets), bound by `restart-binding.yaml` in each namespace where restarts may happen. `patch` on a pod template can set any image, ServiceAccount or privileged container, so it amounts to control of everything that namespace's pods reach: never bind it cluster-wide, and keep kube-system and other sensitive namespaces out. The token is mounted in the pod, so whoever gets it has these grants too.

## Config

Copy the repo's `config.yaml` into this directory (`config.yaml` is gitignored everywhere) and set in its `agent` block:

```yaml
  # The claude CLI gets only allow-listed variables: kubectl needs these two
  # for the in-cluster config. KUBECONFIG must not be listed or set.
  env: [KUBERNETES_SERVICE_HOST, KUBERNETES_SERVICE_PORT]
```

- The token is a file at a fixed path, not under `HOME`, so fix mode works with its fresh `HOME` per session and needs no `allow_real_home`.
- The CLI's credential scrubbing leaves both variables in the Bash tool's shell. That was checked live on 2.1.285, the version the images pin; re-check it when bumping. Values of 8 characters or more from `agent.env` are removed from replies, which includes the service IP.
- A read entry that can read files (`Bash(cat *)`) can read the token. Keep read entries narrow.

## Setup

1. Build an image with kubectl and push it where the cluster can pull it, then set it in `agent.yaml`: the published image carries only the Claude CLI.

   ```
   docker build -t registry.example.com/itakeit-agent-kubectl:latest examples/kubectl && docker push registry.example.com/itakeit-agent-kubectl:latest
   ```

2. Create the namespace and the two Secrets with the tokens. The Secrets are created by hand so the tokens never sit in a file in the repo:

   ```
   kubectl apply -f examples/in-cluster/namespace.yaml
   kubectl -n itakeit create secret generic itakeit --from-literal=SLACK_BOT_TOKEN=xoxb-... --from-literal=SLACK_APP_TOKEN=xapp-...
   kubectl -n itakeit create secret generic itakeit-agent --from-literal=AGENT_SLACK_BOT_TOKEN=xoxb-... --from-literal=AGENT_SLACK_APP_TOKEN=xapp-... --from-literal=CLAUDE_CODE_OAUTH_TOKEN=sk-ant-...
   ```

3. Check what will be applied: `kubectl kustomize examples/in-cluster`.
4. Apply: `kubectl apply -k examples/in-cluster`.
5. Allow restarts where they may happen, one namespace at a time: `kubectl -n web apply -f examples/in-cluster/restart-binding.yaml`.
6. Check the agent's startup: `kubectl -n itakeit logs deploy/itakeit-agent`. It should log `connected to slack`.

`itakeit.yaml` runs itakeit itself with its database on a PVC. Leave it out of `kustomization.yaml` if itakeit runs elsewhere. Either way, run one instance of each per channel: stop `scripts/tmux.sh` and the compose files first.

The agent pod runs with a read-only root filesystem. `HOME` (`/home/node`) and `/tmp` are `emptyDir` volumes, since the CLI writes to both on every run. With `agent.memory`, use the `-mem` image and mount a PVC at `/config/memory`; `/config` itself is the read-only ConfigMap.
