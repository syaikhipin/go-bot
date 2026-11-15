# Muse、OpenMuse 与 Go Harness 的个人 Agent 调研

> 历史设计与调研记录。当前行为、配置和命令以[文档索引](README.md)中的指南为准。

调研日期为 2026-11-13。建议首期做通一个日常任务：用户授权自己的日历和邮箱，Agent 按约定时间生成简报，关掉网页后仍能收到结果，点通知回到原会话。第三方接入优先选 **Google Calendar、Gmail 和 Web Push**。如果首批用户主要在国内团队使用，可以用**飞书日历**替换 Google Calendar 作为第一条验收链路。跨会话记忆、任务记录和审批由 Go Harness 自己掌握。调研基于产品公开资料和 GitHub 源码，服务选型仍需用目标账号验证授权和部署体验。 [Google Calendar 授权范围](https://developers.google.com/workspace/calendar/api/auth) · [Gmail 授权范围](https://developers.google.com/workspace/gmail/api/auth/scopes) · [Web Push](https://developer.mozilla.org/en-US/docs/Web/API/Push_API) · [飞书日历开放接口](https://open.larkenterprise.com/document/quickly-build-a-feishu-calendar-schedule/introduction)

## Muse 和 OpenMuse 的实现

| 项目 | 已核对的做法 | 对 Go Harness 的参考 |
|---|---|---|
| **Meta Muse** | 公开资料描述了每人一台独立 VM、自己的浏览器、跨会话记忆和长期目标。用户关掉应用后工作继续。Muse 可从应用或 WhatsApp 接收指令。独立的 Sentinel 控制外部访问，敏感操作如发邮件和付款需要用户确认，凭证对 Agent 隔离。付款公开提到 Stripe Link。 | 参考交互和权限边界。Meta 的公开资料足以确认这些行为，通用连接器栈、记忆存储和后台调度细节以官方披露为限。 [产品发布](https://about.fb.com/news/2026/09/introducing-muse-personal-ai-agent/) · [Sentinel 和安全设计](https://muse.ai/blog/security-and-safety-for-ai-agents-our-approach-with-muse) |
| **CopilotKit/OpenMuse** | 通过 Google OAuth 直连 Gmail、Google Calendar，服务端加密保存凭证。读写权限分开申请，发邮件和变更日历逐次审批。独立 Playwright worker 持久化 Chromium profile 并提供接管界面。对话用 CopilotKit Intelligence / AG-UI，后台任务用 SQL 租约恢复，公开搜索接 Parallel。 | 最适合参考连接器、浏览器接管和任务活动记录。Google 集成、浏览器和任务恢复是不同能力，可以分别引入。 [仓库说明](https://github.com/CopilotKit/OpenMuse) · [OAuth 源码](https://github.com/CopilotKit/OpenMuse/blob/a4944f4c471d3dc93b8bfc060b5da309b5216ab2/apps/server/src/google-auth.ts) · [浏览器 worker](https://github.com/CopilotKit/OpenMuse/blob/a4944f4c471d3dc93b8bfc060b5da309b5216ab2/apps/worker/src/browser.ts) · [任务 worker](https://github.com/CopilotKit/OpenMuse/blob/a4944f4c471d3dc93b8bfc060b5da309b5216ab2/apps/server/src/engine/worker.ts) |
| **OpenMuseAgent/OpenMuse → nanoMuse** | 原仓库归档并迁到 nanoMuse。原版把个人记忆存 SQLite，用关键词召回，有配置时可加兼容的 embeddings。邮箱触发器用 IMAP UID 检查新增邮件。工具执行前经过 Sentinel 的策略和审批，再取凭证。原版也支持 MCP 和 PWA 推送。 | 参考可查看、编辑、忘记的记忆，以及把外部触发和用户审批做成可追踪状态。手机端和设备接管可随产品需要独立演进。 [归档仓库和迁移说明](https://github.com/OpenMuseAgent/OpenMuse/blob/76b71309f49e5055d216cc57e41a68ae660e3190/README.md) · [记忆存储](https://github.com/OpenMuseAgent/OpenMuse/blob/76b71309f49e5055d216cc57e41a68ae660e3190/openmuse/memory/store.py) · [邮件触发器](https://github.com/OpenMuseAgent/OpenMuse/blob/76b71309f49e5055d216cc57e41a68ae660e3190/openmuse/triggers/mail.py) · [nanoMuse 审批设计](https://github.com/nano-muse/nanoMuse/blob/main/docs/sentinel.md) |
| **diggerhq/openmuse** | Web 应用建立在 OpenComputer 上。主 Agent 把主题工作交给 worker，主 Agent 和 worker 都能读用户资料，worker 保存自己主题的笔记，完成后结果回到主会话。会话历史与可编辑笔记各自持久化。 | 参考「主题工作完成后回到原会话」和「用户偏好、项目笔记分别管理」两点。Go Harness 可以用已有 Host、会话和子 Agent 实现类似体验。 [仓库和架构](https://github.com/diggerhq/openmuse#how-its-built) · [主题 worker](https://github.com/diggerhq/openmuse/blob/2fff664dac90f8d24b67b754912485106660c034/opencomputer/agents/topic-worker/agent.ts) |

## Go Harness 的基础和缺口

| 能力 | 现状 | 下一步 |
|---|---|---|
| 云电脑和执行 | Controller 按账号管理 Docker 或 ECI Host。Host 已有 shell、Chromium、OfficeCLI、文件面板、工具调用和 SSE。 | 给 Chromium 加结构化工具和用户接管，优先验证只读浏览、截图、下载回当前会话。 |
| 持续工作 | 已有 v4 会话日志、队列、Goal、Schedule 和定时投递。活动中的 Schedule 会阻止当前 Host 被空闲回收。 | 用现有 Schedule 做固定时间简报。后续把外部事件的接收和待投递状态放在 Controller，让停止的 Host 也能按需唤醒。 |
| 用户数据 | 目前能配置模型和 DeepSeek 搜索。会话历史和工作目录可持久化。 | 增加账号授权的邮箱/日历，以及由用户控制的跨会话记忆。 |
| 反馈和控制 | Web 会话已能通过 SSE 收到工具输出和提问，入口目前按账号名打开，用于可信本地访问。 | 增加跨会话的活动入口和离线通知。引入真实私有账号前，把授权回调、连接查询和审批绑定到可验证的用户身份。 |

对应实现见 [架构](architecture.md)、[会话与 Schedule API](api.md)、[当前启动方式](../README.md)、[空闲回收](../internal/controller/lifecycle.go)、[Host 活动检测](../internal/hostruntime/native_host.go) 和 [Host 镜像](../build/Dockerfile.host)。

## 第三方服务怎么选

| 优先级 | 服务或依赖 | 接入方式与用户收益 |
|---|---|---|
| **首期** | **Google Calendar API** | 在设置页授权，只读今日和近期行程、找空档、给出会前提醒。第一条链路可以只读事件，以 Google 提供的窄 scope 为准。 [Calendar scopes](https://developers.google.com/workspace/calendar/api/auth) |
| **首期** | **Gmail API** | 先做搜索、读取选定邮件和在会话里生成草稿文本。上线时评估受限 scope 的应用验证，以及服务端处理受限数据适用的评估要求。后续加入真实发送时单独申请权限并逐次审批。 [Gmail scopes](https://developers.google.com/workspace/gmail/api/auth/scopes) · [Google 服务端 OAuth](https://developers.google.com/identity/protocols/oauth2/web-server) |
| **首期** | **Web Push，ntfy 可选** | Web Push 为现有前端提供关页后通知，点开回到对应会话。内测如想快速验证手机提醒，可选 ntfy 的认证主题和 HTTP 接口。通知仅带简短状态和跳转链接，正文在会话里查看。 [Web Push](https://developer.mozilla.org/en-US/docs/Web/API/Push_API) · [ntfy 发布与认证](https://docs.ntfy.sh/publish/) |
| **按目标用户** | **飞书开放平台** | 国内团队优先试飞书日历。按用户授权读取可访问的日程，后续按同一连接器契约加飞书消息或文档。官方 MCP 可作为开发时的接口参考。 [飞书日历](https://open.larkenterprise.com/document/quickly-build-a-feishu-calendar-schedule/introduction) · [飞书官方 MCP](https://github.com/larksuite/lark-openapi-mcp) |
| **第二阶段** | **Playwright + 现有 Chromium，Browserbase 可选** | 先做受控的浏览器工具和接管界面，便于用户登录、继续操作或取回文件。登录态需要单独保护和按账号隔离。若要验证托管浏览器，Browserbase 提供持久 Contexts 和交互式 Live View。 [Playwright 登录态](https://playwright.dev/docs/auth) · [Browserbase Contexts](https://docs.browserbase.com/platform/browser/core-features/contexts) · [Live View](https://docs.browserbase.com/platform/browser/observability/session-live-view) |
| **随连接器数量增长** | **Nango 或 Composio** | 首期两个 Google API 共用一套授权逻辑，适合在 Go 里直接接。需要维护多个厂商授权、刷新和 API 请求时，可选 Nango 托管或自托管的授权代理，继续自行定义业务工具。若需要快速验证许多现成工具，可试 Composio，按账号和工具白名单接入。 [Nango Request Proxy](https://nango.dev/platform/request-proxy) · [Composio 授权](https://docs.composio.dev/docs/authentication) |
| **扩展入口** | **官方 MCP Go SDK** | 为高级用户启用已配置的 MCP 服务。接入已有工具调用链，由服务端决定可用工具及读写审批。官方 SDK 通过 Go Modules 引入。 [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk) |
| **可选对照** | **Parallel Search / Extract** | 保留现有默认 DeepSeek Search。只有遇到英文资料、带引用检索或正文抽取的具体问题时，拿同一组任务对比 Parallel；接入时复用现有 Host provider 接口。CopilotKit 版使用的独立 Search MCP 已标为 legacy，Parallel 为新接入提供 Search / Extract v1 API。 [CopilotKit OpenMuse](https://github.com/CopilotKit/OpenMuse#public-web-search) · [Parallel Search](https://docs.parallel.ai/search/search-quickstart) · [Parallel Extract](https://docs.parallel.ai/extract/extract-quickstart) |

Go Harness 的长期记忆由 Host 内置，采用 Hermes 的本地记忆文件和按需历史检索。Host 在每个模型步骤加载账号的当前记忆。 [当前记忆方案](hermes-agent-memory-design.zh-CN.md)

## 落地方式

**在 Controller 管理连接，在 Host 运行任务。** 设置页显示「连接、已授权范围、断开」。Controller 处理 OAuth 和回调，以可信身份绑定连接。使用 Redis 保存账号的连接元数据和加密的 token，部署侧单独提供加密密钥。Host 通过受限的内部接口执行明确的读取动作，收到结果及来源 ID，第三方凭证留在 Controller 或可选的授权代理里。第三方 HTTP/SDK 调用仍放 `thirdparty/`，工具定义与账号、审批逻辑由各自业务层负责。当前以账号名打开的本地流程适用于可信环境。若部署给多人，私有连接、回调和审批必须绑定已认证的用户，客户端传的账号名不足以授权。 [当前 Controller 账号入口](../internal/controller/http.go) · [第三方集成目录](../thirdparty/README.md) · [Google OAuth](https://developers.google.com/identity/protocols/oauth2/web-server)

**用 Schedule 验证后台体验，再按需要做外部事件。** 首期让用户在已有 Schedule 中设置时间和任务，Host 在到点后查日历和邮箱并写回原会话。Controller 持久化待发通知，并用会话事件标识去重。若要支持「有新邮件时处理」，再由 Controller 维护订阅与游标，收到事件后按账号复用或启动 Host，通过现有入队链路投递。Gmail 的近实时方案使用 Pub/Sub 和 `historyId`，需要续订和补拉。没有外部事件的定时简报可以直接复用现有 Schedule，无需新部署一套调度服务。 [Go Harness 调度](../harness/schedule/service.go) · [Gmail Push](https://developers.google.com/workspace/gmail/api/guides/push)

**为外部写操作保留可恢复的审批。** 加入发送邮件、创建日历事件时，界面展示收件人、内容或日历与时间。批准记录绑定账号、连接和确定的操作参数，执行前再次检查连接仍有效。审批卡可以复用现有提问的 UI 和 SSE 通路，授权决定需由服务端校验。执行完成后记录提供商返回的 ID；结果不明时先查实际结果，再决定是否重试。 [CopilotKit 的授权与审批流程](https://github.com/CopilotKit/OpenMuse#configure-the-agent-and-google) · [现有 API](api.md)

## 首期验收

1. 用户在设置页连接日历和邮箱。Agent 能引用今天的真实日程和指定邮件，用户能看到来源。
2. 用户设置工作日简报后关掉网页。到点运行，结果回到原会话，收到的通知可以打开它。Controller 重启或通知重试后，用户只看到一次结果和一次通知。
3. 用户能断开连接。后续调用显示授权失效，不再读取外部数据。若接入跨会话记忆，修改或删除一条偏好后，新会话使用更新后的数据。

首期依赖限于选中的日历/邮箱 API 和通知通道，其他服务均按功能需要启用。开发优先级可从「日历只读 → 定时简报和通知 → 邮箱 → 审批写入」推进，尽早验证整个闭环。
