from pydantic import BaseModel
from typing import Any
import logging.config
import os


class OpsLog(BaseModel, extra="forbid"):
    level: str
    msg: str
    fields: dict[str, Any]


class LogFormatter(logging.Formatter):

    # Keys which are present in all LogRecord instances.
    # We use this set to identify _novel_ keys which should be included as structured, logged fields.
    LOGGING_RECORD_KEYS = logging.LogRecord(
        "", 0, "", 0, None, None, None
    ).__dict__.keys()

    def format(self, record: logging.LogRecord) -> str:
        # Attach any extra keywords which are not ordinarily in a LogRecord as fields.
        fields = {
            k: getattr(record, k)
            for k in record.__dict__.keys()
            if hasattr(record, k) and k not in self.LOGGING_RECORD_KEYS
        }
        if record.args:
            fields["args"] = record.args

        fields["source"] = record.name
        fields["file"] = f"{record.pathname}:{record.lineno}"

        # Attach any included stack traces.
        if record.exc_info:
            fields["traceback"] = self.formatException(record.exc_info).splitlines()
        elif record.stack_info:
            fields["stack"] = self.formatStack(record.stack_info).splitlines()

        return OpsLog(level=record.levelname, msg=record.msg, fields=fields).model_dump_json()


class EventLogger:
    def __init__(self, logger: logging.Logger):
        self._logger = logger

    # Config updates must contain restatements (not patches) of the connector's config
    # and contain whatever updates are relevant.
    def config_update(self, msg: str, config: dict[str, Any]):
        self._logger.info(
            msg,
            extra={
                "eventType": "configUpdate",
                "config": config,
            }
        )

    def status(self, msg: str):
        self._logger.info(
            msg,
            extra={
                "eventType": "connectorStatus",
            }
        )


class FlowLogger(logging.Logger):
    def __init__(self, name):
        super().__init__(name)
        self.event = EventLogger(self)


def init_logger() -> FlowLogger:
    LOGGING_CONFIG = {
        "version": 1,
        "disable_existing_loggers": False,
        "formatters": {
            "flow": {
                "()": LogFormatter,
                "format": "",
            },
        },
        "handlers": {
            "console": {
                "class": "logging.StreamHandler",
                "stream": "ext://sys.stderr",
                "formatter": "flow",
            },
        },
        "root": {
            "handlers": ["console"],
        },
    }

    logging.setLoggerClass(FlowLogger)
    logging.config.dictConfig(LOGGING_CONFIG)

    logger = logging.getLogger("flow")
    logger.setLevel(os.environ.get("LOG_LEVEL", "INFO").upper())

    assert isinstance(logger, FlowLogger)

    return logger
