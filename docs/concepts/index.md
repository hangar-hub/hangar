# Concepts

The words Hangar uses, in its UI, API and these docs.

| Term | Meaning |
|---|---|
| **Instance** | One running deployment of Hangar: one or more server and worker processes sharing a database and a Blob store. |
| **Arsenal** | Everything stored in an Instance that you can see. |
| **Namespace** | The owner part of a Repo ID: a User or an Organisation. Names are unique across both. |
| **Repo** | A versioned set of files, of type `model` or `dataset`, with an ID `namespace/name`. Every Repo is a git repository. See [Repos and Revisions](repos.md). |
| **Revision** | A branch, a tag or a Commit SHA. |
| **LFS object** | A large file's content, stored once per Instance and addressed by its SHA-256. |
| **Blob store** | Where LFS objects and git data live: a local folder or an S3-compatible bucket. |
| **Upstream** | Another Hugging Face-compatible hub Hangar can fetch from, such as huggingface.co or another Hangar. See [Mirrors and Upstreams](mirrors.md). |
| **Mirror** | A Repo that tracks a Repo on an Upstream, keeping its ID and Commit SHAs. A **Partial mirror** holds the full history but only some files. |
| **Pull** | Fetching a Repo, or some of its files, from an Upstream into the Arsenal. See [Pulls and Dependencies](pulls.md). |
| **Dependency** | A link from one Repo to another it needs (code) or builds on (a base model). |
| **Job** | Background work, such as a Pull, an index or garbage collection. See [Jobs](jobs.md). |
| **Token** | A secret that a client presents as a bearer token or git password. See [Permissions](permissions.md). |
| **Collection** | A named, ordered list of Repos that a Namespace curates. |
