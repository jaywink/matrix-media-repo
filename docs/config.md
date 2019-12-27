# Media repository configuration

Simple deployments are going to make the best use of copy/pasting the sample config and using that. More complicated
deployments might want to make use of layering/splitting out their configs more verbosely. Splitting out the configs
also gives admins the opportunity to have more detailed per-domain control over their repository.

To make use of the layering, use the `-config` switch to point it at the directory containing your config files. In
Docker this takes the shape of setting the environment variable `REPO_CONFIG` to something like `/data/config`.

Config files/directories are automatically watched to apply changes on the fly.

## Structure

Config directories are shallow and do not recurse (this means all your files go into one giant directory). Files are
read in alphabetical order - it is recommended to prefix config files with a number for predictable ordering.

Files override one another unless they are indicated as being per-domain (more on that later on in this doc). Before
reading the config directory, the media repo will apply the default config, which is the same as the sample config.
As it loads each file, it overrides whatever the file has defined.

Simply put, given `00-main.yaml`, `01-bind.yaml`, and `02-datastores.yaml` the media repo will read the defaults, then
apply 00, then 01, then 02. The file names do not matter aside from application order.

## Per-domain configs

When using per-domain configs the `homeservers` field of the main config can be ignored. The `homeservers` option
is still respected for simple configuration of domains, though it is recommended to use per-domain configs if you're
splitting out your overall config.

Per-domain configs go in the same directory as all the other config files with the same ordering behaviour. To flag
a config as a per-domain config, ensure the `homeserver` property is set.

A minimal per-domain config would be:
```yaml
homeserver: example.org
csApi: "https://example.org"
backoffAt: 10
adminApiKind: "matrix"
```

Any options from the main config can then be overridden per-domain with the exception of:
* `homeservers` - because why would you.
* `database` - because the database is for the whole process.
* `repo` - because the listener configuration is for the whole process.
* `sharedSecretAuth` - because the option doesn't apply to a particular domain.
* `rateLimit` - because this configuration is applied before the host is known.
* `metrics` - because this affects the whole process.

To override a value, simply provide it in any valid per-domain config:

```yaml
homeserver: example.org
identicons:
  enabled: false
```

Per-domain configs can also be layered - just ensure that each layer has the `homeserver` property in it.