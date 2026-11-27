# Google Drive 授权

Crypta 用你 `rclone.conf` 里 Drive 远程的 `client_id`、`client_secret` 和 `token`，自己不再走一遍登录。所以只要 rclone 能用，Crypta 就能用。

## 申请自己的 client id（一次性，约 10 分钟）

1. 打开 [Google Cloud Console](https://console.cloud.google.com/)，新建一个项目。
2. “API 和服务 > 库”里启用 **Google Drive API**。
3. “API 和服务 > OAuth 权限请求页面”：用户类型选“外部”，填应用名和邮箱；把自己的 Google 账号加到“测试用户”。
4. **把发布状态改成“正式版”（In production）**。留在“测试”状态的话，登录令牌每 7 天失效一次，需要重新授权。自用不需要提交审核，登录时会看到“Google 尚未验证此应用”，点“高级 > 继续”即可。
5. “凭据 > 创建凭据 > OAuth 客户端 ID”，类型选“桌面应用”，得到 client id 和 client secret。
6. 电脑上运行 `rclone config`，新建 `drive` 类型的远程，填入上面的 client id 和 secret，scope 选 `drive`（完整权限），按提示在浏览器里登录。

## 权限范围

- `drive`（推荐）：能看到整个网盘，已有的 rclone 加密文件夹可以直接用。
- `drive.file`：只能看到这个 client id 自己创建的文件。更保守，但读不到用别的 client id 上传的旧数据。

## 限制

- 每个账号每天上传上限约 750 GB（Google 的限制）。
- 未验证的应用最多 100 个测试用户，自用没有影响。
- 令牌失效时 App 和服务器会提示“Google sign-in was rejected”，在电脑上运行 `rclone config reconnect gdrive:`，再把新的 rclone.conf 给服务器并重新配对（或在手机上重新导入）。
