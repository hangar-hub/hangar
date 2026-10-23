---
title: Hugging Face Hub on Kubernetes with Helm
description: "Deploy Hangar on Kubernetes with its Helm chart: one Pod with SQLite, or scaled out with Postgres and S3, and with Argo CD."
---

# Run Hangar on Kubernetes with Helm

The chart is published as an OCI artefact at `oci://ghcr.io/hangar-hub/charts/hangar`.

## One Pod with SQLite

The default runs one Pod in role `all`, with SQLite and the filesystem Blob store on a PersistentVolumeClaim:

```sh
helm install hangar oci://ghcr.io/hangar-hub/charts/hangar \
  --namespace hangar --create-namespace \
  --set publicURL=https://hangar.example.com \
  --set ingress.enabled=true --set ingress.className=nginx \
  --set 'ingress.hosts[0].host=hangar.example.com'
```

The chart generates the Instance key and the first admin password into a Secret named after the release with `-generated` added, `hangar-generated` for the release `hangar`, and keeps them across upgrades by reading the Secret back from the cluster. (A release whose name does not contain `hangar`, such as `models`, gets `models-hangar-generated`.) Read the password:

```sh
kubectl get secret --namespace hangar hangar-generated \
  -o jsonpath='{.data.HANGAR_SECURITY_BOOTSTRAP_ADMIN_PASSWORD}' | base64 -d; echo
```

With persistence on, the chart refuses to render more than one replica or any workers, because SQLite and a ReadWriteOnce volume belong to one Pod.

Generated secrets need `helm install` or `helm upgrade` with access to the cluster. `helm template`, `helm lint`, `--dry-run=client` and Argo CD cannot read the cluster, so they would make a new Instance key on every render, and Hangar could no longer decrypt the upstream tokens and webhook secrets it stored with the old one. The chart stops with an error there instead. Set `existingSecret` for those tools, as described in [Run with Argo CD](#run-with-argo-cd).

## Scaled out with Postgres and S3

Turn persistence off, point the configuration at Postgres and a bucket, and choose replicas:

```yaml
# values.yaml
publicURL: https://hangar.example.com
persistence:
  enabled: false
config:
  database: {driver: postgres}
  storage:
    backend: s3
    s3: {endpoint: s3.eu-west-2.amazonaws.com, region: eu-west-2, bucket: my-hangar}
existingSecret: hangar-secrets   # HANGAR_DATABASE_DSN, HANGAR_SECURITY_INSTANCE_KEY, ...
web:
  replicas: 2
  autoscaling: {enabled: true, minReplicas: 2, maxReplicas: 6}
  podDisruptionBudget: {enabled: true}
worker:
  replicas: 2
serviceAccount:
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/hangar   # IRSA for S3
```

```sh
kubectl create secret generic hangar-secrets --namespace hangar \
  --from-literal=HANGAR_DATABASE_DSN='postgres://hangar:...@postgres:5432/hangar' \
  --from-literal=HANGAR_SECURITY_INSTANCE_KEY="$(openssl rand -base64 32)" \
  --from-literal=HANGAR_SECURITY_BOOTSTRAP_ADMIN_PASSWORD="$(openssl rand -base64 18)"
helm install hangar oci://ghcr.io/hangar-hub/charts/hangar --namespace hangar -f values.yaml
```

A pre-install and pre-upgrade Job runs `hangar admin migrate`, and `web` and `worker` Pods report ready only when the schema is at the version they expect. With `migrations.mode: initContainer`, each `web` and `worker` Pod runs the migration in an init container instead, with no Helm hooks; see [Run with Argo CD](#run-with-argo-cd). A fuller example is [`deploy/helm/examples/scale-values.yaml`](https://github.com/hangar-hub/hangar/blob/main/deploy/helm/examples/scale-values.yaml).

## Run with Argo CD

Argo CD, Flux and other GitOps tools render the chart with `helm template` and apply the result. Two things differ from `helm install`:

- **Hooks run first.** Argo CD runs Helm's pre-install and pre-upgrade hooks as `PreSync` hooks, before anything else in the Application. If Postgres is in the same Application, the migrate Job waits for a database that Argo CD has not created yet, and the first sync never finishes. Set `migrations.mode: initContainer`: every `web` and `worker` Pod then runs `hangar admin migrate` in an init container, which waits for the database to answer for up to `migrations.waitForDatabase` (5 minutes by default), then fails, and the kubelet restarts it. Migrations hold a lock in the database, so when several Pods start together, one migrates and the others wait for it instead of failing. Postgres releases the lock when the migrating Pod's connection ends, so a Pod killed mid-migration does not block the next one.
- **No cluster access while rendering.** `lookup` returns nothing, so the chart cannot keep generated secrets and stops with an error if `generateSecrets` would generate them. Create the Secret yourself and name it in `existingSecret`.
- **The test Pod is a hook too.** The chart's `helm test` Pod carries `helm.sh/hook: test`. Argo CD skips it, but `helm template` renders it as an ordinary Pod. A tool that applies the render as it is, such as `kubectl apply` or Kustomize's `helmCharts`, starts it before Hangar is up, and it stays in `Error`. Pass `--skip-tests` to `helm template`, or set `skipTests: true` in Kustomize's `helmCharts` entry.

Create the namespace and the Secret once, outside your git repository or with a secrets operator such as Sealed Secrets or External Secrets:

```sh
kubectl create namespace hangar
kubectl create secret generic hangar-secrets --namespace hangar \
  --from-literal=HANGAR_DATABASE_DSN='postgres://hangar:<password>@postgres:5432/hangar' \
  --from-literal=HANGAR_STORAGE_S3_ACCESS_KEY_ID='<access key ID>' \
  --from-literal=HANGAR_STORAGE_S3_SECRET_ACCESS_KEY='<secret access key>' \
  --from-literal=HANGAR_SECURITY_INSTANCE_KEY="$(openssl rand -base64 32)" \
  --from-literal=HANGAR_SECURITY_BOOTSTRAP_ADMIN_PASSWORD="$(openssl rand -base64 18)"
```

Back up `HANGAR_SECURITY_INSTANCE_KEY`: without it, stored upstream tokens and webhook secrets cannot be decrypted.

Then point an Application at the chart with values like [`deploy/helm/examples/gitops-values.yaml`](https://github.com/hangar-hub/hangar/blob/main/deploy/helm/examples/gitops-values.yaml):

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: hangar
  namespace: argocd
spec:
  project: default
  destination:
    server: https://kubernetes.default.svc
    namespace: hangar
  source:
    repoURL: ghcr.io/hangar-hub/charts
    chart: hangar
    targetRevision: 1.2.3   # the chart version to deploy, which is also the Hangar version
    helm:
      releaseName: hangar
      valuesObject:
        publicURL: https://hangar.example.com
        persistence: {enabled: false}
        config:
          database: {driver: postgres}
          storage:
            backend: s3
            # region: the region your MinIO is configured with.
            s3: {endpoint: minio.example.com, region: us-east-1, bucket: hangar, use_path_style: true}
        existingSecret: hangar-secrets
        generateSecrets: false
        migrations: {mode: initContainer}
        web: {replicas: 1}
        worker: {replicas: 1}
  syncPolicy:
    automated: {prune: true, selfHeal: true}
```

Argo CD pulls OCI charts from a Helm repository with `enableOCI: true`; add `ghcr.io/hangar-hub/charts` as one, as the [Argo CD Helm guide](https://argo-cd.readthedocs.io/en/stable/user-guide/helm/) describes. Set `releaseName: hangar` so resource names match the rest of this guide. Argo CD skips the chart's `helm test` Pod, and the PersistentVolumeClaim and generated Secret keep `helm.sh/resource-policy: keep`, which Argo CD treats as `Delete=false`.

To check a render before you commit it, render the chart as Argo CD does. `--skip-tests` leaves out the test Pod, as Argo CD does:

```sh
helm template hangar oci://ghcr.io/hangar-hub/charts/hangar --namespace hangar -f values.yaml --skip-tests
```

To rotate a credential in the Secret, create a Secret with a new name and change `existingSecret`: the Pods' `checksum/secret` annotation covers the name, so the Pods restart and read it. Under `helm upgrade`, the chart also reads the Secret's data, so changing it in place restarts the Pods on the next upgrade.

With persistence on (SQLite in one Pod), you can instead set `generateSecrets: false` and leave the Instance key and admin password unset: Hangar generates them on its volume on first start and prints the password once in its log.

## Values

| Value | What it does |
|---|---|
| `publicURL` | Required. `server.public_url`. |
| `config` | Any configuration, rendered into a ConfigMap as `hangar.yaml`. |
| `secrets`, `existingSecret` | `HANGAR_*` variables in a Secret the chart writes, or one you manage. |
| `generateSecrets` | Generate the Instance key and admin password when you do not set them (default on). Needs `helm install` or `helm upgrade`; renders without cluster access stop with an error. |
| `migrations.mode` | `hook` (default): a pre-install and pre-upgrade Job. `initContainer`: an init container in each `web` and `worker` Pod, for GitOps tools. |
| `migrations.waitForDatabase` | With `migrations.mode: initContainer`, how long the init container waits for the database to answer before it fails. Default `5m`. |
| `persistence.*` | SQLite and filesystem mode on a PersistentVolumeClaim (default on). |
| `web.*`, `worker.*` | Replicas, resources, autoscaling (HPA on CPU, which needs the metrics server in the cluster), PodDisruptionBudget, node selection and topology spread. |
| `ingress.*` | An Ingress; `datasetsServerHost: true` also routes `datasets-server.<host>`, which the `datasets` library calls. |
| `httpRoute.*` | A Gateway API HTTPRoute instead of the Ingress. |
| `serviceAccount.annotations` | For IRSA or Workload Identity. |
| `metrics.*` | A Prometheus port and an optional ServiceMonitor. |
| `networkPolicy.*` | An optional NetworkPolicy. |
| `cacheSizeLimit`, `tmpSizeLimit` | Sizes of the emptyDir volumes for the git pack cache (default 16Gi) and `/tmp`. The pack cache goes over `git.cache_size` (10GiB by default) while Repos are in use, so the chart requires `cacheSizeLimit` to be at least a quarter larger; the kubelet evicts a Pod whose emptyDir passes its limit. |
| `extraEnv`, `extraEnvFrom`, `extraVolumes`, `extraVolumeMounts` | Anything else. |

Every Pod runs as non-root with a read-only root filesystem, no privilege escalation, every capability dropped and the `RuntimeDefault` seccomp profile. `helm test` checks `/readyz` from inside the cluster.

The full list is in the chart's [`values.yaml`](https://github.com/hangar-hub/hangar/blob/main/deploy/helm/hangar/values.yaml).

## Next steps

To load models into other Pods in the cluster from Hangar, without downloading them on every Pod start, see [Load models in Kubernetes](../guides/kubernetes.md).
