"""Evaluate the three historical NHA-107 fallback templates in pinned HA Core.

This is a qualification worker, run only by the existing NHA-006 isolated
local-image harness. No Homeward runtime imports or device connections.
"""

import asyncio
from datetime import datetime
from importlib.metadata import version
import json
import sys
from unittest.mock import patch
from zoneinfo import ZoneInfo

import homeassistant.const
from homeassistant.core import HomeAssistant
from homeassistant.helpers.template import Template
from homeassistant.util import dt as dt_util

EXPECTED_IDS = ("ups-legacy", "floor-legacy", "hvac-legacy")


def typed(value):
    if type(value) is int:
        return {"type": "int", "value": value}
    if type(value) is float:
        return {"type": "float", "value": value}
    if type(value) is str:
        return {"type": "str", "value": value}
    raise TypeError(f"unexpected legacy scalar: {type(value).__name__}")


async def measure(fixtures):
    if homeassistant.const.__version__ != "2026.10.1":
        raise RuntimeError("wrong Home Assistant Core version")
    if version("homeassistant") != homeassistant.const.__version__:
        raise RuntimeError("Home Assistant package/constant version mismatch")
    if sys.version_info[:3] != (3, 14, 6) or version("jinja2") != "3.1.6":
        raise RuntimeError("wrong pinned Python/Jinja runtime")
    if set(fixtures) != {"clock_utc", "timezone", "states", "cases"}:
        raise ValueError("unexpected fixture fields")
    if fixtures["states"] != [] or tuple(row["id"] for row in fixtures["cases"]) != EXPECTED_IDS:
        raise ValueError("worker admits exactly three absent-source historical cases")
    if any(set(row) != {"id", "template"} or type(row["template"]) is not str for row in fixtures["cases"]):
        raise ValueError("unexpected case fields")

    fixed_utc = datetime.fromisoformat(fixtures["clock_utc"])
    if fixed_utc.tzinfo is None or fixed_utc.utcoffset().total_seconds() != 0:
        raise ValueError("fixture clock must have explicit UTC offset")
    zone = ZoneInfo(fixtures["timezone"])
    hass = HomeAssistant("/tmp/nha107")
    await hass.config.async_set_time_zone(fixtures["timezone"])
    if hass.config.time_zone != zone.key:
        raise RuntimeError("Home Assistant timezone mismatch")

    def fixture_now(time_zone=None):
        return fixed_utc.astimezone(time_zone or zone)

    observations = []
    with patch.object(dt_util, "now", fixture_now), patch.object(dt_util, "utcnow", lambda: fixed_utc):
        for row in fixtures["cases"]:
            template = Template(row["template"], hass)
            template.ensure_valid()
            rendered = template.async_render(parse_result=False)
            native = template.async_render(parse_result=True)
            if type(rendered) is not str:
                raise TypeError("HA parse_result=False did not return text")
            observations.append({"id": row["id"], "raw": {"text": rendered, "native": typed(native)}})
    return {
        "runtime": {
            "python": ".".join(map(str, sys.version_info[:3])),
            "homeassistant": homeassistant.const.__version__,
            "jinja2": version("jinja2"),
        },
        "cases": observations,
    }


if __name__ == "__main__":
    print(json.dumps(asyncio.run(measure(json.load(sys.stdin))), sort_keys=True))
