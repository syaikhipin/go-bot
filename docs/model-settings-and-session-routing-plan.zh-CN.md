# 模型设置与会话模型切换

> 历史设计与调研记录。当前行为、配置和命令以[文档索引](README.md)中的指南为准。

## 页面与操作

创建账号只填写账号名。Controller 创建 `userId` 后，前端进入该账号的模型设置页。已有账号也可从聊天侧栏的 **Settings** 打开 `/settings/models`，再返回原来的会话。设置页可以在 Host 尚未启动时使用。用户可以查看已有聊天记录。创建新会话要求账号已配置一个有效的默认模型。

| 区域 | 操作 |
| --- | --- |
| 设置页 | 添加多个端点。每个端点配置显示名称、Base URL、一把 API Key，以及一个或多个服务端接受的模型 ID。模型 ID 手动填写。API Key 保存后显示「已配置」，编辑时可替换。 |
| 默认模型 | 每个端点使用按顺序填写的模型 ID，首个模型为该端点的默认模型。首个端点设为账号默认端点，用户可以在设置中更改。 |
| 聊天输入框左下角 | 显示「端点名称 / 模型 ID」。打开后先选端点，再选该端点内的模型。同名模型按端点区分。保存后只更新当前会话，并显示从下一轮生效。 |
| 新会话 | 点击 **New chat** 或从欢迎页直接发送时，由 Controller 读取账号默认模型。缺少可用默认模型时，页面显示「请先在模型设置中配置端点并选择默认模型」，提供 **打开模型设置** 入口，并保留已输入的内容。创建成功后，可通过输入框左下角的选择器切换当前会话。 |

设置页标注 **仅支持 OpenAI 兼容的 Responses API**。Base URL 示例为 `https://api.example.com/v1`，Host 调用该地址下的 `POST /responses`。首版手填模型清单，保存时校验字段格式和引用，实际请求失败时在聊天中显示相应错误。OpenAI 兼容服务需要支持 Responses 接口及所用模型对应的工具调用能力。

## Redis 数据

每个账号的设置保存在一个 Redis Hash `user:<userId>` 中。完整字段见 [Redis 账号存储](configuration.md#redis-account-storage)。

| 字段 | 类型 | 保存内容 |
| --- | --- | --- |
| `llm` | JSON | 账号端点和默认端点。 |
| `sessionModels` | JSON | 以 `sessionId` 为键，值为 `{"endpointId":"…","modelId":"…"}`，只保存模型引用。 |
| `modelRevision` | 整数 | Controller 和 Host 用它同步模型相关设置。保存端点、默认模型或会话选择时递增。 |

`llm` 字段示例。端点 ID 由 Controller 生成，显示名称修改后 ID 保持稳定。

```json
{
  "defaultEndpointId": "f63bb795-19cf-450f-9a7f-f24ba0fc3787",
  "endpoints": {
    "f63bb795-19cf-450f-9a7f-f24ba0fc3787": {
      "name": "团队网关",
      "baseUrl": "https://models.example.com/v1",
      "apiKey": "<仅在服务端保存>",
      "models": ["reasoner", "fast"]
    },
    "6af363ef-f3d5-4e7b-ac70-63ae483a81c3": {
      "name": "本地网关",
      "baseUrl": "http://host.docker.internal:9000/v1",
      "apiKey": "<仅在服务端保存>",
      "models": ["fast"]
    }
  }
}
```

用 `HGET user:alice sessionModels` 可读取该账号的会话模型选择。端点 API Key、Base URL 和模型清单存放在 `llm`。会话归档后保留选择，更新 Host 镜像、重启或回收实例都保留 Redis 设置。聊天记录、工作区和附件由 Host 写入其文件系统。

账号没有默认端点时，`llm.defaultEndpointId` 为空字符串。账号尚未配置端点时，整个 `llm` 字段可以缺省。Controller 创建用户会话时读取并校验默认引用，将当时的 `{endpointId, modelId}` 写入 `sessionModels` 中对应的 `sessionId`。切换模型时覆盖该条记录。修改账号默认模型只影响之后创建的用户会话。

删除端点或移除模型时，重排默认端点的模型时，新的首个模型用于后续会话。删除默认端点会清空账号默认端点。历史会话的选择保留原引用，界面显示「模型已不可用」并要求重新选择。这样旧会话不会悄悄把上下文发给另一个服务。更新同一端点的 API Key 后，使用这个端点的会话在下一轮读取新凭证。更改 Base URL 的来源（协议、主机或端口）时要求重新填写 Key。

## API

| 方法与路径 | 行为 |
| --- | --- |
| `POST /v1/accounts` | 请求体 `{"userId":"alice"}`，创建账号后返回 `201`。 |
| `GET /v1/settings/models` | 返回端点、模型清单和默认值。端点返回 `hasApiKey`，省略 Key。无需启动 Host。 |
| `POST /v1/settings/models/endpoints` | 保存名称、Base URL、Key 和模型清单，返回端点 ID。 |
| `PATCH /v1/settings/models/endpoints/{endpointId}` | 编辑端点。省略 `apiKey` 时保留旧值。模型列表顺序决定该端点的默认模型。 |
| `DELETE /v1/settings/models/endpoints/{endpointId}` | 删除端点。删除默认端点时清空默认端点。 |
| `PUT /v1/settings/models/default` | 写入 `{"endpointId":"…"}`，传空字符串清除。 |
| `POST /v1/sessions` | Controller 从 Redis 读取默认模型。缺少默认值、端点或模型不可用时，返回 `409` 和 `model_default_required`，并在向 Host 发送创建请求前结束。Controller 先为会话 ID 保存并同步本次选择，再调用 Host 的内部创建接口。创建结果无法确认时，前端刷新会话列表并保留草稿。 |
| `GET /v1/sessions/{sessionId}/model` | 返回会话的选择和 `status`（`ready`、`unconfigured`、`unavailable`）。 |
| `PUT /v1/sessions/{sessionId}/model` | 按会话保存端点 ID 和模型 ID。Controller 向账号的 Host 核实会话存在且未归档，支持检查未加载到内存的会话。重复提交同一选择可以安全重试。 |

设置和会话路由沿用当前 Controller 的同源限制及 `X-User-ID`。会话选择按账号和会话 ID 共同定位，不能通过其他账号的会话 ID 写入设置。未配置或已失效的会话发送返回明确的 `model_unconfigured` 或 `model_unavailable`。设置保存请求等待 Host 确认应用。保存后应用失败时返回 `503 settings_saved_host_apply_failed`。API Key 仅从写入请求进入 Controller 和 Redis，经内部通道同步给所属 Host。公开响应、事件、日志和浏览器持久化都省略 Key。

## Controller 与 Host

Controller 管理账号端点和会话选择，Host 运行期间应用最新同步的设置，并按会话直接请求模型服务。Controller 通过现有的 Host 地址及内部 Token 调用 `PUT /internal/host/model-settings`，传输账号端点、默认模型、会话选择及同一 Redis 版本。Docker 与 ECI 使用同一个接口。 [Controller 启动参数](../internal/controller/controller.go) · [当前 Host 装配](../internal/host/host.go)

1. **启动。** 受管 Host 可以在没有模型凭证时启动并响应进程就绪检查。Controller 同步当前快照后才开放模型请求，空快照也可应用。Host 将快照保存在内存，受管 Host 的容器环境和会话目录无需保存模型凭证。独立启动的 Host 仍可从已有环境变量读取单端点。
2. **写入。** Controller 串行处理同一账号的配置变更。Redis 事务保存本次变更并递增 `revision`。无运行中 Host 时立即确认保存，启动时再应用。有运行中 Host 时推送完整快照。Host 原子替换内存快照，接受同版本重试，并拒绝较旧版本。首版只在启动和配置变更时推送快照，消息发送时无需逐轮传输配置。
3. **应用失败。** Redis 写入成功但 Host 未确认时返回 `503 settings_saved_host_apply_failed`。用户可重新提交更新或重新打开账号。Controller 仅在显式请求中应用设置，发送消息前校验 Host 已应用当前版本。已开始的轮次保留原配置。
4. **执行。** Host 在消费该会话的持久 inbox 前解析一次路由，把端点、模型和凭证固定到这一轮的上下文。工具调用后的后续模型请求、重试、上下文压缩以及本轮派生的子 Agent 复用该路由。已在队列中、尚未开始的消息按真正开始时生效的配置执行。恢复的子 Agent 在新执行轮次继承所属顶层会话的当前模型。
5. **恢复。** 已有会话保留原日志和工作区。切换模型时从会话历史重新组装请求，旧模型的 token 计量不能用于新路由的上下文预算。投递到已有会话的定时任务、目标和后台 Job 从所属会话解析路由。定时任务需要新建目标会话时，也要先检查是否有用户配置的有效默认模型，并将选定模型与目标会话绑定。Host 在 `.dsh/storages/scheduled-models/<sessionId>.json` 持久化这条无凭证的选择。查看和切换均通过当前请求访问 Host。缺少可用模型时，保留待执行消息，定时任务按失败重试规则处理。

复用现有 `ModelRoute` 和 Responses 适配器，将端点 ID 用作路由标识，按当前快照验证模型属于该端点。 [模型路由](../providers/model/mainmodel/model.go) · [执行轮次入口](../harness/agent/native_loop.go)

## 落地与验收

- 前端新增模型设置路由、侧栏入口和共用的模型选择器。账号表单只保留账号名。支持设置页直接打开、保存后返回原会话、窄屏选择器以及切换失败时保留草稿。
- Controller 增加 Redis 模型存储、设置 API 和同步接口，并更新 OpenAPI、Go 客户端和示例。如果需要保留当前测试账号的端点信息，可一次性导入原 `user.model`，由用户在设置页确认默认模型。
- Host 支持无模型启动及热更新，验证 Docker 和 ECI 的启动契约。用两个测试端点验证请求确实送到所选端点，同一端点内切模型时复用该端点的 Key。
- 验收两条会话各自选择不同模型，刷新浏览器并重建 Host 后选择仍能恢复。流式生成中切换只影响下一轮。验证账号没有模型、只有端点但未选择默认模型、默认模型失效这三种情况下，创建会话均返回 `409 model_default_required`，页面给出设置入口且 Host 没有创建会话。修改默认模型后验证已创建会话的选择保持原值。另覆盖端点移除、同步失败重试、定时任务恢复和 API 凭证保护。
