# Go Harness 生图供应商与 Browser Use 方案

> 历史设计与调研记录。当前行为、配置和命令以[文档索引](README.md)中的指南为准。

## Seedream 生图

`generateimage` 接收 `prompt` 和可选的 `reference_image_ids`。上传图片、`readimage` 读取的图片和此前生成的图片都可用作参考。工具从 Host 的附件存储读取参考图，并将供应商返回的一张图片保存到同一附件存储。工具返回附件说明和一个 `image` 内容块。Host 在 `tool/result` 中记录附件引用，并通过现有的工具图片处理流程把图片送入下一步模型上下文。 [Host 初始化](../internal/host/host.go) · [工具注册](../internal/executor/builder.go) · [生图工具](../harness/tools/generate_image.go)

### Host 和 provider

- 在 `providers/image` 增加与现有 `providers/web` 类似的注册表，内置 `seedream`。provider 负责把提示词变成一张图片的数据和 MIME 类型。工具负责保存附件并生成工具结果。Seedream 的 HTTP 请求与响应解析集中在 `providers/image/seedream`。
- Seedream 默认走火山方舟北京区域的 `POST https://ark.cn-beijing.volces.com/api/v3/images/generations`，用独立的 Ark API Key 发送 Bearer 认证。设置页提供可编辑的模型名 `model`，默认填 `doubao-seedream-5-0-260128`（5.0 Lite）。用户可填写方舟支持的模型 ID 或 Endpoint ID，Host 使用保存的值作为请求的 `model`。适配器默认生成一张 `2K` 图片，使用非流式 `b64_json` 并沿用官方水印默认行为。沿用方舟默认单图行为，可直接使用 Endpoint ID。 [火山方舟图片生成 API](https://console.volcengine.com/ark/region:cn-beijing/docs/image-generation-api) · [模型 ID 对照](https://console.volcengine.com/ark/region:cn-beijing/docs/image-generation-tutorial)
- Host 总是注入一个生图服务并注册工具。服务保存当前账号设置的并发安全快照，每次调用读取一次；配置变更从下次调用生效。缺少密钥、权限或额度不足、限流、超时和无效响应都返回具体的工具错误。缺密钥时返回「请在 Settings → Image generation 添加 Seedream API key」。工具参数校验只负责参数，不把缺配置当成参数错误。
- 限制响应大小，验证 Base64、实际图片格式和尺寸后再保存附件。单次工具调用只提交一次生成请求，超时或连接断开且结果未知时反馈给模型，避免重复计费。上游请求 ID 可用于排错，错误正文、认证信息和图片字节不进入会话日志。附件存储按内容去重，并按现有规则保留或规范化图片。 [附件存储](../harness/attachments/store.go)

### Controller 与前端设置

搜索和生图各保留一个账号级供应商选择及各供应商的配置记录。生图记录保存 `model` 和 `apiKey`，搜索记录保存 `apiKey`。Controller 持久化到现有账号 Redis 记录，只把当前选择及所需配置同步给该账号的 Host。Docker 和 ECI 都走同一个已认证的 Host 内部 HTTP 路径。已有只含 `apiKey` 的 Web Search 记录读作 DeepSeek，首次写入时转成带 provider ID 的记录。 [现有 Web Search 设置](../internal/controller/provider_settings.go) · [设置同步](../internal/controller/models.go) · [账号存储](../internal/controller/store.go)

| 项目 | Web Search | Image Generation |
| --- | --- | --- |
| 设置页面 | `/settings/web-search` | `/settings/image-generation` |
| Controller API | `/v1/settings/web-search` | `/v1/settings/image-generation` |
| Host 内部 API | `/internal/host/web-search` | `/internal/host/image-generation` |
| 首个内置供应商 | DeepSeek，`deepseek` | Seedream，`seedream` |

- GET 返回已注册的供应商目录、默认和当前选择、已保存的生图模型名、各条配置的 `hasApiKey` 及同步状态。供应商目录同时提供模型默认值和字段说明。生图 PUT 接收 `provider`、`model` 和可选的 `apiKey`，校验模型名非空，省略密钥时沿用已有密钥，允许单独换模型。DELETE 清除指定供应商的密钥，生图模型名保留。未知供应商 ID 返回 400，已有 DeepSeek 的 PUT 和 DELETE 继续接受原请求形状。
- 页面显示默认供应商卡片，支持填写、替换和移除密钥。生图卡片额外显示可编辑的「模型名」，提示可填写 Model ID 或 Endpoint ID。两页都有「添加供应商」。弹层列出尚可配置的已注册供应商。列表为空时显示「当前版本只内置 [DeepSeek / Seedream]。接入其他供应商需要在代码中实现并注册适配器，再部署新版」。提供开发文档入口。
- 设置保存成功与供应商可用性分别展示。保存阶段检查字段，无需发起付费生成请求。未运行的 Host 在下次启动时接收设置。运行中的 Host 同步失败时保留 Redis 中的新设置，返回同步待处理，由现有 Host 启动和定期对账流程重试。空密钥同步时清除 Host 中的旧凭证，生图工具继续显示。 [运行中 Host 的设置对账](../internal/controller/lifecycle.go)
- 贡献者增加一个 provider 时，实现接口、注册 ID 和配置元数据、定义错误映射并添加适配器测试。通用设置页从目录识别可添加项，工具名和参数不随供应商改变。

## Browser Use

Go Harness 将 **Vercel `agent-browser` 作为内置浏览器能力**。Host 镜像预装 CLI 和官方 `agent-browser` Skill，两种架构都使用 Debian Chromium。Go Harness 保留模型循环，模型按需加载 Skill，通过 `shellexecute` 操作浏览器。用户打开新版 Host 即可使用。现有 Chromium 测试已覆盖截图、打印 PDF 和下载，可复用其中的测试站点。 [Host 镜像](../build/Dockerfile.host) · [Chromium 测试](../internal/executor/chromium_docker_test.go) · [上游 Skill](https://github.com/vercel-labs/agent-browser/blob/v0.43.0/skills/agent-browser/SKILL.md)

### 镜像打包与 Skill 发现

- Host Dockerfile 通过 `npm install -g agent-browser@0.43.0` 安装官方 npm 包。npm 根据镜像架构选择 Linux CLI，命令 `agent-browser skills get core` 直接读取同版本 npm 包自带的指南。两种镜像都通过 Debian 安装 `chromium`，并在 Dockerfile 中设定 `AGENT_BROWSER_EXECUTABLE_PATH=/usr/bin/chromium`。构建时用 `--platform` 选择 `linux/amd64` 或 `linux/arm64`。 [官方安装文档](https://agent-browser.dev/installation) · [浏览器路径配置](https://agent-browser.dev/configuration#environment-variables)
- 镜像构建时在临时项目执行官方 `npx skills add`，以 Codex 为目标安装固定提交里的 `agent-browser` Skill，再把完整目录放入镜像的 `/usr/share/go-harness/skill-seed`。Host 初始化数据根时和 OfficeCLI 一起安装到 `$DSH_HOME/skills`。已有 OfficeCLI 初始化标记的数据根只补装缺少的 `agent-browser`。 [官方 Skill 安装文档](https://agent-browser.dev/skills) · [Host 初始化](../harness/skills/seed.go)

### 会话与工具调用

1. `BuildNative` 为每次 live Session 激活分配独立的短 socket 目录。该会话的 `Executor` 给每次 shell 子进程注入同一个 `AGENT_BROWSER_SOCKET_DIR`。模型按上游指南选用 `--session`，即使不同 Go Harness 会话选了相同名字，daemon 也会在各自的 socket 目录查找。构建验收确认 Chromium 路径和 root 容器启动参数。 [Shell 子进程环境](../internal/executor/executor_linux.go) · [CLI 配置](https://agent-browser.dev/configuration)
2. 模型加载 `agent-browser` Skill，通过 `shellexecute` 读取 CLI 内置指南，再按指南打开页面、查看快照和操作元素。截图和下载写到当前会话的工作区，沿用现有文件链接交付。需要视觉判断时用 `readimage` 读取截图。 [CLI 命令](https://agent-browser.dev/commands) · [现有文件链接](../internal/httpapi/chat_transcript.go)
3. 同一次 live Session 激活中的多个命令和轮次复用同名浏览器。顶层会话和子 Agent 各自分配 socket 目录，Fork 和冷恢复重新分配。默认浏览器状态使用临时目录，工作区里的截图和下载随账号数据持久化。 [命名会话](https://agent-browser.dev/sessions)
4. `Resources.Close` 先停止本会话的 Shell Jobs，再在该会话的 socket 目录内用 `agent-browser close --all` 清理上游 Skill 可能创建的所有命名浏览器。限定关闭超时，确认结束后移除临时目录，保留 daemon 空闲回收作为补充。 [资源释放入口](../internal/executor/builder.go) · [上游清理实现](https://github.com/vercel-labs/agent-browser/blob/v0.43.0/cli/src/main.rs#L1291)

会话划分用于避免正常操作串用浏览器。账号隔离由现有 Host 容器承担，浏览器调试接口不对外发布。Skill 把网页快照当作外部内容，并限制每次返回的数据量。网页浏览沿用部署的网络边界，`web_fetch` 的 public-only 策略继续作用于 `web_fetch`。 [agent-browser 安全文档](https://agent-browser.dev/security)

### 实施与验收

1. 固定二进制和 Skill，构建 Linux AMD64 / ARM64 Host 镜像，检查 Skill 与上游文件逐字节一致，确认 `agent-browser --version` 和 `agent-browser skills get core` 正常。在原生平台验证 CLI 能启动该架构的浏览器。
2. 通过实际 Go Harness 会话加载 Skill，在本地测试站点验证打开页面、读取快照、用元素引用填写并点击、截图、下载，以及最终文件可从现有聊天界面获取。以多次 Shell 工具调用验证浏览器状态能跨命令延续。
3. 并行运行两个 Go Harness 会话，两边使用相同的 CLI 会话名，验证标签页和 Cookie 状态彼此独立。另在一侧创建多个命名浏览器，关闭该 Go Harness 会话后确认其浏览器全部退出，另一侧仍可操作。冷恢复或 Fork 使用新的浏览器，原工作区文件可按原权限读取。
4. 使用已有数据的 Docker 卷启动新版 Host，确认新 Skill 可发现。ECI 使用同一 Host 镜像，实机验收可复用浏览器测试站点。

## 生图与供应商设置验收

- **Seedream 与设置**。`generateimage`（生图）和 `web_search`（网页搜索）各自在缺少本供应商密钥时仍出现在工具列表，调用时返回对应设置入口。用 HTTP fixture 验证 `generateimage` 使用保存的模型名，单独修改模型或密钥后下一次调用生效，清除密钥后返回未配置。覆盖认证失败、限流、超时、返回超限和正常图片入库，并检查账号间配置隔离及密钥回显。
- **供应商入口**：两个设置页均可打开「添加供应商」，只有默认项时显示清晰提示。用测试目录加入第二个 provider，验证它自动出现在添加列表并能保存选择。旧 DeepSeek 设置仍可用。
