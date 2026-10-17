# Use git and git-lfs

Every Repo is a git repository over HTTPS, at the same URL as on the Hub plus `.git`:

```sh
git clone https://hangar.example.com/acme/my-model.git
git clone https://hangar.example.com/datasets/acme/reviews.git
```

Install [git-lfs](https://git-lfs.com) first (`git lfs install`), so large files download as content rather than pointers. git 2.39 or later is supported.

## Sign in

Use any username and a Token as the password. A credential helper keeps it:

```sh
git config --global credential.helper store   # or osxkeychain, manager, libsecret
git clone https://hangar.example.com/acme/private-model.git
# Username: anything, such as your username
# Password: hgr_...
```

A private Repo you cannot read answers as if it did not exist, so check the Token if a clone says the Repo was not found.

## Partial and shallow clones

```sh
git clone --depth 1 https://hangar.example.com/acme/my-model.git
GIT_LFS_SKIP_SMUDGE=1 git clone https://hangar.example.com/acme/my-model.git
cd my-model && git lfs pull --include "*.safetensors"
```

## Push

```sh
git lfs track "*.safetensors" "*.gguf"
git add .gitattributes model.safetensors
git commit -m "Add weights"
git push
```

Repos created through the Hub API start with a `.gitattributes` that already tracks the usual weight and archive types with LFS. Non-LFS files over `git.max_blob_size` (10 MiB by default) are refused, with a message that tells you to track them with LFS.

Commits pushed with git and Commits made through the Hub API are the same Commits, with the same SHAs.

Rules for pushes:

- Force-pushes to a Repo's default branch are refused unless `git.allow_force_push` is on; Instance admins can always force-push.
- Mirrors are read-only, so pushes to them are refused. Detach a Mirror first to make it an ordinary Repo.
- If two pushes to the same branch race, one wins and the other gets git's usual non-fast-forward rejection.
- `git push --atomic` fails with "the receiving end does not support --atomic push", because Hangar does not advertise the capability. Every push is applied all or nothing anyway, so push without `--atomic`.
- A push may update at most 10,000 branches and tags, and no object in it may be larger than 64 MiB (or `git.max_blob_size`, if that is larger) when unpacked.
- A Commit may hold at most 1,000,000 files and folders, and no path may be longer than 4,096 bytes.
- Names that a client could check out as `.git` (such as `.GIT` or `git~1`), the names `.` and `..`, and names with a backslash are refused.
