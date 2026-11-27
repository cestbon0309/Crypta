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
import shutil
import time
from pathlib import Path

import httpx
from telegram import Message, Update
from telegram.error import RetryAfter
from telegram.ext import Application, CommandHandler, ContextTypes, MessageHandler, filters

CORE_URL = os.environ.get("CRYPTA_CORE_URL", "http://core:8080").rstrip("/")
TOKEN_FILE = os.environ.get("CRYPTA_ADMIN_TOKEN_FILE", "/data/admin-token")
DOWNLOADS = Path(os.environ.get("CRYPTA_DOWNLOADS", "/downloads"))
REMOTE_DIR = os.environ.get("TELEGRAM_REMOTE_DIR", "Telegram")
ALLOWED = {int(x) for x in re.split(r"[,\s]+", os.environ.get("TELEGRAM_ALLOWED_USERS", "")) if x}
URL_RE = re.compile(r"https?://\S+")
TOKEN = os.environ.get("TELEGRAM_BOT_TOKEN", "")
LOCAL_MODE = os.environ.get("TELEGRAM_LOCAL_MODE") == "1"
# The data folder of a self-hosted Bot API server, shared with this container.
API_DIR = Path(os.environ.get("TELEGRAM_API_DIR", "/var/lib/telegram-bot-api"))
INTERVAL = 1  # seconds between updates of a progress message
BAR = 10

receiving = 0  # files the Bot API server is fetching for us right now

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


def progress(phase: str, name: str, done: int | None, size: int, speed: int = 0) -> str:
    """The text of the progress message: phase and name, a bar, the numbers."""
    lines = [f"{phase}：{name}"]
    if done is None:
        lines.append(f"共 {human(size)}" if size else "进行中")
    elif size:
        frac = min(done / size, 1)
        filled = int(frac * BAR)
        lines.append(f"{'🟩' * filled}{'⬜' * (BAR - filled)} {frac * 100:.0f}%")
        lines.append(f"{human(done)} / {human(size)}" + (f" · {human(speed)}/s" if speed > 0 else ""))
    else:
        lines.append(human(done) + (f" · {human(speed)}/s" if speed > 0 else ""))
    return "\n".join(lines)


class Status:
    """The bot's one reply to a message, edited in place as the job moves on."""

    def __init__(self, message: Message) -> None:
        self.message, self.text, self.quiet_until = message, message.text, 0.0

    async def show(self, text: str, final: bool = False) -> None:
        if text == self.text:
            return
        if final:  # the last word must arrive even after a flood wait
            await asyncio.sleep(max(self.quiet_until - time.monotonic(), 0))
        elif time.monotonic() < self.quiet_until:
            return
        try:
            await self.message.edit_text(text)
            self.text = text
        except RetryAfter as e:
            wait = e.retry_after.total_seconds() if hasattr(e.retry_after, "total_seconds") else float(e.retry_after)
            self.quiet_until = time.monotonic() + wait
            if final:
                await self.show(text, final=True)
        except Exception as e:
            log.warning("could not update the progress message: %s", e)


async def follow(status: Status, job: dict, note: str = "") -> None:
    """Keep the bot's reply up to date, once a second, until the job ends."""
    phases = {"downloading": "下载到服务器", "uploading": "加密上传"}
    while True:
        await asyncio.sleep(INTERVAL)
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
            await status.show(f"✅ 已加密保存：/{cur['remote']}{note}", final=True)
        elif cur["state"] == "failed":
            await status.show(f"❌ {name} 失败：{cur.get('error', '')}{note}", final=True)
        elif cur["state"] == "canceled":
            await status.show(f"已取消：{name}{note}", final=True)
        elif cur["state"] == "paused":
            await status.show(f"已暂停：{name}")
        elif cur["state"] == "queued":
            await status.show(f"排队中：{name}")
        else:
            await status.show(progress(phases.get(cur.get("phase"), "处理中"), name, cur["done"], cur.get("size", 0), cur.get("speed", 0)))
        if cur["state"] in ("done", "failed", "canceled"):
            return


def fetched() -> int | None:
    """Bytes of the one file the local Bot API server is fetching right now.

    The server answers getFile only once it has the whole file; until then
    the growing file sits in its temp folder. With several files on the way
    the folder does not tell them apart, so there is no figure.
    """
    if receiving != 1:
        return None
    try:
        return sum(f.stat().st_size for f in (API_DIR / TOKEN / "temp").iterdir() if f.is_file())
    except OSError:
        return None


def allowed(update: Update) -> bool:
    return bool(update.effective_user) and update.effective_user.id in ALLOWED


async def start(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    uid = update.effective_user.id
    if not allowed(update):
        await update.message.reply_text(f"你的 Telegram 用户 ID 是 {uid}。把它加入 TELEGRAM_ALLOWED_USERS 后重启即可使用。")
        return
    await update.message.reply_text("把文件、照片、视频或下载链接发给我，我会加密后存进 Google Drive。")


async def on_media(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    global receiving
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
    size = getattr(media, "file_size", None) or 0
    status = Status(await msg.reply_text(f"正在接收：{name}"))
    try:
        # Listed in the core right away, so the app shows the download too.
        job = await core("POST", "/api/transfers", {"kind": "upload", "pending": True, "name": name, "remote": REMOTE_DIR, "size": size})
    except Exception as e:
        await status.show(f"❌ {name} 失败：{e}", final=True)
        return
    target_dir = DOWNLOADS / "telegram" / job["id"]  # message ids repeat across chats
    target = target_dir / name
    seen = {"phase": "从 Telegram 下载", "have": False, "canceled": False}
    receiving += 1
    stale = fetched() or 0  # leftovers of earlier downloads in the temp folder

    async def watch() -> None:
        while True:
            got = fetched()
            done = size if seen["have"] else None if got is None else min(max(got - stale, 0), size or got)
            try:
                cur = await core("POST", f"/api/transfers/{job['id']}/progress", {"done": done or 0, "size": size})
                seen["canceled"] = cur["state"] == "canceled"
                await status.show(progress(seen["phase"], name, done, size, cur.get("speed", 0)))
            except Exception as e:
                log.warning("progress not reported: %s", e)
            await asyncio.sleep(INTERVAL)

    watcher = context.application.create_task(watch())
    source, note = None, ""
    try:
        try:
            # A local Bot API server answers only after it has fetched the whole file.
            tg_file = await media.get_file(read_timeout=3600)
        finally:
            receiving -= 1
        size, seen["have"], seen["phase"] = tg_file.file_size or size, True, "转存到上传目录"
        target_dir.mkdir(parents=True, exist_ok=True)
        if LOCAL_MODE and tg_file.file_path and Path(tg_file.file_path).is_file():
            await asyncio.to_thread(shutil.copyfile, tg_file.file_path, target)
            source = Path(tg_file.file_path)
        else:
            await tg_file.download_to_drive(custom_path=target, read_timeout=600)
        watcher.cancel()
        job = await core("POST", f"/api/transfers/{job['id']}/ready", {"local": str(target.relative_to(DOWNLOADS)), "deleteSource": True})
    except Exception as e:
        watcher.cancel()
        log.exception("media failed")
        shutil.rmtree(target_dir, ignore_errors=True)
        if seen["canceled"]:
            await status.show(f"已取消：{name}", final=True)
            return
        try:
            await core("POST", f"/api/transfers/{job['id']}/fail", {"error": str(e)})
        except Exception:
            pass
        hint = "（官方 Bot API 只能下载 20 MB 以内的文件；更大的文件需要自建 Bot API 服务器，见 README）" if "too big" in str(e).lower() else ""
        await status.show(f"❌ {name} 失败：{e}{hint}", final=True)
        return
    finally:
        # The Bot API server keeps every file it fetched; ours is copied by now.
        if source is not None:
            try:
                source.unlink()
            except OSError as e:
                log.warning("could not delete %s: %s", source, e)
                note = "\n⚠️ Bot API 服务器上的原始文件没能删除（它的数据目录对机器人是只读的），请把挂载改成可写。"
    context.application.create_task(follow(status, job, note))


async def on_text(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    msg = update.effective_message
    if not allowed(update):
        return
    urls = URL_RE.findall(msg.text or "")
    if not urls:
        await msg.reply_text("没有找到链接。发文件或 http(s) 链接给我。")
        return
    for url in urls:
        status = Status(await msg.reply_text(f"已加入离线下载：{url}"))
        try:
            job = await core("POST", "/api/transfers", {"kind": "url", "url": url, "remote": REMOTE_DIR})
        except Exception as e:
            await status.show(f"❌ 失败：{e}", final=True)
            continue
        context.application.create_task(follow(status, job))


def main() -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(name)s %(message)s")
    logging.getLogger("httpx").setLevel(logging.WARNING)
    builder = Application.builder().token(TOKEN).concurrent_updates(True)
    # A self-hosted Bot API server lifts the 20 MB download limit.
    if base := os.environ.get("TELEGRAM_API_BASE"):
        base = base.rstrip("/")
        builder = builder.base_url(f"{base}/bot").base_file_url(f"{base}/file/bot").local_mode(LOCAL_MODE)
    app = builder.build()
    if not ALLOWED:
        log.warning("TELEGRAM_ALLOWED_USERS is empty: the bot will only answer /start with the sender's user id")
    app.add_handler(CommandHandler("start", start))
    app.add_handler(MessageHandler(filters.ATTACHMENT, on_media))
    app.add_handler(MessageHandler(filters.TEXT & ~filters.COMMAND, on_text))
    app.run_polling(allowed_updates=["message"])


if __name__ == "__main__":
    main()
