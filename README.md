**English** · [简体中文](README.zh-CN.md)

<p align="center">
  <img src="assets/harness-whale.png" width="184" alt="go-harness blue whale mascot">
</p>

<p align="center">
  <strong><a href="https://github.com/xyc999-ai/go-harness/releases/latest">Download Android APK</a></strong>
</p>

# go-harness

A Go implementation of DeepSeek Harness and a self-hosted runtime for personal
agents. Run locally with Docker for personal use, development, and testing.
Teams can use Alibaba Cloud Elastic Container Instance (ECI) to build their
own personal agent products like Muse, Grok Bot, and OpenAI dots, with a
dedicated Host for each account, optional persistent storage, and room for their
own frontends and service integrations. [Meta Muse](https://about.fb.com/news/2026/09/introducing-muse-personal-ai-agent/) · [Grok Bot](https://help.x.com/en/using-x/about-grok) · [OpenAI dots](https://openai.com/index/introducing-dots/)

- **Web research and browser automation.** Search the web through a configured provider. Use Chromium to navigate pages, interact with forms, and capture screenshots.
- **Image generation and editing.** Generate images from text or use uploaded images as references. Results appear directly in chat.
- **Office documents.** Read, create, and edit Word, PowerPoint, and Excel files with OfficeCLI, and deliver them as downloadable attachments.
- **Long-term memory and cross-session recall.** Save user preferences and agent notes per account, and search past conversations when a task needs earlier context.

The included web UI is a lightweight test client for chat, file browsing, and
downloads. Use the Controller's HTTP and server-sent events (SSE) API to
build your own mobile, desktop, or messaging interface. [HTTP API](docs/api.md) · [Minimal frontend example](examples/frontend-minimal/index.html)

[Configuration](docs/configuration.md) · [Deployment](docs/deployment.md) · [HTTP API](docs/api.md) · [Architecture](docs/architecture.md)

## Demos

### Mobile demos

The iOS client runs these Chinese requests against a local Controller and Docker
Host. The Word and PowerPoint clips end with the delivered documents open in WPS
Office.

<table width="100%">
  <tr>
    <td width="50%" valign="top" align="center">
      <h4>Browser research</h4>
      <a href="docs/showcase/mobile-research-demo.png"><img src="docs/showcase/mobile-research-demo.png" width="340" alt="Mobile chat checks the Stanford AI Index and delivers a chart, Markdown summary, and CSV"></a>
      <p>Check Stanford HAI’s AI Index and receive a chart, summary, and CSV in chat.<br><a href="docs/showcase/mobile-research-key.png">View full-size key frame</a></p>
    </td>
    <td width="50%" valign="top" align="center">
      <h4>Word report</h4>
      <a href="docs/showcase/mobile-word-demo.png"><img src="docs/showcase/mobile-word-demo.png" width="340" alt="Mobile chat creates and revises the AI industry report, followed by the final Word document in WPS Office"></a>
      <p>Create an industry report, then revise it with the survey context and a team pilot plan.<br><a href="docs/showcase/mobile-word-key.png">View full-size key frame</a></p>
    </td>
  </tr>
  <tr>
    <td width="50%" valign="top" align="center">
      <h4>PowerPoint briefing</h4>
      <a href="docs/showcase/mobile-slides-demo.png"><img src="docs/showcase/mobile-slides-demo.png" width="340" alt="Mobile chat creates and revises a three-slide deck, followed by all three slides in WPS Office"></a>
      <p>Refine the adoption figure and turn the final slide into a three-step pilot.<br><a href="docs/showcase/mobile-slides-key.png">View full-size key frame</a></p>
    </td>
    <td width="50%" valign="top" align="center">
      <h4>Image editing with two references</h4>
      <a href="docs/showcase/mobile-image-demo.png"><img src="docs/showcase/mobile-image-demo.png" width="340" alt="The mobile app sends a blue whale and coral swatch, previews a Seedream result, and saves it to Photos"></a>
      <p>Recolor the whale with Seedream, open the result full screen, and save it to Photos.<br><a href="docs/showcase/mobile-image-key.png">View full-size key frame</a></p>
    </td>
  </tr>
</table>

### Web demos

These demos use the included web UI and a local Docker Host. All requests are
in Chinese. The agent checks the [Stanford HAI 2026 AI Index
economy chapter](https://hai.stanford.edu/ai-index/2026-ai-index-report/economy)
in Chromium, then uses OfficeCLI to turn those findings into a Word brief and a
PowerPoint deck. Follow-up messages ask it to revise both deliverables.

<table width="100%">
  <tr>
    <td width="50%" valign="top">
      <h3>Browser research</h3>
      <img src="docs/showcase/ai-research-demo.gif" width="100%" alt="A Chinese research request, source attachments, and Stanford HAI’s original charts">
      <p>The agent checks industry data in Chromium and returns the original charts and a summary.</p>
      <a href="docs/showcase/ai-research-chat.webp">Chat capture</a> · <a href="docs/showcase/ai-index-source.webp">Source charts</a>
    </td>
    <td width="50%" valign="top">
      <h3>Word brief</h3>
      <img src="docs/showcase/ai-word-demo.gif" width="100%" alt="The full conversation from Word creation to revision, followed by the finished brief in WPS Office">
      <p>The agent creates a one-page brief, then adds survey context and a pilot recommendation. The final document is shown in WPS Office.</p>
      <a href="docs/showcase/ai-word-demo.gif">Open full-size demo</a>
    </td>
  </tr>
  <tr>
    <td width="50%" valign="top">
      <h3>PowerPoint briefing</h3>
      <img src="docs/showcase/ai-slides-demo.gif" width="100%" alt="The full conversation from PowerPoint creation to revision, followed by the three slides in WPS Office">
      <p>The agent clarifies the adoption figure and revises the final slide into a three-step pilot plan. The finished slides are shown in WPS Office.</p>
      <a href="docs/showcase/ai-slides-demo.gif">Open full-size demo</a>
    </td>
    <td width="50%" valign="top">
      <h3>Image editing with two references</h3>
      <img src="docs/showcase/ai-image-demo.gif" width="100%" alt="The uploaded whale and color swatch, the Seedream generation step, and the delivered coral whale">
      <p>Seedream uses the blue whale and a coral swatch to create one recolored 2048 × 2048 image in chat.</p>
      <a href="docs/showcase/ai-image-result.webp">Open the generated image</a>
    </td>
  </tr>
</table>

## Quickstart

Install Docker Engine or Docker Desktop with the Docker Compose plugin
(`docker compose`). From the repository root, run:

```sh
./scripts/start.sh --init-env
./scripts/start.sh
```

The first command creates private `.env` and `.env.eci` profiles and keeps
existing files. The second reads `.env`, builds the local Host image, and
starts Controller and Redis with Compose.

### Web browser

Open the browser after startup. Controller already serves the web app.

1. Open [http://127.0.0.1:8080](http://127.0.0.1:8080) and create an account by
   name. Opening the account starts its Host in a separate Docker container.
2. In **Model settings**, add an OpenAI-compatible Responses endpoint, API key,
   and at least one model ID. Then select **New chat** and send a message.
3. For web search, save a key for DeepSeek, Zhipu, or Doubao under **Settings → Web search**. For image generation, configure Seedream, OpenAI Images, or Google Nano Banana under **Settings → Image generation**. Choose **Use provider** to activate a saved service. [Provider setup](docs/providers.md)

### Phone via Tailscale

Tailscale connects your phone and the computer
running Controller over a private network called a tailnet. This gives the phone
a reachable Controller URL even when it is away from your home Wi-Fi.

1. **Download Tailscale on both devices.** Open the [official Tailscale download page](https://tailscale.com/download) on the computer running Controller and choose your operating system. On the phone, follow the iOS App Store or Android Google Play link on the same page. On Android, download the Go Harness APK from the **Assets** section of the [latest GitHub Release](https://github.com/xyc999-ai/go-harness/releases/latest).
2. **Sign in with the same Tailscale account on both devices.** Follow the VPN setup prompts and turn on Tailscale on the computer and phone. Signing in adds each device to your tailnet. On the computer, open **Devices** and check that both devices are online. Select the computer to see its MagicDNS name. You can also check both devices on the [Tailscale Machines page](https://login.tailscale.com/admin/machines).

   <img src="docs/showcase/tailscale-devices.png" width="800" alt="Tailscale on macOS showing two connected devices and the selected computer’s MagicDNS name. Account and device details are replaced with examples.">

   *Both devices are online. The selected computer’s MagicDNS name appears on the right. The screenshot uses example names and addresses.*

3. **Start Tailscale Serve on the computer running Controller.** Run this after starting Controller. Serve forwards HTTP requests from your tailnet to Controller on `127.0.0.1:8080`. Tailscale encrypts traffic between the devices, so this setup works without HTTPS certificates.

   ```sh
   tailscale serve --bg --http=8080 http://127.0.0.1:8080
   ```

   If your terminal cannot find `tailscale`, see the [Tailscale CLI setup guide](https://tailscale.com/docs/reference/tailscale-cli?tab=macos).
4. **Connect Go Harness on the phone.** Copy the full URL printed by Serve, such as `http://my-computer.example-tailnet.ts.net:8080`. Open it in the phone's browser to check that the go-harness Web UI loads. Then enter that URL and your go-harness account name in the mobile app and tap **Connect**. New accounts can set up a model under **Me → Models**. [Tailscale Serve guide](https://tailscale.com/docs/features/tailscale-serve)

If you already have an HTTPS endpoint that your phone can reach, enter its URL directly in the app.

**Choose a deployment.** Local Docker is suited to personal development and testing. Alibaba Cloud ECI can serve as the compute foundation for an enterprise personal-agent deployment, with one on-demand Host per account and optional NAS persistence. Keep Controller behind your organization’s authenticated access layer.

## What runs where

The **Controller** serves the Web UI and HTTP API, manages Hosts, and forwards
HTTP/SSE. **Redis** stores accounts, settings and Host state. The **Host** runs
the Go agent, calls model providers directly, and manages tools, v4 session
journals, memory, schedules, attachments, and the workspace. Each account has
its own Host, started when the account is opened.

The Host includes OfficeCLI, agent-browser with Chromium, Python and Node.js.
The OfficeCLI and agent-browser skills are installed into
`/root/harness/.dsh/skills/` on first use of each data directory. They can be
edited or deleted like any other skill. The model discovers skills by their
names and descriptions and loads their instructions on demand.

| Runtime | Where Host data lives | What **Stop Host** does |
| --- | --- | --- |
| Local Docker | Named volume for the account, mounted at `/root/harness` | Stops the container and keeps its files |
| ECI with `container` | Instance filesystem | Releases the instance and its files |
| ECI with `nas` | Account directory on NAS, mounted at `/root/harness` | Releases the instance and keeps NAS files |

Closing the browser leaves the Host running. For ECI, use **Stop Host** when
you no longer need the instance to stop its compute charges.

## Mobile app

The mobile app connects to the same Controller from iOS or Android. It provides chats, model switching, attachments, task status, and account settings. Enter your Controller URL and account name to connect. [Mobile development guide](mobile/README.md)

## Alibaba Cloud ECI

Run Controller and Redis with Docker Compose on a machine that can reach the
ECI private network, such as an ECS instance in the same VPC. Each account's
agent computer runs as its own ECI instance. Once the cloud resources and a
Ready image cache are in place, the same launcher starts the deployment.

### 1. Prepare Alibaba Cloud resources

| Resource | What to prepare |
| --- | --- |
| RAM credentials | Create a RAM user with an AccessKey ID and Secret. Grant the ECI API permissions needed to list, create and release instances, inspect caches, and create caches if you build them here. [Create an AccessKey pair](https://www.alibabacloud.com/help/en/ram/user-guide/create-an-accesskey-pair) · [Grant RAM permissions](https://www.alibabacloud.com/help/en/ram/user-guide/grant-permissions-to-the-ram-user) |
| Network | Choose the region, vSwitch and security group. Allow Controller to reach the ECI private IP on TCP 8080. For public registry, model and web access, configure outbound access such as NAT gateway SNAT on the ECI vSwitch. The launcher does not assign an EIP to each Host. [VPCs and vSwitches](https://www.alibabacloud.com/help/en/vpc/vpc-and-vswitch) · [Security group rules](https://www.alibabacloud.com/help/en/ecs/user-guide/security-group-rules) · [ECI internet access](https://www.alibabacloud.com/help/en/eci/user-guide/enable-internet-access) |
| NAS (optional) | Prepare a reachable NAS mount target. Before opening an account, create a writable `/go-harness/<userId>` directory on NAS for it. [Mount NAS on ECI](https://www.alibabacloud.com/help/en/eci/user-guide/mount-a-nas-volume) |
| Image cache | Reuse a Ready cache for a Linux AMD64 Host image in the selected region, or prepare a registry that the ECI cache builder can reach. [Manage ECI image caches](https://www.alibabacloud.com/help/en/eci/user-guide/manage-an-image-cache) |

### 2. Fill in the ECI profile

Run `./scripts/start.sh --init-env` if you have not initialized the profiles.
Edit `.env.eci`, which already selects `HARNESS_RUNTIME=eci`. Set
`ECI_REGION`, `ECI_VSWITCH_ID`, `ECI_SECURITY_GROUP_ID`,
`ALIBABA_CLOUD_ACCESS_KEY_ID` and `ALIBABA_CLOUD_ACCESS_KEY_SECRET`.

Keep `HARNESS_STORAGE=container` for instance storage. For persistence across
instance replacement, choose `HARNESS_STORAGE=nas` and set `NAS_MOUNT_TARGET`
to the mount target hostname. This mounts the account NAS directory before
the Host starts.

### 3. Prepare the Host image cache

If you have a Ready cache containing a compatible Host image, set
`ECI_IMAGE_CACHE_ID` and go to step 4. To build a new cache, set
`REGISTRY_PUSH_IMAGE` to a versioned registry reference such as
`registry.example.com/team/go-harness-host:v0.1.0`. For a private
registry, run `docker login <registry-host>` on the build machine if publishing
requires authentication, and set both `REGISTRY_USERNAME` and
`REGISTRY_PASSWORD` so Alibaba Cloud can pull it.

On the build machine, install Go 1.26 or newer and Docker with Buildx, then run:

```sh
./scripts/buildeci.sh prepare --env-file .env.eci
```

`prepare` builds the Linux AMD64 Host image, tags it locally with `HOST_IMAGE`,
pushes it to `REGISTRY_PUSH_IMAGE`, and requests an ECI image cache. If the
compatible AMD64 image has already been published, use
`./scripts/buildeci.sh cache --env-file .env.eci` to skip the local build and
push. Both commands wait for Ready and write `ECI_IMAGE_CACHE_ID` into the
selected profile. [Cache options and recovery](docs/deployment.md#eci-image-cache)

### 4. Start Controller and open an account

On the Controller machine, use the completed private profile:

```sh
./scripts/start.sh --env-file .env.eci
```

The launcher validates the deployment profile and starts Controller and Redis
with Compose. ECI Host instances start on demand from the configured cache
when you open an account. For a remote Controller, forward its loopback port
from your workstation:

```sh
ssh -N -L 8080:127.0.0.1:8080 user@controller-host
```

Open [http://127.0.0.1:8080](http://127.0.0.1:8080) and follow the account and
model setup in [Quickstart](#quickstart). Account names identify users without
passwords, so keep the Controller entry point private. For custom clients that
connect over a network, use your own authenticated gateway to the loopback API.
[Client access](docs/api.md)

## Configuration and verification

`start.sh` loads the selected profile, applies explicitly set deployment
environment variables, validates the merged configuration, and saves valid
overrides to the profile before launch. An explicitly empty value overrides a
saved value. Model, web search and image generation settings are saved per
account through the UI.

For an offline configuration check or a read-only runtime check, use:

```sh
go run ./cmd/controller check --env-file .env.eci
go run ./cmd/controller doctor --env-file .env.eci
```

Use `.env` for local Docker. `check` validates configuration, including
required fields. `doctor` also checks the selected runtime, such as the local
Host image or the Ready ECI cache. Opening an account exercises actual Host
creation, storage and Controller-to-Host connectivity.
[Configuration fields](docs/configuration.md) · [Troubleshooting](docs/troubleshooting.md)

## Dependencies

| Task | Install on the machine running it |
| --- | --- |
| Run the local or ECI deployment with `start.sh` | Docker, the Docker Compose plugin, and Bash. Go is optional because the launcher can run its configuration tools in Docker. |
| Prepare an ECI cache with `buildeci.sh` | Go 1.26 or newer and Bash. The `prepare` subcommand also needs Docker with Buildx. |
| Develop outside Docker | Go 1.26 or newer for the backend, a Redis instance for a native Controller, and Node.js 22.12+ with npm for frontend development. |

Compose runs Redis and builds the frontend. Python, Node.js, Chromium and
OfficeCLI for agent tasks are included in the Host image.

## Develop and extend

Build workflows around the tools the Host already provides, or add a connector
when the agent needs to query or act on a service. For example, a travel agent
could look up train or flight tickets, compare hotel rates and cancellation
terms, and draft an itinerary. To take bookings, add a provider API integration
and expose a separate confirmation step showing the chosen dates, total cost,
and cancellation terms before placing the order. Deployers supply the provider
credentials and bind access to the authenticated account.

- **Add a Skill** with a `SKILL.md` file to describe a repeatable workflow that uses existing tools. Put account-level skills under `/root/harness/.dsh/skills/` on the Host.
- **Add tools and selected third-party services** in a custom Host build. Implement the Go tool contract and include the tool in the Host's tool set. Keep external SDK and HTTP calls in `thirdparty/`, and use `providers/` for adapters to the runtime, model, search, or image provider contracts. [Tool interface](harness/agent/native_contract.go) · [Host tool assembly](harness/assembly/native_runtime.go) · [Provider guide](docs/providers.md)
- **Build the product interface** on HTTP and SSE. Connect your own chat frontend or messaging channel and handle account authentication, connection permissions, and user approval for actions such as booking or payment. [API guide](docs/api.md) · [Personal-agent integration research](docs/personal-agent-research.zh-CN.md)

[Documentation](docs/README.md) · [Contributing](CONTRIBUTING.md) · [Security policy](SECURITY.md) · [MIT License](LICENSE) · [Third-party notices](THIRD_PARTY_NOTICES.md)

## Community

Join the **go-harness** QQ group to ask questions, share setups, or give feedback.
Scan the QR code in QQ or search for group **710767129**.

<a href="assets/qq-community.jpg"><img src="assets/qq-community.jpg" width="260" alt="QR code for the go-harness QQ group, 710767129"></a>

## Upstream and acknowledgments

- [DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness) is the upstream project that this Go implementation is based on. [Compatibility notes](docs/compatibility.md)
- [Hermes Agent](https://github.com/NousResearch/hermes-agent) informed the design of local long-term memory and cross-session history search. [Memory design](docs/hermes-agent-memory-design.zh-CN.md)
- [OfficeCLI](https://github.com/iOfficeAI/OfficeCLI) supplies the Word, PowerPoint, and Excel tools. The Host also includes [LibreOffice](https://www.libreoffice.org/).
- [agent-browser](https://github.com/vercel-labs/agent-browser) provides the CLI and skill the Host uses to automate [Chromium](https://www.chromium.org/).

See the [open-source memory survey](docs/open-source-agent-memory-survey.zh-CN.md)
for additional designs evaluated, and [third-party notices](THIRD_PARTY_NOTICES.md)
for bundled components and their license terms.
