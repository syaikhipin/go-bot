# Configuration

Run `./scripts/start.sh --init-env` once to create `.env` for Docker and
`.env.eci` for ECI from [.env.example](../.env.example). Use
`--docker-env-file PATH` and `--eci-env-file PATH` to customize the output
paths. Initialization leaves existing files in place.

Startup defaults to `.env`, and `buildeci.sh` defaults to `.env.eci`. Both
accept `--env-file PATH` and require the selected file to exist. The launcher
applies explicitly set shared deployment variables, validates the runtime
configuration, and saves valid overrides atomically.
Compose mounts the profile into Controller. Controller connects to Redis over
the internal Compose network at `redis:6379`. For local inspection, Compose
also publishes Redis at `127.0.0.1:6379` on the Docker host.
The launcher runs the configuration tools with local Go when available, or
builds a Controller image to run them in Docker. `controller check` validates
the profile offline. `controller doctor` also performs a read-only runtime
preflight. In ECI mode, `check` validates the cache ID format and `doctor`
queries Alibaba Cloud for a usable cache. `buildeci` validates its own
image-cache inputs before an ECI cache ID exists.

Use one literal `KEY=value` per line. Blank lines and full-line comments are
allowed. Values can contain spaces, `#`, `$` and `=`. Private profiles use
mode 0600. Git and Docker exclude `.env`, `.env.*`, `*.env` and `*.env.*`
files, except for the root `.env.example` template. For other names, keep
private profiles outside the checkout or under the ignored `.local/` directory.
Create the directory first if passing a new path to `--init-env`.

## Runtime, storage and entry

| Variable | Meaning | Default or requirement |
|---|---|---|
| `HARNESS_RUNTIME` | Runtime provider | `docker` or `eci`, default `docker` |
| `HOST_IMAGE` | Local Host build tag | Initialization sets `go-harness/host:local` for Docker and `go-harness/host:eci` for ECI |
| `HARNESS_STORAGE` | Host filesystem | `container` by default. Docker automatically mounts an account volume. ECI uses the instance filesystem or can be set to `nas` |
| `NAS_MOUNT_TARGET` | Existing NAS mount target | Required for ECI with `nas` |
| `CONTROLLER_LISTEN` | Loopback address for a native Controller | `127.0.0.1:8080` |
| `ECI_REGION` | Alibaba region | `cn-hangzhou` |
| `ECI_VSWITCH_ID` | Existing vSwitch ID | Required for ECI |
| `ECI_SECURITY_GROUP_ID` | Existing security group ID | Required for ECI |
| `ECI_IMAGE_CACHE_ID` | Ready image cache ID | Required for ECI startup |

NAS uses the account-specific `/go-harness/<userId>`
subdirectory. The mount target is the deployment-specific input.

An HTTPS gateway handles TLS and forwards to the loopback HTTP listener. Preserve the original `Host` and `X-User-ID` headers, set `X-Forwarded-Proto` to `https` for HTTPS requests, and forward SSE without buffering. Controller uses the request host and scheme to check browser origins. Tailscale Serve supplies the host and scheme automatically. Restrict access at the gateway or through your private network.

Schedule uses the upstream defaults of 30 days and 200 records per task for
delivery history, and 60 seconds between deliveries. The optional tuning YAML
fields `schedule.delivery_history_days`, `schedule.delivery_history_records`
and `schedule.new_session_interval_seconds` change history retention and the
minimum interval for tasks that create a new session on every delivery.
Apply tuning when creating or replacing the Host.

## Model, search and image generation

Add one or more OpenAI-compatible Responses endpoints in Model settings.
Each endpoint has its own API key and ordered model IDs. New sessions use the
first model of the default endpoint. An individual session can switch models
during the lifetime of its Host. Child agents inherit the parent turn's route.
The Host uses a 1,000,000-token context budget.

In **Settings → Web search**, save a key for DeepSeek, Zhipu, or Doubao,
then choose **Use provider** on your preferred service. Chat and page fetching
work without a search key. In **Settings → Image generation**, configure the
key and model for Seedream, OpenAI Images, or Google Nano Banana and select it.
See the [provider guide](providers.md) for account and key requirements.

## Credentials

| Variable | Scope |
|---|---|
| `ALIBABA_CLOUD_ACCESS_KEY_ID` | Controller and buildeci, required for ECI |
| `ALIBABA_CLOUD_ACCESS_KEY_SECRET` | Controller and buildeci, required for ECI. The same pair is used with optional NAS |
| `REGISTRY_PUSH_IMAGE` | Full registry image reference for publishing or caching a Host image |
| `REGISTRY_USERNAME` | Private registry account for ECI cache creation, supplied with the password |
| `REGISTRY_PASSWORD` | Private registry password for ECI cache creation, supplied with the username |

`buildeci` takes the registry host from `REGISTRY_PUSH_IMAGE` and supplies
the username and password to Alibaba Cloud ECI to pull the private image.
The registry can be hosted by another provider. Local publication uses the
Docker CLI login.

### Redis account storage

Each account has one Redis Hash at `user:<userId>` with no expiration.

| Hash field | Format | Contents |
| --- | --- | --- |
| `desiredState` | String | `stopped` or `running` |
| `hostId` | String | Current Host instance ID |
| `hostToken` | String | Internal credential used by Controller to call the Host |
| `hostEndpoint` | String | Ready Host URL including its port |
| `llm` | JSON | `defaultEndpointId` and `endpoints`. Each endpoint stores its name, URL, API key and ordered model list |
| `webSearch` | JSON | Active `provider` and per-provider `entries` with API keys |
| `imageGeneration` | JSON | Active `provider` and per-provider `entries` with API keys and model names |
| `sessionModels` | JSON | Map from session ID to `{endpointId, modelId}` |
| `modelRevision` | Integer | Version of the model catalog and session selections synchronized with the Host |

Settings fields are added as they are configured. Host instance fields can
be empty. Compose persists Redis in a named volume. Protect that volume and
its backups as credentials. Start with an empty Redis database when switching
from the previous development storage format.

Controller supplies `HOST_ACCOUNT_ID`, `HOST_ID`, `HOST_API_TOKEN` and
`HOST_DATA_MOUNTED` when it starts the Host. It synchronizes the selected search
and image settings, model catalog and session selections over the authenticated
Host control channel. Open an account to start or reconnect its
Host. Use the stop request to stop it. Host-created scheduled sessions
store their model selections in the Host data directory.

## Optional YAML

[configs/default.yaml](../configs/default.yaml) lists runtime tuning defaults.
[configs/buildeci.yaml](../configs/buildeci.yaml) lists optional cache sizing,
retention and wait settings.
