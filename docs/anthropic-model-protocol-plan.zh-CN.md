# OpenAI / Anthropic 模型协议接入与设置交互方案

状态：已实施并完成本地验收。日期：2026-11-18。

## 1. 目标与范围

在现有“一个端点对应一个 API Key、多个模型”的模型设置中，增加 Anthropic Messages 兼容协议。Web 和移动端均可配置，并可在同一会话中切换两类协议的端点。

本次面向 Anthropic 官方 API，以及实现相同 Messages 协议的第三方服务。使用 API Key 认证；Bedrock、Vertex、Claude 订阅登录和云平台 IAM 不属于这个适配器。

接口协议属于端点，不属于模型或会话。模型名字不能决定协议：提供 Claude 模型的服务也可能使用 OpenAI 协议，应以服务商提供的接口为准。

## 2. 当前实现与需要修改的位置

- `internal/modelsettings/settings.go`：端点包含 name、baseUrl、apiKey、models，没有协议。
- `internal/controller/models.go`：负责端点校验、保存、公开视图和同步；读取设置只返回 hasApiKey，不返回密钥。
- `internal/controller/model_store.go`：Redis 的 `user:<userId>` Hash 中，`llm` 存端点目录，`sessionModels` 存会话选择。
- `internal/host/model_settings.go`：按会话选择构建模型路由，但当前把协议固定成 `llm.Responses`。
- `providers/model/mainmodel/model.go`：按当前 turn 的模型配置创建调用客户端。
- `thirdparty/llm`：已有 Responses 和 Chat Completions 编解码、流式调用，没有 Anthropic 实现。
- `harness/agent/llm_adapter.go`：完成模型上下文和内部消息的转换；工具返回的图片当前会拆为额外用户图片消息。
- `harness/agent/llm_stream_projection.go`：将供应商增量转成 Harness 的有序内容块。
- `frontend/src/settings/Models.tsx`、`mobile/src/Me.tsx`：目前明确提示只支持 OpenAI Responses。

继续复用现有 Controller → Host 配置同步和按 turn 固定模型的机制，不增加进程、Redis key 或配置文件。

## 3. 数据结构

端点只增加 `protocol`，使用值：

| 值 | 设置页名称 | 请求路径 |
| --- | --- | --- |
| `responses` | OpenAI Responses | `<baseUrl>/responses` |
| `anthropic_messages` | Anthropic Messages | `<baseUrl>/messages` |

底层已有 `chat_completions` 继续作为内部能力保留，本次不顺带增加第三个设置页选项。未提供 protocol 的既有端点按 Responses 解析；显式提供未知值直接报错。

Redis 示例，仍为 `user:<userId>` 的 `llm` 字段：

```json
{
  "defaultEndpointId": "endpoint-a",
  "endpoints": {
    "endpoint-a": {
      "name": "Main endpoint",
      "protocol": "responses",
      "baseUrl": "https://api.example.com/v1",
      "apiKey": "<用户保存的密钥>",
      "models": ["model-a", "model-b"]
    },
    "endpoint-b": {
      "name": "Claude",
      "protocol": "anthropic_messages",
      "baseUrl": "https://api.anthropic.com/v1",
      "apiKey": "<用户保存的密钥>",
      "models": ["<服务商提供的模型 ID>"]
    }
  }
}
```

`sessionModels` 保持：

```json
{
  "session-id": { "endpointId": "endpoint-b", "modelId": "model-id" }
}
```

不在 sessionModels 重复存协议、URL 或 API Key。公开设置接口增加 protocol，密钥仍只返回 hasApiKey。Host 获取包含协议的既有设置快照；镜像和客户端静态资源不包含密钥。

## 4. Web 设置交互

### 4.1 端点列表

沿用当前清爽的设置页及侧栏。每张端点卡展示：

- 名称，默认端点标记。
- 次要信息：协议标签、Base URL、密钥是否已保存。
- 模型列表，第一项标为默认模型。
- 编辑、设为默认、删除继续使用现有操作体系。

协议标签使用小号中性描边，不为不同协议引入大面积品牌色。页面说明改为“支持 OpenAI Responses 和 Anthropic Messages 兼容接口”。

### 4.2 新增与编辑

字段顺序：名称 → 接口协议 → Base URL → API Key → 模型列表。

协议使用下拉表单（用户最终确认），包含两个选项，附短说明：

- OpenAI Responses：适用于提供 Responses 接口的服务。
- Anthropic Messages：适用于 Anthropic 官方及 Messages 兼容服务。

新增端点默认 Responses；没有默认模型名。新建表单的官方地址按协议预填；用户可填写代理地址。编辑已保存端点时不自动替换已有 URL、模型和密钥。

切换协议时，仅当 URL 仍为空或为表单自动填入且未编辑的官方地址，才跟随切换默认地址。用户手填的代理地址保留。协议变化后仍需明确点击保存，不即时影响运行。

Base URL 约定包含 API 前缀，不包含最终操作路径。例如 Anthropic 填 `https://api.anthropic.com/v1`，下方展示只读的最终请求地址 `https://api.anthropic.com/v1/messages`。若填入 `/messages`、`/responses` 等操作路径，提示填写 API 基础地址，不静默产生双重路径。

模型列表以一行一个模型的轻量行编辑呈现，提供添加、移除、上移/下移；第一项有“默认”标记。保留模型 ID 手动填写，不依赖服务商模型发现接口。模型列表示意文字不能作为真实配置自动保存。

保存只做配置校验及同步，不偷偷发起付费模型测试。保存成功也不表述为“模型调用验证成功”。请求中的鉴权或模型错误在实际调用时明确显示。

编辑密钥留空表示保留；更换 URL 的来源时沿用现有逻辑，要求重新提供密钥。新增端点无密钥不能保存。字段错误展示在字段附近，避免只有一个泛化失败提示。

下拉菜单点外部或 Esc 收起；全表单编辑不是点背景就丢弃输入。若使用关闭按钮且有未保存修改，需确认是否放弃。

## 5. 移动端设置与会话模型选择

移动端沿用“我的 → Settings → 模型”的底部面板风格，列表卡片展示名称、协议、默认标记和模型摘要。编辑面板内使用与 Web 同样的字段、校验和默认值规则。

协议下拉表单撑满可用宽度，展开为锚定字段的选项面板，点外部收起；输入 API Key 和 URL 时关闭自动纠正。键盘弹起后可滚动到保存按钮，避免层叠多个底部面板。轻量选择面板点遮罩收起；有修改的编辑表单通过关闭或返回处理放弃确认。

Web 与移动端会话输入框继续展示“端点 / 模型”。展开后在端点名称旁展示小号协议标签，模型列表维持单选。不让用户在每次发消息时重复选择协议。

选择模型成功后收起；失败时保留选择面板并展示原因。不新增全局“当前协议”。语言文案同步支持简体中文、英文，使用既有本地语言设置。

## 6. Host 与协议适配

### 6.1 职责

Controller 校验并同步协议；Host 使用当前 turn 选定的端点直接访问模型。Harness 负责 Agent 循环、工具执行、审批、队列、记忆和 compaction。

Anthropic 编解码、鉴权、HTTP/SSE 处理放在 `thirdparty/llm`，如 `encode_anthropic.go`、`decode_anthropic.go`、`stream_anthropic.go`。沿用统一 Request、Response 和调用入口；文件分离即可，不再包一套多层 provider 注册框架。

HTTP 请求默认采用 `x-api-key`、`anthropic-version: 2023-06-01`，请求 `/messages`。该兼容基线由代码固定，不增加设置项；不自动转发用户自定义任意 Header，也不将凭证发送到重定向来源。新 API 版本和平台专用认证可作为独立扩展。

### 6.2 请求与工具

| Harness 内容 | Anthropic Messages 投影 |
| --- | --- |
| 系统提示、记忆及技能说明 | 顶层 `system`，保留既有内容与优先级 |
| 用户文本、图片 | user.content 中的 text / image |
| 助手文本、工具调用 | 同一 assistant.content 内有序的 text / tool_use |
| 工具返回 | 随后的 user.content 中的 tool_result，按 tool_use_id 对应 |
| 工具 JSON Schema | tools[].input_schema |
| 工具错误 | tool_result.is_error |

并行调用的一组工具结果放在同一后续用户消息内，结果块在普通文本之前。不得把 steer 文本插入工具调用和对应结果之间。

工具返回的图片在 Anthropic 路径下保留在对应 tool_result 内，复用附件系统读取字节；不复制一套文件，也不拆成不相关的用户图片消息。需调整现有公共投影，保留工具结果与图片的关联，再由各协议编码器决定 wire 格式，避免破坏 Responses 行为。

普通 file 交付继续使用当前 present 和路径下载机制，不顺带接入 Anthropic Files API。工具由 Host 执行，不自动开启 Anthropic 的托管搜索、代码执行或托管电脑。

### 6.3 必填参数与停止原因

Anthropic 的 max_tokens 在调用侧填入：默认 16,384；compaction 等内部调用有显式预算时使用其预算。不新增 env 或用户表单字段。供应商或特定模型不接受该预算时明确报错，不静默改模型。

不把现有 OpenAI reasoning.effort 原样发送给 Anthropic。通用兼容路径默认不发送专属 thinking、缓存及 beta 参数；模型自己返回的 thinking 仍须正确处理。特定模型额外参数的支持不靠模糊匹配模型名字猜测。

end_turn / stop_sequence 表示正常结束；tool_use 进入既有工具循环；max_tokens 映射为既有截断处理，未完整的工具参数禁止执行。refusal、pause_turn、context-window 错误等分别识别，不把任意 stop_reason 都当完成，也不自动引入服务商托管工具续跑。

### 6.4 流式事件

解析 message_start、content_block_start/delta/stop、message_delta、message_stop、ping、error，按 block index 累积文本与工具参数。收到半个 JSON 时只更新流式显示；完整成功响应后才能提交可执行工具调用。

在 `llm_stream_projection.go` 增加显式 Anthropic 映射，输出当前 Harness block-start、text-delta、reasoning-delta、tool-call-delta、block-end 和完成事件。前端继续使用现有内容块和 SSE，不解析原始 Anthropic 协议。

message_stop 是一次模型请求结束，tool_use 情况下不是整个 Agent turn 结束。使用既有 turn 生命周期决定“生成中”何时结束。

流中 error、异常 EOF、超时与取消正确落为失败或中断，不能把部分输出误认成功。已产生可见输出后的重试必须遵循现有 attempt 隔离，不能重复追加文本或执行工具。

Token 统计合并消息起始及增量 usage；正确区分普通输入、缓存读取、缓存写入，避免把缓存重复计入。缺少 thinking token 数量时不伪造。

## 7. Thinking、会话恢复与跨协议切换

当前通用助手块没有原生 thinking signature 等信息，不能只加入一个 HTTP 编码器就宣称完整支持。

在现有会话日志中的助手记录旁增加可选、带来源标记的协议数据，保存原始有序助手 content，包括 thinking signature、redacted_thinking。来源包含 endpointId、modelId、protocol 和端点地址。数据随现有助手提交一起保存，不增加 memory-snapshot.json 或另一套会话文件，不含 API Key。

这部分是模型回放元数据，不作为普通 file/image 或聊天文字发送给前端。同一路由继续调用或恢复时，按 API 要求回传原始块；不从 UI reasoning 摘要重建 signature，不修改原始 thinking 内容。

换端点或模型后，仅复用可移植的用户消息、助手文本、工具调用/结果、附件和 compaction 摘要，不把原服务商的签名、响应 ID 当新服务商可接受的参数。来源不同的 thinking 不伪造签名；如需保留其可见语义，只作为普通历史说明，不作为已验证 thinking。

当前 turn 的端点、协议、模型与凭证按现有 pinTurnModel 固定。生成时切换会话模型，当前 turn 继续用原快照，下一轮生效；UI 提示“将在下一轮使用”。不会在工具执行一半时切换供应商。子 Agent 继承当前主 Agent 的模型路由，不增加 profiles 配置。

Compaction 继续由 Harness 管理，摘要是普通文本，不启用服务商原生压缩替代它。原生 thinking 不能只裁剪一半；保留区间维持完整助手消息和工具配对，压缩掉的部分不在恢复时重新拼回。切换协议后继续使用同一长期记忆和工具体系。

现有 100 万 token 上下文默认值不因本次新增协议而偷偷调整，也不把它宣传为所有兼容模型都支持。较小窗口模型可能提前返回上下文超限；需接入明确的上下文错误识别及有限的 compaction 恢复，恢复失败明确提示。若要完整适配任意窗口，需另行决定模型容量信息的配置或发现方式，不能仅凭协议推断容量。

## 8. 配置检查与错误表现

保存时检查协议枚举、URL、密钥、非空去重模型列表；GET 不泄露密钥。未知协议不回退成 Responses。

Host 同步时再次检查协议与目录有效性；正常 Docker / ECI 启动与 Redis 结构不变。没有默认端点时继续阻止创建会话，不构造隐式模型。

实际调用区分：鉴权失败、余额或限流、模型不可用、路径/协议错误、上下文超限、图片不被支持、流中错误。不展示含密钥的原始请求。配置已保存但 Host 应用失败，沿用当前明确提示，不能显示为已成功生效。

删除端点后，引用它的会话显示模型不可用，提示重新选择，不擅自换到另一个可能计费的供应商。

## 9. 实施顺序与验收

1. 扩展端点字段、Controller API、Redis JSON 和 Host 路由；保留现有 Responses 默认。
2. 实现 Anthropic 非流式、流式与工具/图片编解码；接入现有错误与 token 统计。
3. 完成有来源的原生块持久化、恢复、compaction 和跨协议切换。
4. 修改 Web 与移动端模型设置、选择器、中英文提示。
5. 更新模型接入文档和 README 中的兼容协议说明。
6. 全量构建 Controller、Host、Web 与移动端测试包，再做端到端检查。

自动测试使用本地 HTTP/SSE fixture，覆盖文本、并行工具、参数分片、工具图片、thinking 原样回传、截断、失败、usage、取消及双向协议切换。额外覆盖会话重载、Host 重启后的同路由恢复和 compaction 后工具配对，不能只有单轮问答。

真实调用使用已确认可用的 Anthropic 兼容端点，不假定 OpenAI Key 同时适用。真实验收控制为一次简短 shell 工具任务及一次无工具追问，其余协议边界、恢复和切换使用本地 fixture 验证，避免额外模型开销。

Web 与移动端都查看新增、编辑、协议选择、外部点击收起、错误、生成时切换提示的真实截图；沿用现有清爽浅色风格，不为本次协议能力重做整体主题。

## 10. 官方参考

- [Anthropic Messages：2023-06-01 兼容基线](https://platform.claude.com/docs/en/api/messages/create?version=2023-06-01)
- [API 版本](https://platform.claude.com/docs/en/api/versioning)
- [工具调用与工具结果顺序](https://platform.claude.com/docs/en/agents-and-tools/tool-use/handle-tool-calls)
- [流式 Messages](https://platform.claude.com/docs/en/build-with-claude/streaming)
- [Thinking 文档](https://platform.claude.com/docs/en/build-with-claude/extended-thinking)

上述官方说明用于确认协议约束；本方案中的设置布局、Redis 字段和实施步骤是 go-bot 的设计选择。

## 11. 本地验收记录

2026-11-18：

- `make test vet check` 通过；普通测试不需要真实模型凭证。
- `go test -race ./thirdparty/llm ./harness/agent ./internal/controller ./internal/host` 通过。
- Web 构建及 11 项前端测试通过；移动端 TypeScript 检查通过。
- Controller、Host Docker 镜像构建通过；Android ARM64 Debug 和 iOS 模拟器 Debug 构建通过。
- Docker fixture 验证跨协议切换、当前轮固定路由、Host 重启后的模型选择恢复，以及 Web 聊天链路。
- 使用阿里云 Anthropic 兼容接口与 `deepseek-v4.1-flash` 完成两轮真实会话：一次 shell 工具调用产生 `anthropic-tool-ok`，后续追问正确引用结果；三个助手模型步骤、一次工具执行，两轮正常结束，无失败 attempt。
- 通过浏览器与 iOS 模拟器检查协议下拉表单、选项切换、自定义地址保留、保存回显和移动端点外部收起；移动端可读取同一真实测试会话。
- Anthropic 官方 Claude 服务和 ECI 本次未作真实调用验证；兼容协议边界由本地 HTTP/SSE fixture 覆盖。

截图、构建日志与真实凭证均未加入源码仓库。Go 客户端和 OpenAPI 定义也已补齐端点协议字段。
