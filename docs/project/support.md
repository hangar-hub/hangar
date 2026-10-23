---
title: Get help with Hangar
description: "Where to get help with Hangar: check the docs and the doctor first, then open a GitHub issue with the details that let someone reproduce the problem."
---

# Get help

Hangar is maintained by one person, in the open on GitHub. There is no paid support, no chat channel and no guaranteed response time, so the steps below are the quickest way to an answer.

## Before you ask

1. Run `hangar admin doctor` on the server host. It checks the configuration, the database, the Blob store and git, and says which check fails; see [Troubleshooting](../operate/troubleshooting.md#run-the-doctor).
2. Read [Troubleshooting](../operate/troubleshooting.md) and the [FAQ](../faq.md).
3. Search the [existing issues](https://github.com/hangar-hub/hangar/issues?q=is%3Aissue), open and closed, for the error message.

## Open an issue

Questions, problems and ideas all go to [GitHub issues](https://github.com/hangar-hub/hangar/issues/new/choose). Choose the form that fits:

| Form | Use it for |
|---|---|
| **Bug report** | Something does not work as documented. This includes documentation that is wrong, or that leaves you unable to finish a task. |
| **Compatibility gap** | A client that works with huggingface.co, git, Ollama or LM Studio does not work with Hangar. |
| **Feature request** | Something Hangar should do but does not. |

Include:

- The output of `hangar version`, and how you run Hangar: Docker, Docker Compose, Helm or the binary.
- The database and Blob store, such as SQLite and the filesystem, or Postgres and MinIO.
- The commands or code that show the problem, and what you expected.
- The output of `hangar admin doctor`, and the log lines around the error. A server error names its request ID; search the logs for that `request_id` to find the cause.

Remove secrets first: Tokens (`hgr_...`), passwords, the Instance key, `Authorization` headers, cookies and presigned URLs.

## Security problems

Do not open a public issue for a vulnerability. Report it privately, as the [security policy](security.md) describes.

## Licence questions

What you can and cannot do with Hangar is on the [Licence](licence.md) page and in the [FAQ](../faq.md#licence-and-cost).
