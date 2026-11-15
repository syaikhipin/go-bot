# HTTP API

Controller serves the API at `127.0.0.1:8080` by default. Account-scoped
requests use the account name in the `X-User-ID` header. Opening an account
by name starts or reconnects its Host. Direct Host requests use the internal
Host bearer token. See [OpenAPI](../api/openapi.yaml).

The browser test client uses this public Controller API. Custom web, mobile,
and desktop clients can use the same HTTP endpoints and SSE streams. Keep
access to Controller within a trusted network because account names identify
users without password authentication.

Serve a custom browser UI from the same origin as the Controller API, or use the loopback development origin described in the [frontend guide](../frontend/README.md). Remote clients can use an HTTPS gateway such as Tailscale Serve. [Controller configuration](configuration.md#runtime-storage-and-entry)

| Method/path | Purpose |
|---|---|
| GET /healthz | Process liveness |
| GET /readyz | Application readiness |
| GET /v1/capabilities | Current model protocol/search capabilities |
| POST /v1/sessions | Create, returning sessionId |
| GET /v1/sessions?limit=&cursor= | Page through unarchived top-level sessions with an optional derived title |
| GET /v1/sessions?latest=true | Return only the newest unarchived top-level session by creation time, or an empty list |
| GET /v1/sessions/{id} | Active session state |
| DELETE /v1/sessions/{id} | Archive a top-level session and retain its data on disk |
| POST /v1/sessions/{id}/messages | JSON content or multipart content/files/images |
| GET /v1/sessions/{id}/events?after=&limit= | Durable events and nextCursor for active or cold sessions |
| GET /v1/sessions/{id}/transcript?before=&limit= | Web transcript, paged by whole turns for active and cold sessions |
| GET /v1/sessions/{id}/stream?view=chat | One Web SSE stream for the session, including the transcript projection, queue, permissions and questions |
| GET /v1/sessions/{id}/stream | Native SSE, with `Last-Event-ID` or `after` for both views |
| POST /v1/sessions/{id}/queue/{messageId}/steer | Move one queued user message into the current turn's next step |
| DELETE /v1/sessions/{id}/queue/{messageId} | Remove one queued user message |
| POST /v1/sessions/{id}/turns/{turnId}/cancel | Cancel queued/running turn |
| POST /v1/sessions/{id}/commands/compact | Request compaction |
| GET /v1/sessions/{id}/files?path=&offset= | Workspace directory listing |
| GET /v1/sessions/{id}/files/content?path= | Download a regular file using a workspace-relative path or an absolute Host path within the session workspace |
| GET /v1/sessions/{id}/attachments/{attachmentId}?name= | Download a verified attachment referenced by that Session. The optional name disambiguates display aliases |
| GET /v1/sessions/{id}/usage | Model usage |
| GET /v1/sessions/{id}/goal | Current goal |
| POST /v1/sessions/{id}/goal/resume | Resume by goalId/revision |
| GET/POST /v1/sessions/{id}/plan | Read or set active plan mode |
| GET/POST /v1/sessions/{id}/permissions | Read or set the current chat permission with `{"mode":"workspace-write"}` |
| GET /v1/sessions/{id}/questions | Pending/continued questions |
| POST /v1/sessions/{id}/questions/{callId}/answer | Answer (answers array) |
| POST /v1/sessions/{id}/questions/{callId}/cancel | Cancel pending question |
| GET /v1/sessions/{id}/schedules/{scheduleId}/history?limit=&before= | Delivery receipts |
| POST /v1/accounts | Create an account by name. A duplicate name returns 409 |
| GET /v1/host | Runtime state for the selected account |
| GET /v1/host/files?path=&offset= | Read-only directory listing under the selected account's Host data root |
| GET /v1/host/schedules | Host task catalog with timing, destination, status, revision and execution activity |
| POST /v1/host/schedules | Create a task with a typed time rule and optional destination |
| GET /v1/host/schedules/list | Page and filter by activity |
| GET, PATCH /v1/host/schedules/{scheduleId} | Read one task or edit it with a PATCH action (`update`, `pause`, `resume` or `delete`) |
| POST /v1/host/schedules/{scheduleId}/run | Deliver now without advancing the planned occurrence |
| GET /v1/host/schedules/{scheduleId}/history | Cursor page of delivered instructions and target sessions |
| DELETE /v1/host/schedules/ended | Remove ended tasks and their histories |
| GET /v1/host/schedules/stream | Stream task catalog invalidation events |
| GET /v1/host/schedules/limits | Read the configured minimum interval for a fresh session on each run |
| GET /v1/hosts/{hostId} | Runtime state for a matching instance of the selected account |
| POST /v1/host/start | Start or reconnect the selected account's Host |
| POST /v1/host/stop | Stop Docker or release ECI for the selected account |
| DELETE /v1/host/instance | Delete compute and retain the account and external storage |

For `POST /v1/accounts`, send `{"userId":"alice"}`. Account creation returns
the name with HTTP 201. Model settings are available before the Host starts.

New chats start with `danger-full-access`. The other modes are
`workspace-write` and `read-only`. A selection persists in the Host's session
journal. A running turn, child, or managed Job can make a change return 409.
A Host that cannot establish the restricted Linux policy returns an error.

The chat SSE stream emits `permissions.state` with `mode` and `approval`.
Pending approvals appear in `questions.state` with an `approval` object showing
the tool, original tool call ID, reason, operation detail and optional target
mode. The pending item's top-level `callId` identifies the decision.
POST an answer like `{"answers":[{"id":"approval","selected":["Allow once"]}]}`
to allow the pending operation. Use `"Deny"` to reject it. An approval never
changes the saved mode. A turn without an interactive chat stream fails a
request for approval closed, including scheduled work targeting an unopened
chat. A pending decision expires after two minutes.

The chat stream and transcript expose successful `present` calls as
`assistant/message` events with `delivery: true` and `file` blocks. A block
with `file.source: "presented"` carries `file.path`, the absolute Host path
of the file. URL-encode it as the `path` query parameter to that session's
`/files/content` endpoint. The Host serves the current file and checks that
the path stays within the session workspace. The chat renders the card as
soon as the event arrives. Attachments use the separate attachment endpoint.

| Model settings endpoint | Behavior |
| --- | --- |
| GET /v1/settings/models | Returns `defaultEndpointId` and endpoints keyed by ID, with `hasApiKey` |
| POST /v1/settings/models/endpoints | Accepts `name`, `baseUrl`, `apiKey`, and ordered `models` |
| PATCH /v1/settings/models/endpoints/{endpointId} | Updates supplied fields. Omitted Key is retained |
| DELETE /v1/settings/models/endpoints/{endpointId} | Removes the endpoint and clears it if it was the default |
| PUT /v1/settings/models/default | Accepts `{"endpointId":"…"}`. An empty ID clears the default |
| GET /v1/sessions/{id}/model | Returns `selection` and `status` |
| PUT /v1/sessions/{id}/model | Accepts `{"endpointId":"…","modelId":"…"}` |

The first configured endpoint becomes the account default. Its first model
is used when creating a session. A missing or invalid default produces
`409 model_default_required` before the Host creates a session. A session
selection applies at the next turn boundary. Settings requests wait for a running Host to apply saved changes. A response
of `503 settings_saved_host_apply_failed` means Redis saved the change and the
Host could not confirm it. Reopen the account to apply the saved settings or
submit the update again. Public model settings responses omit API keys.

The account endpoints `/v1/settings/web-search` and
`/v1/settings/image-generation` share a settings contract.

| Request | Behavior |
| --- | --- |
| `GET` | Returns the active selection in `provider`, the available providers, and per-provider `entries` with `hasApiKey` and an optional image model. |
| `PUT {"provider":"zhipu","apiKey":"…","activate":false}` | Saves the key without changing the selection. Provide `model` when first saving an image provider. Omit the key to retain it. |
| `PUT {"provider":"zhipu","activate":true}` | Selects a provider using its saved settings. An unconfigured provider cannot be selected explicitly. |
| `DELETE ?provider=zhipu` | Removes that key. If it was in use, the tool is unavailable until a key is added or another provider is selected. |

For existing API clients, omitting `activate` keeps the original save-and-select
behavior. The Controller stores the selected ID and credentials separately for
each capability and account. Only the selected credentials are sent to the
running Host. Public responses omit keys. A response of
`503 settings_saved_host_apply_failed` means the setting was saved but the
running Host did not confirm it. Retry or reopen the account.

The Host file listing starts at `/root/harness`. Pass a relative directory
path and use `nextOffset` to read the rest of a directory. The response includes
`rootPath`, `path`, `files` and `truncated`. Entries contain `name`, `path`,
`isDir` and `size` in bytes. The Controller provides the storage kind in
`X-Harness-Storage` and the Docker volume name or NAS subdirectory in
`X-Harness-Storage-Source` when applicable. The endpoint lists metadata and
works even when none of the account's sessions are active.

History and workspace file reads work for cold sessions. The chat stream can
stay open while a session is cold and follows it when the Host activates it.
Unknown and archived IDs return 404 for session operations. The events cursor
advances over filtered rows, so continue until `nextCursor` is null, even
when an intermediate page contains no public events. Durable inbox admission
returns messageId before a turn is assigned. Persisted event sequence numbers are
the reconnect cursor; live attempt chunks have a separate transient sequence.

The first transcript page includes `events`, `throughSeq`, `before`, `queue`
and `questions`. Pass `before` to get the previous page. Start the chat SSE
with `after=throughSeq` and use the last processed durable event sequence to
reconnect. The projected `session` events carry ordered user and assistant
blocks, tool outcomes and turn boundaries. The transient frames
`assistant-stream-baseline` and `assistant-stream` cover the active model
attempt. `queue.state` and `questions.state` carry current snapshots.
`chat.idle` indicates a session has no active runtime.

A successful message POST returns 202 with a `messageId`. Queue operations
use that ID and return 204 on success. A queue entry that is no longer
pending returns 404. An attempt to steer while there is no active turn
returns 409. The stream stays open after a turn ends.

Session execution is one turn at a time with a durable inbox for later inputs.
Question timeout can let the current turn continue; late answers are new user
events. Cancellation does not undo filesystem side effects. No message POST is
automatically replayed by controller after a connection failure.

Archiving returns 204, including repeated deletion of the same archived ID.
It hides the session after restart while retaining the log, workspace, and
shared attachments. A busy session or a pending Schedule target returns 409.

The bundled Web UI uses the Controller's own origin. Browser requests from
other origins are denied by default. For local Vite development, pass
`--web-dev-origin http://127.0.0.1:5173` to the Controller. CLI requests
without a browser origin still work. See the [frontend README](../frontend/README.md).
