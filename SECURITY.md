# Security policy

## Report a vulnerability

Please report security problems privately, through GitHub's private vulnerability reporting: open the repository's **Security** tab and select **Report a vulnerability**. Do not open a public issue, discussion or pull request for a vulnerability.

Include what you found, how to reproduce it, the version or commit, and the impact you expect. You should get an answer within a week. Once a fix is ready, we publish a GitHub security advisory and a release, and credit you if you want to be credited.

## Supported versions

Security fixes go into the latest minor release. Upgrade to it to get them; see [Upgrades](https://jamesagarside.github.io/hangar/operate/upgrades/).

## Scope

In scope: the `hangar` binary and container image, the Helm chart and the Compose examples in this repository. Out of scope: vulnerabilities in models or datasets that users store in Hangar (for example malicious pickle files, which Hangar scans and flags but does not run), and denial of service through traffic volume, which a reverse proxy in front of Hangar should limit.

## Hardening

See [Security hardening](https://jamesagarside.github.io/hangar/operate/security/), which also explains how to verify image signatures.
