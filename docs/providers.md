# Extending capabilities

One module, three boundaries:

1. `runtime.Driver`, `web.SearchProvider` and agent model contracts describe the
   capability the application needs.
2. `providers/` implements policies and adapts those contracts.
3. `thirdparty/` is our SDK/API integration code. Actual libraries use Go modules.

## Runtime

Add `providers/runtime/<name>`. Implement Validate, Preflight, Ensure, Inspect,
Stop and Delete, and declare supported storage types. Register its factory in
`internal/controller.Driver`, add strict config fields and credential validation,
and test without provisioning cloud resources.

Ensure must be idempotent for the persisted Host ID (`Spec.RunID`). On uncertain creation,
query platform resources or reuse the same idempotency token; do not blindly
create a second computer. Instances carry RunID and DeploymentID. The Controller
uses the account name as DeploymentID.
Return an endpoint reachable from the controller, then let the controller verify
HTTP readiness and identity. Cloud Running is not application readiness.

Stop and Delete have distinct data semantics. NAS is a storage option, not a
runtime type. SDK-specific DTOs stay behind thirdparty wrappers. Do not add Redis
or cross-Host session execution to a driver.

Declare `RestartStopped` and `NativeStop` capabilities. Docker supports both;
ECI supports neither. For ECI, the Controller attempts authenticated Host
shutdown, then deletes the instance and waits for the runtime to confirm release.

For providers with a separate image preparation step, add a command under
`cmd/build<provider>`. It can reuse `internal/imagebuild` for image publication.
The runtime driver consumes the prepared artifact through its own configuration.

## Search

Add `providers/web/<engine>` implementing web.SearchProvider and use thirdparty
for external calls. Register the factory in `providers/web/registry.go`. The
Host uses that registry to resolve the provider.

| ID | Service | Key |
| --- | --- | --- |
| `deepseek` (default) | DeepSeek Search | DeepSeek search key |
| `zhipu` | BigModel Web Search API, using `search_std` | BigModel platform key authorized for Web Search. [Official docs](https://docs.bigmodel.cn/cn/guide/tools/web-search) |
| `doubao` | Doubao Search Custom, text-to-web | Dedicated Doubao Search Custom key. [Official docs](https://docs.volcengine.com/docs/networked-search/networked-search?lang=zh) |

Search and extraction remain distinct:
search-only providers are not forced to fetch pages. Results use URL/title,
snippet, summary and optional publication time. Preserve empty results and unknown
fields honestly. Honor context, bound responses, reject credential redirects and
test 401/429, cancellation, malformed responses and unknown providers.

Each provider receives the account search key. The factory
returns a provider implementing `web.SearchProvider`. `harness/tools` owns
query merging, rendered text and replay metadata for every backend. See the [web tool parity record](web-tools-parity-plan.zh-CN.md).

## Models and tools

Accounts configure endpoints and choose a model per session. A new protocol adapter must
test streaming, tool-call arguments, usage, cancellation, provider errors and
uncertain outcome handling. Read-only model/search clients belong to Host, not
controller. Preserve durable v4 facts and tool-intent checkpoints.

## Image generation

| ID | Service | Suggested model |
| --- | --- | --- |
| `seedream` (default) | Volcengine Ark Images API | `doubao-seedream-5-0-260128` |
| `openai` | OpenAI Images API with an OpenAI API key. [Official API reference](https://developers.openai.com/api/reference/resources/images/methods/generate) | `gpt-image-2` |
| `nanobanana` | Gemini Developer API with a Google AI Studio API key. [Official API reference](https://ai.google.dev/api/generate-content) | `gemini-3.1-flash-image` |

The model field is editable. `generateimage` accepts a text prompt and an
optional `reference_image_ids` array. Upload reference images in chat, or
ask the agent to reuse image attachments from earlier replies. The agent can
also turn a workspace image into a reference with `readimage`. A request
can include up to 10 normalized attachments totaling 12 MiB and returns one
image through the attachment store. For image-to-image, use a model that
accepts image inputs.

Seedream sends references as data URLs in the Ark request. OpenAI switches
to the Images edits endpoint. Nano Banana sends inline image parts alongside
the text prompt. [Ark image generation API](https://docs.volcengine.com/docs/ark/image-generation-api?lang=zh) · [OpenAI image edits](https://developers.openai.com/api/reference/resources/images/methods/edit) · [Gemini image generation](https://ai.google.dev/gemini-api/docs/generate-content/image-generation)

Add a package under `providers/image/<name>` and register its factory and
descriptor in `providers/image/registry.go`. The descriptor supplies the
settings label and suggested model ID. The adapter receives an account's
selected model and API key, and returns one image plus its MIME type. The Host
uses the account's selection for subsequent `generateimage` calls. The tool
stores the image through the shared attachment store and returns an image block.

Keep request timeouts, response size limits and error mapping inside the
adapter. Test the provider with a local HTTP fixture, including invalid
credentials, throttling, cancellation and malformed image data. Avoid
retrying generation when the upstream outcome is unknown.

The Controller stores search and image settings in the `webSearch` and
`imageGeneration` JSON fields of the Redis Hash `user:<userId>`. Each field
contains the active `provider` ID and an `entries` map with keys and, for
images, models by provider ID. Use
**Save settings** to store or update credentials and **Use provider** to switch
the active service. The Host obtains the current selection at startup and on
changes to the active provider. Public settings return credential presence
only. Saving a setting does not call the upstream API or incur provider charges.
