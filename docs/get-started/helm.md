# Run Hangar on Kubernetes with Helm

The chart is published as an OCI artefact at `oci://ghcr.io/jamesagarside/charts/hangar`.

## One Pod with SQLite

The default runs one Pod in role `all`, with SQLite and the filesystem Blob store on a PersistentVolumeClaim:

```sh
helm install hangar oci://ghcr.io/jamesagarside/charts/hangar \
  --namespace hangar --create-namespace \
  --set publicURL=https://hangar.example.com \
  --set ingress.enabled=true --set ingress.className=nginx \
  --set 'ingress.hosts[0].host=hangar.example.com'
```

The chart generates the Instance key and the first admin password into the Secret `<release>-hangar-generated` and keeps them across upgrades. Read the password:

```sh
kubectl get secret --namespace hangar hangar-hangar-generated \
  -o jsonpath='{.data.HANGAR_SECURITY_BOOTSTRAP_ADMIN_PASSWORD}' | base64 -d; echo
```

With persistence on, the chart refuses to render more than one replica or any workers, because SQLite and a ReadWriteOnce volume belong to one Pod.

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
helm install hangar oci://ghcr.io/jamesagarside/charts/hangar --namespace hangar -f values.yaml
```

A pre-install and pre-upgrade Job runs `hangar admin migrate`, and `web` and `worker` Pods report ready only when the schema is at the version they expect. A fuller example is [`deploy/helm/examples/scale-values.yaml`](https://github.com/jamesagarside/hangar/blob/main/deploy/helm/examples/scale-values.yaml).

## Values

| Value | What it does |
|---|---|
| `publicURL` | Required. `server.public_url`. |
| `config` | Any configuration, rendered into a ConfigMap as `hangar.yaml`. |
| `secrets`, `existingSecret` | `HANGAR_*` variables in a Secret the chart writes, or one you manage. |
| `generateSecrets` | Generate the Instance key and admin password when you do not set them (default on). |
| `persistence.*` | SQLite and filesystem mode on a PersistentVolumeClaim (default on). |
| `web.*`, `worker.*` | Replicas, resources, autoscaling (HPA on CPU), PodDisruptionBudget, node selection and topology spread. |
| `ingress.*` | An Ingress; `datasetsServerHost: true` also routes `datasets-server.<host>`, which the `datasets` library calls. |
| `httpRoute.*` | A Gateway API HTTPRoute instead of the Ingress. |
| `serviceAccount.annotations` | For IRSA or Workload Identity. |
| `metrics.*` | A Prometheus port and an optional ServiceMonitor. |
| `networkPolicy.*` | An optional NetworkPolicy. |
| `extraEnv`, `extraEnvFrom`, `extraVolumes`, `extraVolumeMounts` | Anything else. |

Every Pod runs as non-root with a read-only root filesystem, no privilege escalation, every capability dropped and the `RuntimeDefault` seccomp profile. `helm test` checks `/readyz` from inside the cluster.

The full list is in the chart's [`values.yaml`](https://github.com/jamesagarside/hangar/blob/main/deploy/helm/hangar/values.yaml).
