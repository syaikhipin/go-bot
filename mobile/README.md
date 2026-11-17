# go-bot 移动端

Expo SDK 58 / React Native 项目，运行在 iOS 和 Android 上。用同一个账号连接 `go-bot` Controller，即可打开会话、查看 Host 文件和定时任务，并配置模型、网页搜索和图片生成。

## 本地运行

准备 Node.js 22.13 或更新的兼容版本，以及 iOS 的 Xcode 或 Android 的 Android Studio 和 SDK。先按仓库根目录的部署说明启动 Controller，再在仓库根目录执行以下命令。

```sh
cd mobile
npm ci
npm run ios
# 或者
npm run android
```

开发时可运行 `npm start` 重新启动 Metro。Expo 生成的原生工程由 `app.json` 配置。使用实体设备时，连接或启动设备后运行 `npm run ios -- --device` 或 `npm run android -- --device`。iOS 实体设备需要在 Xcode 中配置签名。

### 重新生成工程和打包

在 `mobile/` 运行 `npx expo prebuild --clean`，从 Expo 配置重新生成 Android 和 iOS 工程。两端的应用 ID 都是 `com.gobot.mobile`。原生配置应写入 `app.json`，这样重新生成时会保留 Android Release 的 R8 设置和连接配置。

在 `android/` 运行 `./gradlew :app:assembleRelease -PreactNativeArchitectures=arm64-v8a` 可生成 ARM64 Release APK。发布时用 Android SDK 的 `zipalign -P 16` 对齐、再用 `apksigner` 签名，最后验证签名和对齐。发布密钥保存在仓库外。

在连接页填写 Controller 入口地址和账号名。可以连接已有账号，也可以在同一页面创建账号。新账号在「我的 → 模型」设置端点和默认模型后即可发送第一条消息。iOS 模拟器可直接填同机的 `http://127.0.0.1:8080`。

## 手机连接

通过 Tailscale 连接时，从 [Tailscale 官方下载页](https://tailscale.com/download) 给电脑和手机安装客户端。两端登录同一个 Tailscale 账号，确认设备在线后，在电脑上启动 Serve，再将输出的完整 `http://…ts.net:8081` 地址填入 App。 [图文步骤](../README.zh-CN.md#手机通过-tailscale-连接)

如果已有受访问控制的 HTTPS 入口，在 App 中填入该入口地址即可。 [代理配置](../docs/configuration.md#runtime-storage-and-entry)

## 使用

- **会话**位于导航中间。App 重开时按账号恢复上次会话，找不到时打开最近创建的可用会话。没有会话时，点击页面中央的「创建新会话」。左上角可新建、切换与归档会话，右上角可逐层浏览 Host 的 `/root/harness`。输入框可切换端点与模型。附件选择后先加入草稿，点发送后随消息上传。
- **任务**可创建、编辑、暂停、恢复或立即运行 Host 上的定时任务，查看下次运行和最近一次投递时间，并打开对应会话查看执行结果。
- **我的**可配置模型、搜索和生图供应商。点击头像可从图库选择或拍照，头像按账号名保存在设备上。语言可在此切换简体中文和 English，重启后保留。系统原生选择器使用系统语言。点击 `Stop Host` 调用 Controller 停止当前账号的 Host，完成后返回连接页。

从系统导入的图片和文件会保存在 App 的本地库。会话中的交付文件要点下载才会存入本地文件区。聊天框中点「＋」进入「图片」或「文件」，这两个本地库在同一 App 安装内共用，切换会话或账号仍可访问。多选后可添加到草稿、通过系统分享面板发送到其他 App，或从本地库删除。会话图片可点开全屏预览，长按「保存图片」同时存入 App 和手机图库。从 App 本地图片库删除时只删除 App 内的副本。

来自同一账号和同一个 Host 路径的交付文件共用一个本地条目。再次下载会刷新副本。同名文件如果来自不同路径，会各自保存原文件名，并在文件列表里显示保存时间。

## 开发检查

```sh
npm run typecheck
npx expo install --check
npx expo export --platform ios --platform android
```

移动端与 Web 共用会话事件投影，并复用 [Controller HTTP API](../docs/api.md)。定时任务的状态、规则和历史由 [Host Schedule](../docs/schedule-parity-plan.zh-CN.md) 管理。设计参考与原型见 [设计说明](design/README.md)。
