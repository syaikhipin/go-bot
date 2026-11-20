# Go Bot 插件系统重新设计：Cordis + Host Bridge

方案日期：2026-11-19；最近核对：2026-11-20。状态：Go 四环节、Host Bridge、内置市场及 Web/Mobile 授权交互均已实施；最终部署回归与交付核对见第 14 节。

2026-11-20 方案调整：从目标设计中移除 PreExecute 的独立 guard 和插件 guard 接口。该次请求只更新方案；随后恢复的实施任务按此决定核对并清理代码。保留执行前策略、审批、取消检查、工具自身权限判断与沙箱，PostExecute 的重复调用提醒也保持独立。

总原则：**核心能力内置到 Go，不向业务插件开放通用的 `pre/execute/post + next` 执行链。还原上游的功能和行为，不复制它为了“一切插件化”采用的组织形式。** Go 以统一的 PreExecute → Execute → PostExecute → Result 管完整调用流程，工具实现自己的业务，Cordis 管插件装配和生命周期，Host 事件供观察。第 1.1 节汇总本次讨论的正式决定；第 15 节保留源码审计依据。

本文重新定义插件系统，不沿用之前方案的接口兼容目标。后续实施以本文为准；`plugin-framework-plan.zh-CN.md`、`go-bot-plugin-system-redesign.zh-CN.md` 和 `cordis-go-host-plugin-plan.zh-CN.md` 不再作为实施规范。

**恢复执行依据：**执行当前 Goal 时继续使用本文件的最新正文，特别是第 1、2.3、6、8.1、12、13 节；不回退到旧的阶段事件链或跨进程 next 方案。Goal 概述中较早的“guard”要求已取消，实施中已删除 PreExecute 的独立 guard 检查及插件 guard 接口，不另设同义的最终限制层；“资产适配”按第 6.8 节收缩为保留内置能力、删除新增插件 Assets 通道，不据此重新开放接口。第 15 节只提供上游调研证据，已排除模块不是实施需求。

本文的“交互”主要指 **Go Host 与 Node 插件进程之间的通信、请求、回调、通知和生命周期**。前端人机交互沿用现有机制，不在本次重新设计界面。

**本次收口规则：**第 1—13 节是目标合同，第 14 节仅记录实施现状，第 15 节仅记录上游研究。研究中出现的接口、旧代码已经实现的接口，都不能自动成为本轮公开能力。公开入口以第 6 节、公开通知以第 7.2 节为完整白名单；实施步骤、删除清单和验收不得重新扩大这两个边界。本轮不开放 Assets 服务，也不为插件新增二进制 spool 或图片/文件交付结果协议。

## 1. 设计决定

采用 **Go 内置 Harness + 独立 Cordis 进程 + 一个内置 Host Bridge 插件**。

Host Bridge 是一个运行在 Cordis 中的基础插件，向业务插件提供 `ctx.host`。Go Host 进程本身不变成 Cordis 子插件，不由业务插件启动，也不搬入 Node。

```text
Go Host：核心实现、状态与执行权
             ⇅ 一条双向异步 JSON-RPC 连接
Node/Cordis
  ├─ 内置 Host Bridge：提供 ctx.host，转发调用和通知
  ├─ 邮件插件
  ├─ 旅行服务插件
  └─ 其他第三方插件/插件自己的 Cordis 服务
```

具体选择：

1. Cordis 只负责插件的装配、服务注入、配置、事件注册和生命周期。
2. `ctx.host` 是本项目定义的 SDK。接口依据 Go 内置模块及业务扩展需求设计；上游服务的功能和行为应先核对，再决定 Go 内部实现和必要的对外接口，不整套照搬其核心插件装配方式。
3. 所有需要 Go 确认的方法均返回 Promise。`await` 只暂停当前调用函数，不阻塞 Node 线程。
4. 工具业务回调与观测通知分开。Go 不通过 Cordis pre/execute/post 事件推进工具调用；ctx.on 订阅本项目 host/* 观测通知，返回值不改变当前执行结果。
5. Go 保留 PreExecute、Execute、PostExecute 和最终 Result 四个处理点，在对应环节内置上游相关核心模块的处理逻辑；Go 负责顺序、权限、审批、结果校验、事实提交和 Agent 推进，Node 只运行插件提供的业务函数。
6. 不提供跨进程 next、工具阶段 continuation、完整 Agent/Session 代理或另一份 Node 核心执行器。插件工具由 Go 在调用其业务函数时通过 RPC 直接执行回调。
7. 不提供插件客户端代码、通用 actions 或 storage；插件仍可提供普通 Cordis 服务给其他插件使用。
8. 开发期直接更换协议、SDK 和配置，不维护旧插件或旧配置迁移。
9. 本次不引入上游 `packages/experimental/*` 的包或实现，也不以其监听器作为必须补齐的核心行为；本项目已有的独立功能继续保留。

### 1.1 本次讨论的正式结论

**核心能力归属：**AgentLoop、Session、工具系统、权限/沙箱、审批、Skills、提示组装、凭证、授权及 MCP 等基础模块在 Go Host 内实现。与现有项目相关的上游核心监听逻辑，应逐项归入相应 Go 模块，保持所需功能与行为；不将这些逻辑重新包装成 Node 核心插件。业务插件通过 Bridge 使用基础能力，不能接管核心流程或会话提交。

| 上游通过核心插件实现的能力 | 我们的实现位置 |
| --- | --- |
| 执行前 checkpoint、调用记录 | Go 会话与工具调用流程 |
| 参数校验、权限和审批 | Go 工具系统及权限模块 |
| 超时、取消、并发控制 | Go 工具执行器 |
| 输出限制、错误规范化、结果校验 | Go 结果处理逻辑 |
| 文件交付、日志提交、前端投影 | Go 会话提交逻辑 |
| 重复调用提醒等运行保障 | Go Agent 运行逻辑 |

PreExecute、Execute、PostExecute 是三个执行处理阶段，Result 是最终结果确认环节；四个处理点共同组成 Go 工具系统的统一流程，并不与某个工具绑定。上游相关核心插件在各事件阶段承担的逻辑，应在对应 Go 阶段直接调用；不只是把逻辑任意分散进各工具函数，也不只是借用阶段名称。这些能力必须在没有 Node 监听器时正常工作。不能只删除上游事件桥接，却遗漏原先借事件完成的 checkpoint、取消收尾、输出治理或结果提交行为；实现时应核对 Go 现状、补齐缺口并验证。此处要求按职责承载已有范围内的核心行为，不扩大成引入全部上游实验模块或运行任意上游插件。

**业务插件：**插件提供自己的工具、Skill、附加提示、MCP 声明和授权业务。提供工具时只需登记定义及业务 `execute()`；不要求每个工具实现三个固定的 pre/execute/post 接口。工具自己的检查、资源准备、实际操作和清理可直接写在业务函数中。

```ts
await ctx.host.tools.register({
  name: 'send_mail',
  description: '发送邮件',
  inputSchema: { type: 'object', properties: {} }, // 由插件定义真实业务参数
  async execute(args, call) {
    // 检查本次业务参数、准备资源；需要审批时先请求 Go 审批服务。
    // 实际发送与清理在此完成，返回 SDK 规定的 ToolResult。
    return { content: [{ type: 'text', text: '业务执行结果' }] }
  },
})
```

**公开事件：**不额外新增工具开始/完成的插件通知。Host 仅按第 7 节公开 Skill、凭证、授权、MCP 等状态变化，插件通过原生 `ctx.on(...)` 订阅并按实例标识等字段筛选；返回值不改变工具执行决定或结果。Go Result 环节与现有前端会话中的工具调用/结果消息保持独立，不增加替代名称的工具事件。Cordis 自身和插件之间的本地事件仍保留原生语义。

**执行前处理范围：**PreExecute 保留调用准备、执行前策略、必要审批和取消检查，不设置独立 guard 检查，也不开放插件 guard 接口。工具自己的业务限制留在 execute 内；权限和沙箱由既有 Go 模块处理。需要用户决定的操作通过统一 Go 审批服务完成。本轮不增加“结构化结果提交后禁止所有后续工具”的公共检查。

**Cordis 与通信：**插件装配、依赖注入、作用域、配置更新、卸载及生命周期使用原生 Loader/Fiber/effect。Host Bridge 提供统一异步 JSON-RPC 适配；Go 调用业务回调，插件可 await 已开放的 Go 服务。不增加跨进程 next、Node 工具执行器、Agent/Session 代理或 Go 自建插件生命周期状态机。

恢复实施时删除与上述边界冲突的旧阶段桥接、全局 guard 接口、重复 Node 核心服务及专属测试；保留 Go 核心处理及业务回调所需的通信能力。方案调整不表示这些删除和生产切换已经完成。

### 1.2 工具主链：Go 统一注册与执行

内置、插件和 MCP 工具共用 Go 工具接口与实际调用流程；工具来源不改变执行权归属。

**注册与发现：**

- 内置工具由 Go 装配，得到包含定义、参数校验、实际执行函数的 Go Tool。
- 业务插件调用 `await ctx.host.tools.register({name, description, inputSchema, execute})`。Node 保留函数；Go 登记名称、定义与回调句柄，产生同一种 Go Tool 接口的 RPC 适配对象。
- MCP 工具同样由 Go MCP Client 提供 Go Tool；不先绕经 Node 再请求同一个 MCP 服务。
- Go 在模型 step 开始时把上述工具合并到同一实际工具表，冲突拒绝、生成统一模型声明。不是三套 AgentLoop 或三套执行管线。

**调用：**

```text
AI tool call
 → Go PreExecute：调用准备、内置策略/必要审批、取消检查
 → Go Execute：执行边界保障，调用固定的工具，取得规范化结果
    ├─ 内置 Tool：调用 Go 函数
    ├─ 插件 Tool：RPC → Node 已登记的 execute 回调
    └─ MCP Tool：Go MCP Client 调用服务端工具
 → Go PostExecute：对应核心结果处理、最终校验/物化
 → Go Result：确认最终结果、执行对应核心收尾、提交事实
 → Agent 继续
```

具体超时、并发、取消和错误分支继续由 Go 管线管理，不为插件工具另造一套。插件卸载后通过 Cordis effect 撤销 Go 登记；已捕获的调用不得解析到另一个同名新实现。

**公共处理与业务逻辑：**工具系统的超时、取消、并发、checkpoint、权限和结果提交直接留在 Go 的对应阶段，具体顺序、来源模块与返回类型见第 8.1 节。工具特有的参数检查、资源准备和清理写在该工具的业务实现中，不要求每个工具实现额外 pre/post 接口。插件若需要确认某个业务操作，在自己的 execute 中、实际副作用之前 await Go 审批服务。

**审批规则来源：**Go 内置执行前策略可依据明确规则返回 allow/deny/ask，由 Go 审批服务处理 ask；不是 Go 自动推断所有业务风险。内置工具自己的权限判断和插件工具自己的业务判断也可申请同一服务。当前源码已有 Go 策略入口，但生产装配尚未配置通用风险规则，不能宣称已默认对全部工具做风险审批。

**对外扩展边界：**本轮不公开通用 pre/execute/post 中间件或 registerGuard，也不在 Go PreExecute 保留独立 guard 调用；既有执行前策略、审批、权限和沙箱继续有效。外部插件贡献工具、Skill、附加提示、MCP 和授权流程；状态观察走 host/* 通知。此前新建的 host-tool-events/host_tool_events 阶段桥接及专属测试应在恢复实施后删除，不作为第二种运行方式保留。

## 2. 当前代码核对与职责归属

本节依据当前工作树，而非根据旧方案推测。核心装配入口是 `internal/host/host.go`、`harness/assembly/native_runtime.go` 和 `harness/assembly/resources.go`。

| 当前模块 | 代码位置 | 新设计中的职责与开放方式 |
| --- | --- | --- |
| Host 与用户运行环境 | `internal/host` | Go 创建基础模块和 Bridge；只开放非敏感的运行环境说明 |
| AgentLoop、step/turn、模型重试、压缩 | `harness/agent` | 完全内置；插件不能替换或手动驱动 |
| 会话事实、队列、工作目录、恢复、归档 | `harness/session`、`internal/hostruntime` | 完全内置；回调只获得当前调用的必要数据 |
| 工具管线 | `harness/agent/tool_pipeline.go`、`native_tool_pipeline.go` | Go 统一执行内置与插件工具；开放工具业务注册，公共处理直接内置 |
| 文件、Shell、沙箱、权限 | `internal/agentfs`、`internal/executor`、`harness/permission` | 保留现有实现，不包装成插件执行器；Bridge 不提供提权和执行器替换 |
| 审批、问题、Plan | `harness/permission/approval.go`、`harness/interaction`、`harness/plan` | 申请审批/提问可复用 Go 服务；Plan 状态仍由内置流程管理 |
| Skills 与种子初始化 | `harness/skills`、`harness/tools/skill.go` | Go 是最终发现/加载主体；插件可贡献目录、虚拟 Skill 或 Provider |
| 系统提示和运行上下文 | `harness/assembly/prompt.go`、`harness/agent/native_prompt_assembly.go` | Go 最终组装；插件只能贡献附加说明 |
| 长期记忆、会话检索 | `harness/memory`、`harness/sessionsearch` | 保留 USER.md/MEMORY.md、FTS 与内置工具；本轮不另开服务 |
| Goal、Todo | `harness/goal`、`harness/session/todo.go` | 保留内置状态与工具，不让插件改自动续轮或日志事实 |
| Subagent、Workflow、结构化输出 | `harness/subagent`、`harness/workflow`、`internal/hostruntime/native_subagents.go` | 保留 Go 生命周期，不提供 Agent 工厂或工作流执行器替换 |
| Jobs | `harness/jobs` | 保留现有 managed Job；插件 Promise 不自动成为 Go Job |
| Schedule | `harness/schedule` | Go 调度/投递和工具保持不变；定时任务中的 Agent 可以正常使用插件工具 |
| 图片附件、上传文件 | `harness/attachments`、`harness/content` | 保留 Go 内置存储与工具使用；本轮不开放插件资产入口 |
| present 和文件下载 | `harness/tools/present.go`、`internal/agentfs/presented.go` | 保留内置 present 的校验、事实提交和投影；不新增插件文件交付通道 |
| 模型、网页搜索、抓取、生图 | `providers/model`、`providers/web`、`providers/image` | 内置 Provider 保持不变；插件可以新增自己的工具，不替换用户模型/供应商配置 |
| 凭证与授权 | `internal/credentials`、`internal/authorization` | Go 唯一实现，Bridge 提供按插件归属的调用入口 |
| MCP 及协议 OAuth | `internal/mcpclient`、`mcp` | Go 唯一实现，插件注册服务，由 Go 发现并接入工具 |
| 插件配置、安装和管理 | `plugin`、`internal/plugins`、`extensions` | 保留有用包管理基础，替换旧协议/SDK 和上游兼容装配 |
| Controller、Redis、Docker/ECI/NAS | `internal/controller`、`providers/runtime` | 属于管理/部署面，不向业务插件开放 |
| OfficeCLI、agent-browser、Node、Python | Host 镜像及预装 Skills | 保持基础环境，不重新插件化 |

开放原则是“贡献能力、复用基础服务”，不是“将每个 Go 导出方法变成 RPC”。没有开放的核心模块仍正常工作，也没有被移到 Node。

### 2.1 上游的插件化组织与我们的区别

已核对 DeepSeek Harness 固定提交 `ca33fd6843b205e279c7ccb6cb73a472c78f5760` 的架构说明、完整 packages 目录、工具定义和 MCP/凭证服务文档。固定版本用于让调研可复查，不构成 SDK 兼容目标。

上游的模型适配、工具系统、会话和 AgentLoop 都能通过 Cordis 包装配/替换；业务插件和基础服务位于同一插件装配体系。它的公共 ctx 因而同时承载核心内部依赖和第三方扩展入口。[上游架构](https://github.com/deepseek-ai/deepseek-harness/blob/ca33fd6843b205e279c7ccb6cb73a472c78f5760/docs/architecture.md)

我们将这些核心职责固定在自己管理的 Go Host 中。Go 内部模块之间直接调用；第三方插件只通过 Bridge 使用明确选出的能力。上游核心插件之间的接口不能整套复制成我们对第三方的 RPC 接口。

### 2.2 上游模块 → Go 内置模块 → 对外功能

下列是职责对应，不是宣称全部特性逐项等价。“内部”表示模块留在 Go 并正常提供能力，但不把其管理对象交给插件。

| 上游包/职责 | 当前 Go 内置对应 | 应向插件提供的功能 | 保持内部的部分 |
| --- | --- | --- | --- |
| core/agent、core/agent-loop | NativeLoop、NativeHost、NativeRuntime | execute 回调中的必要调用数据 | 驱动、创建/恢复 Agent、step/turn、队列、续轮、Agent 注册表 |
| core/session；session/session-persistence-jsonl、session-format、session-projection | `harness/session` 事实日志/恢复/投影 | 当前 sessionId；Go 提交工具结果时自动产生所需事实 | append/flush、writer、格式修复、投影控制、持久事件发布 |
| session/working-directory；workspace/workspace | header cwd、实际工作区与安全文件读取 | 当前 call.cwd/workspace 数据 | 改其他会话目录、管理全部工作区、归档/删除、文件交付校验服务 |
| core/tools、core/tool-executor-native | Go Registry、调度器、ToolPipeline | tools.register；业务 execute；host/* 状态观测 | 通用 pre/execute/post 中间件、跨进程 next、全局 guard、executor 替换、任意工具 dispatch、事实提交 |
| core/scope | 当前调用绑定及插件实例生命周期 | 自动 owner/fiber 归属、invocation | 不引入完整上游 Agent scope，也不要求插件创建 scope 才能注册 |
| core/system-prompt；context/agent-instructions、time-context | Go prompt/runtime context/时间组装 | prompts.register 的附加 system/context 文本 | 核心 system 替换、压缩策略、请求头和模型消息重建 |
| llm/llm、llm-retry、token-meter 及各模型适配 | `thirdparty/llm`、NativeLoop retry/pressure、模型路由 | 仅必要的只读模型描述，不提供模型 client | LLM 请求、密钥、Responses/Chat/Anthropic 协议、token 预算、重试 |
| compaction/compaction-* | native_compaction、prune、image_offload | 无独立插件接口 | 上下文保留/压缩、图片 offload、日志事实和请求投影 |
| fs/fs、fs-local、fs-observation-policy、fs-sandbox、tool-fs | agentfs、Go 文件工具与权限包装 | 当前调用的 cwd/workspace 数据；不新增文件服务 | 文件观察版本、任意 mutation API、绕过权限的文件执行服务 |
| shell/*、subprocess/*、sandbox/* | executor、shell 工具、Linux 沙箱与 permission | 工具回调取消信号、Go 审批申请 | shell executor、环境覆盖、放宽沙箱、权限模式设置 |
| guard/timeout-policy、repeat-tool-reminder | Go 工具 timeout、repeat-tool 检查 | 工具声明 timeoutMs、concurrency；运行保障由 Go 直接处理 | 原生默认策略和结果终结，不由 Node 中间件替换 |
| interaction/user-approval、permission-presets | Go ApprovalService、会话权限策略 | approvals.request；插件业务在操作前主动申请 | 策略修改、用户同意/拒绝答复、越权执行授权来源 |
| interaction/user-questions、tool-ask-user；plan/plan-mode | NativeQuestions、Native Plan 与内置工具 | questions.ask；活动工具内提问 | Plan 状态转换、用户答复入口和非限时 Plan 审查 |
| jobs/jobs、jobs-local、tool-jobs | Go Jobs Registry、job_output/list/kill | 无独立 ctx.host.jobs；插件 body 自己遵循 signal 并返回 | managed Job 身份、完成投递、输出游标和跨会话取消 |
| goal/goal、goal-round-driver、tool-goal；todo/tool-todo | NativeGoal、Todo 日志与工具 | 无独立插件服务 | 目标预算/完成、自动续轮、Todo 事实写入 |
| subagent/*、workflow/* | Native children/fork、Go Workflow 与结构化输出 | 普通插件工具可在子 Agent 中运行，无需额外接口 | 子 Agent 工厂、父子关系、控制消息和 Workflow driver |
| schedule/schedule、tool-schedule | Host Schedule、目标准备与投递 | 到期会话可使用插件工具；不新增 Node 定时器替代调度器 | 创建/编辑/投递服务对象，本轮不向普通插件开放 |
| skill/skill、skill-filesystem、tool-skill | Go Registry、目录扫描、加载工具 | skills.register/registerDirectory/registerProvider/list/get/invalidate | 最终目录合并、用户覆盖优先级、seed 初始化 |
| attachment/attachment、attachment-local | Go 图片对象和上传文件附件 | 本轮无插件接口 | 现有图片入库/读取、上传附件、元数据验证 |
| deliverables/tool-present、deliverable-file-references | Go Present、PresentedPath/OpenPresented、前端 file 投影 | 本轮无插件接口 | 内置 present 与 Go 交付事实提交；不恢复工作区扫描/产物推测 |
| credentials/credentials、credentials-local | `internal/credentials` | 本实例记录的 get/put/update/remove/describe/list；显式引用的 resolve | 底层写入器、任意 owner 批改、全局环境枚举 |
| credentials/authorization | `internal/authorization` | flow 注册与状态/取消；Go 调 run、notify/prompt/commit | 准入、活动尝试、提交确认和答复归属 |
| mcp/mcp-client、mcp-manager、mcp-resources | `internal/mcpclient` Manager/Connection/Scope | mcp.register/list/inspect/restart/authorize/cancelAuthorization/signOut | 连接、协议、工具发现和执行；不因职责对应宣称资源特性已全量实现 |
| session-query/*（本项目已有会话检索独立保留） | `harness/sessionsearch` 的现有 FTS | 无独立插件查询 API | 全会话检索索引和正文；由内置 session_search 使用 |
| document/office-kit、skill-office、browser-use 及实验驱动 | 本项目 OfficeCLI/agent-browser CLI + Skills | 注册额外 Skill/工具，可调用自身业务 SDK | 不引入上游另一套 Office/browser runtime，不强求实现一一相同 |
| boot/plugin-manager、plugin-market、app-boot、hmr | 当前 Go Manager/包管理 + 新 Cordis loader | 当前包的 apply/config/lifecycle；自有 SDK | 安装/启停其他插件、上游 bundle/patch 全树控制 |
| settings/settings；api/*、host/webserver、sdk/server、client/* | Controller、HTTP/SSE、自有 Web/Mobile | 配置 schema 由管理器展示；不提供插件前端代码 | 路由、前端会话、全局 SSE、账号和端口管理 |
| storage/*（不新增公共 storage/actions） | 不作为新的公共基础能力 | 本轮不提供 storage/actions | 不能因上游存在就恢复已决定删除的接口 |
| 本项目 USER.md/MEMORY.md 长期记忆 | `harness/memory`，参考 Hermes 的本项目实现 | 本轮不另开服务，内置 memory 工具继续可用 | 不将这套实现误称为来自上述上游核心插件的直接对应 |

调研来源除架构说明外，还包括固定提交的 [packages 目录树](https://api.github.com/repos/deepseek-ai/deepseek-harness/git/trees/ca33fd6843b205e279c7ccb6cb73a472c78f5760?recursive=1)、[基础装配](https://github.com/deepseek-ai/deepseek-harness/blob/ca33fd6843b205e279c7ccb6cb73a472c78f5760/packages/bundle/base/README.md)、[工具合同](https://github.com/deepseek-ai/deepseek-harness/blob/ca33fd6843b205e279c7ccb6cb73a472c78f5760/docs/subsystems/tools.md)、[凭证服务](https://github.com/deepseek-ai/deepseek-harness/blob/ca33fd6843b205e279c7ccb6cb73a472c78f5760/packages/credentials/credentials/README.md)、[MCP 管理](https://github.com/deepseek-ai/deepseek-harness/blob/ca33fd6843b205e279c7ccb6cb73a472c78f5760/packages/mcp/mcp-manager/README.md)。目录存在只证明模块存在；上表的 Go 责任和开放决定是本项目设计，不从包名推断行为等价。

### 2.3 不应顺带恢复的其他上游模块

上游目录还存在 Agent Teams、LSP、SSH backend、PTC executor、远端控制面、桌面载体、遥测等模块。当前 Go Host 没有全量对应实现，本方案不为了复制插件目录顺带引入它们。将来需要某项能力，先决定其属于 Go 核心还是业务扩展，再设计与已有模块的边界。

**experimental 范围约束：**本次统一排除上游 `packages/experimental/*`，包括 Auto Review、`experimental/hooks-claude-code`、`experimental/hooks-codex`、其他外部 hooks/mods、Liveshare 和实验浏览器/CUA 驱动；其源码只可作为调研参考，不进入实现清单或验收要求。本项目已经独立实现的会话检索、长期记忆等能力继续保留，不能因为上游相似功能位于 experimental 就删除现有功能。

**本次明确排除的上游阶段监听能力：**Stagehand/浏览器 MCP/CUA 资源监听、上游 Office 工具按需激活、实验 mods/hooks 兼容层及上游 UI 的结果监听，不进入第 8 节的 Go 核心迁移清单。浏览器继续由 agent-browser 提供，Office 继续由 OfficeCLI + Skills 提供，自有 Web/Mobile 继续负责展示。第 15 节列出这些上游源文件仅说明调研事实，均不是待实现需求。

因此本次不把上游完整的 ctx 对象作为目标，也不保留兼容别名。“关键运行机制按 Go 核心实现”与“任意上游插件原样可运行”是不同要求，本方案只采用前者所需的职责划分。

## 3. Cordis 与 Host Bridge 的关系

### 3.1 Bridge 是基础插件，不是另一套 Host

Cordis 启动时先加载 Host Bridge，再加载声明 `inject: ['host']` 的业务插件。Bridge 必须握手成功后才提供可用服务；连接失效后让依赖方感知服务不可用。

```ts
// 业务插件写法。类型由 @go-bot/plugin-sdk 提供。
export const name = 'mail'
export const inject = ['host']

export async function apply(ctx, config) {
  await ctx.host.tools.register({
    name: 'send',
    description: 'Send an email through the configured mailbox.',
    inputSchema: mailInputSchema,
    async execute(args, call) {
      const credential = await ctx.host.credentials.get('mailbox')
      const result = await sendMail(credential, args, call.signal)
      return { content: [{ type: 'text', text: `Sent: ${result.id}` }] }
    },
  })
}
```

所有远程贡献自动绑定到发起注册的真实 Cordis 插件实例。卸载自动撤销，业务插件不需要为正常注册再写一套手动清理。register 返回的 Registration 可以用于提前撤销。

业务插件可以照常 `ctx.provide` 自己的服务、`ctx.inject` 其他业务服务或使用本地自定义事件。这些是 Cordis 的本地能力，不需要 Go 为每种业务服务提供 RPC。

### 3.2 基础库选择

本轮保留当前已锁定的 `@deepseek-ai/cordis` 作为 Cordis 基础运行库，避免同时更换容器内核。它的包名不意味着继续兼容 DeepSeek Harness 服务。保留所需的 Cordis loader 基础库；删除不再需要的 `dsh-tools/skill/system-prompt/credentials/authorization/scope/llm` 等运行依赖，类型也不再引用这些包。

将来如换成 `cordis` 官方发行包，是基础库升级，不是插件框架的第二种运行模式。新 SDK 只依赖选定的单一 Cordis 身份，防止插件各带一份运行内核。

依据：[Cordis 服务](https://github.com/cordiverse/cordis/blob/main/packages/core/src/service.ts)、[插件依赖注入](https://github.com/cordiverse/cordis/blob/main/packages/core/src/registry.ts)、[事件机制](https://github.com/cordiverse/cordis/blob/main/packages/core/src/events.ts)。本项目使用锁定版本的真实实现，不修改事件注册函数来模拟另一套内核。

### 3.3 服务归属不能固定为 Bridge 自己

Host Bridge 可使用 Cordis Service 实现并以 host 命名提供服务。每次访问 `ctx.host.tools` 等子入口时，基于 Cordis 跟踪到的调用 Context 创建轻量 façade，从真实所属实例和 fiber 得到 owner/lifetime；不能让所有注册都归到 Host Bridge 的根 fiber。

子入口只绑定通信端与所属上下文，不缓存 Go 的工具、凭证或会话状态作为查询结果；本地 callback handles、pending、Cordis 生命周期清理记录仍是必要的通信数据，不是第二套 Go 状态镜像。业务插件内部子 fiber 的贡献随该 fiber 回收，根实例卸载回收全部所属贡献。两个配置实例使用同一包时也必须有独立 owner、凭证地址和撤销范围。

Bridge 自己的启动/维护调用使用单独的内部入口，不伪装成第三方 owner；来自无有效业务实例的注册直接失败。

## 4. 统一异步通信框架

### 4.1 一个连接，两种消息语义

Go 启动一个 Node 进程，通过容器私有 Unix socket 建立一条双向连接。双方均可发起请求和响应。无需额外公开端口、Redis 消息队列、同步 Worker 或共享内存。

采用 JSON-RPC 2.0：有 id 的 request 必须有成功/失败响应；无 id 的 notification 不等待业务确认。这是传输语义，不是两套同步/异步实现。[协议规范](https://www.jsonrpc.org/specification)

| 用途 | 消息形式 | 何时完成 |
| --- | --- | --- |
| 插件查询/修改 Go 能力 | request → response | Go 完成方法定义的操作后 resolve/reject |
| Go 调用插件工具、Provider、授权流程等 | 反向 request → response | 对应 JS 函数完成后返回结果 |
| Go 通知已发生的状态变化 | notification | 只表示按序排出，不等待监听器完成 |
| 请求取消、连接关闭 | notification / socket 生命周期 | 对方终止对应工作；具体终结结果仍按请求处理 |

RPC 框架只负责 framing、请求分发、id 匹配、pending、错误和取消。工具策略、凭证写入和结果提交的含义由 Go 模块定义，不放进通用 RPC 层。

### 4.2 握手与注册

启动过程：

1. Go 创建私有 socket 和本次扩展运行期，启动 Node；socket 位于临时目录，不进入 NAS。
2. Node 发送 `bridge.initialize`，确认协议主版本、SDK 版本及消息大小限制。
3. Go 返回支持的能力名称及非敏感环境信息。版本不匹配明确失败，不猜测旧字段。
4. Node 完成 Host Bridge 服务装配。
5. 加载器依次/按依赖加载业务插件，等待 async apply。
6. 各 register 的 Promise 必须对应 Go 的实际确认，确认后该贡献即登记可用；async apply 的完成及实例状态由 Cordis 原生 Loader/Fiber 表达，不增加 Go 的 ready 状态或批量激活门槛。

初始化能力列表是兼容协商，不是 Go 运行状态缓存。没有启用业务插件时，Go 不必启动空 Node 进程。

### 4.3 一次普通调用

```text
插件调用 ctx.host.credentials.get('mailbox')
  → Bridge 得到调用插件实例 owner
  → 生成 n42，保存 pending[n42]
  → 发出 request，立即返回 Promise
  → Go 按白名单分派，解析 owner 和 key，读取唯一凭证服务
  → response(id=n42, result=...)
  → Bridge resolve pending[n42]，await 后的代码继续
```

request id 使用方向前缀 n/g 防止双向冲突。收到请求时独立执行 handler，不在 socket 读循环里 await 业务完成；写帧只短暂串行化。等待响应时仍能接收新的反向请求。

### 4.4 一次工具回调与嵌套请求

```text
Go 工具管线 → callback.invoke(executeHandle, args, invocation)
Node execute → await ctx.host.credentials.get(...)
Go 继续处理该 get 请求并响应
Node execute → 返回 ToolResult
Go 收到 callback.invoke 的响应 → 继续校验和提交
```

Go 等待 Node 时不能持有 Session writer、工具注册表、凭证读取锁等对回调所需方法有影响的锁。异步通信不自动解决业务循环：同一工具不能等待自己完成，凭证 update 回调不能递归等待同一 Store 写队列中的其他 mutation。

### 4.5 私有协议清单

插件 SDK 不暴露任意 `ctx.host.call(method, params)`。Bridge 内部使用以下白名单协议；名字不是第三方插件 API：

| 私有方法 | 方向 | 数据/含义 |
| --- | --- | --- |
| `bridge.initialize` | Node → Go request | 协议和能力协商 |
| `contribution.register` | Node → Go request | owner、kind、声明、callback handles；返回 registrationId |
| `contribution.remove` | Node → Go request | 撤销 owner 自己的 registrationId |
| `host.invoke` | Node → Go request | 枚举的 service/method、参数、可选活动调用凭据 |
| `callback.invoke` | Go → Node request | callbackId、所属实例、参数、可选 invocation；返回该函数的结果 |
| `host.event` | Go → Node notification | event name 与只读 payload |
| `request.cancel` | 双向 notification | 原请求 id 和规范化取消原因 |
| `runtime.apply` | Go → Node request | 让原生 Loader 应用实例配置，等待其原生完成/清理结果；不承担 npm 包安装 |
| `runtime.inspect` | Go → Node request | 读取 Loader 实际实例状态和公开诊断；不读 Go 自建实例状态副本 |
| `runtime.shutdown` | Go → Node request | 停止注册、卸载实例并异步清理 |

不提供 tools.stage、tools.continue、dispatch continuation 或任意对象反射。Go 发 callback.invoke 时直接指定登记的业务回调；业务插件只调用第 6 节的 SDK 方法，Bridge 内部再编码为 host.invoke 等私有请求。Go Session 代理和 Node 核心提交回调均不提供。

### 4.6 返回、错误和取消

- 普通成功必须有 `result`；void 编码为 null，缺失业务记录也返回显式 null。不能把没有 result 的畸形响应当成功，不再跨进程模拟任意 undefined 对象。
- 错误使用标准 error envelope，data 包含本项目 code，如 INVALID_ARGUMENT、CONFLICT、NOT_FOUND、UNAVAILABLE、CANCELLED。SDK 还原 HostError，不依赖上游错误类身份。
- AbortSignal 在本地重建，通过 request.cancel 传播，不序列化 signal 对象。插件回调的 signal 同时受调用与插件实例生命周期约束。
- 管理查询/注册使用有限超时；工具 body 继承 Go 工具执行预算；普通审批不套短 RPC 超时。通知不等待响应，不将异步监听器结果当决策。
- 发出变更请求后丢失响应，结果可能未知。不能自动重发邮件、注册或写入。相关运行期失效后由 Go 清理其贡献；业务副作用需要插件按业务 id 自行检查，框架不造通用重试任务表。
- 取消工具 body 后等待它停止/收尾，不能将仍在改文件或发请求的 body 当作已结束。超过有限清理预算则终止共享 Node 进程并报告未知结果，不伪造正常成功。

### 4.7 消息大小与数据范围

保留小 JSON 控制帧，默认 1 MiB 上限，超限明确拒绝。本轮 SDK 只传已定义的 JSON 参数、文本及结构化数据；不提供图片/文件字节或 base64 传输，不建立二进制 spool 协议。

现有 Go 内置附件、生成图片、read_image、present 及前端下载链路继续使用原实现，不经过插件 RPC。业务插件可以在自己的业务代码中处理文件，但写出文件或返回路径文本不自动产生助手 image/file 块或交付事实。将来出现明确的插件资产需求时另行设计，不在本轮预设通道。

## 5. 所有权、注册与调用身份

### 5.1 三种运行期标识

| 标识 | 用途 | 是否插件配置 |
| --- | --- | --- |
| instanceId | 同一个包的某个配置实例，例如 work-mail | 是，创建实例时确定，Host 内唯一 |
| registrationId | 某项工具/Skill/提示等贡献的本次注册 | 否，Go 生成；卸载后失效 |
| invocation | Go 接受某次工具调用后给 Node 的不透明调用凭据 | 否；不能用 sessionId 自造 |

它们不替代现有 userId、hostId 或 sessionId。owner 从 Cordis 实例上下文绑定，Node façade 不接受业务插件传入“替别人操作”的 owner 字段。受信任插件的命名归属不是 OS 安全隔离。

```ts
interface Registration {
  readonly id: string
  dispose(): Promise<void>
}

interface ToolCall {
  readonly id: string
  readonly sessionId: string
  readonly cwd: string
  readonly workspace: string
  readonly signal: AbortSignal
  // SDK 私有保存 invocation；不存在可用字符串重建调用的公共构造器。
}
```

### 5.2 生命周期与注册可见性

所有 register 先在 Node 保存函数 handle，再请求 Go 校验/注册。resolve 表示 Go 已接纳此贡献，后续工具快照/查询即可使用；不等整个 apply 完成后再批量发布。失败清理函数 handle。一个工具被捕获到当前 step 后，后续卸载不能悄悄将其换成同名的新实现。

插件注册的工具在同一账号 Host 内统一可用，不提供 Agent 级 scope、工具 shadow 或 available 回调。Skill 查询仍可按实际工作目录读取文件，这是目录语义，不是插件 Agent 作用域。

register 的 Promise 对应 Go 资源登记确认，初始化成功/失败及回收使用 Cordis 原生状态和 effect。Go 不维护 initializing/ready/failed 等插件实例副本，不新建贡献激活协议或插件就绪状态机。启动/配置请求等待 Cordis Loader 的原生完成/清理 Promise，再返回其实际结果。插件必须 await 必需的注册操作；未等待的 Promise 不被假定为初始化成功。

async apply 后续失败时，由 Cordis 原生清理撤销此前已登记的资源。不保证这些资源在失败前从未被查询/调用，也不回滚已发生的外部业务副作用；本轮没有跨注册原子发布事务。管理器只返回原生状态及应用错误，不重新实现一套实例状态机。

卸载由 Cordis 发起并执行 effect：Bridge 的清理函数撤销 Go 资源、传播取消并释放 callback handles。未完成注册返回时若原生 scope 已关闭，Bridge 仍撤销相应 Go 资源，不能泄漏。Registration.dispose 幂等；Go 只处理资源自身的撤销/取消，不接管插件卸载流程，也不等待正在调用自身的回调造成死锁。

### 5.3 单个进程的实际边界

每个 Host 一个共享 Node 进程，插件间不做进程级隔离。插件代码按受信任扩展处理；不能宣称 Go 的 shell 沙箱会自动限制插件自行使用 node:fs 或 child_process。

Bridge 的白名单、owner 和 invocation 主要保证框架调用正确，不是面对恶意 Node 代码的秘密隔离。当前不引入插件权限弹窗、独立容器或第二套安全沙箱。

## 6. `ctx.host` 公共接口清单

下表是本方案的完整公共边界。所有 Go 请求都返回 Promise；本地 ctx.on 注册和纯函数除外。正式 SDK 自行定义 JSON-safe 类型，不继承上游完整服务类型。

### 6.1 环境与工具

| 方法 | Promise 结果 | 说明 |
| --- | --- | --- |
| `ctx.host.info()` | HostInfo | userId、hostId、dataRoot、apiVersion；不返回密钥、内部 token 或连接句柄 |
| `ctx.host.tools.register(definition)` | Registration | 注册插件工具与 execute 函数 |

```ts
interface ToolDefinition {
  name: string
  description: string
  inputSchema: JsonSchema
  concurrency?: 'exclusive' | 'parallel' // 默认 exclusive
  timeoutMs?: number                   // 省略不增加工具专属 deadline，仍受调用/生命周期取消约束
  execute(args: JsonValue, call: ToolCall): Promise<ToolResult>
}

interface ToolResult {
  content: ResultBlock[]
  data?: JsonValue // 可选结构化结果，不能替代必须的展示/模型内容
}

type ResultBlock = { type: 'text'; text: string }
```

工具有效名为 `<instanceId>__<name>`，只接受模型协议允许的名称字符，最终长度不超过 64；冲突或超长直接报错，不截断、不 shadow 内置工具。不把 businessId 强制塞进每个参数。Go 验证输入 schema 和结果结构；当前 step 捕获实际工具定义，不因执行途中重注册同名工具而换实现。这是执行一致性，不是 Agent 级可见范围。

本项目不要求插件提供 output.render/projectContent/finalizeContent 等多层函数。本轮插件返回有序 text 块和可选 data，Go 转换到已有 canonical result、日志和 UI 投影；不接受 image/file 块、任意 Meta 或伪造的附件/交付引用。data 是附加结构化值，不作为会话事实或交付命令解释；错误通过 throw HostError/普通 Error，由 Go 规范化。现有内置工具的 image/file 能力不受此 SDK 范围限制。没有必需的新 maxOutputTokens、模型 profile 等配置。

Go 执行前策略直接服务于统一工具流程；插件自己的业务限制写在 execute 内，需要用户决定时使用 approvals.request。工具被注册并不自动表示需要审批，也不需要通过某个事件监听器才能执行。
### 6.2 Skills

| 方法 | Promise 结果 | 说明 |
| --- | --- | --- |
| `ctx.host.skills.register(skill)` | Registration | 注册 name、description、content、可选资源目录和调用属性 |
| `ctx.host.skills.registerDirectory(path)` | Registration | 将包内 Skill 根目录交给 Go 现有发现器；支持多个 SKILL.md |
| `ctx.host.skills.registerProvider(provider)` | Registration | 动态 list/get 来源；函数保留在 Node |
| `ctx.host.skills.list({cwd?}?)` | `{items: SkillSummary[], complete: boolean}` | Go 最终目录及完整性，不是 Node 的局部目录 |
| `ctx.host.skills.get(name, {cwd?}?)` | SkillDefinition 或 null | Go 最终胜出的正文和资源信息 |
| `ctx.host.skills.invalidate()` | void | 标记本实例来源变化并通知后续查询；已有相关缓存才失效，不新建第二个目录缓存 |

Provider 为 `{id, list(context), get(key, context)}`，二者均可 Promise；list 返回 `{items, complete}`，item 带稳定 JSON key 和摘要，get 使用该 key 返回正文。无需跨进程搬运任意 JS locator 对象。context 包含 cwd 和 signal，不包含 Agent 实例。

公共 SkillSummary/SkillDefinition 统一使用第 6.11 节的平铺字段；Go 适配器转换原生 catalog 中的 invocation 等嵌套结构，不把内部结构直接泄漏到 SDK。Provider 失败可按现有发现语义使目录 complete=false，但必须保留诊断；调用取消应传播取消，不能伪装为空目录成功。Provider 的 list/get 回调内不能递归调用合并后的 skills.list/get，避免再次进入自身发现；允许读取凭证等无循环的 Go 服务。

registerDirectory 解析相对于安装包根的路径并检查存在；包安装目录是资源来源，不复制到预装 seed，也不让 Node 执行另一套全局 Skill 排序。Go 为插件目录赋现有 bundled 来源优先级，用户文件覆盖规则继续有效。OfficeCLI/agent-browser 的 seed 和用户动态安装流程保持原样。

### 6.3 附加提示

| 方法 | Promise 结果 | 说明 |
| --- | --- | --- |
| `ctx.host.prompts.register({id, location, text, order?})` | Registration | location 为 system 或 context；追加插件说明 |

text 可以是字符串或 `(context) => string | Promise<string>`；Go 在当前组装时调用，context 仅含 cwd、sessionId、必要模型描述和 signal。返回空字符串不贡献正文。按 order、instanceId、id 稳定排序。

不存在 complete 覆盖、变量注册表、section 常量同步查询和 assemble waterfall。插件在 text 中直接组装自己的文案，Go 继续负责原核心提示、记忆、Skill 目录、沙箱指导和上下文压缩。

### 6.4 Credentials

| 方法 | Promise 结果 | 说明 |
| --- | --- | --- |
| `ctx.host.credentials.get(key)` | CredentialRecord 或 null | 读取本实例的记录 |
| `ctx.host.credentials.put(key, record)` | void | 提交本实例记录 |
| `ctx.host.credentials.update(key, mutate)` | CredentialRecord 或 null | Go 串行读—回调—写，支持 token refresh |
| `ctx.host.credentials.remove(key)` | void | 删除本实例记录 |
| `ctx.host.credentials.describe(key)` | CredentialDescription | configured、kind、可写性；无原始值 |
| `ctx.host.credentials.list()` | CredentialSummary[] | 只列本实例记录，不含值 |
| `ctx.host.credentials.resolve(ref)` | string 或 null | 按 Go Store 解析配置中的显式命名凭证引用；不枚举进程环境 |

key 为插件内部短名称，例如 mailbox；Bridge 映射为稳定的本实例 owner/key。CredentialRecord 沿用 Go 已有 api-key/grant 数据能力：api-key 保存 key/env，grant 保存业务 JSON payload；MCP 的专用记录由 Go 内部 owner 管理。owner 标准化和地址校验由框架完成，不让每个插件手工拼 scope。

resolve 的 ref 是 Go 凭证 Store 中的命名引用，不是任意 os.Getenv 接口，也不是本实例记录的 key；解析沿用现有文件引用优先、Controller 提供值后备的规则，SDK 仅返回 value 或 null。它是显式引用查询，与本实例 records 的 get/list 边界不同，不宣称这两者存在相同的 owner 过滤。业务 OAuth grant 与 MCP OAuth 记录由各自协议模块解释，但都使用同一个 Go Store。

update 的 mutate 异步接收当前记录并返回替换记录；null 表示保持不变，删除用 remove。复用当前 Store 的串行写队列，不在等待 Node 时持普通读取锁；回调允许读取已提交记录，但不能嵌套等待任何该 Store 写操作，包括另一个 key 或授权 commit。Bridge 传递 mutation 上下文并对这类重入明确报错，避免死锁；本轮不改成多 key 事务或 CAS。写入成功后才发变更通知。授权提交复用同一 Go Store，不复制第二份令牌库。

这些 API 的归属过滤是避免误用，不是恶意插件隔离；受信任插件在同一容器内的权限边界见 5.3。

### 6.5 Authorization

| 方法 | Promise 结果 | 说明 |
| --- | --- | --- |
| `ctx.host.authorization.register(flow)` | Registration | 贡献获取本实例某个凭证的协议流程 |
| `ctx.host.authorization.list()` | AuthorizationSummary[] | 本实例可用方法和活动状态 |
| `ctx.host.authorization.cancel(key)` | void | 确认取消已被 Go 接受；收尾由所属授权调用完成 |

flow 为 `{key, title, methods, run(session)}`。run 是 Node 回调；Go 根据自有管理请求启动它，session 提供 method、signal、`notify(notice): Promise<void>`、`prompt(request): Promise<string>`、`commit(record): Promise<void>`。commit 仅对 Go 当前接纳的授权尝试及其本实例凭证有效；没有任意设置另一授权尝试为成功的入口。

浏览器授权还可调用 `const callback = await session.callback({state})`，得到只属于当前尝试的 `callback.url`，再以 `await callback.wait()` 接收 query 参数数组。state 来自插件所用服务商库，Go 绑定尝试并校验匹配与一次性接收；插件仍校验 issuer、响应和 PKCE，并交换、验证、提交凭证。URL 的公开入口由 Controller 验证后的地址派生，插件不能选择其他账号或注册任意 HTTP 路由。接收到回调不表示授权完成，query/code 不出现在前端的授权视图或 Redis 中。等待回调和等待 prompt 一样不套管理请求的短超时。

本项目 notify 也采用有确认的 Go 请求：await 只确认通知已交给 Go 交互 surface，不表示用户已阅读，更不等待用户回答。prompt/commit 则分别等待真实答复/实际持久化。没有通用 120 秒 RPC 超时。

Go 控制同一 key 的授权准入、取消、记录提交和结束。Node 只实现具体供应商的跳转、设备码、交换 token 等协议。Go 返回成功前确认该次授权确实提交了记录；run 正常返回不等于已授权。

供应商响应和 token 有效性的业务验证仍由 flow 实现；Go 的提交校验保证记录格式、归属及活动尝试一致，不等于 Go 能独立证明任意第三方 token 有效。凭证授权与工具执行权限审批是不同服务，不能相互替代。

### 6.5.1 上游与当前 Go 实现的实际关系（2026-11-20 源码核对）

**MCP OAuth 与业务插件共用通用 authorization 和 credentials；协议实现各自负责。** 不能将模块不同或管理请求入口不同，表述成两套独立授权基础设施。

上游固定版本 `ca33fd6843b205e279c7ccb6cb73a472c78f5760`：

1. `McpOAuthAuthorization.start()` 获取 `ctx.authorization` 与 `ctx.webServer`。
2. 临时 `registerFlow({key, label, methods, run})`，调用 `authorization.begin(...)`。
3. 通用服务管理本次流程与取消；`run(session)` 回到 MCP 实现，负责发现、回调、PKCE、换取 token 等协议工作。
4. MCP 通过 `session.commit(oauthRecord(grant))` 写入共享 credentials，通用服务确认提交后才报告成功；之后清理临时 flow 和回调。
5. MCP token 刷新由其凭证适配器通过共享 Store 的 `modifyRecord` 串行处理，不要求所有业务插件采用 MCP 的 grant payload。

当前 Go：`internal/host/host.go` 创建唯一 `credentialStore` 和 `commonAuthorization`，同时传给 MCP Manager 与插件 Manager。`internal/mcpclient/oauth.go` 已使用同一个服务的 `Register(Flow)`、`BeginDrained`、`Session.Commit/AdmitCommit`；MCP 协议由 Go MCP 模块和所用 OAuth/MCP 库处理。Node 业务插件的 flow 则经 RPC 在同一个 Go 服务中注册与运行。MCP 与普通 flow 的状态投影和前端入口已按第 9.5 节组合到所属插件详情。

上游通用词汇是 methods、notice 和 prompt：method 提供 id/label；notice 提供 message/url/code；prompt 提供 text/secret/select。它协调与人的交互，不自动知道任意服务商的 OAuth 端点与业务规则。前端优先复用这些合同，不为了每个供应商另写一套页面。

依据：[通用 authorization](https://github.com/deepseek-ai/deepseek-harness/blob/ca33fd6843b205e279c7ccb6cb73a472c78f5760/packages/credentials/authorization/src/index.ts)、[交互类型](https://github.com/deepseek-ai/deepseek-harness/blob/ca33fd6843b205e279c7ccb6cb73a472c78f5760/packages/credentials/authorization/src/types.ts)、[MCP OAuth](https://github.com/deepseek-ai/deepseek-harness/blob/ca33fd6843b205e279c7ccb6cb73a472c78f5760/packages/mcp/mcp-client/src/oauth.ts)、[MCP 凭证与刷新](https://github.com/deepseek-ai/deepseek-harness/blob/ca33fd6843b205e279c7ccb6cb73a472c78f5760/packages/mcp/mcp-client/src/oauth-credentials.ts)。

### 6.6 MCP

| 方法 | Promise 结果 | 说明 |
| --- | --- | --- |
| `ctx.host.mcp.register({name, connection, toolLoading?})` | Registration | 注册本实例服务，由 Go 连接、发现、管理工具 |
| `ctx.host.mcp.list()` | McpServiceView[] | 本实例所属服务的实时 Go 状态 |
| `ctx.host.mcp.inspect(name)` | McpServiceView 或 null | 单项查询 |
| `ctx.host.mcp.restart(name)` | McpServiceView | 等待旧连接清理和新一代初始连接处理，返回实际状态 |
| `ctx.host.mcp.authorize(name)` | AuthorizationAttempt | 启动协议级授权；等待创建尝试，不等待用户整个授权完成 |
| `ctx.host.mcp.cancelAuthorization(name, attemptId)` | void | 取消指定尝试 |
| `ctx.host.mcp.signOut(name)` | void | 按 Go 现有机制清理对应授权，不删除整个插件 |

connection 使用现有 Go 支持的 stdio/Streamable HTTP 配置、凭证引用和协议 OAuth 参数，逐字段校验；没有实现的 transport 明确拒绝。不继承上游宽泛服务对象，不暴露 Connection 指针、任意 MCP RPC 透传或单独的 probeFromClient。

MCP 服务名为 `<instanceId>__<name>`，按当前 Go 规则检查最终名称不超过 32 字符；工具名仍另按 Go/模型名称空间规则校验，不能与服务名长度限制混用，也不静默截断。MCP 工具加入 Go 统一工具表，不由 Bridge 增加另一套 Agent 可见范围。

复用当前 Go Register/Restart 的初始连接等待和取消清理，不新增后台准入队列。register 返回 Registration 表示 Go 已接受资源并完成本次初始处理，不保证 connected；Go 允许保留的等待凭证/授权等状态，通过 list/inspect 及状态通知读取。Go Register 返回错误时，Bridge reject 并清理未成功的登记，不改成“返回成功但偷偷保留失败连接”。之后已登记服务的状态变化由 Go 维护；restart 返回实际状态或明确错误，错误后的状态仍可 inspect。协议 OAuth 由 Go 处理，普通插件不必重复实现它。

不额外提供 watch 与事件两种并行订阅入口；状态通知使用统一 ctx.on 机制。用户仍通过插件接入 MCP，不新增面向用户的独立 MCP 配置界面。

### 6.7 审批与问题服务的通信入口

| 方法 | Promise 结果 | 说明 |
| --- | --- | --- |
| `ctx.host.approvals.request(call, {reason, detail?})` | ApprovalOutcome | 仅活动工具 body 内复用 Go 单次审批 |
| `ctx.host.questions.ask(call, request)` | QuestionAnswer | 等待回答或取消；复用 Go 非限时问题服务 |

审批结果为 allowed/rejected/cancelled/unavailable，Bridge 映射 Go 既有 outcome；只有 allowed 可继续本次操作。审批不是通用有限超时 RPC。当前权限策略和答复通道仍由 Go 判断，插件不能自己答复。

插件 questions.ask 只提供等待真实回答的 Promise，不在本轮暴露 timed/pending 变体；call.signal 或工具生命周期取消时结束。它等待业务结果，但不阻塞 Node 事件循环。现有内置 ask_user_question 的限时与迟到回答机制保持原样。

原因来自当前代码：`harness/interaction/projection.go` 的限时恢复依赖 ask_user_question 的工具名、请求 schema 和 tool/call/result；任意插件工具内部发问不能直接假定已具备相同恢复事实。此处选择不复制它的特殊时序合同，也不为插件询问新增一套持久事件。Plan 审查仍为 Go 内置非限时流程，不暴露给插件作为权限提升。

此处仅定义进程通信入口，不重新设计问题 UI、SSE 或前端审批入口。

### 6.8 图片与文件：本轮不开放 Assets 服务

图片附件、文件读取、生成图片、显式 present 及下载是已有 Go 内置能力，保留原实现；它们不因存在第三方插件就需要各增加一个公开 RPC 接口。

本轮不提供 ctx.host.assets，不提供图片读写 spool、插件 ImageRef/PresentedFileRef 或插件 file/image 结果块。此前为这些接口新增的 Bridge、SDK 类型及专属测试应清理，不作为待补齐功能。第 8 节中的图片输出治理和 present 最终成功确认仍属于内置工具执行流程，不能连带删除。

插件可以用自身代码生成工作区文件，在文本中说明路径；需要交付时仍由 Agent 调用既有内置 present。文本路径、Markdown 链接和 data 不会自动转换为交付事实。此处不允许插件任意调用 Go 工具，也不引入自动产物扫描。

### 6.9 当前调用的范围

call 是数据与活动调用凭据，核心状态仍在 Go；不包含 `.agent.ctx`、`.session`、writer、model client 或 Redis。凭据只对当前 body 有效。sessionId/cwd 可以作为业务说明，不能当作权限凭据传回创建有效调用。

本轮不提供 `concludeTurn`、`deferContext` 等额外控制方法；插件结果正常返回即可，普通附加说明放 content，长期说明用 prompts/skills。将来确有控制执行流的业务再单独设计，不因旧 SDK 曾经存在就保留。

### 6.10 明确不开放的服务

不提供 ctx.host.agents/sessions/jobs/schedule/memory/models/runtime/redis/shell 等完整管理服务，不开放日志提交、任意工具调用、全局 post-result 改写、核心提示覆盖、用户答复者和插件客户端代码。

这些能力的已有内置工具和 Go 模块保持不变。插件工具可以被普通会话、子 Agent 或 Schedule 投递启动的会话调用；第三方不必拥有这些模块的执行权才能提供外部应用能力。

### 6.11 公共数据类型与状态含义

| 类型 | 应提供的字段/语义 |
| --- | --- |
| HostInfo | userId、hostId、dataRoot、apiVersion；属于只读说明，不是控制凭据 |
| PreparationContext | sessionId、cwd、workspace、signal，以及可选 model:{provider,name} 的非敏感描述；不包含 API Key、请求头或可调用模型对象 |
| SkillSummary | name、description、来源/provider、可选 path/resourceBase/whenToUse、modelInvocable/userInvocable |
| SkillDefinition | SkillSummary 加正文 content；资源信息由 Go 规范化 |
| CredentialRecord | api-key 的 key/env，或 grant 的 JSON payload；数据内容由所属业务解释 |
| AuthorizationSummary | key、title、methods、phase；不返回 token，phase 不等于工具执行权限 |
| McpServiceView | 本实例逻辑 name、实际状态、工具数量、可选授权尝试和公开错误；不用布尔 connected 混淆等待凭证/授权/连接失败 |
| QuestionRequest | Go 现有问题字段：questions 中各项 id、question、header、options、multiSelect；不允许插件将 intent=approval/plan-review 当作授权 |
| QuestionAnswer | 逐项 id、selected、自定义文本，使用当前 Go Answer 的结构；不存在插件替用户填写答案的方法 |

所有 DTO 是序列化快照，修改 JS 对象不会改变 Go 核心状态。需要改变已公开能力时必须调用相应方法；不存在自动同步“属性赋值”的代理机制。字段的 JSON 编码和最大长度需在正式 SDK/Go decoder 中共同定义并测试，禁止直接导出整个 Go 内部 struct。

## 7. 原生 `ctx.on` 与 Host 事件派发

### 7.0 与上游 Cordis 事件语义的区别

上游 `ctx.on` 本身不是“只读通知”接口；它负责注册监听函数。事件是否等待监听器、是否采用返回值以及是否允许影响执行，由事件定义和触发方使用的派发方式决定。Cordis 原生提供 `emit`、`parallel`、`serial`、`bail` 和 `waterfall`。例如，上游 `tools/pre-execute`、`tools/execute`、`tools/post-execute` 是使用 `waterfall` 的执行扩展点；`tools/result`、`tools/change` 则是 `emit` 通知。依据：上游固定版本 `ca33fd6843b205e279c7ccb6cb73a472c78f5760` 的 `packages/core/tools/src/events.ts`、`pipeline.ts` 和 `vendor/cordis/src/events.ts`。

本项目不复制上游工具 waterfall。上游工具系统是事件的派发者，不是靠监听事件驱动自身；next 是它选择的中间件组合方式，与核心服务是否插件化并无必然关系。我们的 Go 工具系统直接调用公共处理和具体工具函数。

公开 host/* 事件只用于观测，返回值不修改工具执行决定。插件仍可在通知内调用公开 ctx.host 方法产生独立操作，不能据此接管当前 Go 调用。Cordis 自身与插件之间的本地事件语义保持原生，并不被统一改成只读。
### 7.1 注册与派发

```ts
ctx.on('host/skills-changed', event => {
  // 观察来源变化；需要最新内容时查询 Go Skills 服务。
  ctx.logger.info('skills changed', event.instanceId)
})
```

ctx.on/once 使用 Cordis 原生本地注册和 disposer，不发注册 RPC、不覆盖 events.register、不伪造同步 Go 调用。Bridge 接收 host.event 后使用真实 Cordis 监听表向当前 Host 的活动监听器派发；不广播 token、密钥、任意工具参数或全部会话正文。

host/* 是同一账号 Host 内的公共观测事件；涉及插件资源的 payload 带 instanceId，监听方可以筛选自身或观察其他业务扩展的公开状态。方法调用中的 owner 限定不等于事件总线天然私有；这里不伪造 Cordis 的“每个 child Context 自动拥有独立事件总线”语义。

Go 每个 Host 的状态通知按输出队列顺序发送；涉及资源的通知带 instanceId 及对应 key/name 等标识。Node 按接收顺序开始分发，不承诺异步监听器完成顺序。原生监听参数和 prepend 可保留；通知的业务返回值一律忽略。

### 7.2 开放的 Host 事件

| 事件 | payload | 何时发送 |
| --- | --- | --- |
| `host/skills-changed` | instanceId、来源变更标识 | Go 接受贡献变更/失效后 |
| `host/credential-changed` | instanceId、key、configured | 凭证已提交变更，不含值 |
| `host/authorization-changed` | instanceId、key、phase、可公开错误 | 授权状态变化 |
| `host/mcp-changed` | instanceId、name、state、可公开错误、toolCount | MCP 状态变化 |
| `host/stopping` | reason | Go 正常关闭前的尽力通知；不保证崩溃时存在 |

通知中需要时间戳时使用 Unix 毫秒，不改变 Go 模块已有事实的原始时间编码。状态通知不提供可执行 invocation，不能借通知发起当前工具调用的提问或审批。不为工具执行另建开始/结束通知、耗时统计或配对事件合同。

本轮不把 agent/session 所有内部事件逐条输出到插件层。上表是完整的 Host 公开通知白名单，不等于新增一个前端消息流或持久事件系统。

### 7.3 返回值、异常与慢监听器

Host 事件由 Bridge 使用当前锁定 Cordis 的 events.dispatch 取得真实活动监听器快照，再逐个隔离同步异常和异步 reject；不因裸 emit 的某个抛错中断其他监听，也不接管事件注册/卸载。Go 不 await 这些函数，因此它们不能批准、拒绝、替换结果或阻塞会话事实提交。

事件队列有界；慢插件造成积压时，状态类通知按 key 合并并记录必要诊断，不逐 delta 无界堆积。不做自动历史重放；插件重连后需要实时状态就调用对应查询。通知不用于任务保证投递或触发必须执行一次的业务。

### 7.4 审批与结果处理不依赖通知

Go 内置执行前策略直接返回决定；需要审批时 Go 调用自己的 ApprovalService 等待结果。内置工具和插件工具也可在实际操作前主动申请同一服务。上述请求有业务返回值，和 ctx.on 通知是不同通信入口。

Go 直接做输出限制、交付校验、结果规范化和日志提交。不将它们包装成 Node post-execute 监听器，也不向插件提供替换其他工具执行器或结果的通用入口。

## 8. 实际调用流程

### 8.1 四个内部处理点：PreExecute → Execute → PostExecute → Result

本节为统一调用流程的实施规范。**Go 保留三个执行阶段和一个最终结果处理环节，对应上游 tools/pre-execute、tools/execute、tools/post-execute、tools/result；所有内置、插件、MCP 工具共用，不要求每个工具实现四组回调，也不向 Cordis 暴露四个阶段事件。** 上游把核心模块装配成插件并在阶段事件中调用它们，我们在相应 Go 阶段直接调用内置模块。插件工具只在执行本体时经过业务 RPC，不经 Node 的阶段派发或 continuation。

| 上游对应 | 我们的 Go 内部阶段 | 处理目标 | 阶段返回 |
| --- | --- | --- | --- |
| prepare、tools/pre-execute、审批解析 | PreExecute（可复用 Prepare） | 完成本次调用准备及执行前策略判断，得到执行或拒绝/取消结果 | Go PreToolDecision；解析审批并形成 ToolPreparation |
| tools/execute、dispatchToolBody、核心执行保障 | Execute（可复用 Dispatch） | 在可靠执行边界内调用既定工具，取得成功/失败结果 | Go ToolPipelineResult，外加原生 NeedsPost 流转标记 |
| tools/post-execute、finalize/materialize | PostExecute（可复用 Complete 的结果处理和 finish 的最终校验） | 应用核心结果处理，确定不可再被观察者修改的最终结果 | 最终 Go ToolPipelineResult |
| tools/result 及所采用的核心结果监听逻辑 | Result（由 Go 直接调用最终处理与提交） | 根据最终成功/失败处理核心收尾，按现有顺序提交事实、完成既有前端投影 | 不返回处理决定；核心提交错误返回调用者，不当作观察者错误忽略 |

顺序固定为 PreExecute → Execute → PostExecute（末尾完成最终校验/物化）→ Result，最终校验不是第五个阶段。上游 post 在 result 之前；post 可以决定如何处理结果，result 的监听器只接收最终冻结结果，不再返回 accept/block 或替换内容。Go 的 Result 单独承担对应核心模块的最终处理，不能笼统合并成 post 后任意一个通知。已有批调度器继续负责有序写入，不要求搬走 Session writer 或新增执行状态机。

#### 8.1.1 PreExecute：上游在执行前具体做什么、Go 如何对应

| 上游模块/位置 | 在 pre-execute 附近的实际逻辑 | Go 对应处理 |
| --- | --- | --- |
| core/tools 的 prepare | 固定调用身份及参数快照，检查取消，保留本次调用使用的定义和上下文 | Go 准备当前调用，保留 callId、sessionId、turn/step、工具定义和参数；不允许执行中重写已记录的名称/参数或切换到同名新工具 |
| jobs/tool-jobs 的 pre 监听 | 捕获本次调用使用的可见输出上限，供后续 Job 输出终结使用 | Go Job/输出模块在 PreExecute 捕获所需输出限制，PostExecute 使用同一份规则；不依赖 Node WeakMap 保存此状态 |
| deliverables/workspace-changes 的 pre 监听 | 找到该会话的变更记录器，等待已有变更处理稳定，保存与本次调用的关联 | 这是上游文件变更追踪能力；当前项目已选择显式 present，不在本次恢复自动扫描/推测产物。若已有 Go 文件观察状态需要同步，由对应内置模块在此处理；不能为复制监听器额外新增 artifact-probe |
| core/tools 的审批解析与取消检查 | ask 转交统一审批服务；获准后重新检查取消 | Go ApprovalService 等待真实答复，未获允许不执行；获准后检查取消并进入 Execute |

PreExecute 不执行被调用工具的业务副作用，不因阶段划分额外创建业务资源或自动重试工具。所需核心处理由 Go 已有模块按适用条件运行；上游 experimental 的风险审查、外部 hooks、协作授权等不进入本次阶段处理清单。

输入 JSON/schema 校验必须在实际工具副作用前完成。当前 Go 的 schema/Tool.Validate 位于 executeNativeToolDirect 入口；本方案可保留这个位置，对应上游工具调用入口的校验，而不是为了给阶段凑功能强行迁入 PreExecute。PreExecute 的参数快照、策略判断与 Execute 入口的参数合法性检查必须区分。

**上游固定返回格式及 Go 采用方式：**

| PreToolDecision 形状 | 含义 | Go 下一步 |
| --- | --- | --- |
| `{kind:'allow'}` | 策略允许继续 | 检查取消后进入 Execute |
| `{kind:'deny', reason, info?}` | 策略拒绝，带原因及可选错误身份 | 生成拒绝结果；不调用工具 |
| `{kind:'cancel'}` | 取消此次执行 | 生成取消结果；不把取消伪装成业务拒绝 |
| `{kind:'ask', reason?, displayReason?}` | 请求用户决定 | 调 Go 审批服务；同意后才继续，其余结果转拒绝/取消 |

复用当前 Go PreToolDecision；这是 Go 内部策略结果，不是向插件暴露的新 JSON-RPC 协议。策略返回非法 kind 或执行异常时失败关闭，不能默认允许；没有配置的通用策略默认为 allow，真实权限与工具自身的限制仍照常生效。

#### 8.1.2 Execute：上游执行期间的核心保障与真实工具调用

| 上游模块/位置 | 实际逻辑 | Go 对应处理 |
| --- | --- | --- |
| session/session-checkpoint-policy 的 tools/execute 监听 | 顶层工具副作用前 flush 会话；失败则不调本体；嵌套调用复用外层持久边界 | 将“调用事实已可靠落盘”作为 Go Execute 的副作用前置条件。当前 appendNativeToolIntent 已提前记录并 Flush，可满足该边界；不为阶段命名重复落盘，也不移到 Node |
| guard/timeout-policy 的 tools/execute 监听 | 依据 timeoutMs 派生取消信号，等待操作收尾，再把自身到期归类成 TOOL_TIMEOUT | Go 在 Execute 内管理 deadline/cause，传递给实际 Go 操作、插件 RPC 或 MCP；取消后等待在途操作收尾，不只返回一个超时而放任后台副作用继续 |
| core/tools 的 dispatchToolBody | 保留调用身份、合并原始取消信号，解析既定工具并调用其 execute；把返回或异常变成规范结果 | Go 只调用本次固定的 Tool；统一入口先做 schema/Tool.Validate，随后进入业务函数；取消、错误和上下文保持 Go 所有 |
| 工具自己的 execute | 执行业务，必要时主动申请审批后才产生副作用；可返回结构化业务值 | 内置工具运行 Go 函数；插件工具 RPC 调 Node 回调；MCP 工具由 Go Client 调服务端。工具业务内部审批仍使用同一个 Go ApprovalService |

Execute 的阶段入口与工具业务 execute 不是同一个概念：前者属于 Go 执行器，包含上述执行边界保障；后者是阶段内被调用的一项业务实现。bodyStarted 应反映是否已调用业务函数/外部适配器，不能仅凭进入阶段就认定已执行。远端操作是否真的产生副作用可能无法确认，断连时保留未知结果语义，禁止自动重发。

并发调度继续由 Go 批调度器管理：按调用顺序准备、只有明确可并行的工具允许重叠 Execute、按原调用顺序完成和提交。不是每个工具另造一个队列，也不是让 Node 管整个批次。

**上游与 Go 的固定结果形状：**上游的工具业务函数返回其声明的 canonical value，工具系统验证/渲染后才得到 ToolExecutionResult；并非所有工具 execute 自行构造整个系统结果。其规范结果分为：

| 结果 | 必要内容 | 可选内容/约束 |
| --- | --- | --- |
| 成功 | `isError:false`、JSON `value`、有序 `content` | meta、additionalContexts、concludesTurn；没有 error |
| 失败 | `isError:true`、`error:{message, info?}`、有序 `content` | meta、additionalContexts；没有成功 value，不得声明成功结束 turn |

Go 复用现有 ToolPipelineResult/codec 作为内部规范结果，不再建立一套 Node 阶段结果协议。公共插件 SDK 的 ToolResult 仅为 text content + 可选 data，异常通过 throw 返回；Go 适配器验证并转换为上述内部成功/失败格式，不接收插件生成的会话 Meta、图片引用或交付声明。Go 内置工具仍按既有内部合同处理 image/file 等内容，不受插件 SDK 的结果子集限制。

执行前 ask 已在 PreExecute 解决后才启动工具执行预算。工具内部发起的审批不新增独立的 120 秒超时，但仍受本次调用取消和工具已声明预算的约束；不将工具 deadline 和普通审批服务的超时混为同一概念。

#### 8.1.3 PostExecute：结果变换、输出治理与最终物化

| 上游模块/位置 | 在 post-execute 及其终结处理中的实际逻辑 | Go 对应处理 |
| --- | --- | --- |
| guard/repeat-tool-reminder | 统计同工具、同参数的连续调用，达到阈值附加模型上下文提醒；不阻止调用、不改写业务结果 | Go 重复调用状态在 PostExecute 收尾更新，提醒按原调用顺序放进下一次模型上下文；当前 repeatGuard/commitNativeToolResult 已有对应路径，应保留重置条件和适用分支 |
| fs/tool-fs-search 的 grep/glob | 对过长搜索结果保留受限预览，必要时保存完整结果并返回恢复引用 | 对应 Go 搜索/输出模块负责相同的结果保留行为，由 PostExecute 统一接入；不能用 Node post 监听器补，也不能只截断就宣称已还原可恢复输出 |
| spill/spill-policy | 按输出预算限制文本/图片展示，将完整文本和图片访问引用写入 spill，产生可读取的恢复提示；失败按其规定保留可用内容 | Go 输出治理模块在 PostExecute 承担。当前只有部分工具的截断/spill 路径，通用预算与恢复引用需逐项核对补齐，不宣称已经全量等价 |
| jobs/tool-jobs 的输出终结逻辑 | 使用 pre 捕获的上限保留 Job 状态后缀、限制可见输出 | Go Job 输出处理器使用 PreExecute 捕获的同一限制，在 PostExecute 应用；不丢 Job 状态或重复附加后缀 |
| core/tools 的 project/finalize/materialize | 将工具值变成模型内容、执行适用的内置变换、验证并物化最终 JSON，区分成功与错误 | Go 保留必要的转换和最终校验；不公开插件 render/project/finalize 全局回调，不因删除远端 post hook 而删除 Go 终结逻辑 |

这些是同一 PostExecute 的适用处理器，不是要求每个结果被每个处理器重复截断。搜索/Job 的特有输出处理和通用 spill 必须共享本次预算及完整输出来源，明确处理顺序，保留恢复引用和 Job 状态；已经受限的预览不能冒充完整原文再次保存。重复调用提醒在按原调用顺序收尾时只计算一次，所产生的附加上下文随本次最终结果交给 Result 提交。

上游 PostToolDecision 有固定格式：

| PostToolDecision 形状 | 行为与限制 |
| --- | --- |
| `{kind:'accept'}` | 保留当前结果 |
| `{kind:'accept', content, additionalContexts?}` | 替换模型可见内容，可附加合法上下文 |
| `{kind:'accept', value, additionalContexts?}` | 替换成功业务值，再经过该工具的输出校验和渲染；不能用于把失败结果改成成功 |
| `{kind:'block', feedback, info?, additionalContexts?}` | 以反馈生成失败结果；不能撤销已经发生的工具副作用 |

accept 也可仅附加 additionalContexts；value 与 content 不能同时替换。Go 可复用现有 PostToolDecision 作为内置结果处理器的返回，统一应用后由 PostExecute 返回最终 ToolPipelineResult。这个决定只供 Go 模块直接调用，不暴露通用 post 注册接口或 RPC。没有相应已验证输出 renderer 的工具，不凭空允许 value 替换；插件 content/data 走自己的已校验适配合同。

最终校验要求包括错误身份、合法 JSON 和内容块、合法的附加上下文；对 Go 内置图片/交付结果还包括不可变图片引用、工作区交付路径。Go 内部输出规则可调整可见内容；不得据此静默重跑工具或将失败交付标为成功。

#### 8.1.4 Result：最终结果确认、核心模块收尾与提交

上游 tools/result 是 emit 型最终结果通知：它在普通 post 处理、最终内容转换和物化之后发生，监听器不能通过返回值再修改结果。上游所采用核心模块在此做的事情，在我们这里仍必须由 Go Result 环节直接完成；对外观测通知不能代替这些核心步骤。

| 上游模块/位置 | 根据最终结果做什么 | Go Result 对应处理 |
| --- | --- | --- |
| deliverables/tool-present 的 tools/result | 仅最终成功时确认交付事实，失败则丢弃暂存交付 | 内置 present 根据已校验文件和最终结果形成交付事实，经既有 writer 提交；不在业务 execute 刚返回时提前发布，不新增插件交付入口 |
| subagent/structured 的 tools/result | 只有最终成功的结构化输出才成为正式捕获结果，失败不确认 | Go 对暂存的结构化输出做最终确认/撤销；保留该工具自身防止重复提交的业务检查，不新增 PreExecute guard 或禁止其他工具的公共限制 |
| context/agent-instructions 的 tools/result | 归集最终成功调用触及的文件路径，为后续指令上下文更新状态 | Go 指令观察模块根据最终结果处理触及路径；取消/失败不确认成功触及，必要的嵌套归并仍归 Go |
| 工具系统及会话提交方 | 最终结果供调用者、日志和 UI 使用 | Go 批调度/会话路径按既有顺序写 tool/result、交付事实及适用上下文，并在规定边界 Flush |

上游重复调用提醒的计算位于 post-execute，我们也归入有序 PostExecute；当前 commit 收尾处若仍包含计算，应抽到该职责位置，不能在 Result 再计算一次。Result 只提交已经形成的提醒及上下文，保留原有适用分支。

**Result 的固定输入/输出：**输入是最终的只读 ToolPipelineResult；正常完成不返回业务决定，Go 核心收尾失败可返回 error。上游 emit 监听器的业务返回值被忽略，我们同样不接受 Result 返回 accept/block 或内容替换。此处不再接受 PostToolDecision，不重新渲染/改写结果，不重跑工具。内存核心状态的确认应与持久化边界协调，不能在提交失败时继续按已成功保存推进；所需现有 writer/状态模块由 Go 直接调用。

**提交边界：**先根据最终结果准备本次适用的交付/结构化确认/指令更新，再由现有有序会话提交路径写入所需事实并在规定边界 Flush；依赖这些事实成功的内存确认及正常推进必须在该边界成功之后。可由已有事实恢复的状态继续由事实恢复，不增加另一份结果日志或跨模块事务。失败不发布成功交付，不重跑已产生副作用的 body；本轮不承诺多条 JSONL 事实具备数据库事务式的原子提交。

核心提交错误和外部观察者错误必须区分：tool/result、交付事实或所需持久状态写入失败要返回错误并停止正常推进；插件观测回调失败只做异常隔离，不能使已提交事实失效。

Result 不额外向插件发送工具开始/完成通知，也不靠 Node 的工具结果监听器执行 present、结构化确认或其他核心提交。既有会话日志及面向前端的 HTTP/SSE 工具消息继续使用原实现；删除自定义插件通知不删除这些能力。

#### 8.1.5 失败、拒绝、取消的固定流转

| 情况 | Execute 本体 | PostExecute 处理方式 | Result / 会话投影 |
| --- | --- | --- | --- |
| 正常返回 | 一次 | 完整结果处理与最终校验 | 按序提交，沿既有会话链路向前端投影 |
| 工具抛错、业务失败、工具超时 | 已调用；取消时先收尾 | 规范错误、执行适用的失败处理，不执行成功交付 | 提交失败结果，保留错误身份 |
| Pre 决定拒绝、审批未获允许 | 不调用 | 对拒绝/取消结果执行适用的结果处理；不可恢复成放行 | 可提交拒绝/取消结果，保留工具本体未被调用的内部事实 |
| Pre 内部异常、非法决定、进入执行前已取消 | 不调用 | 直接最终物化；跳过会造成业务变换的普通处理 | 按已有事实边界提交明确错误/取消，不假称工具已运行 |
| 调用预算拦截 | 不调用 | 使用内置终结路径，不增加一次“重复调用”统计 | 按既有批次规则提交预算结果 |
| checkpoint 失败 | 不调用 | 返回持久化失败；不伪造可可靠提交的业务结果 | 停止正常推进，不向调用方或前端伪称成功 |
| 插件/服务断连 | 根据实际是否已派发判断 | 未派发则不可用；已派发且无法确认则保留未知结果 | 不自动重试副作用，不伪称确定没执行 |

沿用当前 NeedsPost 对 post-result/final-result 的区分：不是所有异常都执行普通 post 变换，但都需要最终物化/资源清理；由工具系统接纳并形成最终结果的调用，仍进入适用的 Result 收尾。调用预算等在 AgentLoop 提前合成的结果保留原有分支，不能新增不适用的重复统计或成功确认。四个处理点不表示每次调用都必定执行业务 body 或所有成功处理器。

#### 8.1.6 当前 Go 代码对应与必须核实的缺口

| 当前代码 | 对应阶段/职责 | 实施要求 |
| --- | --- | --- |
| native_tool_source.go 的 refreshTools/installTools | step 工具集合 | 合并内置、插件和 MCP 工具；同一步保留实际对象，不能按名称重新绑定另一实现 |
| native_tool_pipeline.go、ToolInvocation.Prepare | PreExecute | 保留调用准备、Go 执行前策略、审批与取消检查；移除 Node pre waterfall。独立 guard 相关代码按第 12 节清理，不属于目标执行流程 |
| ToolInvocation.Dispatch、executeNativeToolDirect | Execute | 保留 Go 超时/取消/业务调用；删 Node around/next。准确区分入口校验与本体开始 |
| native_tool_scheduler.go 的 appendNativeToolIntent | Execute 的持久化前置边界 | 验证每个真实工具在副作用前均已有可靠调用事实；已有 Flush 不无意义重复 |
| ToolInvocation.Complete/finish、tool_pipeline_codec.go | PostExecute | 保留规范化与最终物化；把必要输出处理接成 Go 内置处理器，不留远端阶段控制 |
| result_limits.go、Shell/Web/Search/Job 现有输出处理 | PostExecute 对应能力 | 核实各入口覆盖、完整结果恢复、状态后缀和图片引用；当前分散实现不等于通用 spill 已完成 |
| repeatGuard、commitNativeToolResult | PostExecute 收尾 | 保留顺序、阈值、用户新消息重置及跳过分支；避免重复更新 |
| NativeStructuredOutput、present、instruction 相关模块 | Result 最终确认 | 核实最终成功才确认及必要状态更新；结构化工具自身防重复提交保持在工具业务内，不在 Pre 增设公共限制 |
| logNativeToolOutcome/commitNativeToolResult | Result 提交收尾 | 工具结果、内置 present 交付、上下文沿既有 writer 提交；不为插件结果新增交付 Meta |

这是基于当前源码的实施核对清单，不是已经完成/测试通过的声明。额外业务能力仍按第 2 节范围；不因为列出上游可选插件就自动引入。

### 8.2 Skill 与提示查询

Go 组装 step → 读取本地与预装 Skill → 必要时 RPC 调用插件 Provider → Go 合并目录/优先级 → 模型按需读取正文。动态提示同样只贡献当前附加文本，最终记忆/上下文和工具声明仍由 Go 生成。

静态目录无需每轮经过 Node 重新扫描；由 Go 发现器读取包内文件，按现有缓存/失效机制处理。

### 8.3 MCP 插件

插件 apply → await mcp.register → Go 接受并进行初始连接/发现处理 → 根据实际状态注册可用工具 → 模型调用某个 MCP 工具 → Go 的工具管线和 MCP Client 执行。不再先回 Node 再由 Node 调同一个 MCP 服务；缺少授权等状态通过管理入口处理。

MCP status 变化通过 host.event 给对应实例；协议 OAuth 调用 Go 内置授权/凭证服务，插件无需持有 Go 连接对象。

### 8.4 授权流程回调

自有管理请求到 Go → Go 准入授权 → RPC 调用 flow.run → Node 实现供应商协议 → session.prompt/notify/commit 请求回 Go → Go 完成对应操作 → run 返回 → Go 确认提交并结束。

调用方正在 await flow.run 时，Go 仍能处理该 flow 发回的请求。不得将同一账号/插件整个 RPC handler 串行锁住，否则会在此处死锁。

## 9. 包、配置与存储

### 9.1 包形态

一个插件包是普通 npm JS 模块，可以包含多项工具、Skill、提示、MCP 声明和业务 Cordis 服务。TypeScript 编译后发布 JS；Go/Python 业务可通过 Node 入口调用本地程序或远程服务。

package.json 增加本项目 metadata：`goBot.api: '1'`，与 `@go-bot/plugin-sdk` 主版本匹配。可选 title/description 和 configSchema 供自有管理表单读取；入口通过 exports/main 解析。无需 dsh manifest、cordis.patch.yml 或上游包身份 pinning。

```json
{
  "name": "@example/go-bot-mail",
  "version": "0.1.0",
  "type": "module",
  "exports": "./dist/index.js",
  "peerDependencies": { "@go-bot/plugin-sdk": "^1.0.0" },
  "goBot": { "api": "1", "title": "Mail" }
}
```

SDK 提供运行所需的唯一 Cordis 类型/帮助导出，业务包不再依赖 dsh-*。普通 npm registry、私有 registry、固定 Git 提交、tarball 和 Host 内开发目录仍可作为安装源；安装器固定版本和锁文件，不要求都发布到 npm 官方 registry。

SDK 仍为 private 包，由 Host 镜像一并提供完整 package.json、类型、运行时导出及许可证。安装器将 profile 中的 SDK 根依赖和同名依赖解析固定到这份本地包，业务插件可声明 SDK peer dependency；不依赖不存在的公共 npm 发布。开发阶段使用仓库内 SDK，TypeScript 的 import type 编译后移除，HostError 等运行时导出也可从 Host 提供的包解析。SDK 不列入用户插件列表，也不能作为业务包卸载。

### 9.2 配置模型

保留简单的 packages/instances，不向用户暴露 Cordis 的 intercept/isolate/patch 组合图：

```json
{
  "packages": {
    "@example/go-bot-mail": {
      "source": { "type": "registry", "spec": "@example/go-bot-mail@0.1.0" }
    }
  },
  "instances": [
    {
      "id": "work-mail",
      "package": "@example/go-bot-mail",
      "enabled": true,
      "config": { "mailbox": "work", "apiKeyRef": "MAIL_API_KEY" }
    }
  ]
}
```

config 按包 schema 校验；凭证值走 Go credentials，Redis 配置只放引用和非敏感业务参数。新方案不保留 Settings.Credentials 明文 map、Bundles/Patch/ApprovedBuilds/IndexSources 等旧公开配置。安装脚本默认禁用；确需安装构建的包按部署者明确的包安装策略处理，不新建复杂的审批后台。

普通用户的插件发现和安装以第 9.4 节的项目维护清单为准。输入包名、GitHub 地址、tarball 或 Host 目录不再是普通用户的安装流程；底层来源解析器只供项目分发及受控开发测试使用。UI 不保留上游组合/补丁管理概念。

### 9.3 数据归属

| 数据 | 归属 |
| --- | --- |
| packages/instances 期望配置 | Controller Redis user hash 的 plugins 字段 |
| 包版本和安装结果 | Go 包管理器；请求返回给 Controller，自有 UI 展示 |
| 实例加载/应用结果 | Cordis Loader 原生状态；Go 经 runtime.inspect/apply 读取并返回 UI，不另建实例状态机 |
| 包文件、包锁和必要配置投影 | `$DSH_HOME/plugins`；同一 Host 数据根由 volume/NAS 承载 |
| 插件凭证和授权记录 | `$DSH_HOME/.credentials.yaml`，Go 唯一 Store |
| 图片、用户上传文件 | 既有 `$DSH_HOME/attachments/v1`，不另建插件附件库 |
| 插件业务生成的工作区文件 | 当前调用实际 workspace；需要交付时由 Agent 调用内置 present，之后沿既有路径下载 |
| callback handles、invocation、pending、通知队列 | 仅内存，不进 Redis |
| RPC socket | 容器临时目录，不进 NAS，退出清理；本轮不创建资产 spool |

Host 不访问 Redis；Controller 调用 Host 应用配置并等待结果。保存期望配置成功不等于插件已经生效，API 返回二者的实际结果；失败明确标记，不靠五分钟后台循环偷偷重试。

移除插件默认保留用户凭证和工作区文件，便于明确重装；真正删除凭证使用独立 credentials 操作。不提供通用插件 KV/文件 storage，业务包若需要自己的业务数据可以使用自身代码和外部服务。

### 9.4 项目维护的插件市场（已实施并完成定向验收）

**市场展示本项目提供和审核通过的可用插件。** 普通用户从列表选择，不搜索 npm/GitHub，也不填写下载源。底层仍使用既定包管理器与 Cordis，不重新设计插件执行和生命周期。

- 在 `extensions/catalog/plugins.json` 维护可用清单，随 Controller 构建内置。新增或更新条目通过项目 PR 审核；本轮不新增远端清单地址配置、用户自定义市场源或自动后台更新任务。
- 条目提供稳定 pluginId、原始名称、中英文 description、版本、图标路径、维护者、许可证，以及内部使用的包名和安装源。展示字段由包内 plugin.json 提供，不额外增加 capabilities/setup/connection。icon 指定 Controller 提供的静态图片路径，资源随插件包保存；两端按 URL 显示，不维护图标枚举、品牌映射或插件客户端代码。
- 包可来自已支持的 npm registry、固定 Git 提交或分发包；这些是维护者的分发决定，不作为普通用户的安装输入。发布版本必须可重复定位，不能把 latest/浮动分支存成已安装版本。
- 正式业务插件才进入市场。内置浏览器、Office、模型、搜索、记忆、AgentLoop 等不改装为市场插件；`examples/plugins` 的开发示例不冒充可用产品。
- 清单只是可选项；Redis 的 `user.plugins` 仍只保存该用户实际安装的 packages 和实例配置。package 增加 catalogId 以关联条目，并保留当次安装的固定 source 快照，确保重启不会被市场的新版本静默替换。凭证不进入清单或 Redis config。

普通流程：**浏览市场 → 查看能力及连接要求 → 安装 → 配置实例 → 启用 → 按该插件提供的方式连接账号。** 安装包不等于已配置或已授权；插件初始化必须能先注册授权入口，不能因为尚无账号凭证而让用户永远无法发起授权。工具业务在凭证缺失时明确报错，不擅自访问第三方。

Web 与移动端均提供“插件市场／已安装”。详情展示能力、版本、配置、连接与授权、启停和卸载；默认实例名由框架给出，普通使用不要求手填技术标识。多个实例能力保留在详情的扩展操作中。配置基于包 metadata 的 schema 和必要说明；不加载任意插件前端脚本，也不把 JSON、来源类型或私有 registry 认证作为市场主界面。

市场入口不放在设置中：Web 侧边栏提供独立的“插件市场”入口，页面为 `/plugins`；手机会话页右上角提供组合菜单，包含“文件系统”和“插件市场”。从这些入口打开默认显示市场页签，已安装管理仍在同一页面。手机“我的”的设置列表不增加插件市场。

公开安装/更新 API 接收 pluginId，由 Controller 查清单并解析来源；服务端也要限制，不能仅删除前端输入框而继续允许提交任意 source。普通实例修改只接收 config/enabled 等业务设置。Host 私有安装与完整 settings 应用接口仍供 Controller 和受控测试使用。更新必须由用户明确操作，不自动更换在用版本。清单暂不可用或某包下架时，已有固定安装记录不被静默删除；界面可展示现有包信息、停用或卸载。

### 9.5 插件的连接方式与认证交互（已实施并完成定向验收）

前端只展示该插件当前实际注册的授权方式：单方式直接进入，多方式供用户选择，无认证需求不显示授权入口。不预设每个插件都支持 API Key 或 OAuth；具体字段由选定流程运行时的 prompt 请求生成，授权输入不进入聊天或模型上下文。

**统一交互词汇和凭证基础设施，供应商协议由对应实现负责。** 不统一成一个固定 API Key 框，不要求用户知道背后是否为 MCP，也不因为接入 MCP 就一律显示 OAuth 按钮。

| 接入情况 | 协议与凭证处理者 | 插件详情里的用户入口 |
| --- | --- | --- |
| 不需认证的 MCP/API | 插件或 Go MCP 直接连接 | 配置或启用；不显示多余授权表单 |
| MCP 标准 OAuth | Go MCP OAuth，复用 Go authorization 与同一 credentials Store | 授权连接按钮、服务商授权链接、结果与重试 |
| MCP 要求 API Key/固定 token | 包装插件通过通用 flow 收集和验证；Go Store 保存；Host 私有 headers/env 或受控引用用于连接 | 该服务声明的密钥输入和保存入口 |
| 非 MCP 的 API Key、应用凭证、账号密码等 | 业务插件的 flow，复用 Go authorization/credentials | 插件定义字段；secret 掩码、text 输入、select 选择，支持分步或多个待输入项 |
| 非 MCP 的 OAuth/设备码等 | 业务插件使用供应商 SDK/OAuth 库实现协议；通用服务管理交互与提交 | 浏览器授权、复制设备码、等待、成功/失败及取消 |

MCP 官方规范将认证定义为可选能力；标准 HTTP 授权规范不代表每个 MCP 都支持 OAuth。stdio 通常从环境取得凭证。还应区分“访问 MCP 服务的凭证”和“MCP 背后第三方应用的凭证”，由包装插件说明真正需要连接的账号。[MCP 认证规范](https://modelcontextprotocol.io/specification/latest/basic/authorization)

#### 9.5.1 方法与页面由谁决定

- 业务插件通过 flow 的 methods 声明可选认证方式，label 直接说明“使用 API Key”“浏览器授权”等。只支持一种时直接显示对应入口；支持多种时让用户选择，不出现所有供应商共有的固定字段集合。
- 用户选择后启动已有 Go 授权服务。插件通过 notice/prompt 描述下一步，前端按 text/secret/select、url、code 等内容渲染。多个字段可以分步收集，也可以展示同时等待的多个 prompt；不因为要支持表单就先引入另一套认证服务。
- MCP 包装插件声明连接，由 Go MCP 模块承担标准 OAuth；前端根据该实例实际注册的 MCP 资源与其授权能力提供入口。MCP OAuth 的通用 flow 在启动时才临时注册，不能只读取 `authorization.list()` 就声称已找全所有可授权连接。
- 统一的插件详情可组合普通 flow 和 MCP 原有授权视图。适配层只映射展示/请求，不复制 MCP OAuth 状态机，不在普通 flow 外再启动一个包裹它的独立授权流程，也不复制一份 token。
- MCP 的技术连接状态、传输方式和工具数量不作为用户必须理解的配置。界面显示“未配置／授权进行中／已配置／失败”等实际状态；Cordis 已就绪、凭证已保存和第三方当前可访问是不同事实，不能互相冒充。

Web 使用与现有风格一致的授权面板，移动端使用底部面板；二者使用相同数据和动作语义。跳转系统浏览器后返回应用可刷新当前授权结果。收起界面与主动取消要明确；Host 保存活动授权状态，前端不持有协议运行状态或 token。尚未实际支持的交互应明确报错，不能根据“点击过授权”就展示成功。

#### 9.5.2 OAuth 细节、回调与存储边界

OAuth 的协议细节包括授权/令牌端点、应用 ID 与权限范围、回调、state/PKCE、授权码交换、令牌刷新及撤销/错误处理。MCP 标准 OAuth 复用 Go 内置实现。普通业务插件优先复用供应商 SDK 或 OAuth 库，而非每个插件手写；通用 authorization 本身不自动知道这些服务商规则。

Go 提供当前授权尝试的准入、取消、凭证提交和受控回调转交；供应商协议逻辑仍属于插件。MCP 继续使用原有 Controller→Host 回调链；普通业务 flow 使用第 6.5 节的 session.callback，Controller 的 `/v1/plugins/authorization/callback/{userId}/{token}` 只将当前尝试的回调转交 Host。二者共享同一 authorization/credentials，不互相包裹为新的授权流程。普通回调已接通 Go 与 Promise RPC，并有匹配 state、拒绝重放和不伪造授权完成的测试；Web 和 Android 已完成本地受控服务的浏览器授权验收，证据见第 14 节。不能因 notice.url 或单次收件成功就宣称任意服务商 OAuth 已验证。

所有凭证继续进入唯一 `$DSH_HOME/.credentials.yaml`。MCP 保留其 client/grant payload 与绑定规则；业务插件保留自身记录格式。统一 Store 不等于统一各服务商的 token JSON。MCP 的标准刷新由 MCP 模块处理；业务插件的刷新由供应商 SDK/业务代码处理，并复用 Go Store 的安全更新入口。前端只获得已配置标志和脱敏结果，不回显保存的密钥；Redis 保存非敏感配置及引用。保存凭证、获得授权与重新连接的结果要分别按实际情况报告，失败不靠后台定时循环静默重试。

协议依据：[OAuth 授权框架](https://www.rfc-editor.org/rfc/rfc6749)、[PKCE](https://www.rfc-editor.org/rfc/rfc7636)、[OAuth 安全实践](https://www.rfc-editor.org/rfc/rfc9700)、[设备授权](https://www.rfc-editor.org/rfc/rfc8628)。

## 10. 故障与一致性边界

- 无 Node/未启用插件：Go 原生 Agent、会话和工具照常工作，不为核心发布/提交等待兼容回调。
- 某工具回调报错：Go 按这次工具失败处理；不误判成模型协议错误，不自动重试副作用。
- 插件初始化失败：通过 Cordis 原生状态报告失败，原生 effect 撤销此前已接受的贡献；其他成功插件不回滚。注册确认前后可见性按第 5.2 节，不承诺 apply 级原子发布，也不回滚已发生的外部副作用。
- Node 崩溃：所有该进程 pending/callback 失败，插件工具不可用；Go 核心保留，已接纳运行的副作用结果可能未知。共享进程意味着所有业务插件均受影响。
- 重连：不复用旧 callback handle 或 invocation，不重放工具执行；明确重载实例以重建注册。Go 内置策略和限制不依赖 Node 连接状态。
- 并发配置变更：由现有管理请求串行准入；不增加每次操作持久化流水、后台重试和跨组件事务。

## 11. 目录与实现拆分

```text
internal/plugins/
  manager.go          # 期望配置应用、包和实例管理
  runtime.go          # Node 进程与连接生命周期
  rpc.go              # 通用双向 RPC
  bridge/             # 白名单 service handlers、owner/调用身份校验
  contributions/      # Go 工具、Skill、prompt 等注册适配

extensions/cordis/
  src/main.mjs        # Cordis 进程入口
  src/rpc.mjs         # 双向 Promise RPC
  src/host-plugin/    # 唯一 ctx.host 服务和本地 façade
  src/loader/         # 包加载、配置与实例生命周期

extensions/sdk/       # 自有 TS 类型、HostError、包开发帮助
examples/plugins/     # 邮件风格工具、Skill 目录、MCP、授权等最小示例
```

这是职责目标，不要求为了文件名完全一致创建大量空目录。Go credentials/authorization/MCP 继续留在已有模块；thirdparty 仍承载调用外部库的业务适配，不承载下载的 SDK 源码。

## 12. 旧代码的保留、改写与删除

实施前按引用关系处理，不能整目录回退与模型/SSE/前端功能交叉的改动。用户已明确允许清理不需要的旧实现，不做历史兼容层。

| 当前内容 | 决定 |
| --- | --- |
| `harness/agent`、`session`、`hostruntime` 等核心实现 | 保留；只拆除旧 Node 兼容调用，保留 Go 自身必要执行阶段 |
| `internal/credentials`、`internal/authorization`、`internal/mcpclient` | 保留唯一基础实现；按新 owner/调用契约做必要适配 |
| `internal/plugins/rpc.go`、`extensions/cordis/src/rpc.mjs` | 复用 framing/pending/双向分发基础；删除旧 next/probe 特判和上游错误身份处理，补 JSON-RPC 合法响应检查 |
| `internal/plugins/sync_rpc.go`、`sync-client.mjs`、`sync-worker.mjs` 与 sync fixtures | 删除，包括 socket、启动参数、SAB/Atomics 逻辑和专属测试 |
| `extensions/cordis/src/tools-service.mjs`、`tool-hooks.mjs` | 替换为 ctx.host façade 与 callback registry；删除上游 ToolsService 重复实现及工具阶段/continuation 适配 |
| `internal/plugins/tool_pipeline.go`、`tool_selection.go` | 保留可用的 Go 执行身份/取消基础，只保留实际需要的业务调用/取消适配；删除 Node pre/around/post/result 阶段接管与重复 Agent scope |
| 新增 `host-tool-events.mjs`、`host_tool_events.go` 及阶段专属测试 | 删除实验性 next/stage/continuation 桥接，不替换为新的一套阶段协议 |
| 已排除的 PreExecute guard 及插件 guard 接口 | 已删除 Go CheckGuards/ToolGuardDenial、旧 ctx.tools.guard 与新 registerGuard、对应 Node/RPC 适配和专属测试；保留执行前策略/审批/取消、工具自身权限和沙箱。Post 阶段 repeatGuard 是重复调用提醒，不属于本项删除范围 |
| 新增 `host_assets.go`、资产 spool 桥接、SDK Assets/图片/文件引用及专属测试 | 删除本轮扩张的插件资产接口；内置附件、read_image/生图、present、下载链路及它们实际需要的受控内部结构保留，按引用核对，不整段回退共享代码 |
| 方案自行新增的工具开始/完成插件通知及专属示例/测试 | 删除，不改名补回；保留 Go 内部 Result 和现有前端会话工具消息 |
| `harness/agent/tool_pipeline*.go` 等 | 保留 Go 必要管线；删除只为外部 middleware 存在的 continuation 分支。不能因去掉插件 post hook 而删原生结果校验和终结 |
| `runtime.mjs` 中 PromptService/ToolsService/SkillsService 核心继承和 scope 模拟 | 删除，替换 Host Bridge；Go 提供最终查询/组装 |
| `credentials-service.mjs`、`authorization-service.mjs`、`mcp-service.mjs` | 仅保留可用通信逻辑，重写为 ctx.host 子入口；删除上游服务类依赖和状态镜像 |
| `package-identities.mjs` 及上游 core 包身份固定 | 删除；新 SDK 自己定义类型，不为上游 plugin 身份做兼容 |
| `execution-context.mjs` 中完整上游 scope/policy 适配 | 删除不再需要部分；用简单只读 DTO 和 invocation 取代 |
| `internal/plugins/actions.go`、`storage.go`、`plugin/actions.go` 及对应 Node 服务/路由 | 删除，含 SDK、Controller/client 和前端入口引用；不改名恢复 |
| 旧 SDK `index.d.ts/index.mjs` 与上游类型依赖 | 重写为 ctx.host API，不保留 ctx.tools 等 alias、createScope overload 或旧模板 |
| `composition.mjs`、Bundle/Patch/Isolate/Intercept 配置桥 | 删除公开模型与旧兼容逻辑，使用自有 packages/instances 驱动真实 Cordis loader |
| 包来源解析与 pnpm 安装基础 | 有用部分保留；改自有 metadata 校验，不保留 dsh manifest/市场为必需项 |
| 旧市场/Bundle/Patch/Actions 等 Web/Mobile 页面 | 删除不再使用入口；插件列表、配置、状态和已有通用授权交互组件按新结构复用 |
| 旧同步/任意上游插件兼容测试、pipeline oracle | 删除相应内容，换成自有框架契约验收 |
| `tests/upstream-plugins/model-selection-oracle.mjs` 等非插件框架 oracle | 先查引用；属于模型等独立功能的保留，不能因目录名直接整目录删除 |
| 旧示例 catalog-app/cordis.patch 等 | 删除或重写；不留无法在新框架运行的示例 |
| Host 镜像复制、依赖锁、NOTICES 与 CI 命令 | 随最终依赖更新；保留 Node/Python/Chromium/Office/浏览器基础环境，不新增无关 CI 测试 |
| 旧三份插件方案 | 新方案实施完成后删除，正式文档只保留当前架构和开发指南；不留互相矛盾的有效方案 |

## 13. 实施顺序与验证

### 13.1 实施顺序

1. 新建自有 SDK 类型和协议合同，确定 ctx.host/ToolResult/Registration/HostEvent；先不把旧 SDK 当新方案继续补丁。
2. 收敛通用双向 RPC，删除阻塞同步通道，建立 Host Bridge 插件和 async apply/卸载生命周期。
3. 按第 8.1 节建立统一 Go PreExecute/Execute/PostExecute/Result 四环节，将已采用的上游核心模块行为放入对应位置；接通工具注册、业务 execute 和 host/* 通知，删除远端阶段/next 链、PreExecute guard 及其插件接口。
4. 接通 credentials、authorization、MCP，清理 Node 中重复服务和旧错误类型依赖。
5. 接通 Go Skill 目录/Provider、附加 prompt；清理新增插件 Assets/spool，保留内置图片和 present 投影。
6. 替换 packages/instances 管理合同、Controller 与自有 UI 数据调用；沿用现有视觉风格，不在本任务重新设计前端。
7. 按第 12 节删除旧服务、兼容逻辑、依赖和示例；更新开发文档、构建和既有 CI 引用。
8. 本地完整重建验证；不将方案完成等同于代码已完成，不额外进行未授权的生产部署。

### 13.2 验证清单

| 范围 | 必须证明的行为 |
| --- | --- |
| 通用通信 | 双向请求、乱序响应、反向嵌套请求、错误、void/null、断连和取消；读循环不被业务 await 堵住 |
| 生命周期 | register 确认后贡献可被后续快照使用；原生 apply 失败/途中卸载无贡献泄漏；不假设原子发布；dispose 幂等；无自建实例状态机、Worker/SAB/sync socket |
| 工具四环节 | Pre 调用准备/策略/审批/取消、Execute 持久边界/超时/真实业务、Post 输出治理/规范化、Result 最终核心确认/提交；无独立 guard 步骤，顺序与失败分支符合第 8.1 节，Result 不再修改最终结果 |
| 工具执行 | 注册函数由 Go 业务入口直接调用；无远端阶段/next；输入/内容校验、并发限制、取消收尾；checkpoint 失败不执行 |
| 核心阶段能力 | Job 输出上限捕获及终结、重复提醒、完整输出恢复、成功后交付/结构化确认/指令观察；不能仅以阶段函数存在作为还原证明 |
| 内置规则与审批 | Go 执行前策略的 allow/deny/ask 决定、审批结果处理及取消仍正常；工具业务内部审批发生在实际副作用之前；无 PreExecute guard、插件 guard 接口或跨进程 next；工具权限/沙箱和 Post 重复调用提醒保持原能力 |
| ctx.on | 同一 Host 的公开元数据通知及 instanceId 过滤、异常隔离、顺序起始、有限队列、无密钥/全部会话内容泄漏；不以通知承担可靠命令 |
| Skill/prompt | Go 最终合并、动态 Provider、目录失效、动态附加提示；没有 Node 第二个全局核心注册表 |
| 凭证/授权 | 真实 Go 存储、owner 地址、原子 update、run 中请求回 Go 不死锁、提交成功才 authorized |
| MCP | 注册声明和连接结果区分；Go 直接执行 MCP 工具；OAuth 复用基础模块；Node 无双重 Client |
| 结果与范围 | 插件仅接收/返回已定义 JSON 和 text/data，拒绝 image/file/任意交付 Meta 及超限帧；无 Assets/spool 公共入口；内置图片和 present/下载/投影仍正常 |
| 清理 | rg/依赖图确认旧重复服务/同步调用/SDK alias 不再被生产路径引用；没有未使用文件或废弃示例 |
| 核心回归 | 会话创建/恢复、模型调用、权限、Schedule、记忆、present、全 Host SSE 和自有前端保持原能力 |

以自有最小插件分别覆盖工具、Skill 目录、动态 Provider、MCP 和授权，不再以原样运行上游任意插件为验收标准。集成时使用受控服务/模型 mock 验证通信，真实模型验收沿用户另行指定的范围进行。

## 14. 实施与验收记录

核对日期：2026-11-20。下表对应本文的目标合同；日志与真实客户端截图保存在本地忽略目录 `.cache/bridge-validation/`，不作为开源运行依赖。

| 目标 | 当前实现及验收依据 |
| --- | --- |
| Go 核心职责与统一四环节 | `harness/agent/tool_pipeline.go`、`native_tool_pipeline.go`；管线、checkpoint、固定工具选择、取消/超时、长输出恢复、Job 输出和结构化结果测试通过。插件 body 仅在 Execute 通过 RPC 调用，不参与会话提交。 |
| 异步通信与 SDK | `internal/plugins/host_bridge.go`、`rpc.go` 与 `extensions/cordis/src/`；版本/能力/帧限制握手、反向请求、失联、取消、超限、JSON 无损校验及晚到注册撤销测试通过。普通调用均为 Promise，无阻塞同步通道。 |
| 工具、Skills 与附加提示 | 真实 Node/Go Bridge 测试覆盖工具登记与本体执行、动态 Provider、目录更新、用户覆盖、invalidate、动态模型上下文及稳定提示排序；原生 Cordis 卸载会撤销贡献。 |
| 凭证、授权与 MCP | `internal/credentials`、`internal/authorization`、`internal/mcpclient` 仍为唯一 Go 服务。测试覆盖实例凭证地址、查询/更新、嵌套 RPC、授权提交、回调 state/重放、取消和已准入写入；MCP 由 Go 连接并执行。 |
| 审批与问题 | Bridge 测试覆盖四种 Go 审批结果、普通审批不套短 RPC 超时、真实 Go turn 内提问及回答进入模型上下文。保留已有前端审批/问题机制。 |
| 生命周期与公开通知 | 原生 Loader/Fiber/effect 负责装配和撤销；真实 Cordis 测试覆盖依赖注入、加载失败及卸载。Host 通知有界、可合并、异常隔离，返回值不改变执行；无工具阶段通知或 next。 |
| 安装、分发与正式示例 | Host 随镜像提供本地 SDK，实际镜像验证带 SDK peer dependency 的包安装、运行时导入、工具调用和卸载；三个 JavaScript 示例及编译后的 TypeScript 示例通过真实 Cordis 测试。无需预先发布 npm SDK。 |
| 项目内置市场 | 正式 `extensions/catalog/plugins.json` 当前为 `[]`。公开安装/更新仅接受受审 pluginId，默认停用；Web/Mobile 覆盖安装、配置、启停、显式更新、卸载及空状态。测试目录源和条目只用于隔离验收。 |
| 认证交互 | 两端已覆盖无认证、API Key、并行 text/secret/select、普通业务 OAuth、标准 MCP OAuth、失败、取消、收起后继续和浏览器返回刷新。Android 使用真实 Chrome；仅测试本地受控供应商。 |
| 恢复与管理 | 最新 QA Host/Controller 镜像已实际运行，重建 Host 后会话和凭证保留、MCP 恢复连接。Web 操作插件重启后实例恢复就绪。重启直接携带 Redis 当前配置，不以失败实例再次 apply 成功为前置条件。 |
| 旧实现清理 | 第 12 节列出的旧同步通道、阶段/next、guard、Assets/spool、Agent/Session 代理、Actions/Storage、重复 Node 核心服务及上游 core 类型依赖已移除。三份旧插件方案已删除。清理扫描仅命中用于拒绝旧接口的负向测试。 |
| 文档与既有 CI | SDK、示例、市场维护说明、API、OpenAPI、架构及双语 README 已对应当前合同；已有 CI 改为安装和测试当前 Cordis/SDK，未为本任务增加额外 CI 作业。 |

最终验证：

- [x] `go test ./... -count=1`、`make vet check`、`make race`；授权模块另连续 30 轮 race 测试，覆盖取消传播前的提交拒绝与已准入提交继续完成。
- [x] Cordis 11 项测试、SDK 类型与真实编译示例、Web 20 项测试及生产构建、移动端类型检查。
- [x] Host 和 Controller Docker 镜像重建；镜像内 OfficeCLI/LibreOffice、Python、Chromium、agent-browser 冒烟及 SDK 包分发测试通过。
- [x] 最新 Host 镜像的全部 10 项 Docker 集成测试通过，覆盖会话恢复/文件、记忆/检索、模型切换、Schedule/附件恢复、全 Host SSE 审批重连、队列/问题、浏览器及内置工具。
- [x] `python3 scripts/smoke-cli.py`：真实 Compose Controller + Redis + 独立 Hosts，覆盖账号 Hash、隔离、Redis 重启、停止和释放。
- [x] 最新 Android arm64 QA APK 重建、安装，并连接隔离 Controller；真实 Web/Android 授权矩阵和截图已核验。QA APK 使用测试签名，不是公开 release。
- [x] `git diff --check`、所有 Go 源文件格式检查及依赖边界检查通过；测试插件、日志、截图均被忽略。

范围说明：ECI 按用户要求未验收；未验证任意第三方供应商或任意上游插件，也未发布正式业务插件。没有修改 LifeClaw、生产部署或远端 Git，没有提交和推送。本地受控 OAuth 成功仅证明本方案的协议与交互链路。

核心结论：**Go 管核心执行和状态，Cordis 管插件装配与生命周期，Host Bridge 提供自有异步能力接口；业务工具回调与状态观察分开。**

## 15. 工具阶段事件职责审计：先理解业务，再决定开放边界

日期：2026-11-19。此节记录用户暂停实施后要求的源码研究。正式四环节职责、返回类型和代码对应已收敛在第 8.1 节；这里的上游监听器仅作为调研证据，不作为 Cordis 阶段桥接实施指令。凡 experimental 及已明确排除的浏览器/CUA、Office 激活和上游 UI 内容均不迁入。实施已恢复，按本方案当前正文执行。

### 15.1 研究范围和证据

核对上游固定提交 `ca33fd6843b205e279c7ccb6cb73a472c78f5760`。下载该提交源码到仓库外的研究临时目录，扫描 `packages/**/src` 中实际监听注册，并排除测试和文档示例；对下表源文件校验 Git blob SHA 与该提交完整树一致。结果为 7 个源文件、8 处 `tools/execute` 监听注册。不是所有外部 npm 插件的生态普查，不能据此声称第三方不存在其他用途。

### 15.2 上游 tools/execute 监听器到底做什么

| 上游源文件 | 实际工作 | 工具过滤 | 我们应如何承载 |
| --- | --- | --- | --- |
| `guard/timeout-policy/src/index.ts` | 读取工具 timeoutMs，派生取消信号，等待真实操作收尾后将自身到期归类为 TOOL_TIMEOUT，恢复原始信号 | 按当前工具声明判断是否有超时预算 | Go 统一工具执行器内置；现有 native_loop 已有 context deadline/cause，不应再重复加载 Node 超时插件 |
| `session/session-checkpoint-policy/src/index.ts` | 顶层工具开始副作用前 flush 会话；flush 失败不执行；嵌套调用复用外层持久边界 | 有 Agent 且非嵌套调用 | Go 会话/工具提交边界内置；现有 appendNativeToolIntent 已在 dispatch 前 Flush |
| `experimental/browser-use-stagehand-native/src/index.ts` | 浏览器资源取得、参数预检、Agent 身份确认、资源互斥与取消绑定、调用结束清理 | 所注册工具名称的 Set | 若采用该业务能力，应由相应 Go 模块或该插件自身 execute 包装处理；当前 agent-browser 不是这个 Stagehand 模块，不因审计直接迁入 |
| `experimental/browser-use-runtime/src/mcp.ts`，两处 | 浏览器 MCP 与会话资源归属检查、可用性检查、独占操作和资源取消信号合并 | 工具前缀；资源工具还检查参数中的 server | MCP 连接/工具执行由 Go MCP 模块管理；特定浏览器资源策略留在对应模块，不自动引入上游 Agent scope |
| `experimental/computer-use-cua-driver-native/src/index.ts` | 将插件资源生命周期信号与工具信号合并，跟踪在途操作，结束时移除跟踪 | 所注册工具名称的 Set | 框架负责已有调用/插件生命周期取消；业务资源可在插件 execute 中 try/finally 处理，不需要为此开放全局 around |
| `experimental/cua-macos/src/held-keys.ts` | 保持程序执行期间的按键状态，在取消或结束时释放，避免按键残留 | run_code/cua_run_code | 特定电脑控制模块的业务资源逻辑；当前未提供相同模块，不扩充 Go 核心来模拟 |
| `experimental/claude-code-mods/src/index.ts` | 桥接另一套 mods 的 tool.call 拦截体系，可拒绝、委托或替代结果，并禁止重写已记录的工具名称/参数 | mods 注册表、调用来源和工具别名 | 兼容另一套 hook/mods 生态的可选能力，不属于我们的基础执行必需项；当前无需迁入 |

以上没有一个监听器承担“根据工具名称寻找某个插件登记的 execute 函数”的通用注册表职责。上游工具系统的 pipeline 仍提供末端 dispatchToolBody，并由该函数解析、校验和调用工具本体。事件监听器包裹这个函数。

### 15.3 通用事件如何判断关注哪些工具

上游事件给监听器传入 exec，其中有 name、arguments、调用标识、signal，以及上游自身的 Agent/嵌套调用信息。监听器自己判断名称、前缀、参数或状态；例如 Stagehand 用 names.has(exec.name)，浏览器 MCP 用前缀及 arguments.server，按键清理只关注 run_code/cua_run_code。无匹配时调用 next 委托后续逻辑。

上游原生 scope 可以按 Agent 过滤，并不等于“一个插件只收到它自己登记的工具”。这是对上游监听器行为的解释；本方案不开放这些工具阶段事件，也不向插件广播全部工具参数，因此不需要为其新增工具选择规则或 Agent 作用域。第 7 节的状态通知仍按自己的公开 payload 派发。

### 15.4 pre/post 的现成逻辑也不都该成为外部插件

- pre：tool-jobs 捕获输出限制；workspace-changes 等待文件变更记录器稳定；experimental auto-review 做风险审查；hooks-codex/hooks-claude-code 接另一套外部 hooks。
- post：repeat-tool-reminder 注入重复调用提醒；grep/glob 控制搜索输出并保存完整结果；spill-policy 限制长工具结果；外部 hooks 对结果给出决定。
- result：present 提交交付事实；structured output 提交最终捕获结果；agent-instructions 更新触及路径；另有 UI/实验模块的观测处理。

其中会话持久化、交付事实、Agent 上下文、输出治理和工具资源管理应由我们的 Go 内置模块完成；不能因为上游用 ctx.on 做这些事情，就让 Node 参与我们的事实提交。其他可选生态兼容功能按需引入，不属于为了插件工具能运行必须开放的接口。

### 15.5 调研结论归入统一四环节

按第 8.1 节执行：Go 保留统一 PreExecute/Execute/PostExecute/Result，相关核心模块的行为在对应位置直接调用；不公开通用 Cordis 阶段事件或跨进程 next。执行前策略、审批、取消和结果处理保留明确的内部合同；PreExecute guard 不属于本轮采用的行为，其现有 Go/Node 实现与插件接口按第 12 节删除。插件只实现业务回调及基础能力调用。恢复实施时按模块与行为逐项核对缺口，不能用删除 Node 钩子代替完成 Go 内置逻辑。实施已恢复，按本方案当前正文执行。

主要依据：[timeout](https://github.com/deepseek-ai/deepseek-harness/blob/ca33fd6843b205e279c7ccb6cb73a472c78f5760/packages/guard/timeout-policy/src/index.ts)、[checkpoint](https://github.com/deepseek-ai/deepseek-harness/blob/ca33fd6843b205e279c7ccb6cb73a472c78f5760/packages/session/session-checkpoint-policy/src/index.ts)、[Stagehand](https://github.com/deepseek-ai/deepseek-harness/blob/ca33fd6843b205e279c7ccb6cb73a472c78f5760/packages/experimental/browser-use-stagehand-native/src/index.ts)、[浏览器 MCP](https://github.com/deepseek-ai/deepseek-harness/blob/ca33fd6843b205e279c7ccb6cb73a472c78f5760/packages/experimental/browser-use-runtime/src/mcp.ts)、[工具契约](https://github.com/deepseek-ai/deepseek-harness/blob/ca33fd6843b205e279c7ccb6cb73a472c78f5760/packages/core/tools/src/contracts.ts)。
