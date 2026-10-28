"""Actual OS power controls, exclusively inside the task-owned AVD context.

Forced Doze is an OS-state proof, not a battery or real-LAN network measurement.
https://developer.android.com/training/monitoring-device-state/doze-standby
"""
from contextlib import contextmanager
from enum import Enum
import json
import re
import time


class IdleState(Enum):
    ACTIVE = 'ACTIVE'
    INACTIVE = 'INACTIVE'
    IDLE_PENDING = 'IDLE_PENDING'
    SENSING = 'SENSING'
    LOCATING = 'LOCATING'
    IDLE = 'IDLE'
    IDLE_MAINTENANCE = 'IDLE_MAINTENANCE'
    QUICK_DOZE_DELAY = 'QUICK_DOZE_DELAY'


class Wakefulness(Enum):
    AWAKE = 'Awake'
    ASLEEP = 'Asleep'
    DREAMING = 'Dreaming'
    DOZING = 'Dozing'


class PowerCycle:
    def __init__(self, device, proof):
        self.device = device
        self.proof = proof

    def observe(self, name):
        idle = self.device('shell', 'dumpsys', 'deviceidle', 'get', 'deep').stdout.strip()
        power = self.device('shell', 'dumpsys', 'power').stdout
        found = re.search(r'^\s*mWakefulness=(\w+)\s*$', power, re.M)
        if found is None:
            raise RuntimeError('Android wakefulness missing')
        value = (IdleState(idle), Wakefulness(found[1]))
        (self.proof / (name + '-power.txt')).write_text(power)
        (self.proof / (name + '.json')).write_text(json.dumps(dict(
            deep_idle=value[0].value, wakefulness=value[1].value,
            monotonic_seconds=time.monotonic()), indent=2) + '\n')
        return value

    def wait(self, name, expected):
        deadline = time.monotonic() + 10
        while True:
            value = self.observe(name)
            if expected(*value):
                return value
            if time.monotonic() >= deadline:
                raise RuntimeError('Android power state missing: ' + name)
            time.sleep(.2)

    @contextmanager
    def idle(self):
        # Preserve a pre-existing power simulation. The caller has already
        # established the fresh owned AVD identity; no USB/other serial is used.
        before = self.observe('power-before')
        if before != (IdleState.ACTIVE, Wakefulness.AWAKE):
            raise RuntimeError('Preserve existing non-active Android power state')
        simulated = False
        try:
            simulated = True
            self.device('shell', 'dumpsys', 'battery', 'unplug')
            self.device('shell', 'input', 'keyevent', 'KEYCODE_SLEEP')
            self.wait('screen-off', lambda _, wake: wake in (Wakefulness.ASLEEP, Wakefulness.DOZING))
            self.device('shell', 'dumpsys', 'deviceidle', 'force-idle', 'deep')
            self.wait('forced-doze', lambda idle, wake:
                      idle is IdleState.IDLE and wake in (Wakefulness.ASLEEP, Wakefulness.DOZING))
            yield self
        finally:
            if simulated:
                # Restore even when the scenario raises. Do not leave an AVD
                # with battery overrides or forced idle after qualification.
                try:
                    self.device('shell', 'dumpsys', 'deviceidle', 'unforce')
                finally:
                    try:
                        self.device('shell', 'dumpsys', 'battery', 'reset')
                    finally:
                        self.device('shell', 'input', 'keyevent', 'KEYCODE_WAKEUP')
                self.wait('power-restored', lambda idle, wake:
                          idle is IdleState.ACTIVE and wake is Wakefulness.AWAKE)
