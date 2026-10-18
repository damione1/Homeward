
import asyncio
from importlib.metadata import version
import json
import sys

import homeassistant.const
from homeassistant.core import HomeAssistant
from homeassistant.helpers.template import Template
import jinja2

def outcome(fn):
    try:
        value = fn()
    except Exception as exc:
        cause = exc.__cause__
        return {
            "status": "error",
            "error": f"{type(exc).__module__}.{type(exc).__name__}",
            "message": str(exc),
            "cause": None if cause is None else {
                "error": f"{type(cause).__module__}.{type(cause).__name__}",
                "message": str(cause),
            },
        }
    if value is None:
        return {"status": "ok", "type": "none", "value": None}
    for kind, value_type in (("bool", bool), ("int", int), ("float", float), ("str", str)):
        if type(value) is value_type:
            return {"status": "ok", "type": kind, "value": value}
    raise TypeError(f"unexpected native result: {type(value).__module__}.{type(value).__name__}")

async def measure(fixtures):
    if homeassistant.const.__version__ != "2026.10.1":
        raise RuntimeError("wrong Home Assistant version")
    if version("homeassistant") != homeassistant.const.__version__:
        raise RuntimeError("package/version mismatch")
    if version("jinja2") != "3.1.6":
        raise RuntimeError("wrong Jinja version")
    hass = HomeAssistant("/tmp/nha176")
    await hass.config.async_set_time_zone("UTC")
    jinja = jinja2.Environment(autoescape=False)
    observations = []
    for case in fixtures["cases"]:
        template = Template(case["template"], hass)
        template.ensure_valid()
        variables = case["variables"]
        ha_text = outcome(lambda: template.async_render(variables=variables, parse_result=False))
        ha_native = outcome(lambda: template.async_render(variables=variables, parse_result=True))
        jinja_text = outcome(lambda: jinja.from_string(case["template"]).render(**variables))
        observations.append({
            "id": case["id"], "ha_text": ha_text,
            "ha_native": ha_native, "jinja_text": jinja_text,
        })
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
