# Get help with Hangar

Start with the [documentation](https://hangar.page/). It has guides for every client and for running Hangar, and [Troubleshooting](https://hangar.page/latest/operate/troubleshooting/) covers the most common problems. `hangar admin doctor` checks the configuration, database, Blob store, presigned URLs, clock and git of an Instance.

## Ask a question or report a problem

Open an [issue](https://github.com/hangar-hub/hangar/issues/new/choose) and choose the form that fits:

- **Question**: how to set up or use Hangar.
- **Bug report**: something doesn't work as the documentation describes.
- **Compatibility gap**: a client that works with huggingface.co, git, Ollama or LM Studio doesn't work with Hangar. [Hub API compatibility](https://hangar.page/latest/reference/hub-api-compatibility/) lists what Hangar supports.
- **Feature request**: something Hangar should do.

Include the output of `hangar version`, how you run Hangar (Docker, Docker Compose, Helm or the binary), your database and Blob store, and any logs. Remove tokens, passwords and other secrets first.

## Report a security problem

Don't open a public issue. Report it privately, as [SECURITY.md](SECURITY.md) describes.
