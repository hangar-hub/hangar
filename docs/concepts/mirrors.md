---
description: "How Hangar mirrors Repos from Upstreams such as huggingface.co, keeping their IDs and Commit SHAs, with partial mirrors, tracking and pull-through."
---

# Mirrors and Upstreams

## Upstreams

An Upstream is a hub that speaks the Hugging Face Hub API. huggingface.co is built in, with the ID `huggingface`. Admins can add others, such as another Hangar Instance or a corporate mirror, under **Admin**, **Upstreams**, and turn any of them off.

Requests to an Upstream use your own upstream token if you saved one (**Settings**, **Upstream tokens**), otherwise the Instance token configured for that Upstream, otherwise none.

An Upstream can be on your local network, such as another Hangar on the LAN. Hangar connects to an Upstream's own host wherever it is, but follows a redirect to a private address only when the Upstream's own host is private too, so huggingface.co or another public Upstream cannot point Hangar at services inside your network. Hangar never connects to link-local or cloud metadata addresses. If Hangar reaches Upstreams through a proxy (`HTTPS_PROXY`), the proxy decides which addresses it can reach.

## Mirrors keep upstream IDs

A Pull of `jinaai/jina-embeddings-v3` creates the Repo `jinaai/jina-embeddings-v3` in Hangar, with the same Commit SHAs. That is what makes `HF_ENDPOINT` a drop-in swap.

- The first Pull into an unknown Namespace creates it as an Organisation owned by the Instance. Admins can hand it to a user or Organisation later.
- If a local Namespace already has the name, the Pull fails unless the admin sets a prefix for that Upstream (`upstreams.<id>.namespace_prefix`, such as `hf-`). Prefixed Mirrors such as `hf-jinaai/jina-embeddings-v3` no longer work as drop-in replacements.
- Mirrors are read-only to `git push` and the commit API, so they stay identical to the Upstream. A Namespace admin can **detach** a Mirror, which turns it into an ordinary Repo that keeps its history.

## Partial mirrors

A Partial mirror has the full git history but only some LFS objects, for example one GGUF quantisation. Files that are not held show as **Not in arsenal** in the UI and the CLI, and you can pull them later.

## Tracking

A Mirror can follow its upstream branch (`--track` on `hangar pull`, or **Track upstream** in the UI). It then checks for new Commits on a schedule, six-hourly by default, and pulls new versions of the files it already selected. A GGUF quant selection follows the quant, not a file name, so a renamed file of the same quant is still followed. Commits replaced by an upstream force-push stay reachable under `refs/hangar/previous/<time>`, so nothing pinned to them breaks.

## Pull-through

Pull-through is off by default. When it is on (`pull.pull_through.enabled`) and a Repo ID matches `pull.pull_through.allow`, a Hub API or git request for a Repo that is not in the Arsenal starts a Pull automatically, and a download of a file that is not present starts that file's Pull and streams it as it arrives.
