# 本地 Controller、Host 工作目录与 Skills 设计方案

> 历史设计与调研记录。当前行为、配置和命令以[文档索引](README.md)中的指南为准。

本文记录原先的 CORS 决策。内置 Web 预览版改为同源浏览器 API，开发源由 Controller 参数显式允许。 [当前 API 契约](api.md)

2026-11-11。实施范围为独立的 Go Harness 仓库。下文保留改造前后的对照。DeepSeek Harness 对照基线为 `fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159`。

本文记录当时的存储和启动选项。当前 Host 由 Controller 启动，Docker 自动挂载账号专属卷，ECI 支持容器层和 NAS。 [当前部署说明](deployment.md#storage-choices)

## Controller 的两个字段

| 字段 | 当前作用 | 拟议行为 |
| --- | --- | --- |
| `CONTROLLER_API_TOKEN` | 保护 controller 的 HTTP 入口，包括就绪、会话、文件、SSE 和 Host 启停。留空会在 controller 状态目录生成 `api.token`。CLI、示例和客户端使用 Bearer 访问 | 删除这个环境变量和公开 HTTP 的 Bearer 检查。所有 controller HTTP 调用走回环地址，CLI 不再读取或生成 `api.token` |
| `CONTROLLER_ALLOWED_ORIGINS` | 对带 `Origin` 的请求做精确匹配，空列表会拒绝这类请求，即使请求来自同一台电脑。命中时响应 CORS 头及预检 | 删除配置字段和 Origin 匹配分支。对支持的 API 固定返回 `Access-Control-Allow-Origin: *`，允许 GET、POST、DELETE 和预检所需的请求头，不启用凭证式 CORS |

现有实现中的公开 token 和 Host token 是两组凭证。拟议删除的是浏览器、SDK 和 CLI → controller 的公开 token。controller → Host 的内部 token 继续自动生成，代理继续用它访问 Host 的会话及生命周期接口。Docker Host 端口发布默认只在本机，ECI Host 使用私网端点，两者都保留这一层认证。独立运行 Host 继续使用自动生成的 Host token。 [当前 controller 路由](../internal/controller/controller.go) · [当前 CLI](../cmd/controller/main.go) · [当前 Host 入口](../internal/host/host.go)

### 本机访问约定

- 保留 `CONTROLLER_LISTEN` 以便更换端口，默认 `127.0.0.1:8080`。新校验只允许回环地址，支持 `127.0.0.1`、`::1` 及归一化到回环的 `localhost`。在任何云资源操作前完成校验。HTTP `Host` 也只接受回环主机名或地址，用于阻止外部域名经 DNS 重绑定访问本机，浏览器的 `Origin` 无需进入白名单。
- 本机 UI 可以来自不同端口或 origin。预检按 API 支持的 HTTP 方法与请求头响应，无需 token 或 cookie。相同 CORS 头用于常规响应和 SSE。已有 SDK 临时携带的 `Authorization` 可作为兼容请求头接收，入口忽略该值，转发层仍覆盖为内部 Host token。
- 在此契约下，任何能连到本机端口的进程，以及获得浏览器本地网络访问许可的网页，都拥有该个人 Agent 的 API 权限，包括创建会话、读取文件及触发工具。浏览器还可能自行要求本地网络许可。远程访问通过 SSH 转发或部署方的认证入口实现；ECI 仍要求 controller 能访问 Host 的 VPC 私网。 [浏览器 CORS](https://developer.mozilla.org/en-US/docs/Web/HTTP/Guides/CORS) · [浏览器本地网络访问](https://developer.mozilla.org/en-US/docs/Web/Security/Defenses/Local_network_access) · [ECI 私网部署说明](deployment.md)
- 升级时接受旧 `.env` 中的两个字段并忽略其值，避免严格解析导致旧部署启动失败。新的模板、写回逻辑、Go client、OpenAPI、README 与 HTML 示例停止使用这两个字段。历史 `api.token` 文件可供回滚，新版本不读取它。部署状态锁、实例归属校验及内部 `host.token` 继续使用。

## 容器与 NAS 的现状

本地镜像 `go-harness/host:local` 实测 UID 为 0、`HOME=/root`、进程初始目录为 `/root/harness`，OfficeCLI 为 v1.0.160。镜像 Dockerfile 和 ECI 启动请求使用同一个 Host 入口及工作目录。Docker 的命名卷、绑定目录和 ECI NAS 都挂到同一个容器路径。 [Dockerfile](../build/Dockerfile.host) · [Docker driver](../providers/runtime/docker/driver.go) · [ECI SDK 请求](../thirdparty/aliyun/eci.go)

| 运行模式 | `/root/harness` 的实际来源 | 重建计算实例 |
| --- | --- | --- |
| Docker `container` | 容器可写层 | 新容器获得新的目录；启动原容器仍读原可写层 |
| Docker `volume` | Docker 命名卷，默认由部署 ID 派生 | 使用原卷恢复 |
| Docker `bind` | `STORAGE_SOURCE` 指向的本机绝对路径 | 使用原目录恢复 |
| ECI `container` | 容器可写层，启动请求省去 NAS | 新实例使用新的目录 |
| ECI `nas` | `NAS_MOUNT_TARGET` 对应 NAS 的 `/go-harness/<deployment-id>`，通过 `alicloud/nas` FlexVolume 挂载。旧部署使用 controller 已记录的路径 | 在同一个私网挂载目标下挂回原子目录 |

以 NAS 模式为例，容器里的 `/root/harness/workspaces/<session-id>/report.docx` 对应 NAS 上的 `/go-harness/<deployment-id>/workspaces/<session-id>/report.docx`。NAS 是运行平台在启动前挂入的，Host 只校验挂载和执行文件 I/O。现有请求使用 NFS v3，新的设计沿用相同的 NAS 子路径、VPC 要求与实例归属规则。同一数据根由一台 Host 写入，切换实例时先确认旧实例已退出。真实 NAS 上的权限、文件语义和复挂仍需独立验收。 [部署状态和路径](../internal/controller/controller.go) · [Host 挂载校验](../internal/executor/verify_linux.go) · [阿里云 NAS 挂载说明](https://www.alibabacloud.com/help/en/eci/user-guide/mount-a-nas-volume)

### Session 工作目录

创建顶层 Session 时，Host 分配 `/root/harness/workspaces/<session-id>`，并把这个绝对路径写入 v4 header 的初始 `cwd`。Shell 的默认目录和相对文件工具路径都来自该工作目录。v4 日志单独落在 `/root/harness/.dsh/sessions/<project-key>/<session-id>/session.v4.jsonl`，其中 `project-key` 由初始 `cwd` 计算。Host 级 Schedule、图片对象和数据身份同样位于持久化根内。子 Session 继承父 Session 的有效 CWD 和所属 workspace，现有子任务可在创建时指定 CWD，实际变化通过工作目录事件保存。 [Host 的 Session 装配](../internal/host/host.go) · [Session 存储](../harness/session/file_store.go) · [子 Session 与 CWD](../harness/assembly/native_runtime.go)

### 上游日志存储与恢复

上游共享配置将 JSONL 后端指向 `$DSH_HOME/sessions`，默认的 `DSH_HOME` 是 `~/.dsh`。每个 Session 使用其 header 中的初始 `cwd` 生成可读项目目录，再以编码后的 Session ID 建独立子目录。上游允许缺少初始 `cwd` 的会话放入 `_no-cwd`，Go Host 创建的会话都有绝对初始工作目录。有效 CWD 的变化作为事件追加，日志路径由初始目录固定。 [上游共享配置](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/packages/bundle/base/cordis.patch.yml) · [上游路径代码](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/packages/session/session-persistence-jsonl/src/format.ts)

日志首行是版本化 header，后续按顺序追加会话事件。消息视图从事件投影得到。上游在首次追加或显式 flush 时发布文件，批量写入后同步落盘，打开时校验事件并处理可恢复的末尾残缺记录。列表只读 header。上游默认使用带校验和的 Zstandard 帧，也支持 `compression: none` 的纯文本 JSONL。上游识别每个会话最高的格式代，按需读取或迁移旧格式。 [上游持久化后端](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/packages/session/session-persistence-jsonl/README.md) · [上游持久化子系统](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/docs/subsystems/persistence.md)

Go Host 采用同一目录层级和纯文本 v4。创建会话时即发布 header，后续追加事件。列举顶层会话时扫描日志目录并读取 header，冷恢复从该文件重放。子 Session 按所属顶层会话的初始工作目录分组，每个子 Session 有自己的文件。图片和 Schedule 也使用同一数据根内的 `.dsh`。 [Go Host 日志列表](../internal/host/host_list.go) · [Go v4 存储](../harness/session/file_store.go)

```text
Host  /root/harness/.dsh/sessions/<project-key>/<session-id>/session.v4.jsonl
NAS   /go-harness/<deployment-id>/.dsh/sessions/<project-key>/<session-id>/session.v4.jsonl
```

NAS 行展示新部署的默认路径。已有部署的实际 NAS 根以 Controller 状态中的 `nasPath` 为准。切换 ECI 实例时挂回原来的 NAS 子目录即可读取原有日志。Docker volume 和 bind 使用相同的相对路径。旧构建产生的会话日志如需继续使用，需在 Host 停止后把 `.harness/sessions` 下的项目目录移入 `.dsh/sessions`，保持原有层级。 [NAS 请求装配](../internal/controller/controller.go) · [ECI 挂载](../thirdparty/aliyun/eci.go)


### 上游 checkpoint 的两层数据

执行 checkpoint 在发出模型请求前、执行顶层工具前和下一模型步骤前，把普通事件刷新到该 Session 自己的日志中。崩溃恢复读取连续有效的事件并收尾中断的调用、步骤和轮次。 [执行 checkpoint 策略](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/packages/session/session-checkpoint-policy/README.md) · [日志恢复](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/docs/subsystems/persistence.md#crash-recovery-preserves-an-interrupted-turn)

投影缓存单独保存于 `$DSH_HOME/storages/session_projcache/sessions/<session-id>.json`，记录派生状态、投影版本、已处理事件序号和日志身份。恢复时通过身份及版本校验后才从缓存进度继续计算，日志负责提供完整事实。共享配置的定期阈值为 200 个事件或 5 秒，创建、轮次结束和释放时另有写入点。本次 Go 改造沿用现有 v4 日志恢复流程。 [上游投影缓存](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/packages/session/session-projection-cache/README.md)


## Schedule 和附件存储

下文基于同一份上游提交。会话日志、Schedule 任务域和附件存储都在 Host 的 `$DSH_HOME` 下。

### Schedule

上游将任务作为 Host 级 `schedule` 存储域管理。共享配置选用 JSON 后端，整个域写在 `$DSH_HOME/storages/schedule.json`，文件使用 `unit.name`、`unit.version`、`global` 和 `tables.tasks`，以任务 ID 为键。上游当前域版本为 2。它独立读取任务状态，历史会话的 `schedule/change` 仅用于回放。上游还支持其他存储后端，独立 Go Host 的默认部署只需要与上游共享配置一致的 JSON 布局。 [共享配置](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/packages/bundle/base/cordis.patch.yml) · [Schedule 存储格式](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/packages/schedule/schedule/README.md#storage-format) · [JSON 后端](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/packages/storage/storage-json/README.md)

改造前，Go Host 把任务写入 `.harness/schedule/schedule.json` 的 `version + tasks[]` 文档。实现复用 Host 级单写入者、持久化收件箱确认、错过周期处理和回执裁剪。 [Go Schedule 服务](../harness/schedule/service.go) · [Go 文件存储](../harness/schedule/file_store.go)

1. **存储。** 改用 `$DSH_HOME/storages/schedule.json`，复现上游 whole-unit envelope 和按任务 ID 键控的任务表。读取时校验域名、版本、键与记录 ID、规则、目标和回执。不存在的文件视为空域，无法确认完整性的文件拒绝启动调度并保持文件原样。写入使用同目录临时文件、文件同步、原子替换与目录同步，继续复用 Go 的串行提交和不确定写入处理。用固定上游样本验证读写。
2. **任务与投递。** 在当前已有会话目标基础上补上上游的 `session`、`new_once`、`new` 三种目标、`active`、`paused`、`ended` 生命周期及 `rrule`。保持现有顶层 Session UUID 契约。需要新建会话时，先在任务里保存目标 UUID，再通过 Host 的会话入口恢复或创建，重试复用同一目标。新会话使用当前 Host 的默认工作区和模型配置。管理接口的兼容与新增能力按目标、状态和规则分别验证。 [上游任务 schema](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/packages/schedule/schedule/src/storage.ts) · [上游调度行为](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/docs/subsystems/schedule.md)
3. **恢复与回执。** Host 启动后加载任务，恢复最近错过的一次重复触发。会话收件箱确认持久化后，再保存任务下一次触发时间、状态和回执，回执保留实际目标 Session ID。按上游默认保留最近 30 天且至多 200 条，每条回执表示收件箱已接收。收件箱落盘和任务状态写入之间仍可能发生重复投递。存在活跃任务时沿用 Go Host 的空闲回收约束。手动停止或 ECI 退出期间等待下一次启动恢复。 [上游执行顺序](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/packages/schedule/schedule/src/runtime.ts) · [Go Host 活动状态](../internal/hostruntime/native_host.go)

### 图片和普通附件

上游在 `$DSH_HOME/attachments/v1` 持久化对象。图片以规范化后的内容计算 SHA-256，放到 `objects/<摘要前两位>/<完整摘要>`。普通文件原样写入 `file-objects/<摘要前两位>/<完整摘要>`，并通过硬链接提供 `files/<摘要前两位>/<完整摘要>/<安全文件名>`。会话记录保存引用及描述字段。图片请求尺寸变体放在独立的可重建缓存。上游保留对象以支持会话回放和派生会话。 [上游附件后端](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/packages/attachment/attachment-local/README.md) · [上游文件布局](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/packages/attachment/attachment-local/src/file-store.ts)

改造前，Go 图片对象使用 `.harness` 下的散列目录，普通文件在各 Session 工作区的 `.agent/attachments/v1`。改造后，两类对象共用 `$DSH_HOME/attachments/v1`。前端通过会话范围的附件下载接口读取已在该会话日志中出现的引用。 [Go 图片对象存储](../harness/attachments/store.go) · [Go 普通附件存储](../harness/attachments/file_store.go) · [Go 图片规范化](../harness/attachments/normalize.go)

1. **对象位置。** 图片及普通上传文件都改由 Host 在 `$DSH_HOME/attachments/v1` 按上游相对路径管理。先复用已有的原子发布、读回校验和散列引用，确认对象持久化后才提交引用它的会话事件。Go 生成或交付给用户的普通工作区文件继续用 Session workspace。模型和前端解析附件时先从所属会话取得引用，后端再由摘要派生对象路径并验证内容。模型侧需要可写文件时从对象复制到 workspace。
2. **处理策略。** 用固定上游样本分别验证图片的输入大小、像素数、EXIF 方向、透明通道、格式和压缩候选顺序。把上传时的持久化规范化与模型路由的临时尺寸变体分开。持久对象及既有摘要保持字节稳定，缓存失效只触发重新生成。Go Host 保留干净且符合预算的源字节，重编码时使用透明图 WebP 和不透明图 JPEG 的 85、75、60 质量阶梯。模型请求变体按路由的尺寸与字节预算生成并缓存在 Host 进程内。Go 与上游 Sharp 的缩放和 JPEG 编码实现各自独立，重编码后的字节可不同。
3. **恢复验证。** 覆盖重复上传去重、同内容不同文件名、对象缺失或损坏、无权访问其他会话的对象，以及容器重建后的模型回放和前端下载。首版保留所有已写入对象，避免破坏未加载会话和子会话的引用。

### NAS 布局和实施边界

```text
<nasPath>/.dsh/
  sessions/<project-key>/<session-id>/session.v4.jsonl
  storages/schedule.json
  attachments/v1/objects/<前缀>/<摘要>
  attachments/v1/file-objects/<前缀>/<摘要>
  attachments/v1/files/<前缀>/<摘要>/<文件名>
```

`<nasPath>` 在 ECI 中挂到 `/root/harness`。部署配置继续使用现有的 NAS 开关和挂载目标。容器层和 Docker volume/bind 使用相同目录结构。NAS 采用一份数据根由一个活跃 Host 写入的部署方式。本地 Docker bind 验收覆盖容器重建后恢复逾期任务、重新读取图片和普通附件、拒绝从其他会话访问对象。fake ECI 已验证替换实例时沿用原 NAS 子目录。真实 ECI 重挂受本地预检所需的密钥和镜像缓存 ID 缺失限制，未运行。上一次构建留在旧目录里的数据需先按目标布局离线导入。 [ECI 请求装配](../thirdparty/aliyun/eci.go) · [现有文件系统能力检查](../internal/executor/verify_linux.go)


## 原 Skills 实现与上游约定

改造前，镜像构建输入包括项目自带的 `office` bundle 和 `catalog.json`。现行镜像通过官方 OfficeCLI 安装器生成技能目录。 [Dockerfile](../build/Dockerfile.host)

| 环节 | 当前 Go Host | 固定版本 DeepSeek Harness |
| --- | --- | --- |
| 发现 | 镜像内 `/root/.agent/skills/catalog.json` 登记名称、简介、bundle 路径和模式。打开 registry 时确定静态授权集合 | filesystem provider 根据有效 CWD 和用户 home 扫描技能文件，registry 按 scope、rank 和稳定顺序选出同名胜者 |
| 文件格式 | 只按登记路径打开 `bundles/<bundle>/<path>/SKILL.md`。简介来自 JSON，当前读取正文时直接使用文件内容 | 允许根目录下的 `<name>/SKILL.md` 和 `<name>.md`，从 YAML frontmatter 取得身份、简介和调用权限，向模型提供去除 frontmatter 并修剪空白的正文 |
| 工具和用户调用 | `loadskill` 查找静态授权，返回文本；`/name` 共用正文模板和同一授权检查 | `skill` 返回标准值并渲染文本；模型和用户调用各自检查独立权限，正文使用同一渲染器 |
| 动态变更 | 重新读取已授权技能，新增文件不会自动进入已打开的 registry；目录不完整时保留上一份可见目录 | 发现和正文分开，安装、删除和改名触发后续步骤更新，调用时重读正文；目录不完整时发布已确认的子集并标明不完整 |
| 会话和镜像 | 镜像 catalog 决定生产加载，另有 v4 `skill-catalog` 提示 | 安装状态来自 provider，v4 只记录每个 Session 看到的目录和已加载内容 |

现有 Go 的 v4 投影、按需正文加载、目录句柄约束和普通文件检查可复用。实现切换后直接删除生产 `catalog.json`、静态授权、镜像旧目录以及只服务该布局的导入辅助代码。新项目直接使用 `skill`。 [Go registry](../harness/skills/registry.go) · [Go 加载工具](../harness/tools/skill.go) · [Go 步骤目录](../harness/agent/native_skill_catalog.go) · [上游 skills 子系统](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/docs/subsystems/skills.md)

DeepSeek Harness 的用户配置目录默认是 `~/.dsh`，通过 `DSH_HOME` 可指定位置。它另外扫描 `~/.agents`，是为了复用其他 agent 的共享技能，`DSH_AGENTS_HOME` 可以配置这一路径。Go Host 保留这两个原生用户根。上游在扫描时读取存在的目录，支持这两个默认路径并不意味着每次启动都自动创建它们。它从以下位置按优先级发现同名 skill，数字越小优先级越高。项目根取当前 CWD 最近的含 `.git` 的祖先，没有项目标记时取当前 CWD。 [上游 home 路径](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/packages/util/home-paths/src/index.ts) · [上游 filesystem provider](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/packages/skill/skill-filesystem/README.md)

| 优先级 | 来源 | 路径 |
| --- | --- | --- |
| 100 | 项目 DSH | `<project-root>/.dsh/skills` |
| 200 | 项目共享 | `<project-root>/.agents/skills` |
| 300 | 自定义 | 上游提供 `customSkillDirs` 扩展 |
| 400 | 用户 DSH | `$DSH_HOME/skills`，跳过 `.system` 子目录 |
| 500 | 用户共享 | `$DSH_AGENTS_HOME/skills` |
| 600 | 随包技能 | `bundledSkillDir` 或 `DSH_BUNDLED_SKILL_DIR` |

上游 filesystem provider 直接发现根目录下的 `<name>/SKILL.md` 或 `<name>.md`。它从 YAML frontmatter 读取名称、简介和调用开关，registry 归并并按名称提供结果。`skill(name)` 与用户显式 `/name` 读取调用时的最新正文。新增、改名、删除或修改简介会影响下一模型步骤看到的目录。这个目录是可以写入 v4 会话的快照，安装技能的来源是磁盘文件，旧 Go 实现曾把静态 `catalog.json` 用作安装授权。历史会话里已加载的正文继续留在历史中。 [上游加载与目录刷新](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/packages/skill/tool-skill/README.md)

## 拟议的统一 Host 布局

Docker 和 ECI 保留 DeepSeek Harness 的两个用户级技能根。镜像设置 `HOME=/root`、`DSH_HOME=/root/harness/.dsh` 和 `DSH_AGENTS_HOME=/root/harness/.agents`，进程仍从 `/root/harness` 启动。两个根走同一套安装、发现和加载规则。两个用户根同名时 `.dsh/skills` 优先。项目级技能可由当前 Session 的有效 CWD 发现，优先级高于用户根。

```text
/root                                            HOME
/root/harness                                    容器数据根和进程初始工作目录
├── .dsh/                                        DSH_HOME
│   ├── skills/                                  Harness 用户技能根
│   │   ├── officecli/SKILL.md                    OfficeCLI 入口技能
│   │   ├── officecli-docx/SKILL.md               独立的 Word 技能
│   │   ├── morph-ppt/reference/                  Morph 技能携带的参考文件
│   │   ├── agent-browser/SKILL.md               浏览器技能
│   │   └── my-skill/SKILL.md                     后续安装的 skill
│   ├── sessions/<project-key>/<session-id>/    持久化 v4 会话事件日志
│   ├── storages/schedule.json                   定时任务及投递回执
│   ├── attachments/v1/                          图片与普通上传附件
│   └── .skills-seeded                           该数据根的一次性初始化标记
├── .agents/skills/                              DSH_AGENTS_HOME 下的共享技能根
└── workspaces/<session-id>/                     会话初始 CWD
    ├── .dsh/skills/                             该 workspace 的项目技能，可按需创建
    └── .agents/skills/                          该 workspace 的共享项目技能，可按需创建

/usr/share/go-harness/skill-seed/<bundle>/        镜像中的安装源
```

### 安装与 NAS 持久化

1. **打包。** 使用 SHA256 校验通过的 OfficeCLI v1.0.160 发布二进制。官方默认安装生成 `officecli/` 入口；用官方专项安装命令向隔离的 Codex 目录输出其余 10 个 bundle。构建时断网执行这些命令，保留原有目录名及引用的文档、脚本和模板。用官方 Skills CLI 获取 `agent-browser` 的完整上游目录，将它与 OfficeCLI 的目录一起放入镜像的 `/usr/share/go-harness/skill-seed`。 [官方安装命令](https://github.com/iOfficeAI/OfficeCLI/wiki/command-skills) · [对应版本安装器源码](https://github.com/iOfficeAI/OfficeCLI/blob/v1.0.160/src/officecli/Core/SkillInstaller.cs)
2. **首次启动。** Host 在验证数据目录后建立两个用户技能目录。对于镜像中的每个 bundle，若任一用户根已经有有效同名技能，则保留已有版本；否则把完整目录发布到 `$DSH_HOME/skills/<bundle>/`。目录复制成功后才发布各自入口，全部完成后记录一次性标记。中断后启动可补齐剩余 bundle。用户随后编辑、升级或删除技能，重启和更换镜像会保留这些操作。已有 `.officecli-seeded` 标记的数据根只补装缺少的 `agent-browser`，保留此前对 OfficeCLI 的修改和删除。
3. **使用中**，用户可把完整 bundle 或平铺 Markdown 安装到 `$DSH_HOME/skills` 或 `$DSH_AGENTS_HOME/skills`。预置技能安装后也按同样规则处理。两个用户目录中的技能可被该 Host 的所有会话发现。也可以安装到当前项目的两个目录，技能只在对应项目可见。各来源都以技能文件和 frontmatter 提供运行时数据，镜像安装源只供首次安装。
4. **ECI 挂 NAS**时，`/root/harness/.dsh/skills/my-skill/SKILL.md` 对应远端 `/go-harness/<userId>/.dsh/skills/my-skill/SKILL.md`。共享技能使用同一映射，`/root/harness/.agents/skills` 对应远端 `/go-harness/<userId>/.agents/skills`。重建实例时挂回原 NAS 子目录，两个目录及初始化标记一起恢复。本地 Docker 使用账号数据卷；ECI 不挂 NAS 时，数据随实例生命周期变化。 [ECI 挂载与路径映射](deployment.md#eci-image-cache)

### OfficeCLI 在 Harness 中的目录

下列路径均相对于 `/root/harness/.dsh/skills/`。每个目录有独立的 `SKILL.md`，按普通 Harness 技能发现和加载。可使用 `skill(name)` 或直接输入 `/<name>`。专项目录同时保留官方安装输出的资源。

| 用途 | 目录 |
| --- | --- |
| OfficeCLI 入口 | `officecli/` |
| Word、Excel、PowerPoint | `officecli-docx/`、`officecli-xlsx/`、`officecli-pptx/` |
| 学术论文、表单 | `officecli-academic-paper/`、`officecli-word-form/` |
| 数据仪表盘、财务模型 | `officecli-data-dashboard/`、`officecli-financial-model/` |
| 融资路演、二维 Morph、三维 Morph | `officecli-pitch-deck/`、`morph-ppt/`、`morph-ppt-3d/` |

### 发现、更新与生命周期

1. **解析本次查找范围。** Host 启动时装配一个文件系统 provider；Session 在模型步骤、`skill` 调用或 `/name` 时传入有效 CWD、会话范围及取消信号。沿 CWD 向上查找最近的 `.git`，包含 Git worktree 的 `.git` 文件，无标记时使用有效 CWD。Go Host 将项目查找限制在当前 Session 所属 workspace 内，防止在其他会话或整个 NAS 根上获得项目级技能。无可用项目根时仍可扫描两个用户根。
2. **扫描、验证并归并。** 扫描两个项目根和两个用户根的顶层，支持 bundle 与平铺文件，忽略用户 DSH 根的 `.system`。用 frontmatter 的 `name` 定身份，验证小写 kebab-case、非空字符串简介和两个调用开关；按上游接受的布尔写法解析，旧驼峰字段和非法值跳过并记录原因。保留可选 `whenToUse`、对象型 `metadata` 与资源目录。按上游 rank 选同名胜者，同根使用确定顺序，结果按名称排序。选出胜者之后才筛调用权限，所以高优先级的禁用定义仍会遮住同名低优先级定义。
3. **跟踪变更。** 源文件新增、修改、改名、删除和目录出现都在下一次模型步骤可见。首版每个步骤做有界扫描，避免 Docker bind、NAS 外部写入或根目录重建时漏掉 watcher 事件。正文变动且简介不变时，目录不重复发布，下一次加载读到新正文。资源文件修改无需重发目录，引用时读取当前版本。文件读取采用根目录句柄、非阻塞打开和普通文件检查；仅跟随解析目标仍在相应受信任根内的链接。
4. **区分缺失和失败。** 可确认不存在的目录视为空；无效 frontmatter 或缺失入口跳过；权限、存储 I/O 或目录扫描错误标记不完整并保留其他根的可用结果。高优先级根失败时，低优先级同名项可临时生效。恢复后下一步骤重新选择。上游使用 watcher 与失效通知优化扫描，Go 首版保留相同可观察行为。 [上游 provider 与失败规则](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/docs/subsystems/skills.md)

Go 的目录映射只负责选择文件系统根。结构保留 provider 和 scope 选择能力，上游可配置的自定义根与额外打包 provider 作为扩展点。预装技能初始化后位于用户根，按普通文件技能参与 rank 比较，镜像安装源无需再注册为运行时 provider。

### `skill` 与模型可见契约

| 环节 | 实施 |
| --- | --- |
| 声明与参数 | 只注册 `skill`，必填字符串 `name`。采用上游 schema、说明和名称语法。替换工具声明、系统提示和目录提示 |
| 模型调用 | 使用本步骤的会话范围查找候选，读取正文时重新校验当前名称、定义和 `disable-model-invocation`。对齐非法名称、未知或移除、禁止模型调用，以及参数 schema 错误的结果文本和错误形状 |
| 标准值与模型文本 | 标准值为 `name`、`provider`、`content` 和可选 `resourceBase`。本地文件使用 `{ kind: "directory", path }`，bundle 以自身目录为基准，平铺文件以扫描根为基准。渲染上游的 `<skill_content>` 块，分开处理名称属性和路径转义，正文按解析结果保留 |
| 用户调用 | 只识别本步骤收到的真实用户文本，扫描空白分隔的 `/name`，按首次出现去重。未知名称和 `user-invocable: false` 留在原消息中。允许用户显式调用只对模型禁用的技能。正文使用与工具相同的渲染，写入 `skill-invocation` 用户消息 |
| 可见性 | 会话目录跟随当前可调用的具体 `skill` 工具定义。受限步骤隐藏该工具时，目录同步退役，防止模型继续引用之前可见的技能 |

### v4 会话中的目录

每个模型步骤按上游规则把可由模型调用的技能按名称排序，简介规范化空白并限为 500 个 UTF-16 单元。目录只含名称和简介。通过可见事件的条目与发现完整性判断是否发布，模型可见文本与 v4 源字段一并对齐。

| 本次观察 | 会话行为 |
| --- | --- |
| 首次完整且为空 | 省略目录消息 |
| 首次有技能或扫描不完整 | 持久化首次目录。发现不完整时附已确认条目及 `incomplete: true` |
| 条目、可见性或完整性变化 | 持久化替换目录。全部移除时发空替换，读取恢复时清除不完整提示 |
| 与当前可见目录一致 | 复用已有可见结果，避免重复事件 |
| 已有会话的目录因压缩离开模型视野 | 下一步按当前观察重新发布 |

静态 `catalog.json` 在扫描器、OfficeCLI 初始化和工具一起切换后从镜像及生产代码删除。新建 Session 的 `skill-catalog`、`skill-invocation` 和工具结果继续持久化，用于进程重启或 ECI 重建后的回放。 [上游工具与目录逻辑](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/packages/skill/tool-skill/src/index.ts) · [上游正文渲染](https://github.com/deepseek-ai/deepseek-harness/blob/fcc0ad462172522aaf5c93a7b2e01f6b9f0b0159/packages/skill/skill/src/index.ts)

### 实施顺序与验收

1. **本机入口。** 加入回环校验，随后移除公开 token 和 Origin 白名单，切换 CLI、示例和 API 文档。验证新旧 `.env`、无 token HTTP、不同 origin 的预检及 SSE、非回环启动失败，以及 Host 直连接口仍要求内部 token。
2. **路径与镜像。** 保留容器挂载点、NAS 路径、数据身份、Session 绝对 CWD 和日志位置。实现两个用户根的初始化与 OfficeCLI 一次性安装。验证首次安装、同名文件、中断后重试、用户编辑或删除后重启和镜像更新。
3. **目录与工具。** 在 `harness/skills` 实现新文件系统 provider 和归并，在 `harness/tools` 实现 `skill` 标准值和正文渲染，通过 `harness/assembly` 与 `harness/agent` 接入有效 CWD、目录更新和显式调用。切换后删除静态 catalog、旧工具声明和镜像旧目录校验。用上游固定提交生成用例，比较结构化值、模型文本、错误、首发和替换事件。
4. **动态验收。** 用两个 Session 验证项目隔离与用户技能共享，覆盖项目与用户同名、两个用户目录同名、目录重命名、技能删除、正文单独修改、权限切换、不可读根及恢复。验证一个 bundle 和一个平铺技能能通过 `skill` 与 `/name` 加载，并在无 `catalog.json` 的环境运行。对新生成的 journal 运行上游严格 v4 解码并验证冷恢复。
5. **运行矩阵。** 本地 Docker 跑容器层、卷和 bind 三种方式，检查 Shell 的 `HOME`、`DSH_HOME`、`DSH_AGENTS_HOME`，OfficeCLI 能在会话 workspace 生成文件。fake ECI 验证挂载和无 NAS 请求；真实 ECI 验证在两个用户根动态安装技能后重建，挂回原子目录，新会话可发现原技能。真实云验收单独记录。

当前验证记录见 [validation.md](validation.md)。
