# Web 会话页与流式内容块方案

> 历史设计与调研记录。当前行为、配置和命令以[文档索引](README.md)中的指南为准。

## 发送消息和接收回复

沿用现有会话级 SSE。浏览器打开会话时订阅 `GET /v1/sessions/{id}/stream?view=chat`，同时用普通 HTTP 发送消息和操作队列。Controller 把请求发给该账号的 Host，并流式转发响应。Host 保存消息、执行 Agent、发布会话状态。浏览器用 Host 推送的状态渲染页面，Controller 无需保存队列。 [现有消息入队](../internal/httpapi/native_worker_http.go#L269) · [现有 SSE 写入](../internal/httpapi/native_worker_http.go#L420)

模型文本、工具调用、工具结果、待回答问题和队列变化共用当前会话的 SSE。模型供应商的 SSE 随单次模型调用结束，浏览器的 SSE 持续接收同一轮后续步骤和下一轮事件。发送第二条消息仍用同一个 POST 接口，共用已经打开的 SSE。一轮结束时只更新轮次状态。离开会话或关闭页面时，浏览器关闭订阅。网络中断时重新订阅，Host 上已接收的工作继续运行。 [公开工具事件](../internal/httpapi/chat_transcript.go#L144)

| 用户操作 | Host 行为 | 页面表现 |
| --- | --- | --- |
| 空闲且队列为空时发送 A | `POST /v1/sessions/{id}/messages`。Host 先保存消息并返回 `202`，Agent 随后领取并开始一轮 | 收到 `202` 就在正文显示 A 的用户气泡，跳过排队提示。收到轮次开始后显示「生成中」 |
| A 执行中发送 B | 同一个 POST 将 B 写入 `next-turn`，A 继续执行 | B 进入输入框上方的队列，当前轮照常显示 |
| 点击 B 的「插话」 | `POST /v1/sessions/{id}/queue/{messageId}/steer`。Host 将该消息转到 `next-step`，在 A 的下一步纳入上下文 | B 显示「等待插话」，Host 领取后出现在 A 的执行过程里 |
| 点击 B 的「删除」 | `DELETE /v1/sessions/{id}/queue/{messageId}`。Host 移除这一条待执行消息 | 队列中移除 B，A 继续执行 |

如果提交时网络断开，保留草稿并同步会话，交由用户决定是否重发。

## 运行中的追加消息

DeepSeek Harness 的 Agent 用 `followup()` 将普通消息加入 `next-turn`，用 `steer()` 将插话加入 `next-step`。它的 Web 版也通过 Host 提交消息和操作队列。用户把已排队的消息转为插话时，Host 将它移到下一步队列，保留消息内容和身份，其余排队消息保持原有顺序。队列状态由 Host 发给页面，重连后也能看到。 [DeepSeek Harness Agent](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/packages/core/agent/README.md#drive-an-agents-conversation) · [DeepSeek Harness Web Queue / Steer](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/.agents/notes/archived/feature/2026-07-30-web-queue-steer-action.md#agent-and-lifecycle-boundary)

Go Host 已有两个队列。当前浏览器提交的消息走 `next-turn`，Host 在当前轮结束后按队列顺序领取，浏览器继续通过同一条 SSE 接收。前端的队列视图复用 Host 的持久收件箱，在 Agent 内完成插话转换和单条删除。Go Host 用一条带 `moveToNextStep` 的收件箱事件记录跨队列移动，恢复时可以完整重放。Host 用现有消息 ID 定位被操作的条目。插话在当前模型请求或工具执行让出下一步时生效，当前工作照常运行。Host 确认操作后，通过 SSE 发出最新队列。 [Host 收件箱](../harness/session/inbox.go#L9) · [接收消息和插话](../harness/agent/native_loop.go#L328) · [步骤领取](../harness/agent/native_loop.go#L568)

Host 接收消息和 Agent 领取消息是两个时点。空闲且队列为空时，前端把首条成功提交的消息直接显示为普通用户气泡。用提交响应中的 `messageId` 合并后续 SSE 正文事件，消息只显示一次。即使先收到包含这条消息的队列快照，也跳过它的排队提示。

只有当前有轮次执行时，输入框上方才显示等待中的消息。输入框仍可发送，队列按顺序展示内容摘要及「插话」「删除」按钮。点击「插话」后展示「等待插话」，领取时用正文中的用户气泡接替这条提示。普通排队消息被领取后出现在新一轮的开头。操作失败时显示简短提示。

从 Host 发出轮次开始到轮次结束，助手区域统一显示「生成中」。工具进度在对应卡片内显示，Agent 提问继续在输入框上方显示浮层，回答走专用接口。单条删除只影响该排队消息。轮次停止仍走现有接口，它会取消当前轮并清空待处理消息。 [轮次取消实现](../internal/hostruntime/native_runtime.go#L647)

## 会话内容怎么组织

Host 已经能生成有序的 `content[]`。用户消息支持文本、图片和文件。助手消息保留文本与工具调用的顺序，工具结果通过 `toolCallId` 与调用关联。模型的实时响应也已经拆为块开始、增量和块结束。会话页按块渲染，保留同一条消息内的文本和工具调用顺序。 [用户内容块](../internal/httpapi/native_worker_http.go#L269) · [助手内容块](../harness/agent/native_loop.go#L871) · [工具结果](../harness/agent/native_loop.go#L1084) · [模型流式块](../harness/agent/llm_stream_projection.go#L36) · [当前前端解析](../frontend/src/chat/ChatContent.tsx)

页面按「轮次 → 有序消息与工具活动 → 有序内容块」组织。普通用户消息放在轮次开头，插话放在当前轮实际进入的步骤，和助手文字、工具执行按发生顺序显示。定时任务等没有直接用户输入的轮次显示可识别的来源，例如「定时任务」。Host 依据持久日志和轮次边界组装这一结构。

```ts
type ContentBlock =
  | { id: string; type: 'text'; text: string }
  | { id: string; type: 'image'; attachment: AttachmentRef }
  | { id: string; type: 'file'; name: string; description?: string; file: FileRef }
  | { id: string; type: 'tool'; callId: string; name: string; label: string;
      status: 'in_progress' | 'succeeded' | 'failed' | 'cancelled'; detail?: string }

type AttachmentRef = {
  attachmentId: string
  name: string
  mediaType: string
  bytes?: number
  width?: number
  height?: number
}

type FileRef =
  | { source: 'attachment'; attachmentId: string }
  | { source: 'presented'; path: string }

type Turn = {
  turn: number
  status: 'running' | 'completed' | 'cancelled' | 'failed'
  source: 'user' | 'schedule' | 'background'
  items: (
    | { id: string; type: 'user'; blocks: ContentBlock[] }
    | { id: string; type: 'assistant'; blocks: ContentBlock[] }
  )[]
}
```

待处理消息作为独立的队列状态返回，包含消息 ID、内容和位置（`queued` 或 `steering`）。消息进入轮次后，页面用同一个 ID 衔接队列条目和正文消息。

正式消息可以保留内部消息 ID 作为渲染键。工具块按 `callId` 关联结果，按调用首次出现的位置排列。并行执行时，哪个结果先回来就先更新哪张卡片，卡片位置保持稳定。Host 将工具事件整理成用户能读懂的名称、状态和有界详情。历史和实时都消费这个公开结构，工具参数、模型推理和内部提示不作为聊天内容。

流式文本按 `attemptId` 和块索引定位，再按增量顺序追加。正式消息持久化后，以同轮同一步的正式块替换临时块。模型重试时废弃旧尝试的临时块。已经持久化的工具卡片和前几步的文本仍能保留。工具状态和用户可操作问题来自对应的 Host 事件或接口。 [公开事件筛选](../internal/httpapi/chat_transcript.go#L144)

### 用户上传文件

文本和附件统一由 `POST /v1/sessions/{id}/messages` 接收。当前网页输入框继续发送 `application/json`，请求体为 `{"content":"…"}`。支持附件的客户端用同一个 URL，以 `multipart/form-data` 在一次请求中提交 `content` 和附件字段。本次方案的网页输入框范围是纯文本。 [前端发送请求](../frontend/src/api/controller.ts#L153) · [Host 消息接口](../internal/httpapi/native_worker_http.go#L270)

Host 保存文件原始字节，在用户消息中记录 `{type:"file", attachment:{attachmentId,name,bytes}}`。同样的内容按 SHA-256 共用一份对象，带安全文件名的硬链接供工具读取。上传件落在 Host 的 `/root/harness/.dsh/attachments/v1/file-objects/` 和 `files/` 下，模型请求沿用现有的文件路径说明。普通文件卡片随用户消息显示，点击时通过会话附件 ID 下载。 [附件存储](../harness/attachments/file_store.go) · [上游附件存储](https://github.com/deepseek-ai/deepseek-harness/blob/master/packages/attachment/attachment-local/README.md)

### 助手交付文件

按 DeepSeek Harness 的流程，由助手生成文件后调用 `present({files:[{path, description?}]})`。工具在当前轮执行，接收 1 到 8 个文件，按会话当前工作目录解析相对路径，并逐个验证普通文件。目录、最终符号链接、不存在的文件和越界路径都应使该次调用失败。Go Host 以会话工作区为文件访问边界。绝对路径也可作为输入，前提是它落在该工作区内。文件在调用时解析为固定路径，后续切换工作目录不会改变交付指向。 [上游工具实现](https://github.com/deepseek-ai/deepseek-harness/blob/master/packages/deliverables/tool-present/src/index.ts) · [Go 工作区文件访问](../internal/httpapi/workspace_files.go#L21)

工具成功完成后，由执行所属会话的 Agent 在工具结果提交处追加 `deliverables/presented`。沿用上游事件字段 `{turn,callId,files:[{path,description?}]}`。工具输出给模型简短的交付确认，交付事件负责记录交付事实。失败或取消的调用不发布交付；子 Agent 的声明归它自己的会话，父会话交付同一文件时由父 Agent 调用 `present`。工具说明和系统提示引导助手在文件可交付时显式调用 `present`。 [上游事件类型](https://github.com/deepseek-ai/deepseek-harness/blob/master/packages/deliverables/tool-present/src/types.ts) · [上游系统提示](https://github.com/deepseek-ai/deepseek-harness/blob/master/packages/deliverables/deliverable-file-references/README.md#model-experience) · [Go 工具结果提交](../harness/agent/native_tool_scheduler.go#L76)

Host 把日志中的 `deliverables/presented` 投影为现有 `event: session` 内的 `assistant/message`。公开消息沿用日志序号，携带 `turn`、`delivery: true` 和 `message.blocks` 中的 `file` 块。标记用于更新文件卡片并保留流式草稿。交付记录持久化后，页面立即显示文件卡片，后续文字按发生顺序接在卡片之后。同轮再次声明同一路径时，前端用稳定的块 ID 更新原卡片的描述。卡片复用用户附件的文件名和下载操作，助手侧额外显示交付描述。Markdown 链接按正文规则渲染。 [Go 聊天投影](../internal/httpapi/chat_transcript.go)

助手卡片的 `file.path` 是交付文件的 Host 绝对路径。点击时，前端将路径编码后传给 `GET /v1/sessions/{id}/files/content?path=…`。Host 校验路径位于请求所属会话的工作区内，再读取文件当前内容。文件被移走或删除时，卡片保留并提示不可用。冷会话也可以从日志恢复卡片并按路径下载。Docker 卷和 ECI+NAS 分别承载各自的工作区及附件目录。 [下载接口](api.md)

## 页面表现

保持当前侧栏和底部输入区的位置，重做中间的会话记录。用户消息靠右，助手内容靠左，两者各有稳定的阅读边界。桌面正文列约 760 px，窄屏占可用宽度。代码、长文件名和工具详情允许在自己的区域换行或横向滚动。

```text
                                                整理附件里的销售数据，做一份报告
                                                          [ sales.xlsx · 下载 ]

助手
我先读取工作表，确认列名和统计口径。
▸ 读取 sales.xlsx                                      已完成
▸ 生成销售报告                                         处理中

生成中                                                    [ 停止生成 ]

队列 1 条
  顺便按地区分组                                    [ 插话 ] [ 删除 ]
─────────────────────────────────────────────────────────────────
 输入消息…
  [ 端点 / 模型 ▾ ]                                      [ 发送 ]
```

| 内容 | 用户侧表现 | 助手侧表现 |
| --- | --- | --- |
| 文本 | 浅色气泡，按纯文本显示，保留换行。已有轮次执行时，排队消息显示在输入框上方 | 无大面积气泡，使用 Markdown 正文排版。多个文本块按原顺序出现，流式更新只改正在生成的块 |
| 图片、文件 | 图片显示有尺寸占位的缩略图，点击查看附件。上传文件显示文件名和下载入口 | 工具返回的图片在正文中独立成行。`present` 成功后立即显示下载卡片 |
| 工具 | — | 默认折叠为一行，显示工具动作与状态。展开后显示摘要、来源链接或可查看的结果。失败留在对应卡片，助手后续回复仍可出现 |
| 整轮状态 | 当前会话执行期间可以继续发送 | 当前轮结束前统一显示「生成中」。失败提示贴在对应轮次下方，后续成功的轮次不会覆盖它 |

用户停在底部时跟随新增内容。向上阅读历史时保留当前位置，并显示「回到最新」。键盘可以展开工具卡片和操作附件。读屏只播报关键状态，避免逐 token 播报。

### 工具图片在聊天中的位置

`generateimage` 和 `readimage` 返回的结构化 `image` 内容块都可以在助手回复中显示。图片紧接在产生它的工具调用后，和文字、文件一样参与正文排版。连续图片块并排放在同一行，窄屏可横向滚动。图片保持原有比例、加载占位和点击查看附件的行为，加载失败时原位显示提示。

例如，工具 A、B、C、D 依次出现，B 返回两张图时，正文排列如下。

```text
[ A · B ]       连续工具合并，可展开
[ 图片 1 ][ 图片 2 ]
[ C · D ]       连续工具合并，可展开
```

任何图片行都会结束前一组工具调用，后续工具从新的一组开始。图片和工具都按原有内容顺序排布。若 B、C 分别返回图片，B 的图片在 B 后，C 的图片在 C 后，中间的 C 卡片会将两行分开。并行工具即使乱序完成，图片仍各自跟随所属的工具。图片行属于同一条助手回复，共用回复头像。

1. Host 在现有 `tool/result` 的公开内容中增加可选的 `images`，每项复用已有的 `image` 块结构。只投影有效的图片附件引用，继续发送 `callId`、状态和错误码。块 ID 使用工具结果消息 ID 和块索引，保留多张图在结果里的顺序。实时和历史共用这套投影。 [公开事件筛选](../internal/httpapi/chat_transcript.go#L144)
2. 前端按 `callId` 保存工具状态和图片块，渲染时在对应的工具块后插入图片，再一次遍历相邻块，分别合并连续工具块和连续图片块。历史分页与 SSE 重连使用同一套规则，重复事件沿用现有序号去重。 [前端事件处理](../frontend/src/chat/transcript.ts#L109)
3. 图片行复用现有缩略图组件和会话附件接口。事件携带引用和尺寸等元数据，图片字节由浏览器单独请求。附件接口已经能从 `tool/result` 校验会话归属。 [附件读取](../internal/httpapi/session_attachment.go#L37) · [缩略图组件](../frontend/src/chat/ChatContent.tsx#L119)

验收覆盖单图、多图并排、窄屏滚动、图片前后工具各自成组、并行结果乱序完成，以及刷新和断线重连后的顺序与去重。

## Agent 提问

`ask_user_question` 真正进入待回答状态时，Host 通过当前会话的 SSE 发送 `questions.state`。它包含本轮仍可回答的问题，沿用现有结构中的 `callId`、`turn`、`step`、`state` 和 `questions`。前端用这些数据在输入框上方打开浮层，显示题目、单选或多选选项和自定义答案。问题浮层由当前待回答状态驱动，用户滚动历史时也能看到。该次工具提问由浮层呈现，聊天正文按原有内容块渲染。 [工具等待流程](../harness/tools/native_question.go#L45) · [待回答状态](../internal/hostruntime/native_runtime.go#L397)

用户通过现有 `POST /questions/{callId}/answer` 提交答案。Host 解除工具等待，通过同一条 SSE 继续推送后续文本和工具事件。前端收到问题状态更新后收起浮层。定时等待超时后若 Host 返回 `continued`，浮层改为可稍后回答的提示，Agent 可以先继续工作。重新打开会话或 SSE 断线后，用 `GET /questions` 恢复仍可回答的问题，活跃状态以 Host 为准。冷会话从持久日志恢复可继续回答的提问，提交迟到的答案时按需恢复 Agent。 [问题接口](../internal/httpapi/native_worker_http.go#L174) · [迟到答案处理](../internal/hostruntime/native_runtime.go#L432)

## 重新打开会话

新增 `GET /v1/sessions/{id}/transcript` 作为 Web 会话记录接口。Host 从同一个 v4 日志生成公开内容块，活跃会话和冷会话都能读。历史按**完整轮次**分页，初次打开只取最新一页，页面默认定位到底部。向上加载时用旧轮次游标，并在插入内容后保持阅读位置。 [现有持久历史读取](../internal/httpapi/session_history_http.go#L109)

| 请求 | 返回和合并方式 |
| --- | --- |
| 首次打开 `GET /transcript?limit=20` | 返回最新完整轮次、当前排队消息、正在执行的轮次状态、`before` 和 `throughSeq`。数据和游标来自同一个持久化读视图 |
| 向上滚动 `GET /transcript?before=…&limit=20` | 取更早的完整轮次。较旧的分页只向前插入历史，不回退实时游标，也不覆盖较新的轮次状态 |
| 断线重连 `GET /stream?view=chat&after=…` | 从已处理的持久序号补发更新，然后继续直播，刷新当前队列和轮次状态。历史和实时更新按消息、块 ID 合并 |

实时和历史共用 `turn/start`、`user/message`、`assistant/message`、`tool/call`、`tool/result` 和 `turn/end` 的 Web 投影。持久更新带 `seq`，消息带完整有序块，工具状态按调用 ID 关联。临时 `assistant-stream` 增量按 `turn`、`step`、`attemptId` 和块索引合并。

打开会话时用历史返回的 `throughSeq` 接上 SSE。持久更新推进重连游标，临时增量用于即时显示。队列以 Host 的完整快照为准，初次打开和重连都能恢复。后台任务的更新也走同一条流。冷会话订阅时可以先读落盘数据，等会话开始执行再接上实时输出。

冷会话读历史无需启动 Agent。图片和上传文件走会话附件接口，助手交付文件走按事件定位的下载接口。首次加载失败时提供重试，加载更早记录失败时保留已经读到的内容。

## 实施与验收

1. 复用 Host 的会话 SSE 和普通消息接口，增加队列快照及单条插话、删除操作。验证空闲时发送 A，即使 Host 先推送入队再推送轮次开始，页面也只显示一个用户气泡，排队区不会闪现。A 运行时发送 B、C，B 插话进入 A 的下一步，C 仍可排队等待或单独删除。全过程共用一条 SSE。
2. 在 Host 增加会话记录组装，供历史分页和流式更新共同使用。覆盖同内容的两条用户消息、并行工具乱序返回、模型重试、冷会话和仅有附件的消息。
3. 在前端按块渲染对话，接入队列和提问浮层。验证执行工具和弹出提问时，当前轮仍显示「生成中」。HTTP 回答后通过同一条 SSE 继续输出。刷新后恢复队列和会话内容，文件在冷会话也能下载。
4. 验证离开页面时任务继续、重连后消息只显示一次。通过 Controller 跑一次真实 Docker Host 流程，并更新 API 文档。
5. 验证 Bash 或 OfficeCLI 生成文件后，成功调用 `present` 即显示文件卡片。覆盖多个文件、同轮重复声明、失败或取消的调用、只有交付而没有助手收尾文本的轮次，以及 Markdown 路径仍按正文显示。
6. 验证 SSE 重连、历史分页和 Host 重启后还原相同卡片，用户上传附件仍可按附件 ID 下载。验证交付下载只能读取对应会话声明过的文件，拒绝越界或符号链接路径，并在原文件删除后返回不可用。
