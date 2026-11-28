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
from collections import OrderedDict
import logging
import os
import re
import shutil
import threading
import time
import uuid
from pathlib import Path
from urllib.parse import quote, unquote, urlparse

import httpx
from telegram import Message, Update
from telegram.error import RetryAfter
from telegram.ext import Application, CommandHandler, ContextTypes, MessageHandler, filters
from telethon import TelegramClient, errors
from telethon.tl.functions.messages import GetDiscussionMessageRequest

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
# https://t.me/name/12, https://t.me/c/1234567890/12, with a topic id in between or /s/ in front;
# ?comment=34 is a comment under post 12.
LINK_RE = re.compile(r"^https?://(?:t|telegram)\.me/(?:s/)?(?:c/(\d+)|([A-Za-z]\w{3,31}))/(?:\d+/)?(\d+)(?:\?(\S*))?$")
accounts: dict[int, TelegramClient] = {}  # by the bot user who logged in
logins: dict[int, dict] = {}  # logins under way: step, phone, hash
INTERVAL = 1  # seconds between updates of a progress message
BAR = 10

# job id -> (Telegram's id, size) of the files the Bot API server is fetching for us right now
waiting: dict[str, tuple[str, int]] = {}
leftover = 0  # bytes that lay in its temp folder before the first of them
pace = {"at": 0.0, "bytes": 0, "speed": 0}  # how fast that folder grows

log = logging.getLogger("crypta-telegram")

_REDACT_LOCK = threading.Lock()
_SENSITIVE: OrderedDict[str, str] = OrderedDict()
_SENSITIVE_LIMIT = 2048

_RE_UUID = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$", re.IGNORECASE)
_RE_BOT_TOKEN = re.compile(r"\bbot\d+:[A-Za-z0-9_-]{20,}\b")
_RE_RAW_TOKEN = re.compile(r"\b\d{6,}:[A-Za-z0-9_-]{20,}\b")
_RE_URL = re.compile(r"https?://[^\s\"'<>]+")
_RE_PIKPAK = re.compile(r"pikpak:[^\s\"'<>]+")
_RE_BOT_API_PATH = re.compile(r"/var/lib/telegram-bot-api/[^\s\"':,)\]]+")
_RE_DOWNLOAD_PATH = re.compile(
    r"/downloads/(?!telegram/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/file\b)[^\s\"':,)\]]+",
    re.IGNORECASE,
)
_RE_WIN_PATH = re.compile(r"[A-Za-z]:\\[^\s\"':,)\]]+")


def _add_sensitive(token: str, replacement: str) -> None:
    token = token.strip()
    if not token or token in (".", "/", "\\") or token == replacement or _RE_UUID.match(token):
        return
    with _REDACT_LOCK:
        _SENSITIVE[token] = replacement
        _SENSITIVE.move_to_end(token)
        while len(_SENSITIVE) > _SENSITIVE_LIMIT:
            _SENSITIVE.popitem(last=False)


def register_sensitive(value: str | Path | None, replacement: str) -> None:
    """Remember a filename, path, or URL so log output replaces it with a UUID."""
    if value is None:
        return
    raw = str(value).strip()
    if not raw:
        return
    _add_sensitive(raw, replacement)
    slashed = raw.replace("\\", "/")
    _add_sensitive(slashed, replacement)
    if raw.startswith(("http://", "https://")):
        parsed = urlparse(raw)
        if parsed.path:
            _add_sensitive(parsed.path, replacement)
            pbase = parsed.path.rstrip("/").rsplit("/", 1)[-1]
            if pbase:
                _add_sensitive(pbase, replacement)
            punesc = unquote(parsed.path)
            if punesc != parsed.path:
                _add_sensitive(punesc, replacement)
                pubase = punesc.rstrip("/").rsplit("/", 1)[-1]
                if pubase:
                    _add_sensitive(pubase, replacement)
    base = slashed.rstrip("/").rsplit("/", 1)[-1]
    if base:
        _add_sensitive(base, replacement)
    unesc = unquote(raw)
    if unesc != raw:
        _add_sensitive(unesc, replacement)
        unesc_base = unesc.replace("\\", "/").rstrip("/").rsplit("/", 1)[-1]
        if unesc_base:
            _add_sensitive(unesc_base, replacement)
    esc = quote(raw, safe="/:@")
    if esc != raw:
        _add_sensitive(esc, replacement)


def sanitize_log_text(text: str) -> str:
    """Scrub filenames, local paths, URLs, and Telegram secrets from log text."""
    if not text:
        return text
    if TOKEN:
        text = text.replace(TOKEN, "<bot-token>")
    if ACCOUNT_API_HASH:
        text = text.replace(ACCOUNT_API_HASH, "<api-hash>")
    with _REDACT_LOCK:
        items = list(_SENSITIVE.items())
    items.sort(key=lambda kv: len(kv[0]), reverse=True)
    for tok, repl in items:
        if len(tok) >= 4 or "/" in tok or "\\" in tok:
            text = text.replace(tok, repl)
        else:
            text = re.sub(rf"(?<![A-Za-z0-9_]){re.escape(tok)}(?![A-Za-z0-9_])", repl, text)
    text = _RE_BOT_TOKEN.sub("bot<redacted>", text)
    text = _RE_RAW_TOKEN.sub("<token>", text)
    text = _RE_URL.sub("<url>", text)
    text = _RE_PIKPAK.sub("pikpak:<redacted>", text)
    text = _RE_BOT_API_PATH.sub("<bot-api-path>", text)
    text = _RE_DOWNLOAD_PATH.sub("<download-path>", text)
    text = _RE_WIN_PATH.sub("<path>", text)
    return text


class RedactingFormatter(logging.Formatter):
    """Formatter that redacts both log messages and full exception tracebacks."""

    def format(self, record: logging.LogRecord) -> str:
        return sanitize_log_text(super().format(record))


def configure_logging() -> None:
    handler = logging.StreamHandler()
    handler.setFormatter(RedactingFormatter("%(asctime)s %(name)s %(message)s"))
    root = logging.getLogger()
    root.handlers.clear()
    root.setLevel(logging.INFO)
    root.addHandler(handler)
    for noisy in ("httpx", "httpcore", "telethon", "telegram"):
        logging.getLogger(noisy).setLevel(logging.WARNING)


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

    def __init__(self, message: Message, file_id: str | None = None) -> None:
        self.message, self.text, self.quiet_until, self.file_id = message, message.text, 0.0, file_id

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
            if self.file_id:
                log.warning("file=%s could not update the progress message: %s", self.file_id, e)
            else:
                log.warning("could not update the progress message: %s", e)


async def follow(status: Status, job: dict, note: str = "", stage: Path | None = None) -> None:
    """Keep the bot's reply up to date, once a second, until the job ends.

    stage is the folder the job's file was put in; the core deletes the
    file after the upload and the folder goes once it is empty.
    """
    file_id = job["id"]
    status.file_id = file_id
    phases = {"downloading": "下载到服务器", "uploading": "加密上传"}
    while True:
        await asyncio.sleep(INTERVAL)
        try:
            jobs = (await core("GET", "/api/transfers"))["transfers"]
        except Exception as e:  # core restarting: try again
            log.warning("file=%s core unreachable: %s", file_id, e)
            continue
        cur = next((j for j in jobs if j["id"] == file_id), None)
        if cur is None:
            return
        name = cur.get("name") or job.get("name") or "文件"
        register_sensitive(name, file_id)
        if cur.get("remote"):
            register_sensitive(cur["remote"], file_id)
        if cur["state"] == "done":
            log.info("file=%s transfer completed state=done", file_id)
            await status.show(f"✅ 已加密保存：/{cur['remote']}{note}", final=True)
        elif cur["state"] == "failed":
            log.warning("file=%s transfer completed state=failed", file_id)
            await status.show(f"❌ {name} 失败：{cur.get('error', '')}{note}", final=True)
        elif cur["state"] == "canceled":
            log.info("file=%s transfer completed state=canceled", file_id)
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


def temp_bytes() -> int | None:
    try:
        return sum(f.stat().st_size for f in (API_DIR / TOKEN / "temp").iterdir() if f.is_file())
    except OSError:
        return None


def fetched() -> int | None:
    """Bytes of the files the local Bot API server is fetching right now.

    The server answers getFile only once it has the whole file; until then
    the growing file sits in its temp folder. With several files on the way
    the folder does not tell them apart: the figure is that of all of them.
    """
    got = temp_bytes()
    return None if got is None else max(got - leftover, 0)


def together(phase: str, name: str, got: int) -> str:
    """The progress message while several files are fetched at once."""
    now = time.monotonic()
    if now - pace["at"] >= INTERVAL:
        if pace["at"]:
            pace["speed"] = int(max(got - pace["bytes"], 0) / (now - pace["at"]))
        pace["at"], pace["bytes"] = now, got
    files = dict(waiting.values())
    total = sum(files.values())
    lines = progress(phase, name, min(got, total or got), total, pace["speed"]).split("\n")
    lines.insert(1, f"{len(files)} 个文件同时下载，下面是合计进度")
    return "\n".join(lines)


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
    if update.effective_chat.type != "private":  # a group would read the code and the password
        await msg.reply_text("登录只能在和我的私聊里进行。")
        return
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
            if not re.search(r"\d", text):
                await msg.reply_text("发来验证码，数字之间加空格。/cancel 取消。")
                return
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
    link_id = str(uuid.uuid4())
    register_sensitive(link[0], link_id)
    status = Status(await msg.reply_text(f"正在读取消息：{link[0]}"))
    try:
        client = await account(uid)
        if not await client.is_user_authorized():
            await status.show("要下载消息链接，先用 /login 登录你的 Telegram 账号。", final=True)
            return
        chat = int(f"-100{link[1]}") if link[1] else link[2]
        number = int(link[3])
        try:
            await client.get_input_entity(chat)
        except ValueError:  # a private chat this login has not met yet
            await client.get_dialogs()
        if comment := re.search(r"(?:^|&)comment=(\d+)", link[4] or ""):
            # The comment is a message of the group where the post is discussed.
            thread = await client(GetDiscussionMessageRequest(peer=chat, msg_id=number))
            chat, number = thread.messages[0].peer_id, int(comment[1])
        first = await client.get_messages(chat, ids=number)
        found = [first] if first else []
        if first and first.grouped_id and "single" not in (link[4] or ""):
            near = await client.get_messages(chat, ids=list(range(max(number - 9, 1), number + 10)))
            found = [m for m in near if m and m.grouped_id == first.grouped_id]
        found = [m for m in found if m.file]
    except Exception as e:
        log.exception("link=%s read failed", link_id)
        await status.show(f"❌ 读取失败：{e}", final=True)
        return
    if not found:
        log.info("link=%s resolved files=0", link_id)
        await status.show("找不到这条消息，或者里面没有文件（登录的账号要能看到它）。", final=True)
        return
    log.info("link=%s resolved files=%d", link_id, len(found))
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
        fallback_id = str(uuid.uuid4())
        register_sensitive(name, fallback_id)
        log.warning("file=%s failed to register telegram-link transfer: %s", fallback_id, e)
        await status.show(f"❌ {name} 失败：{e}", final=True)
        return
    file_id = job["id"]
    status.file_id = file_id
    register_sensitive(name, file_id)
    log.info("file=%s received kind=telegram-link size=%d", file_id, size)
    # Stage under an opaque UUID path so local errors and ffmpeg logs never leak the filename.
    target_dir = DOWNLOADS / "telegram" / file_id
    target = target_dir / "file"
    seen = {"done": 0, "canceled": False}
    download = asyncio.current_task()

    async def watch() -> None:
        while True:
            try:
                cur = await core("POST", f"/api/transfers/{file_id}/progress", {"done": seen["done"], "size": size})
                await status.show(progress("从 Telegram 下载", name, seen["done"], size, cur.get("speed", 0)))
                if cur["state"] == "canceled":
                    seen["canceled"] = True
                    download.cancel()
                    return
            except Exception as e:
                log.warning("file=%s progress not reported: %s", file_id, e)
            await asyncio.sleep(INTERVAL)

    watcher = context.application.create_task(watch())
    try:
        target_dir.mkdir(parents=True, exist_ok=True)
        # Writing to an open file object prevents Telethon from appending extensions or renaming.
        with target.open("wb") as fp:
            saved = await client.download_media(m, file=fp, progress_callback=lambda done, total: seen.update(done=done))
        if not saved:
            raise RuntimeError("这条消息的文件下载不了")
        watcher.cancel()
        job = await core("POST", f"/api/transfers/{file_id}/ready", {"local": str(target.relative_to(DOWNLOADS)), "deleteSource": True})
        log.info("file=%s ready kind=telegram-link size=%d", file_id, job.get("size", size))
    except (Exception, asyncio.CancelledError) as e:
        watcher.cancel()
        shutil.rmtree(target_dir, ignore_errors=True)
        if seen["canceled"]:
            log.info("file=%s canceled kind=telegram-link", file_id)
            await status.show(f"已取消：{name}", final=True)
            return
        log.exception("file=%s link download failed", file_id)
        try:
            await core("POST", f"/api/transfers/{file_id}/fail", {"error": str(e)})
        except Exception:
            pass
        await status.show(f"❌ {name} 失败：{e}", final=True)
        return
    await follow(status, job, "", target_dir)


async def on_media(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    global leftover
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
        fallback_id = str(uuid.uuid4())
        register_sensitive(name, fallback_id)
        log.warning("file=%s failed to register media transfer: %s", fallback_id, e)
        await status.show(f"❌ {name} 失败：{e}", final=True)
        return
    file_id = job["id"]
    status.file_id = file_id
    register_sensitive(name, file_id)
    log.info("file=%s received kind=media size=%d", file_id, size)
    # Stage under an opaque UUID path so local errors and ffmpeg logs never leak the filename.
    target_dir = DOWNLOADS / "telegram" / file_id  # message ids repeat across chats
    target = target_dir / "file"
    seen = {"phase": "从 Telegram 下载", "have": False, "canceled": False, "done": 0}
    if not waiting:
        leftover = temp_bytes() or 0
        pace.update(at=0.0, bytes=0, speed=0)
    # The same file sent twice is fetched once.
    waiting[file_id] = (getattr(media, "file_unique_id", None) or file_id, size)

    async def watch() -> None:
        while True:
            got = fetched()
            shared = not seen["have"] and len(dict(waiting.values())) > 1  # this file's own share is not known
            done = size if seen["have"] else None if got is None or shared else min(got, size or got)
            if done is not None:
                seen["done"] = done
            try:
                # The app keeps the last figure this file had of its own.
                cur = await core("POST", f"/api/transfers/{file_id}/progress", {"done": seen["done"], "size": size})
                seen["canceled"] = cur["state"] == "canceled"
                if shared and got is not None:
                    await status.show(together(seen["phase"], name, got))
                else:
                    await status.show(progress(seen["phase"], name, done, size, cur.get("speed", 0)))
            except Exception as e:
                log.warning("file=%s progress not reported: %s", file_id, e)
            await asyncio.sleep(INTERVAL)

    watcher = context.application.create_task(watch())
    source, note = None, ""
    try:
        try:
            # A local Bot API server answers only after it has fetched the whole file.
            tg_file = await media.get_file(read_timeout=3600)
        finally:
            waiting.pop(file_id, None)
        size, seen["have"], seen["phase"] = tg_file.file_size or size, True, "转存到上传目录"
        local = None
        fetched_to = Path(tg_file.file_path) if LOCAL_MODE and tg_file.file_path else None
        if fetched_to is not None:
            register_sensitive(fetched_to, file_id)
        earlier = moved.get(fetched_to)
        if fetched_to and not fetched_to.is_file() and earlier and earlier.is_file():
            # Another message with the same file took it first: share its copy.
            target_dir = API_DIR / "crypta" / file_id
            target = target_dir / "file"
            target_dir.mkdir(parents=True, exist_ok=True)
            os.link(earlier, target)
            local = f"{API_SHARE}/{target.relative_to(API_DIR)}"
        elif fetched_to and fetched_to.is_file():
            source = fetched_to
            if API_SHARE and source.is_relative_to(API_DIR):
                target_dir = API_DIR / "crypta" / file_id
                target = target_dir / "file"
                target_dir.mkdir(parents=True, exist_ok=True)
                try:
                    source.rename(target)  # same volume: no copy
                    moved[source] = target
                    source, local = None, f"{API_SHARE}/{target.relative_to(API_DIR)}"
                except OSError as e:
                    log.warning("file=%s could not move local file, copying instead: %s", file_id, type(e).__name__)
                    shutil.rmtree(target_dir, ignore_errors=True)
                    target_dir = DOWNLOADS / "telegram" / file_id
                    target = target_dir / "file"
            if local is None:
                target_dir.mkdir(parents=True, exist_ok=True)
                await asyncio.to_thread(shutil.copyfile, source, target)
        else:
            target_dir.mkdir(parents=True, exist_ok=True)
            await tg_file.download_to_drive(custom_path=target, read_timeout=600)
        watcher.cancel()
        job = await core("POST", f"/api/transfers/{file_id}/ready", {"local": local or str(target.relative_to(DOWNLOADS)), "deleteSource": True})
        log.info("file=%s ready kind=media size=%d", file_id, size)
    except Exception as e:
        watcher.cancel()
        log.exception("file=%s media failed", file_id)
        shutil.rmtree(target_dir, ignore_errors=True)
        if seen["canceled"]:
            log.info("file=%s canceled kind=media", file_id)
            await status.show(f"已取消：{name}", final=True)
            return
        try:
            await core("POST", f"/api/transfers/{file_id}/fail", {"error": str(e)})
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
                log.warning("file=%s could not delete local Bot API file: %s", file_id, type(e).__name__)
                note = "\n⚠️ Bot API 服务器上的原始文件没能删除（它的数据目录对机器人是只读的），请把挂载改成可写。"
    context.application.create_task(follow(status, job, note, target_dir))


async def on_text(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    msg = update.effective_message
    if not allowed(update):
        return
    uid = update.effective_user.id
    if uid in logins and update.effective_chat.type == "private":
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
        fallback_id = str(uuid.uuid4())
        register_sensitive(url, fallback_id)
        status = Status(await msg.reply_text(f"已加入离线下载：{url}"))
        try:
            job = await core("POST", "/api/transfers", {"kind": "url", "url": url, "remote": REMOTE_DIR})
        except Exception as e:
            log.warning("file=%s failed to queue url transfer: %s", fallback_id, e)
            await status.show(f"❌ 失败：{e}", final=True)
            continue
        file_id = job["id"]
        status.file_id = file_id
        register_sensitive(url, file_id)
        log.info("file=%s queued kind=url", file_id)
        context.application.create_task(follow(status, job))


def main() -> None:
    configure_logging()
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
