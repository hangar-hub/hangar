# Hangar Helm chart

Hangar is a self-hosted model and dataset hub, compatible with the Hugging Face Hub API, git, Ollama and LM Studio. This chart runs it on Kubernetes 1.29 or later.

- Documentation: <https://hangar.page/>
- Helm guide, including Argo CD: <https://hangar.page/latest/get-started/helm/>
- Source: <https://github.com/hangar-hub/hangar>

## Install

The chart is published as an OCI artefact at `oci://ghcr.io/hangar-hub/charts/hangar`, with the same version as the Hangar image it runs. `publicURL` is required: set it to the address clients use to reach Hangar.

```sh
helm install hangar oci://ghcr.io/hangar-hub/charts/hangar \
  --namespace hangar --create-namespace \
  --set publicURL=https://hangar.example.com \
  --set ingress.enabled=true --set ingress.className=nginx \
  --set 'ingress.hosts[0].host=hangar.example.com'
```

The default runs one Pod in role `all`, with SQLite and the filesystem Blob store on a PersistentVolumeClaim. The chart generates the Instance key and the first admin password into the Secret `hangar-generated` (for a release named `hangar`), and the install notes print the command that reads the password. Sign in as `hangar-admin`.

Back up the Instance key. Without it, Hangar can't decrypt the upstream tokens and webhook secrets it stores.

## Scale out

To run several `web` and `worker` replicas, turn `persistence` off, point `config` at Postgres and an S3-compatible bucket, and keep credentials in a Secret named in `existingSecret`. The [Helm guide](https://hangar.page/latest/get-started/helm/#scaled-out-with-postgres-and-s3) has a full example, and [`scale-values.yaml`](https://github.com/hangar-hub/hangar/blob/main/deploy/helm/examples/scale-values.yaml) is one to start from.

## Values

`helm show values oci://ghcr.io/hangar-hub/charts/hangar` prints every value with its description. The main ones:

| Value | What it sets |
|---|---|
| `publicURL` | The URL clients use to reach Hangar. Required. |
| `config` | Hangar configuration, rendered into `hangar.yaml`. Any key in the [configuration reference](https://hangar.page/latest/reference/configuration/). |
| `secrets`, `existingSecret` | `HANGAR_*` variables that hold secrets, from the values or from an existing Secret. |
| `generateSecrets` | Generate the Instance key and admin password when nothing else sets them. Needs `helm install` or `helm upgrade` with cluster access. |
| `persistence` | SQLite and filesystem mode on a PersistentVolumeClaim, for one Pod. |
| `web`, `worker` | Replicas, autoscaling, resources, PodDisruptionBudgets and scheduling for each role. |
| `migrations.mode` | With persistence off, run migrations in a pre-install and pre-upgrade Job (`hook`), or in an init container of each Pod (`initContainer`) for GitOps tools. |
| `ingress`, `httpRoute` | An Ingress or a Gateway API HTTPRoute. |
| `metrics` | Prometheus metrics on a separate port, with an optional ServiceMonitor. |

## Upgrade

```sh
helm upgrade hangar oci://ghcr.io/hangar-hub/charts/hangar --namespace hangar --reuse-values
```

Migrations run as the new version starts: in the Pod itself with persistence on, otherwise as `migrations.mode` sets. Read [Upgrade Hangar](https://hangar.page/latest/operate/upgrades/) and back up first.

## Verify the chart

The release workflow signs each chart version, and the image it runs, with cosign keyless signing. To check a version before you install it:

```sh
VERSION=1.2.3   # the chart version to check
cosign verify ghcr.io/hangar-hub/charts/hangar:$VERSION \
  --certificate-identity https://github.com/hangar-hub/hangar/.github/workflows/release.yml@refs/tags/v$VERSION \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Replace `charts/hangar` with `hangar` to check the image. [Security hardening](https://hangar.page/latest/operate/security/#verify-the-image) also shows how to check the image's SBOM attestation and build provenance.

## Licence

Hangar is source-available under the [Elastic License 2.0](https://github.com/hangar-hub/hangar/blob/main/LICENSE).
