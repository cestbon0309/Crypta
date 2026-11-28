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

    ensure_stub("httpx", {"AsyncClient": object})
    ensure_stub("telegram", {"Message": object, "Update": object})
    ensure_stub("telegram.error", {"RetryAfter": type("RetryAfter", (Exception,), {})})
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


if __name__ == "__main__":
    unittest.main()
