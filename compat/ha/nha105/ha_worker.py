"""Run real HA/Astral functions; stdin contains synthetic fixtures exclusively."""

from datetime import date, datetime, timedelta, timezone
import hashlib
from importlib import metadata
import json
from pathlib import Path
import platform
from types import SimpleNamespace
from unittest.mock import patch
from zoneinfo import TZPATH, ZoneInfo

import astral
import astral.sun
from homeassistant.components.sun import entity as sun_entity
from homeassistant.helpers import sun as sun_helper
from homeassistant.util import dt as dt_util
import pytz


EVENTS = (
    ("dawn", "dawn"),
    ("dusk", "dusk"),
    ("midnight", "midnight"),
    ("noon", "noon"),
    ("rising", "sunrise"),
    ("setting", "sunset"),
)


def present(value):
    return {"kind": "present", "instant_utc": value.astimezone(timezone.utc).isoformat()}


def absent(reason):
    return {"kind": "absent", "reason": reason}


def next_event(observer, event, after):
    try:
        return present(sun_helper.get_observer_astral_event_next(observer, event, after))
    except ValueError as err:
        # Only the bounded helper's exhaustion is translated. Unexpected input
        # or numerical exceptions fail the probe rather than forge an absence.
        if not str(err).startswith("Unable to find event after one year"):
            raise
        return absent("no_event_within_search_horizon")


class FakeBus:
    def async_listen(self, *_args):
        return lambda: None


def calculate_sun(hass, after):
    timers = []

    def record_timer(_hass, _callback, point):
        timers.append(point)
        return lambda: None

    # Calculations and state/attribute access below are real HA methods. Only
    # their clock, notification, state sink and timer side effects are replaced.
    with (
        patch.object(dt_util, "utcnow", return_value=after),
        patch.object(sun_entity, "async_dispatcher_send"),
        patch.object(sun_entity.event, "async_track_point_in_utc_time", side_effect=record_timer),
        patch.object(sun_entity.Sun, "async_write_ha_state"),
    ):
        sun = sun_entity.Sun(hass)
        try:
            try:
                sun.update_location(initial=True)
            except ValueError as err:
                if not str(err).startswith("Unable to find event after one year"):
                    raise
                return {"error": "no_event_within_search_horizon"}
            attrs = sun.extra_state_attributes
            result = {
                "state": sun.state,
                "elevation": attrs["elevation"],
                "azimuth": attrs["azimuth"],
                "rising": attrs["rising"],
                "phase": sun.phase,
                "next_change": sun._next_change.isoformat(),
                "next": [present(getattr(sun, f"next_{name}")) for name, _ in EVENTS],
            }
            position_timers = [point for point in timers if point < sun._next_change]
            if position_timers:
                if len(position_timers) != 1:
                    raise AssertionError("Sun installed more than one position timer")
                result["next_position"] = position_timers[0].isoformat()
            return result
        finally:
            sun.remove_listeners()


def calculate_case(fixture):
    zone = ZoneInfo(fixture["timezone"])
    dt_util.set_default_time_zone(zone)
    hass = SimpleNamespace(
        config=SimpleNamespace(
            latitude=fixture["latitude"],
            longitude=fixture["longitude"],
            elevation=fixture.get("elevation_m", 0),
            time_zone=zone,
        ),
        bus=FakeBus(),
        data={},
    )
    observer = sun_helper.get_astral_observer(hass)
    after = datetime.fromisoformat(fixture["now_utc"]).astimezone(timezone.utc)
    requested_date = date.fromisoformat(fixture["date"])
    result = {"id": fixture["id"], "date": requested_date.isoformat(), "events": []}
    if fixture.get("after_event"):
        event = dict(EVENTS)[fixture["after_event"]]
        anchor = sun_helper.get_observer_astral_event_next(observer, event, after)
        result["anchor_utc"] = anchor.isoformat()
        after = anchor + timedelta(microseconds=fixture.get("after_shift_us", 0))
    result["after_utc"] = after.isoformat()
    for name, event in EVENTS:
        value = sun_helper.get_astral_event_date(hass, event, requested_date)
        result["events"].append({
            "event": name,
            "date": present(value) if value is not None else absent("no_transit_on_date"),
            "next": next_event(observer, event, after),
        })
    # Keep direct primary-library position evidence even when HA's full entity
    # raises while searching an unreachable extra phase. This does not replace
    # the entity's real outcome or introduce a second astronomical algorithm.
    elevation = round(astral.sun.elevation(observer, after), 2)
    azimuth = round(astral.sun.azimuth(observer, after), 2)
    next_by_name = {item["event"]: item["next"] for item in result["events"]}
    result["position"] = {
        "elevation": elevation,
        "azimuth": azimuth,
        "state": sun_entity.Sun.state.fget(SimpleNamespace(solar_elevation=elevation)),
        "rising": datetime.fromisoformat(next_by_name["noon"]["instant_utc"]) < datetime.fromisoformat(next_by_name["midnight"]["instant_utc"]),
    }
    result["sun"] = calculate_sun(hass, after)
    return result


def distribution_version(name):
    try:
        return metadata.version(name)
    except metadata.PackageNotFoundError:
        return None


def source_hash(module):
    return hashlib.sha256(Path(module.__file__).read_bytes()).hexdigest()


def timezone_metadata():
    result = {"paths": list(TZPATH), "tzdata_package": distribution_version("tzdata")}
    for root in TZPATH:
        version_file = Path(root) / "tzdata.zi"
        if version_file.is_file():
            data = version_file.read_bytes()
            result["system_data_sha256"] = hashlib.sha256(data).hexdigest()
            result["system_version_header"] = data.splitlines()[0].decode("ascii")
            break
    return result


fixtures = json.loads(__import__("sys").stdin.read())
if fixtures.get("profile") != "ha-2026.10/core-1" or not 1 <= len(fixtures.get("cases", [])) <= 64:
    raise ValueError("unsupported synthetic fixture profile or count")
ha_version = distribution_version("homeassistant")
astral_version = distribution_version("astral")
if (ha_version, astral_version) != ("2026.10.1", "2.2"):
    raise RuntimeError("HA or Astral version differs from the qualified profile")
if source_hash(astral.sun) != "ed614bed7553004ef4686e43d916783fb749f49710a419820bccca36f211a9e6":
    raise RuntimeError("loaded Astral sun source differs from the qualified wheel")
print(json.dumps({
    "python": platform.python_version(),
    "homeassistant": ha_version,
    "astral": astral_version,
    "pytz": pytz.__version__,
    "astral_sun_sha256": source_hash(astral.sun),
    "ha_sun_helper_sha256": source_hash(sun_helper),
    "ha_sun_entity_sha256": source_hash(sun_entity),
    "timezone_data": timezone_metadata(),
    "cases": [calculate_case(fixture) for fixture in fixtures["cases"]],
}, allow_nan=False))
