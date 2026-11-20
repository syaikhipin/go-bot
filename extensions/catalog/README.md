# Reviewed plugin catalog / 经审核的插件清单

plugins.json is compiled into Controller. Clients choose pluginId, never a user-supplied package address. The first-party agent-browser and OfficeCLI wrappers ship as bundled sources in the Host image; users install and enable them through the same market.

plugins.json 随 Controller 构建内置。用户选择 pluginId，不提交包地址。agent-browser 和 OfficeCLI 包装包随 Host 镜像分发，通过统一市场安装、启用。

First-party plugin.json files supply original name, bilingual description, icon, version, maintainer and license. The catalog selects the package/release and installation policy; Controller assembles display metadata at build time. Sources are reviewed bundled packages, exact npm versions, full Git commits or fixed HTTPS release archives. No floating branches, latest tags, placeholder URLs, development directories or credential values.

展示字段来自各包 plugin.json：原始名称、双语 description、icon、version、maintainer、license；清单只记录包身份、版本选择、来源和脚本放行策略。来源使用随 Host 分发的 bundled 包、固定 npm 版本、完整 Git commit 或固定 HTTPS 发布归档，不能使用占位地址、开发目录或凭证。

Redis stores only user catalogId/version/enabled selections. Maintain selected releases for the product versions that support them; updates require a user action. Package source resolution is Host-private. goBot.api must equal "1". Standard npm lifecycle scripts, approved through pnpm allowBuilds, prepare dependencies during installation, never during inspection or Cordis activation. See ../sdk/README.md.

Redis 只存用户的 catalogId/version/enabled。维护受支持的已选版本，更新由用户主动执行。安装时由 pnpm 调用包声明并经审核放行的标准安装脚本；浏览、检查包与 Cordis apply 不安装软件。
