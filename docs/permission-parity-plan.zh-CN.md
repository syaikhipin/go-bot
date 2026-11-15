# 会话执行权限与单次审批

权限选择作用于当前会话。上游将文件沙箱与审批策略独立存入会话日志，预设把两者组合。Go Harness 保留 `danger-full-access` 作为新会话默认值；聊天框可切换 `workspace-write` 和 `read-only`。会话重开后恢复选择，其他会话不受影响。

| 选项 | Agent 的文件写入范围 | 审批 |
| --- | --- | --- |
| 全权限 | Host 工具进程原本可写的范围 | 不请求提权 |
| 工作区可写 | 当前工作区与隔离临时区 | 可申请更宽权限，限一次调用 |
| 只读 | Linux 沙箱所需的输出设备，不允许普通文件写入 | 可申请工作区可写或全权限，限一次调用 |

Host 执行限制。Shell、后台 Job 和写入／编辑工具使用 Linux 内核沙箱，复用上游也支持的 Landlock 后端语义。部署时验证内核和容器策略是否支持；不可用时明确失败。Host 自己仍负责日志、附件等运行状态。不把文件沙箱当成网络策略或租户身份认证。

工具定义和沙箱拒绝结果指导模型在必要时发起提权，Host 在运行该调用前等待决策。审批卡复用现有 SSE 和问题交互的交付机制，显示请求权限、具体命令或路径和理由，提供「拒绝」与「仅允许一次」。关闭、超时、没有前端或取消 turn 均不能放行。批准不切换会话权限，也不授权子 Agent 或后续调用。会话忙时前端暂时关闭权限切换，避免正在运行的操作与选择含义冲突。

验收覆盖新会话默认值、会话隔离与重启恢复，真实 Linux 下的写入、重命名、链接与后台执行，真实 Controller 请求的拒绝和单次放行，以及浏览器和 iOS 模拟器中的选择器、审批卡和结果截图。

上游参考：[权限预设](https://github.com/deepseek-ai/deepseek-harness/blob/39163dd30f703a79079e2d2446545cff3c453738/docs/subsystems/permission-presets.md)、[进程沙箱](https://github.com/deepseek-ai/deepseek-harness/blob/39163dd30f703a79079e2d2446545cff3c453738/docs/subsystems/sandbox.md)、[人工审批](https://github.com/deepseek-ai/deepseek-harness/blob/39163dd30f703a79079e2d2446545cff3c453738/docs/subsystems/approval.md)。


## 当前实现

上游在受限执行前选择 Linux 沙箱后端。Go Harness 采用其 Linux 后端之一 Landlock，由独立进程加载规则，再启动 Shell 或执行文件工具的变更阶段。运行时至少需要 Landlock ABI 6。它约束创建、写入、截断、删除、重命名和链接等文件操作。Landlock 对部分元数据和进程通信的能力受内核版本限制，例如它不完整约束 `chmod` 和 `chown`。三档权限也不表达文件读取隔离或网络访问策略。每次受限调用仍单独加载规则，沙箱初始化失败时停止执行。

提示与工具定义沿用上游的配合方式。Agent 在首次模型请求以及生效策略变化时收到 `runtime-context` 快照。Shell、写入和编辑工具声明 `sandbox_permissions` 与 `justification`。沙箱拒绝提供相应的重试提示，批准只绑定到这次待执行调用。会话日志记录 `permission/preset`、`sandbox/mode`、`approval/policy` 和配对的 `approval/asked`、`approval/decided`。

`NativeFactory.ToolApproval` 是已注册工具执行前的审批策略入口。它接收已经验证的工具名与参数，可返回一个带理由和操作详情的审批请求，也可返回空请求让调用继续。Host 复用同一个单次审批服务。现有工具没有配置主动审批策略。

客户端登录账号，聊天页只订阅当前会话的 SSE。定时任务也走同一套工具权限。目标会话有交互订阅时可提交审批，尚未打开时返回审批渠道不可用。客户端切回时可重新查看仍待决策的审批；未处理的审批将在两分钟后取消。

子 Agent 默认沿用委派时的上限，禁用交互提权，并继续受主会话权限上限约束。Shell Job 继承启动调用的沙箱。会话运行期间及仍有子 Agent 或 Job 在工作时，禁止切换档位。


## 本地验收

Linux Docker 容器实际拦截了只读写入、越界写入、借助软链接的越界写入、跨边界重命名和硬链接。工作区内的写入与隔离临时目录可用。通过 Controller 验证了后台 Job 和原生文件写入采用同样的权限，独立会话仍默认全权限，Host 重启后保留原会话选择。打开已冷却会话后发出的第一条请求也能收到审批。

真实 Web 和 iPhone 17 Pro 模拟器都通过同一套 Controller、Redis、Host 跑过两种受限模式。测试使用可复现的本地 Responses 模型服务，让模型接口先返回普通调用，再在收到沙箱拒绝后请求提权。两端均核对了允许后的文件存在、拒绝后的文件不存在，以及会话日志中的审批结果。无交互连接时验证了返回审批不可用。另有自动化测试验证工具可主动阻止分发并请求审批。

标准 Dockerfile 构建遇到 Docker Hub 连接重置。本地验收镜像复用了已安装依赖的基础层，并重新编译了当前 Host、Controller 和 Web 资源。iOS 使用重新构建并安装的 Release 模拟器包。未在阿里云 ECI 上运行验收。
