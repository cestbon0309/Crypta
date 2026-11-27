"""Crypta Telegram plugin.

Send the bot a file, photo, video or audio and it saves it into the shared
downloads folder, then asks the Crypta core to encrypt and upload it. Send a
link and the core downloads it itself. The bot only talks to the core through
its HTTP API, which is all any other download plugin has to do as well.
"""

import asyncio
import logging
import os
import re
import time
from pathlib import Path

import httpx
from telegram import Message, Update
from telegram.ext import Application, CommandHandler, ContextTypes, MessageHandler, filters

CORE_URL = os.environ.get("CRYPTA_CORE_URL", "http://core:8080").rstrip("/")
TOKEN_FILE = os.environ.get("CRYPTA_ADMIN_TOKEN_FILE", "/data/admin-token")
DOWNLOADS = Path(os.environ.get("CRYPTA_DOWNLOADS", "/downloads"))
REMOTE_DIR = os.environ.get("TELEGRAM_REMOTE_DIR", "Telegram")
ALLOWED = {int(x) for x in re.split(r"[,\s]+", os.environ.get("TELEGRAM_ALLOWED_USERS", "")) if x}
URL_RE = re.compile(r"https?://\S+")

log = logging.getLogger("crypta-telegram")


def core_token() -> str:
    token = os.environ.get("CRYPTA_ADMIN_TOKEN")
    if token:
        return token
    return Path(TOKEN_FILE).read_text().strip()


async def core(method: str, path: str, body: dict | None = None) -> dict:
    async with httpx.AsyncClient(timeout=30) as client:
        r = await client.request(method, CORE_URL + path, json=body, headers={"Authorization": f"Bearer {core_token()}"})
    data = r.json()
    if r.status_code != 200:
        raise RuntimeError(data.get("error", f"HTTP {r.status_code}"))
    return data


def human(n: float) -> str:
    for unit in ("B", "KB", "MB", "GB"):
        if n < 1024:
            return f"{n:.0f} {unit}" if unit == "B" else f"{n:.1f} {unit}"
        n /= 1024
    return f"{n:.1f} TB"


async def follow(status: Message, job: dict) -> None:
    """Keep the bot's reply up to date until the job ends."""
    last = ""
    phases = {"downloading": "下载到服务器", "uploading": "加密上传"}
    while True:
        await asyncio.sleep(3)
        try:
            jobs = (await core("GET", "/api/transfers"))["transfers"]
        except Exception as e:  # core restarting: try again
            log.warning("core unreachable: %s", e)
            continue
        cur = next((j for j in jobs if j["id"] == job["id"]), None)
        if cur is None:
            return
        name = cur.get("name") or job.get("name") or "文件"
        if cur["state"] == "done":
            text = f"✅ 已加密保存：/{cur['remote']}"
        elif cur["state"] == "failed":
            text = f"❌ {name} 失败：{cur.get('error', '')}"
        elif cur["state"] == "canceled":
            text = f"已取消：{name}"
        else:
            pct = f"{cur['done'] / cur['size'] * 100:.0f}%" if cur.get("size") else human(cur["done"])
            text = f"{phases.get(cur.get('phase'), '排队中')}：{name} {pct}"
        if text != last:
            last = text
            try:
                await status.edit_text(text)
            except Exception:
                pass
        if cur["state"] in ("done", "failed", "canceled"):
            return


def allowed(update: Update) -> bool:
    return bool(update.effective_user) and update.effective_user.id in ALLOWED


async def start(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    uid = update.effective_user.id
    if not allowed(update):
        await update.message.reply_text(f"你的 Telegram 用户 ID 是 {uid}。把它加入 TELEGRAM_ALLOWED_USERS 后重启即可使用。")
        return
    await update.message.reply_text("把文件、照片、视频或下载链接发给我，我会加密后存进 Google Drive。")


async def on_media(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    msg = update.effective_message
    if not allowed(update):
        return
    media = msg.document or msg.video or msg.audio or msg.voice or msg.animation or msg.video_note or (msg.photo[-1] if msg.photo else None)
    if media is None:
        return
    name = getattr(media, "file_name", None)
    if not name:
        ext = {"photo": ".jpg", "video": ".mp4", "voice": ".ogg", "audio": ".mp3", "video_note": ".mp4", "animation": ".mp4"}
        kind = next((k for k in ext if getattr(msg, k, None)), "photo")
        name = f"{kind}-{time.strftime('%Y%m%d-%H%M%S')}-{msg.message_id}{ext[kind]}"
    name = Path(name).name
    status = await msg.reply_text(f"正在接收：{name}")
    target_dir = DOWNLOADS / "telegram" / str(msg.message_id)
    target_dir.mkdir(parents=True, exist_ok=True)
    target = target_dir / name
    try:
        tg_file = await media.get_file()
        await tg_file.download_to_drive(custom_path=target)
        job = await core("POST", "/api/transfers", {
            "kind": "upload", "local": str(target.relative_to(DOWNLOADS)), "remote": REMOTE_DIR, "deleteSource": True,
        })
    except Exception as e:
        log.exception("media failed")
        hint = "（官方 Bot API 只能下载 20 MB 以内的文件；更大的文件需要自建 Bot API 服务器，见 README）" if "too big" in str(e).lower() else ""
        await status.edit_text(f"❌ {name} 失败：{e}{hint}")
        return
    context.application.create_task(follow(status, job))


async def on_text(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    msg = update.effective_message
    if not allowed(update):
        return
    urls = URL_RE.findall(msg.text or "")
    if not urls:
        await msg.reply_text("没有找到链接。发文件或 http(s) 链接给我。")
        return
    for url in urls:
        status = await msg.reply_text(f"已加入离线下载：{url}")
        try:
            job = await core("POST", "/api/transfers", {"kind": "url", "url": url, "remote": REMOTE_DIR})
        except Exception as e:
            await status.edit_text(f"❌ 失败：{e}")
            continue
        context.application.create_task(follow(status, job))


def main() -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(name)s %(message)s")
    logging.getLogger("httpx").setLevel(logging.WARNING)
    builder = Application.builder().token(os.environ["TELEGRAM_BOT_TOKEN"]).concurrent_updates(True)
    # A self-hosted Bot API server lifts the 20 MB download limit.
    if base := os.environ.get("TELEGRAM_API_BASE"):
        base = base.rstrip("/")
        builder = builder.base_url(f"{base}/bot").base_file_url(f"{base}/file/bot").local_mode(os.environ.get("TELEGRAM_LOCAL_MODE") == "1")
    app = builder.build()
    if not ALLOWED:
        log.warning("TELEGRAM_ALLOWED_USERS is empty: the bot will only answer /start with the sender's user id")
    app.add_handler(CommandHandler("start", start))
    app.add_handler(MessageHandler(filters.ATTACHMENT, on_media))
    app.add_handler(MessageHandler(filters.TEXT & ~filters.COMMAND, on_text))
    app.run_polling(allowed_updates=["message"])


if __name__ == "__main__":
    main()
