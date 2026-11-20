# Deployment

## Default local Docker

Start Docker with Docker Compose. Run from the repository root.

```bash
./scripts/start.sh --init-env
./scripts/start.sh
```

Initialization creates `.env` and `.env.eci`. The launcher requires an
existing profile and reads `.env` by default. Every Docker launch builds the
Host image locally with the `HOST_IMAGE` tag and starts Controller and Redis
with Compose. Open http://127.0.0.1:8080 and
create an account by name and add a model endpoint in Model settings. Add a DeepSeek search API key in Web search settings to enable searching. Opening an account starts or
reconnects its Host. You can also prepare `.env`, build the Host image
with `make image` and run `docker compose up -d --build`.

The Host image, including its tools and bundled skills, is built from
`build/Dockerfile.host`. Compose builds the Controller and its embedded Web UI
from `build/Dockerfile.controller` and runs the published `redis:8-alpine` image.
[Host Dockerfile](../build/Dockerfile.host) · [Controller Dockerfile](../build/Dockerfile.controller) · [Compose](../compose.yaml)

Go Bot uses a separate Compose project, Redis volume, and Host storage from
Go Harness. To switch on the same machine, stop the old deployment so its
ports are free, then initialize fresh environment profiles for Go Bot. Keep
the old profiles and storage separately if you need the previous deployment.

```bash
./bin/controller check
./bin/controller doctor
./bin/controller host status --user alice
./bin/controller host stop --user alice
./bin/controller host start --user alice
./bin/controller host delete --user alice
```

Build the CLI with `make build`. `doctor` checks the runtime without creating
compute. Account commands use the running Controller. The API accepts the
same account name through the `X-User-ID` header.

`docker compose down` shuts down Controller and Redis and retains the Redis
volume. Stop or release each Host through the Controller before bringing
Compose down if you want to stop its compute. `docker compose down --volumes`
also removes the Redis volume and the accounts stored in it.

## Storage choices

Controller-managed Docker Hosts mount `go-bot-<userId>-data` at `/root/harness`
automatically. After building an updated image, release the old Host with
`controller host delete --user alice` and reopen the account. The replacement
container uses the new image and mounts the same volume. The volume is local to
the Docker daemon. ECI can retain its data across instance replacement with NAS.

The account's long-term memory files live under `/root/harness/.dsh/memories`.
The Host keeps its SQLite FTS5 index at `/var/lib/go-bot/session-search.sqlite`
in both runtimes and rebuilds it from the account's v4 journals after replacing
the container.

For an older Docker Host that stored data in its writable layer, stop it and
copy its `/root/harness` contents into the account volume before releasing it.
Use `docker inspect` to check the mounts on an existing container.

Controller launches each Docker Host as a separate container outside Compose. Docker
maps its port 8080 to a dynamic host port. The containerized Controller
uses `host.docker.internal` and the discovered port. Direct Host requests
require its internal bearer token. A native Controller uses a dynamic
loopback port for each Host.

Hosts remain running after a client disconnects. Use **Stop Host** or the
Controller stop API to stop an account's Host. Docker keeps its stopped
container. ECI releases its instance. Opening the account again resumes
Docker or creates a new ECI instance. Use the stop API again if a request
returns an error and you still want to stop the Host.

## ECI image cache

See the [ECI prerequisites and setup walkthrough](../README.md#alibaba-cloud-eci)
for RAM, VPC, security-group and NAS setup.

Run `./scripts/start.sh --init-env` to create both profiles. Edit the ECI
profile and fill in the shared Alibaba AccessKey pair, region, vSwitch
and security group. For cache creation, set `REGISTRY_PUSH_IMAGE` to a registry-qualified
reference with a unique version tag. If the registry requires authentication,
set `REGISTRY_USERNAME` and `REGISTRY_PASSWORD` for the ECI pull. The registry
host is derived from the image reference. The publish command uses the current Docker login.

```bash
./scripts/buildeci.sh
./scripts/start.sh --env-file .env.eci
```

Before starting Controller, set `HARNESS_RUNTIME=eci` in the same profile.
Configure search per account in the Web UI after deployment. The default
`prepare` mode builds and publishes a Linux AMD64 Host image, creates the ECI
cache, waits for `Ready` and writes `ECI_IMAGE_CACHE_ID` into the profile.
Use `./scripts/buildeci.sh cache` for a prepublished image. Both modes use
`.env.eci` by default and accept `--env-file PATH`. The script reports an error if the selected
profile is missing. Cache creation provisions
Alibaba Cloud resources. See [Alibaba Cloud cache billing](https://www.alibabacloud.com/help/en/eci/product-overview/image-caches).

The default wait is 90 minutes. Set `wait_timeout` in the YAML passed with
`--config` to adjust it. Rerun the same operation after a timeout or lost
create response to resume the saved cache operation.
Use `./scripts/buildeci.sh cache --operation <new-name>` to
create a replacement cache for the same published image. Build and cache
outputs include the image reference and any known digest. Use a new
version tag for every publication.

You can set an existing Ready `ECI_IMAGE_CACHE_ID` directly and start the
controller without running `buildeci`. The ECI driver resolves the cache's
single Host image and configures cache-only startup. The launcher checks
configuration before Compose starts. Run `controller doctor --env-file .env.eci`
to check Alibaba Cloud access and cache readiness before opening an account.
The controller needs
private-network access to the ECI port 8080. The ECI Host needs network
access to the configured model and search services. For a private ECI Host that
needs outbound internet access, cover its vSwitch with a NAT gateway SNAT rule.
[Alibaba Cloud ECI networking](https://www.alibabacloud.com/help/en/eci/user-guide/enable-internet-access)

For persistent NAS set `HARNESS_STORAGE=nas` and `NAS_MOUNT_TARGET` to an
existing mount target hostname. Create `/go-bot/<userId>` on NAS and allow
the Host to write to it before opening that account. The driver mounts it at
`/root/harness`. Session journals live at
`<nasPath>/.dsh/sessions/<project-key>/<session-id>/session.v4.jsonl`.
Schedules, attachments and user skills share that account's NAS directory.
OfficeCLI and agent-browser are optional plugins installed from the plugin market.
Their original Skills are contributed from the installed plugin packages; the Host
no longer copies a first-start skill seed into user directories.
ECI container storage uses the writable layer for the lifetime of the instance.

See [validation](validation.md) for real Docker coverage and the remaining
cloud checks.

## Plugin and image dependencies

The base Host supplies Go, Node/npm/pnpm, Python, Chromium, LibreOffice and general
file-processing libraries. OfficeCLI and agent-browser CLIs are installed when the
user installs their plugins, rather than being preinstalled in the runtime image.

A disposable build stage exports the upstream Skills using official installers and
includes their complete resources in each wrapper package. Enabling a plugin
registers its Skills from the package; disabling it withdraws that contribution.
User-created Skills remain in the existing user Skill roots.

OfficeCLI installation runs `npm install -g @officecli/officecli`, without an added
version/checksum lock or an automatic-update override. Plugin package updates are
separate from CLI version selection. Installation policy belongs to the plugin;
the Host only coordinates package installation and reports the result.

Other build dependency versions are recorded in `build/dependencies.lock.yaml`
and the respective package lockfiles. No third-party binary or SDK source is
committed.
