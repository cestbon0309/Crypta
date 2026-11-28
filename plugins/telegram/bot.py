"""Crypta Telegram plugin.

Send the bot a file, photo, video or audio and it saves it into the shared
downloads folder, then asks the Crypta core to encrypt and upload it. Send a
link and the core downloads it itself. Send the link of a Telegram message and
the file in it is fetched with the Telegram account the sender logged in with
/login, which reaches messages that cannot be forwarded to a bot. The bot only
talks to the core through its HTTP API, which is all any other download plugin
has to do as well.
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
from telethon import TelegramClient, errors

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
# Where the core sees that folder, relative to its downloads folder. When set,
# a fetched file is moved inside the folder instead of copied to the downloads.
API_SHARE = os.environ.get("TELEGRAM_API_SHARE", "").strip("/")
# Where each fetched file was moved to: the Bot API server hands the same
# path to every message that carries the same file.
moved: dict[Path, Path] = {}
# A Telegram account reads the messages a bot cannot: api id and hash from
# my.telegram.org, and a folder that keeps each user's login.
ACCOUNT_API_ID = int(os.environ.get("TELEGRAM_API_ID") or 0)
ACCOUNT_API_HASH = os.environ.get("TELEGRAM_API_HASH", "")
SESSIONS = Path(os.environ.get("TELEGRAM_SESSION_DIR", "/sessions"))
# https://t.me/name/12, https://t.me/c/1234567890/12, with a topic id in between or /s/ in front.
LINK_RE = re.compile(r"^https?://(?:t|telegram)\.me/(?:s/)?(?:c/(\d+)|([A-Za-z]\w{3,31}))/(?:\d+/)?(\d+)(?:\?(\S*))?$")
accounts: dict[int, TelegramClient] = {}  # by the bot user who logged in
logins: dict[int, dict] = {}  # logins under way: step, phone, hash
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


async def follow(status: Status, job: dict, note: str = "", stage: Path | None = None) -> None:
    """Keep the bot's reply up to date, once a second, until the job ends.

    stage is the folder the job's file was put in; the core deletes the
    file after the upload and the folder goes once it is empty.
    """
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
            if stage is not None:
                try:
                    stage.rmdir()
                except OSError:
                    pass
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
    await update.message.reply_text(
        "把文件、照片、视频或下载链接发给我，我会加密后存进 Google Drive。\n"
        "不能转发的消息：先用 /login 登录你的 Telegram 账号，再把消息链接发给我。/logout 退出登录。"
    )


async def account(uid: int) -> TelegramClient:
    """The Telegram account of a bot user, connected; logged in or not."""
    if not ACCOUNT_API_ID or not ACCOUNT_API_HASH:
        raise RuntimeError("服务器没有配置 TELEGRAM_API_ID 和 TELEGRAM_API_HASH")
    client = accounts.get(uid)
    if client is None:
        SESSIONS.mkdir(parents=True, exist_ok=True)
        SESSIONS.chmod(0o700)
        client = accounts[uid] = TelegramClient(str(SESSIONS / str(uid)), ACCOUNT_API_ID, ACCOUNT_API_HASH)
    if not client.is_connected():
        await client.connect()
    return client


async def login(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    if not allowed(update):
        return
    uid, msg = update.effective_user.id, update.effective_message
    try:
        client = await account(uid)
        if await client.is_user_authorized():
            me = await client.get_me()
            await msg.reply_text(f"已经登录：{me.first_name}（+{me.phone}）。/logout 退出登录。")
            return
    except Exception as e:
        await msg.reply_text(f"❌ 登录不可用：{e}")
        return
    logins[uid] = {"step": "phone"}
    await msg.reply_text("发来你的手机号，带国家区号，例如 +8613800000000。/cancel 取消。")


async def logout(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    if not allowed(update):
        return
    uid, msg = update.effective_user.id, update.effective_message
    logins.pop(uid, None)
    try:
        client = await account(uid)
        if await client.is_user_authorized():
            await client.log_out()  # also deletes the saved login
        else:
            await client.disconnect()
        accounts.pop(uid, None)
        await msg.reply_text("已退出登录。")
    except Exception as e:
        await msg.reply_text(f"❌ 退出失败：{e}")


async def cancel(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    if allowed(update) and logins.pop(update.effective_user.id, None):
        await update.effective_message.reply_text("已取消登录。")


async def login_step(msg: Message, uid: int) -> None:
    """One answer of a login under way: the phone, then the code, then the password."""
    state, text = logins[uid], (msg.text or "").strip()
    try:
        client = await account(uid)
        if state["step"] == "phone":
            phone = "+" + re.sub(r"\D", "", text)
            sent = await client.send_code_request(phone)
            state.update(step="code", phone=phone, hash=sent.phone_code_hash)
            await msg.reply_text(
                "验证码已发到你的 Telegram。数字之间加空格再发给我，例如 1 2 3 4 5："
                "原样发出的验证码会被 Telegram 立即作废。"
            )
            return
        if state["step"] == "code":
            try:
                await client.sign_in(state["phone"], re.sub(r"\D", "", text), phone_code_hash=state["hash"])
            except errors.SessionPasswordNeededError:
                state["step"] = "password"
                await msg.reply_text("这个账号开了两步验证，发来密码。我读完就删掉这条消息。")
                return
        else:
            try:
                await msg.delete()
            except Exception as e:
                log.warning("could not delete the password message: %s", e)
            await client.sign_in(password=msg.text or "")
        del logins[uid]
        me = await client.get_me()
        await msg.reply_text(f"✅ 已登录：{me.first_name}。现在把消息链接发给我就能下载。")
    except (errors.PhoneCodeInvalidError, errors.PasswordHashInvalidError):
        await msg.reply_text("不对，再发一次。/cancel 取消。")
    except errors.PhoneCodeExpiredError:
        logins[uid] = {"step": "phone"}
        await msg.reply_text("验证码已失效（原样发出的验证码会被作废）。重新发手机号，再把新验证码隔开发。")
    except Exception as e:
        logins.pop(uid, None)
        await msg.reply_text(f"❌ 登录失败：{e}\n/login 重新开始。")


async def on_link(msg: Message, context: ContextTypes.DEFAULT_TYPE, uid: int, link: re.Match) -> None:
    """Fetch the file of the linked message, or every file of its album."""
    status = Status(await msg.reply_text(f"正在读取消息：{link[0]}"))
    try:
        client = await account(uid)
        if not await client.is_user_authorized():
            await status.show("要下载消息链接，先用 /login 登录你的 Telegram 账号。", final=True)
            return
        chat = int(f"-100{link[1]}") if link[1] else link[2]
        number = int(link[3])
        try:
            first = await client.get_messages(chat, ids=number)
        except ValueError:  # a private chat this login has not met yet
            await client.get_dialogs()
            first = await client.get_messages(chat, ids=number)
        found = [first] if first else []
        if first and first.grouped_id and "single" not in (link[4] or ""):
            near = await client.get_messages(chat, ids=list(range(max(number - 9, 1), number + 10)))
            found = [m for m in near if m and m.grouped_id == first.grouped_id]
        found = [m for m in found if m.file]
    except Exception as e:
        log.exception("link failed")
        await status.show(f"❌ 读取失败：{e}", final=True)
        return
    if not found:
        await status.show("找不到这条消息，或者里面没有文件（登录的账号要能看到它）。", final=True)
        return
    for i, m in enumerate(found):
        if i:
            status = Status(await msg.reply_text("正在读取消息"))
        context.application.create_task(fetch(client, m, status, context))


async def fetch(client: TelegramClient, m, status: Status, context: ContextTypes.DEFAULT_TYPE) -> None:
    """Download one message's file with the account and hand it to the core."""
    name = Path(m.file.name or f"telegram-{m.chat_id}-{m.id}{m.file.ext or ''}".replace("--", "-")).name
    size = m.file.size or 0
    try:
        job = await core("POST", "/api/transfers", {"kind": "upload", "pending": True, "name": name, "remote": REMOTE_DIR, "size": size})
    except Exception as e:
        await status.show(f"❌ {name} 失败：{e}", final=True)
        return
    target_dir = DOWNLOADS / "telegram" / job["id"]
    target = target_dir / name
    seen = {"done": 0, "canceled": False}
    download = asyncio.current_task()

    async def watch() -> None:
        while True:
            try:
                cur = await core("POST", f"/api/transfers/{job['id']}/progress", {"done": seen["done"], "size": size})
                await status.show(progress("从 Telegram 下载", name, seen["done"], size, cur.get("speed", 0)))
                if cur["state"] == "canceled":
                    seen["canceled"] = True
                    download.cancel()
                    return
            except Exception as e:
                log.warning("progress not reported: %s", e)
            await asyncio.sleep(INTERVAL)

    watcher = context.application.create_task(watch())
    try:
        target_dir.mkdir(parents=True, exist_ok=True)
        await client.download_media(m, file=str(target), progress_callback=lambda done, total: seen.update(done=done))
        watcher.cancel()
        job = await core("POST", f"/api/transfers/{job['id']}/ready", {"local": str(target.relative_to(DOWNLOADS)), "deleteSource": True})
    except (Exception, asyncio.CancelledError) as e:
        watcher.cancel()
        shutil.rmtree(target_dir, ignore_errors=True)
        if seen["canceled"]:
            await status.show(f"已取消：{name}", final=True)
            return
        log.exception("link download failed")
        try:
            await core("POST", f"/api/transfers/{job['id']}/fail", {"error": str(e)})
        except Exception:
            pass
        await status.show(f"❌ {name} 失败：{e}", final=True)
        return
    await follow(status, job, "", target_dir)


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
        local = None
        fetched_to = Path(tg_file.file_path) if LOCAL_MODE and tg_file.file_path else None
        earlier = moved.get(fetched_to)
        if fetched_to and not fetched_to.is_file() and earlier and earlier.is_file():
            # Another message with the same file took it first: share its copy.
            target_dir = API_DIR / "crypta" / job["id"]
            target = target_dir / name
            target_dir.mkdir(parents=True, exist_ok=True)
            os.link(earlier, target)
            local = f"{API_SHARE}/{target.relative_to(API_DIR)}"
        elif fetched_to and fetched_to.is_file():
            source = fetched_to
            if API_SHARE and source.is_relative_to(API_DIR):
                target_dir = API_DIR / "crypta" / job["id"]
                target = target_dir / name
                target_dir.mkdir(parents=True, exist_ok=True)
                try:
                    source.rename(target)  # same volume: no copy
                    moved[source] = target
                    source, local = None, f"{API_SHARE}/{target.relative_to(API_DIR)}"
                except OSError as e:
                    log.warning("could not move %s, copying it: %s", source, e)
                    shutil.rmtree(target_dir, ignore_errors=True)
                    target_dir = DOWNLOADS / "telegram" / job["id"]
                    target = target_dir / name
            if local is None:
                target_dir.mkdir(parents=True, exist_ok=True)
                await asyncio.to_thread(shutil.copyfile, source, target)
        else:
            target_dir.mkdir(parents=True, exist_ok=True)
            await tg_file.download_to_drive(custom_path=target, read_timeout=600)
        watcher.cancel()
        job = await core("POST", f"/api/transfers/{job['id']}/ready", {"local": local or str(target.relative_to(DOWNLOADS)), "deleteSource": True})
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
        # The Bot API server keeps every file it fetched; ours is copied or moved by now.
        if source is not None:
            try:
                source.unlink()
            except OSError as e:
                log.warning("could not delete %s: %s", source, e)
                note = "\n⚠️ Bot API 服务器上的原始文件没能删除（它的数据目录对机器人是只读的），请把挂载改成可写。"
    context.application.create_task(follow(status, job, note, target_dir))


async def on_text(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    msg = update.effective_message
    if not allowed(update):
        return
    uid = update.effective_user.id
    if uid in logins:
        await login_step(msg, uid)
        return
    urls = URL_RE.findall(msg.text or "")
    if not urls:
        await msg.reply_text("没有找到链接。发文件或 http(s) 链接给我。")
        return
    for url in urls:
        if link := LINK_RE.match(url):
            await on_link(msg, context, uid, link)
            continue
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
    app.add_handler(CommandHandler("login", login))
    app.add_handler(CommandHandler("logout", logout))
    app.add_handler(CommandHandler("cancel", cancel))
    app.add_handler(MessageHandler(filters.ATTACHMENT, on_media))
    app.add_handler(MessageHandler(filters.TEXT & ~filters.COMMAND, on_text))
    app.run_polling(allowed_updates=["message"])


if __name__ == "__main__":
    main()
