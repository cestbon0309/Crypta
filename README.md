# Crypta

自用的加密网盘：文件用 **rclone crypt 原版格式**加密后存进 Google Drive，手机上是一个简洁的原生 iOS App，服务器负责离线下载。和电脑上的 rclone 完全互通——同一个加密文件夹，两边都能读写。

```
ios/                 SwiftUI App（浏览、预览、上传、下载、配对）
core/                Go 核心：rclone crypt 格式、Google Drive、断点续传队列、HTTP API
  cmd/crypta         服务器程序（serve / pair / devices / revoke / token）
  mobile             打包进 iOS App 的同一份核心（gomobile）
plugins/telegram     Telegram 机器人插件（Python）
docker-compose.yml   服务器部署
docs/                Google 授权、服务器 API（写插件用）
```

## 现在能做什么

- 浏览、新建文件夹、重命名、移动、删除（删除是移到 Drive 回收站）
- 上传照片、视频和任意文件；下载到本机（“文件”App 里的 Crypta 文件夹）
- 预览图片、文本，视频和音频用 VLCKit 播放（MKV、MP4、RMVB 等），拖动进度条时只取对应的加密块
- 断点续传：上传用 Drive 的续传会话，下载记录位置；App 被杀或服务器重启后从中断处继续
- 服务器：直链离线下载、从 PikPak 网盘拉取、Telegram 机器人收文件，下载完直接加密上传；进度网页；扫码或配对码配对手机

暂时没有：安卓、“文件”App 扩展、分享扩展、相册自动备份、多账号、BT 下载。

## 一、准备 rclone 配置

Crypta 直接读你的 `rclone.conf`，需要里面有一个 Google Drive 远程和一个套在它上面的 crypt 远程：

```ini
[gdrive]
type = drive
client_id = 你自己的 client id
client_secret = ...
scope = drive
token = {"access_token":"...","refresh_token":"...","expiry":"..."}

[secret]
type = crypt
remote = gdrive:encrypted
password = ...
password2 = ...
```

- 还没有的话，在电脑上 `rclone config` 建好这两个远程即可。Google 授权怎么申请见 [docs/google-setup.md](docs/google-setup.md)。
- **必须用自己的 client_id**，rclone 自带的共享 client id 不给第三方程序用。
- 支持的 crypt 选项：`filename_encryption = standard | off`、`directory_name_encryption`、`filename_encoding = base32 | base64`、`password2`。不支持 `obfuscate` 和 `base32768`。
- 如果 rclone.conf 设了配置密码，先用 `rclone config show` 导出明文版本。

### 大文件分片

超过 256 MB 的文件按 **rclone chunker 原版格式**存放（`名字.rclone_chunk.001`、`.002`…加一个记录大小和 MD5 的小文件），每片各自加密，几片同时上传（默认 8 片），用来绕开 Google Drive 对单个文件的上传限速。电脑上的 rclone 要读这些文件，在 rclone.conf 里再加一个套在 crypt 远程上面的 chunker 远程，并通过它访问：

```ini
[crypta-chunked]
type = chunker
remote = crypta:
chunk_size = 256M
```

- Crypta 读到这一段时用它的 `chunk_size`，没有这一段时按 256 MB；只支持 chunker 的默认命名和元数据格式。
- 直接用 crypt 远程访问也行，只是大文件会显示成一堆分片。
- 服务器的并行片数用 `.env` 里的 `CRYPTA_UPLOAD_PARALLEL` 调（1 到 16），手机在“设置”里选。
- 手机下载到本机时，一个文件分成几段同时下载（默认 4 段，1 到 16，在“设置”里选），中断后从已完成的部分继续。

## 二、服务器

一行安装（Linux 服务器；没有 Docker 会先装 Docker，然后拉代码、问几个问题生成 `.env`、启动并显示配对码）：

```bash
curl -fsSL https://raw.githubusercontent.com/cestbon0309/Crypta/main/scripts/install.sh | bash
```

仓库是私有的时候要带一个能读这个仓库的 GitHub 令牌：

```bash
export GITHUB_TOKEN=ghp_xxx
curl -fsSL -H "Authorization: Bearer $GITHUB_TOKEN" https://raw.githubusercontent.com/cestbon0309/Crypta/main/scripts/install.sh | bash
```

- 默认装到 `~/crypta`；再跑一次就是更新，已有的 `.env` 不会被改。
- 不想被提问可以提前给变量：`CRYPTA_DOMAIN`、`RCLONE_CONF`、`TELEGRAM_BOT_TOKEN`、`TELEGRAM_ALLOWED_USERS`、`PIKPAK_USERNAME`、`PIKPAK_PASSWORD`，再加 `CRYPTA_YES=1`。
- 不填 rclone.conf 时是演示模式。不填域名时没有 HTTPS：演示模式下 App 可以在局域网内配对；接了 Google Drive 时服务只监听本机，要填域名（或自己在前面放 HTTPS 代理）手机才能连。

手动安装：

```bash
cp .env.example .env        # 填 CRYPTA_PUBLIC_URL、CRYPTA_DOMAIN
cp ~/.config/rclone/rclone.conf ./rclone.conf
docker compose --profile https up -d          # 核心 + Caddy 自动 HTTPS
docker compose exec core crypta pair          # 显示二维码和配对码
```

- 想先不接 Google Drive 试一下：`.env` 里设 `CRYPTA_DEMO=1`，加密文件会存在数据卷里。
- 进度网页就是服务器地址的首页，令牌用 `docker compose exec core crypta token` 查看。
- 已有反向代理的话不用 `--profile https`，把代理指到 `127.0.0.1:8080`。
- PikPak：`.env` 填 `PIKPAK_USERNAME` 和 `PIKPAK_PASSWORD`（密码用单引号括起来）后重启，离线下载的链接一栏就可以填 `pikpak:/网盘路径`，文件或文件夹都行；服务器先把它下载到本机再加密上传，所以磁盘要放得下。
- Telegram 机器人：`.env` 填 `TELEGRAM_BOT_TOKEN` 和 `TELEGRAM_ALLOWED_USERS`（给机器人发 `/start` 能看到自己的数字 ID），然后 `docker compose --profile telegram up -d`。机器人收到文件后回一条进度消息，下载和加密上传期间每秒更新一次；同一个任务也出现在手机 App「传输 → 下载」和进度网页里，上传完成后服务器上的文件自动删除。官方 Bot API 只能下载 20 MB 以内的文件；要到 2 GB，在 [my.telegram.org](https://my.telegram.org) 申请 api id 和 hash，`.env` 填 `TELEGRAM_API_ID`、`TELEGRAM_API_HASH`、`TELEGRAM_API_BASE=http://telegram-bot-api:8081`、`TELEGRAM_LOCAL_MODE=1`，启动时再加 `--profile telegram-bot-api`。不能转发给机器人的消息可以发消息链接（`https://t.me/频道/123`、`https://t.me/c/…/123`）：先在机器人里 `/login`，按提示发手机号、验证码（数字之间加空格，原样发出的验证码会被 Telegram 作废）和两步验证密码，机器人就用你自己的账号取出消息里的文件，相册取整组；`/logout` 退出。这需要 `.env` 里有 `TELEGRAM_API_ID` 和 `TELEGRAM_API_HASH`，登录信息存在 `telegram-sessions` 卷里，登录的账号能读到你能读到的所有聊天。
- 管理设备：`crypta devices`、`crypta revoke <id>`。

服务器保存着 Drive 授权和加密密码（它要替你加密上传），配对时会通过 HTTPS 把这份配置交给手机，所以请务必用 HTTPS，并保管好服务器。

想把自己的下载程序接进来（比如已有的 Telegram 机器人、以后的 qBittorrent），见 [docs/server-api.md](docs/server-api.md)：把文件放进共享的下载目录，再调一次接口就行。

## 三、iOS App

构建需要 macOS、Xcode、Go 和 xcodegen：

```bash
./scripts/build-ios.sh          # 生成 build/Crypta.ipa（未签名）
```

把 `Crypta.ipa` 用 SideStore 或 LiveContainer 安装即可（免费 Apple ID 签名 7 天一续）。打开后三选一：

1. **与服务器配对**：扫 `crypta pair` 的二维码或输入配对码，手机直接拿到配置；
2. **导入 rclone.conf**：不需要服务器，手机直接连 Google Drive；
3. **演示模式**：不连 Google Drive，先看看界面。

说明：
- 手机上的预览、上传、下载都是手机直接和 Google Drive 通信，在手机上加解密，不经过服务器。
- iOS 会在 App 退到后台后不久暂停它。正在进行的传输会停下来，回到 App 时自动从中断处继续，不会重传。

## 开发

```bash
cd core && go test ./...                       # 需要 PATH 里有 rclone 才会跑互通测试
RCLONE_BIN=/path/to/rclone go test ./...
```

互通测试用真的 rclone 程序和 Crypta 读写同一个目录：rclone 写的 Crypta 能读（含随机位置读取），Crypta 写的 rclone 能读并通过 `rclone cryptcheck`。

## 许可

视频播放使用 [VLCKit](https://code.videolan.org/videolan/VLCKit)（LGPL 2.1），构建时从 VideoLAN 下载预编译版本，不包含在仓库里。
