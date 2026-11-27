# 服务器 API

所有接口都需要 `Authorization: Bearer <令牌>`。令牌有两种：

- 管理令牌：`crypta token` 查看，给进度网页和插件用。同一台机器上的插件可以直接读数据卷里的 `/data/admin-token`。
- 设备令牌：手机配对时生成，`crypta revoke <id>` 可以单独吊销。

## 下载插件怎么接入

插件和核心共享一个下载目录（容器里是 `/downloads`）。一个插件要做的只有两步：

1. 把下载好的文件放到 `/downloads` 下面；
2. 告诉核心去加密上传：

```http
POST /api/transfers
{"kind": "upload", "local": "telegram/123/video.mp4", "remote": "Telegram", "deleteSource": true}
```

- `local`：相对 `/downloads` 的路径（只接受这个目录里的文件）
- `remote`：网盘里的目标文件夹，不存在会自动创建
- `deleteSource`：上传成功后删除本地文件

如果只是一个链接，交给核心自己下载：

```http
POST /api/transfers
{"kind": "url", "url": "https://example.com/file.zip", "remote": "Downloads"}
```

返回值里的 `id` 用来查进度。`plugins/telegram/bot.py` 是一个完整的例子。

## 任务

| 接口 | 说明 |
| --- | --- |
| `GET /api/transfers` | 全部任务：`state`（queued / running / paused / done / failed / canceled）、`phase`（downloading / uploading）、`size`、`done`、`speed`、`error` |
| `POST /api/transfers/{id}/pause` `resume` `cancel` `remove` | 控制任务；`resume` 也用于重试失败的任务 |
| `POST /api/transfers/clear` | 清除已结束的任务 |

进度网页（`GET /`）只是这几个接口的一个界面，以后做看板可以直接读 `GET /api/transfers`。

## 文件

| 接口 | 说明 |
| --- | --- |
| `GET /api/list?path=a/b` | 列出文件夹（解密后的名字和大小） |
| `POST /api/mkdir` `{"path"}` | 新建文件夹 |
| `POST /api/move` `{"from","to"}` | 重命名或移动 |
| `POST /api/delete` `{"path"}` | 移到回收站 |
| `GET /api/file?path=...` | 读取解密后的内容，支持 `Range` |

## 配对

`POST /api/pair` `{"code","name"}` 是唯一不需要令牌的接口：用 `crypta pair` 生成的一次性配对码（10 分钟有效，输错 10 次全部作废）换取设备令牌和打开加密文件夹所需的配置。
