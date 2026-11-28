import asyncio
import importlib.util
import io
import logging
import sys
import types
import unittest
import uuid
from pathlib import Path


def load_bot_module():
    """Import plugins/telegram/bot.py with lightweight stubs for optional runtime deps."""
    stubs = {}

    def ensure_stub(name: str, attrs: dict):
        if name not in sys.modules:
            mod = types.ModuleType(name)
            for k, v in attrs.items():
                setattr(mod, k, v)
            sys.modules[name] = mod
            stubs[name] = mod

    class RetryAfter(Exception):
        def __init__(self, retry_after=0.01):
            super().__init__(f"Flood control exceeded. Retry in {retry_after} seconds")
            self.retry_after = retry_after

    class TimedOut(Exception):
        pass

    class BadRequest(Exception):
        pass

    ensure_stub("httpx", {"AsyncClient": object})
    ensure_stub("telegram", {"Message": object, "Update": object})
    ensure_stub("telegram.error", {"BadRequest": BadRequest, "RetryAfter": RetryAfter, "TimedOut": TimedOut})
    ensure_stub(
        "telegram.ext",
        {
            "Application": object,
            "CommandHandler": object,
            "ContextTypes": types.SimpleNamespace(DEFAULT_TYPE=object),
            "MessageHandler": object,
            "filters": types.SimpleNamespace(ATTACHMENT=None, TEXT=0, COMMAND=0),
        },
    )
    ensure_stub("telethon", {"TelegramClient": object, "errors": types.SimpleNamespace()})
    ensure_stub("telethon.tl", {})
    ensure_stub("telethon.tl.functions", {})
    ensure_stub("telethon.tl.functions.messages", {"GetDiscussionMessageRequest": object})

    bot_path = Path(__file__).with_name("bot.py")
    spec = importlib.util.spec_from_file_location("crypta_bot_under_test", bot_path)
    mod = importlib.util.module_from_spec(spec)
    assert spec and spec.loader
    spec.loader.exec_module(mod)
    return mod


class TestBotLogRedaction(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.bot = load_bot_module()

    def setUp(self):
        self.bot._SENSITIVE.clear()
        self.bot.TOKEN = "123456789:AAFakeSecretBotTokenValue1234567890"
        self.bot.ACCOUNT_API_HASH = "abcdef1234567890abcdef1234567890"

    def test_redacts_filenames_urls_paths_and_tokens(self):
        fid = str(uuid.uuid4())
        secret_name = "绝密视频 [4K].mkv"
        secret_url = "https://files.example.com/private/Ultra%20Secret%20Film.mp4?sig=999"
        bot_api_file = f"/var/lib/telegram-bot-api/{self.bot.TOKEN}/documents/{secret_name}"

        self.bot.register_sensitive(secret_name, fid)
        self.bot.register_sensitive(secret_url, fid)
        self.bot.register_sensitive(bot_api_file, fid)

        stream = io.StringIO()
        handler = logging.StreamHandler(stream)
        handler.setFormatter(self.bot.RedactingFormatter("%(name)s %(message)s"))
        logger = logging.getLogger("test-redaction")
        logger.handlers.clear()
        logger.setLevel(logging.INFO)
        logger.propagate = False
        logger.addHandler(handler)

        logger.info("file=%s received kind=media size=1024", fid)
        try:
            raise OSError(
                f"failed on {bot_api_file} and {secret_url} (Ultra Secret Film.mp4) with {self.bot.ACCOUNT_API_HASH}"
            )
        except OSError:
            logger.exception("file=%s media failed", fid)

        out = stream.getvalue()
        self.assertIn(f"file={fid}", out)
        for forbidden in (
            "绝密视频",
            "[4K]",
            ".mkv",
            "Ultra Secret Film",
            "Ultra%20Secret",
            ".mp4",
            "files.example.com",
            "sig=999",
            self.bot.TOKEN,
            self.bot.ACCOUNT_API_HASH,
            "/var/lib/telegram-bot-api/",
        ):
            self.assertNotIn(forbidden, out, f"leaked {forbidden!r} in:\n{out}")

    def test_preserves_uuid_staging_path_redacts_named_download_path(self):
        fid = str(uuid.uuid4())
        text = (
            f"ok=/downloads/telegram/{fid}/file "
            f"bad=/downloads/telegram/123/secret_file.zip "
            f"win=C:\\Users\\Alice\\secret.mp4"
        )
        out = self.bot.sanitize_log_text(text)
        self.assertIn(f"/downloads/telegram/{fid}/file", out)
        self.assertNotIn("secret_file.zip", out)
        self.assertNotIn("secret.mp4", out)
        self.assertIn("<download-path>", out)
        self.assertIn("<path>", out)


class TestBotConcurrencyAndStatus(unittest.IsolatedAsyncioTestCase):
    @classmethod
    def setUpClass(cls):
        cls.bot = load_bot_module()

    def setUp(self):
        self.bot._chat_pacers.clear()
        self.bot.SEND_GAP = 0.0
        self.bot.EDIT_GAP = 0.0

    async def test_status_survives_initial_timeout_and_delivers_final(self):
        sent = []
        calls = {"n": 0}
        TimedOut = self.bot.TimedOut

        class FakeSent:
            def __init__(self, text):
                self.text = text
                self.edits = []

            async def edit_text(self, text):
                self.text = text
                self.edits.append(text)

        class FakeIncoming:
            chat_id = 1001

            async def reply_text(self, text):
                calls["n"] += 1
                if calls["n"] <= 2:
                    raise TimedOut("Timed out")
                m = FakeSent(text)
                sent.append(m)
                return m

        status = self.bot.Status(FakeIncoming(), file_id=str(uuid.uuid4()))
        await status.send_initial("排队中：video.mp4")
        # Initial reply timed out cleanly without raising.
        self.assertIsNone(status.message)
        # Final status still delivers by sending a fresh reply.
        await status.show("✅ 已加密保存：/Telegram/video.mp4", final=True)
        self.assertEqual(len(sent), 1)
        self.assertIn("已加密保存", sent[0].text)

    async def test_status_ignores_not_modified_and_shares_retry_after(self):
        BadRequest = self.bot.BadRequest
        RetryAfter = self.bot.RetryAfter
        edits = []

        class FakeSent:
            def __init__(self, text):
                self.text = text
                self.first_edit = True

            async def edit_text(self, text):
                if self.first_edit:
                    self.first_edit = False
                    raise BadRequest("Message is not modified")
                edits.append(text)
                self.text = text

        class FakeIncoming:
            chat_id = 2002

            def __init__(self):
                self.raised = False

            async def reply_text(self, text):
                if not self.raised:
                    self.raised = True
                    raise RetryAfter(0.01)
                return FakeSent(text)

        status = self.bot.Status(FakeIncoming(), file_id=str(uuid.uuid4()))
        await status.send_initial("排队中：a.mp4")
        self.assertIsNotNone(status.message)
        # First edit hits BadRequest("not modified") and updates self.text silently.
        await status.show("从 Telegram 下载：a.mp4", final=False)
        self.assertEqual(status.text, "从 Telegram 下载：a.mp4")
        await status.show("✅ 完成", final=True)
        self.assertEqual(edits, ["✅ 完成"])

    def test_default_download_concurrency_is_one(self):
        self.assertEqual(self.bot.DOWNLOAD_CONCURRENT, 1)


if __name__ == "__main__":
    unittest.main()
