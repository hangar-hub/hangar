---
description: "Install Hangar in a few minutes with Docker, Docker Compose, Helm or the binary, then sign in and pull your first model from Hugging Face."
---

# Get started

Pick the way you want to run Hangar. Each takes a few minutes on a clean machine. [System requirements](requirements.md) says what each needs, and how much disk to plan for.

| Option | Best for |
|---|---|
| [Docker](docker.md) | Trying Hangar, a laptop or a single homelab server. |
| [Docker Compose](compose.md) | Postgres and S3 storage, a scaled-out setup, or trying OIDC sign-in. |
| [Helm](helm.md) | Kubernetes. |
| [Binary](binary.md) | Running without containers, or the `hangar` CLI on your own machine. |

Then:

1. [Sign in for the first time](first-login.md) and create a Token.
2. [Pull your first model](first-pull.md) into the Arsenal.
3. [Point `HF_ENDPOINT` at Hangar](hf-endpoint.md), so your Python code uses it.
