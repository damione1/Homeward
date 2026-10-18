"""Run NHA-043 fixtures against the real, pinned Home Assistant template helper.

The caller supplies synthetic JSON on stdin. This worker runs in a Docker
container without network access or host mounts; it never starts HA Core.
"""

import asyncio
from datetime import date, datetime
from importlib.metadata import version
import json
import sys
from unittest.mock import patch
from zoneinfo import ZoneInfo

import homeassistant.const
from homeassistant.core import HomeAssistant
from homeassistant.helpers.template import ResultWrapper, Template
from homeassistant.util import dt as dt_util


def typed(value):
    """Keep native type and value separate from the rendered text."""
    if value is None:
        return {"type": "none", "value": None}
    if type(value) is bool:
        return {"type": "bool", "value": value}
    if type(value) is int:
        return {"type": "int", "value": value}
    if type(value) is float:
        return {"type": "float", "value": value}
    if type(value) is str:
        return {"type": "str", "value": value}
    if isinstance(value, (list, tuple)):
        result = {
            "type": type(value).__name__,
            "category": "list" if isinstance(value, list) else "tuple",
            "value": [typed(item) for item in value],
        }
        if isinstance(value, ResultWrapper):
            result["render_result"] = value.render_result
        return result
    if type(value) is date:
        return {"type": "date", "value": value.isoformat()}
    if type(value) is datetime:
        return {"type": "datetime", "value": value.isoformat()}
    raise TypeError(
        f"unsupported HA native result type: {type(value).__module__}."
        f"{type(value).__name__}"
    )


async def measure(fixtures):
    if homeassistant.const.__version__ != "2026.10.1":
        raise RuntimeError("wrong Home Assistant Core version")
    if version("homeassistant") != homeassistant.const.__version__:
        raise RuntimeError("Home Assistant package/constant version mismatch")
    if sys.version_info < (3, 14, 2):
        raise RuntimeError("Python does not meet HA 2026.10.1 minimum")
    if not 10 <= len(fixtures["cases"]) <= 64:
        raise ValueError("expected 10 to 64 cases")

    fixed_utc = datetime.fromisoformat(fixtures["clock_utc"])
    if fixed_utc.tzinfo is None or fixed_utc.utcoffset().total_seconds() != 0:
        raise ValueError("fixture clock must have an explicit UTC offset")
    local_zone = ZoneInfo(fixtures["timezone"])
    hass = HomeAssistant("/tmp/nha043")
    await hass.config.async_set_time_zone(fixtures["timezone"])
    if hass.config.time_zone != local_zone.key:
        raise RuntimeError("Home Assistant timezone was not configured")

    def fixture_now(time_zone=None):
        return fixed_utc.astimezone(time_zone or local_zone)

    with patch.object(dt_util, "now", fixture_now), patch.object(
        dt_util, "utcnow", lambda: fixed_utc
    ):
        for state in fixtures["states"]:
            entity_id = state["entity_id"]
            if not entity_id.split(".", 1)[-1].startswith("nha_"):
                raise ValueError(f"non-synthetic entity ID: {entity_id}")
            hass.states.async_set(entity_id, state["state"], state["attributes"])

        variables = fixtures.get("variables", {})
        if not isinstance(variables, dict):
            raise TypeError("fixture variables must be an object")

        def render(template_text):
            template = Template(template_text, hass)
            template.ensure_valid()  # Bad HA fixture syntax is a test failure.
            text = template.async_render(variables=variables, parse_result=False)
            native = template.async_render(variables=variables, parse_result=True)
            if type(text) is not str:
                raise TypeError("HA parse_result=False did not return text")
            return {"text": text, "native": typed(native)}

        observations = []
        for case in fixtures["cases"]:
            raw = render(case["template"])
            diagnostics = [
                {
                    "mode": diagnostic["mode"],
                    "template": diagnostic["template"],
                    "outcome": render(diagnostic["template"]),
                }
                for diagnostic in case.get("diagnostics", [])
            ]
            observations.append({"id": case["id"], "raw": raw, "diagnostics": diagnostics})

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
