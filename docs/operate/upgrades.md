# Upgrade Hangar

Hangar follows semantic versioning. Read the [changelog](../project/changelog.md) before you upgrade, and back up first ([Backup and restore](backup.md)).

## One container

```sh
docker pull ghcr.io/jamesagarside/hangar:latest
docker stop hangar && docker rm hangar
docker run -d --name hangar ...   # the same options as before
```

A process in role `all` applies migrations when it starts.

## Several processes

1. Run the new version's `hangar admin migrate` once. Migrations only add to the schema within a major version, so processes of the previous version keep working while it runs.
2. Roll the `worker` processes, then the `web` processes. New processes report ready once the schema matches.

The Helm chart runs the migration in a pre-upgrade Job and rolls the Deployments for you:

```sh
helm upgrade hangar oci://ghcr.io/jamesagarside/charts/hangar --namespace hangar -f values.yaml
```

## Downgrades

Migrations do not run backwards. To go back to an earlier version, restore a backup made with it.
