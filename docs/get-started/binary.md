---
description: "Install the hangar binary, both server and client, from a release archive or Homebrew on Linux, macOS or Windows, then run a server or use the CLI."
---

# Run the hangar binary

One binary is both the server and the client. Install it with any of these:

=== "Release archive"

    Download the archive for your system from the [releases page](https://github.com/hangar-hub/hangar/releases), check it, and put `hangar` on your `PATH`:

    ```sh
    curl -LO https://github.com/hangar-hub/hangar/releases/latest/download/checksums.txt
    curl -LO https://github.com/hangar-hub/hangar/releases/latest/download/hangar_<version>_linux_amd64.tar.gz
    sha256sum --ignore-missing -c checksums.txt
    tar xzf hangar_*_linux_amd64.tar.gz hangar && sudo mv hangar /usr/local/bin/
    ```

    Archives are built for Linux, macOS and Windows on amd64 and arm64.

=== "Homebrew"

    ```sh
    brew install --cask hangar-hub/tap/hangar
    ```

=== "go install"

    ```sh
    go install github.com/hangar-hub/hangar/cmd/hangar@latest
    ```

    This builds without the web UI embedded, which is fine for the CLI. Use a release archive or the image to run a server with the UI.

## Run a server

The server needs `git` 2.39 or later on the `PATH`.

```sh
hangar server --public-url http://localhost:8080 \
  --set server.data_dir="$HOME/hangar" \
  --set database.dsn="$HOME/hangar/hangar.db" \
  --set storage.fs.path="$HOME/hangar/blobs" \
  --set git.cache_dir="$HOME/.cache/hangar/git"
```

The first start prints the generated admin password and Instance key once. Keys can also come from a YAML file (`--config hangar.yaml`) or `HANGAR_*` environment variables; see the [configuration reference](../reference/configuration.md).

## Use the CLI

```sh
hangar login --url http://localhost:8080
hangar pull hf-internal-testing/tiny-random-bert
```

See [The hangar CLI](../guides/hangar-cli.md).
