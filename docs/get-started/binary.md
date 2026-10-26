---
description: "Install the hangar binary, both server and client, from a release archive or Homebrew on Linux, macOS or Windows, then run a server or use the CLI."
---

# Run the hangar binary

One binary is both the server and the client. Install it with any of these:

=== "Release archive"

    Download the archive for your system from the [releases page](https://github.com/hangar-hub/hangar/releases), check it, and put `hangar` on your `PATH`. Set `VERSION` to the release you want, without the `v`:

    ```sh
    VERSION=1.2.3
    curl -LO "https://github.com/hangar-hub/hangar/releases/download/v$VERSION/checksums.txt"
    curl -LO "https://github.com/hangar-hub/hangar/releases/download/v$VERSION/hangar_${VERSION}_linux_amd64.tar.gz"
    sha256sum --ignore-missing -c checksums.txt
    tar xzf "hangar_${VERSION}_linux_amd64.tar.gz" hangar && sudo mv hangar /usr/local/bin/
    hangar version
    ```

    On macOS, download `hangar_${VERSION}_darwin_arm64.tar.gz` (or `darwin_amd64` on an Intel Mac) and check it with `shasum -a 256 --ignore-missing -c checksums.txt`. To check that `checksums.txt` itself came from Hangar's release workflow, verify its signature as [Verify the image](../operate/security.md#verify-the-image) describes.

    Archives are built for Linux, macOS and Windows on amd64 and arm64, named `hangar_<version>_<os>_<arch>`: `.tar.gz` for Linux and macOS, and `.zip` for Windows. CI tests the server on Linux only; see [Tested versions and platforms](../reference/compatibility.md#platforms).

=== "Homebrew"

    ```sh
    brew install --cask hangar-hub/tap/hangar
    ```

    The cask removes macOS's quarantine attribute from the binary when it installs it, because the binary is not notarised, so Gatekeeper does not check it. `brew upgrade --cask hangar-hub/tap/hangar` installs new releases.

=== "go install"

    ```sh
    go install github.com/hangar-hub/hangar/cmd/hangar@latest
    ```

    This builds without the web UI embedded, which is fine for the CLI, and `hangar version` reports `dev`. Use a release archive or the image to run a server with the UI.

## Run a server

The server needs `git` 2.39 or later on the `PATH`.

```sh
hangar server --public-url http://localhost:8080 \
  --set server.data_dir="$HOME/hangar" \
  --set database.dsn="$HOME/hangar/hangar.db" \
  --set storage.fs.path="$HOME/hangar/blobs" \
  --set git.cache_dir="$HOME/.cache/hangar/git"
```

The first start generates an admin password and an Instance key and saves them in `secrets.yaml` in `server.data_dir`, here `$HOME/hangar/secrets.yaml`. It prints the admin password only when you run it in a terminal; otherwise read it with `grep bootstrap_admin_password "$HOME/hangar/secrets.yaml"`. It never prints the Instance key. Keys can also come from a YAML file (`--config hangar.yaml`) or `HANGAR_*` environment variables; see the [configuration reference](../reference/configuration.md).

## Use the CLI

```sh
hangar login --url http://localhost:8080
hangar pull hf-internal-testing/tiny-random-bert
```

See [The hangar CLI](../guides/hangar-cli.md).
