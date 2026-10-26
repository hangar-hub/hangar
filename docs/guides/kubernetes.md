---
title: Cache Hugging Face models for Kubernetes Pods
description: "Stop Kubernetes Pods downloading models from huggingface.co on every start: serve them from Hangar, keep them on a volume, and load them offline."
---

# Load models in Kubernetes without downloading them on every Pod start

A Pod that loads a model with `huggingface_hub`, such as a vLLM server or a `transformers` job, downloads it into the container's Hugging Face cache when it starts. The cache lives in the container's own file system, so a restarted, rescheduled or new Pod downloads the whole model again from huggingface.co. With large models and several replicas, that makes starts slow and can reach [huggingface.co's rate limits](ci.md#why-ci-reaches-the-limits).

Three changes fix it, and each works without the others:

1. Serve the models from a Hangar Instance on your own network, so downloads do not go to huggingface.co.
2. Keep the Hugging Face cache on a volume, so a restarted Pod finds the files it downloaded before.
3. Download before the server starts, in an init container, and load with `HF_HUB_OFFLINE=1`, so the server starts only with every file present and makes no requests.

The examples use vLLM. Any image that loads models with `huggingface_hub` works the same way.

## 1. Serve the models from Hangar

Run Hangar in the cluster with the [Helm chart](../get-started/helm.md), or anywhere the Pods can reach. Then pull the models into the Arsenal. Pull whole Repos: servers read the config and tokenizer files as well as the weights, and a file that is not present fails the load.

```sh
hangar pull Qwen/Qwen3-0.6B --server-only
```

Create a `read` Token for the Pods under **Settings**, **Tokens**, limited to the Repos they serve, and keep it in a Secret:

```sh
kubectl create secret generic hangar-token --from-literal=token=hgr_...
```

Point the Pods at the Instance:

```yaml
env:
  - name: HF_ENDPOINT
    value: https://hangar.example.com
  - name: HF_TOKEN
    valueFrom:
      secretKeyRef: {name: hangar-token, key: token}
```

Use the Instance's public URL (`server.public_url`) as `HF_ENDPOINT`, even from inside the cluster: Hangar builds some links it sends to clients from it, such as the next page of a long file listing. Mirrors keep the upstream Commit SHAs, so a `--revision` pinned on huggingface.co works unchanged.

Now huggingface.co sees one download per file, from Hangar's Pull, however many Pods start. On S3 storage in `redirect` mode, Pods download from presigned URLs of the bucket, so they must be able to reach its endpoint; otherwise use `proxy` mode (`storage.s3.download_mode`). See [Storage backends](../operate/storage.md).

## 2. Keep the cache on a volume

Mount a PersistentVolumeClaim and point the Hugging Face cache at it with `HF_HOME`. The cache is then `$HF_HOME/hub`, whatever the image's home folder:

```yaml
env:
  - name: HF_HOME
    value: /models
volumeMounts:
  - name: models
    mountPath: /models
```

A restarted Pod that gets the same volume loads from it instead of downloading again. `huggingface_hub` still asks the Instance whether the branch has a newer Commit; step 3 removes those requests.

How you give each replica a volume depends on your storage:

- For one replica, use a `ReadWriteOnce` claim, as in the example below.
- For several replicas that each keep their own copy, use a StatefulSet with `volumeClaimTemplates`, so each Pod keeps its own volume across restarts.
- For several replicas that share one copy, use a `ReadWriteMany` claim, if your storage class supports it. Fill it once, for example from a Job that runs the init container below, and mount it read-only in the serving Pods.

## 3. Download in an init container

The Hangar container image includes the `hangar` CLI, and `hangar pull` downloads a Repo into a Hugging Face cache in the layout `huggingface_hub` reads. Run it in an init container, then start the server with `HF_HUB_OFFLINE=1`:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: models
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 20Gi
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: vllm
spec:
  replicas: 1
  strategy:
    type: Recreate   # the new Pod needs the volume the old one holds
  selector:
    matchLabels: {app: vllm}
  template:
    metadata:
      labels: {app: vllm}
    spec:
      securityContext:
        fsGroup: 65532   # the group the Hangar image runs as, so it can write to the volume
      initContainers:
        - name: download-model
          image: ghcr.io/hangar-hub/hangar:latest
          args: ["pull", "Qwen/Qwen3-0.6B", "--cache-dir", "/models/hub"]
          env:
            - name: HANGAR_CLI_URL
              value: https://hangar.example.com
            - name: HANGAR_CLI_TOKEN
              valueFrom:
                secretKeyRef: {name: hangar-token, key: token}
          volumeMounts:
            - name: models
              mountPath: /models
      containers:
        - name: vllm
          image: vllm/vllm-openai:latest
          args: ["--model", "Qwen/Qwen3-0.6B"]
          env:
            - name: HF_HOME
              value: /models
            - name: HF_HUB_OFFLINE
              value: "1"
          volumeMounts:
            - name: models
              mountPath: /models
      volumes:
        - name: models
          persistentVolumeClaim:
            claimName: models
```

- On the first start, `hangar pull` downloads every file of the Repo. The cache names each file by its hash, so on later starts it downloads only files whose content is not on the volume yet, such as those of a new Revision.
- `hangar pull` checks each download against its SHA-256 or git object ID, and resumes an interrupted one, so a Pod killed part-way through finishes the download on its next start.
- With a `read` Token, the Repo must already be in the Arsenal: starting a Pull needs a `write` Token. If it is not there, the init container fails and the Pod does not start, which is safer than a server that starts without its model.
- To pin a Revision, add `--revision <commit SHA>` to both `hangar pull` and `vllm`.
- The init container asks the Instance which files the Repo has on every start, so a Pod starts only while the Instance is up. The server container needs no Token and makes no requests.

If you would rather not use the Hangar image, run `hf download Qwen/Qwen3-0.6B` in an init container from any image with `huggingface_hub` installed, with `HF_ENDPOINT`, `HF_TOKEN` and `HF_HOME` set as in the earlier steps.

## Errors you may see

| Error | Cause | Fix |
|---|---|---|
| The init container fails with exit code 4 | The Token is missing, wrong or expired. | Check the Secret, and create a new Token if needed. |
| The init container fails with "Permission denied" writing to `/models` | The Hangar image runs as user and group 65532, and the volume is not writable by that group. | Set `fsGroup: 65532`, as in the example, or prepare the volume's ownership another way. |
| `LocalEntryNotFoundError` in the server with `HF_HUB_OFFLINE=1` | The files are not in the cache the server reads. | Check that `--cache-dir` is `$HF_HOME/hub`, and that the model name and Revision match. |
| `RepositoryNotFoundError`, "Repository Not Found for url" | Without `HF_HUB_OFFLINE`, the Repo is not in the Arsenal or the Token cannot read it. | Pull the Repo, and check the Token. |
| A new Pod stays `ContainerCreating` with a `Multi-Attach error` event | A `ReadWriteOnce` volume is still attached to the old Pod on another node. | Use the `Recreate` strategy, a StatefulSet, or a `ReadWriteMany` volume. |

## Related pages

- [vLLM](vllm.md): serve models with vLLM from Hangar, in a container or on a host.
- [Run Hangar on Kubernetes with Helm](../get-started/helm.md): put the Instance in the same cluster.
- [Share one model cache across a team](shared-cache.md): the same idea for people and servers outside Kubernetes.
- [The hangar CLI](hangar-cli.md): `hangar pull` and its flags.
