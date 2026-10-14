# Repos and Revisions

A Repo is a real git repository with a type, `model` or `dataset`, and an ID of the form `namespace/name`, such as `acme/sentiment-small` or, for a dataset, `datasets/acme/reviews` in URLs. A model and a dataset can share the same `namespace/name`.

The Hub HTTP API, git and the web UI are three views of the same Commits: a file uploaded with `huggingface_hub` appears in `git log`, and a `git push` shows up in `model_info`.

## Revisions

A Revision names a Commit: a branch (`main`), a tag (`v1.0`), a full Commit SHA, or a pull request ref (`refs/pr/1`). Commit SHAs are git SHA-1 values, as on huggingface.co, and a Mirror keeps the upstream SHAs, so a Revision pinned in code resolves to the same files.

Datasets also have `refs/convert/parquet`, the branch that holds their [Parquet export](../guides/datasets.md#parquet-export).

## Large files

Files that `.gitattributes` tracks with LFS (weights, archives, Parquet and so on) are stored whole in the Blob store and addressed by SHA-256. git trees hold a small pointer instead. An object that already exists anywhere in the Instance is never stored twice, so two Repos with the same weights use the space once.

Hangar never chunks or transforms your files: an admin can recover any file from the Blob store with standard tools, at `lfs/sha256/<first 2>/<next 2>/<sha256>`.

## Visibility

| Visibility | Who can read |
|---|---|
| `public` | Anyone signed in, and anonymous users if the Instance allows anonymous reads (`security.anonymous_read`). |
| `internal` | Any signed-in user. |
| `private` | Members of the Namespace and the Repo's collaborators. |

## Renames, transfers and deletes

Renaming a Repo or moving it to another Namespace keeps redirects from the old ID, for the HTTP API, git and the UI. Deleting a Repo is a soft delete: an admin can restore it for `repos.restore_window` (7 days by default), after which it is purged.
