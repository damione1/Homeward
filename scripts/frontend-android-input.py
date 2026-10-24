"""Observed screen labels -> actual input on the task-owned API35 emulator.

No Slint model or route is injected. Each selected input preserves its screenshot,
OCR bounds and touch coordinates; the helper never addresses a physical device.
"""
from enum import Enum
import json
import hashlib
import os
from pathlib import Path
import runpy
import shutil
import struct
import subprocess
import time
import unicodedata


class LabelMatch(Enum):
    EXACT = 'exact'
    CONTAINS = 'contains'


def prepare_apk(root, proof, run):
    """Freeze the APK/helper and retire only owned rebuildable outputs."""
    frontend = runpy.run_path(str(root / 'scripts/frontend.py'))
    config = json.loads((root / 'frontend.local.json').read_text())
    target = Path(config['backend_cache']) / 'workspace/target'
    if Path(os.environ['CARGO_TARGET_DIR']).resolve() != target.resolve():
        raise RuntimeError('Use the reserved frontend target')
    run(['python3', 'scripts/build-frontend-android.py'])
    apk = proof / 'Homeward.apk'
    if apk.exists():
        raise RuntimeError('Preserve an existing proof APK')
    packaging = target / 'debug/apk'
    shutil.copyfile(packaging / 'homeward-dev.apk', apk)
    def digest(path):
        with path.open('rb') as source:
            return hashlib.file_digest(source, 'sha256').hexdigest()
    clean = ['cargo', 'clean', '--target', 'aarch64-linux-android']
    for name in frontend['PACKAGES']:
        clean.extend(['-p', name])
    run(clean)
    expected = {'homeward-dev.apk', 'homeward-dev-unaligned.apk', 'homeward-dev.apk.idsig',
                'AndroidManifest.xml', 'lib/arm64-v8a/libhomeward_app.so'}
    assert packaging.is_dir() and not packaging.is_symlink()
    files = list(packaging.rglob('*'))
    assert all(not path.is_symlink() and path.stat().st_uid == os.getuid() for path in files)
    assert {str(path.relative_to(packaging)) for path in files if path.is_file()} == expected
    assert digest(packaging / 'homeward-dev.apk') == digest(apk)
    opened = subprocess.run(['lsof', '-t', '+D', str(packaging)], capture_output=True, timeout=30)
    assert opened.returncode == 1 and not opened.stdout
    shutil.rmtree(packaging)
    ocr = proof / 'navigation-ocr'
    modules = Path(config['frontend_cache']) / 'session-spike/swift-modules'
    modules.mkdir(parents=True, exist_ok=True)
    run(['xcrun', 'swiftc', '-module-cache-path', modules, root / 'qualification/frontend/navigation-ocr.swift', '-o', ocr])
    assert modules.is_dir() and not modules.is_symlink()
    opened = subprocess.run(['lsof', '-t', '+D', str(modules)], capture_output=True, timeout=30)
    assert opened.returncode == 1 and not opened.stdout
    shutil.rmtree(modules)
    (proof / 'android-build.json').write_text(json.dumps({'apk_sha256': digest(apk),
        'ocr_sha256': digest(ocr), 'retired_rebuildable_packaging': str(packaging),
        'cleaned_android_packages': frontend['PACKAGES']}, indent=2) + '\n')
    return apk, ocr


def normalized(text):
    return ''.join(char for char in unicodedata.normalize('NFKD', text).casefold() if char.isalnum())


class AndroidInput:
    def __init__(self, directory, ocr, adb, serial, device):
        if serial != 'emulator-5558':
            raise RuntimeError('Only the task-owned emulator may receive qualification input')
        self.directory, self.ocr, self.adb, self.serial, self.device = directory, ocr, adb, serial, device
        self.sequence = 0

    def capture(self, name):
        path = self.directory / (name + '.png')
        if path.exists():
            raise RuntimeError('Preserve an existing capture')
        result = subprocess.run([str(self.adb), '-s', self.serial, 'exec-out', 'screencap', '-p'], capture_output=True, check=True, timeout=15)
        if not result.stdout.startswith(b'\x89PNG\r\n\x1a\n'):
            raise RuntimeError('Expected an actual emulator PNG screenshot')
        path.write_bytes(result.stdout)
        width, height = struct.unpack('>II', result.stdout[16:24])
        return path, width, height

    def labels(self):
        self.sequence += 1
        path, width, height = self.capture(f'input-{self.sequence:04d}')
        result = subprocess.run([str(self.ocr), str(path)], capture_output=True, text=True, check=True, timeout=30)
        rows = json.loads(result.stdout)
        if not isinstance(rows, list) or len(rows) > 1024:
            raise RuntimeError('Unexpected native OCR result')
        path.with_suffix('.labels.json').write_text(json.dumps(rows, indent=2) + '\n')
        return rows, width, height, path

    def find(self, label, match=LabelMatch.EXACT, timeout=20, scroll=False):
        if not isinstance(match, LabelMatch):
            raise TypeError('Label matching requires its closed policy enum')
        expected = normalized(label)
        deadline = time.monotonic() + timeout
        while True:
            rows, width, height, path = self.labels()
            for row in sorted(rows, key=lambda item: item['confidence'], reverse=True):
                actual = normalized(row['label'])
                match match:
                    case LabelMatch.EXACT:
                        matches = actual == expected
                        confidence = .25
                    case LabelMatch.CONTAINS:
                        matches = expected in actual
                        confidence = .5
                if matches and row['confidence'] >= confidence:
                    return row, width, height, path
            if time.monotonic() >= deadline:
                raise AssertionError('Missing observed Android label: ' + label)
            if scroll:
                start_y = height * 4 // 5
                end_y = start_y - min(96, height // 5)
                self.device('shell', 'input', 'swipe', str(width * 3 // 4), str(start_y), str(width * 3 // 4), str(end_y), '700')
                time.sleep(.35)
            time.sleep(.15)

    def tap(self, label, match=LabelMatch.EXACT, offset_y=0, scroll=False):
        row, width, height, path = self.find(label, match=match, scroll=scroll)
        x, y = round(row['x'] + row['width'] / 2), round(row['y'] + row['height'] / 2 + offset_y)
        if not (0 <= x < width and 0 <= y < height):
            raise RuntimeError('Refuse input outside the observed emulator display')
        path.with_suffix('.tap.json').write_text(json.dumps({'requested': label, 'observed': row, 'x': x, 'y': y, 'display': [width, height]}, indent=2) + '\n')
        self.device('shell', 'input', 'tap', str(x), str(y))
        time.sleep(.2)
        return row

    def navigation(self):
        row, width, height, path = self.find('Homeward')
        # At the qualification's 160dpi, x=40 is within the public leading
        # 48dp header slot; its vertical center follows the observed heading.
        x, y = 40, round(row['y'] + row['height'] / 2)
        if not (x < width and 0 <= y < height):
            raise RuntimeError('Header control is outside the observed display')
        path.with_suffix('.tap.json').write_text(json.dumps({'requested': 'Navigation header slot', 'observed_heading': row, 'x': x, 'y': y, 'display': [width, height]}, indent=2) + '\n')
        self.device('shell', 'input', 'tap', str(x), str(y))
        time.sleep(.2)

    def observe_press(self, label, scroll=False):
        row, width, height, path = self.find(label, scroll=scroll)
        if (width, height) != (1024, 600):
            raise RuntimeError('The partial-opener probe requires its declared tablet format')
        # Place the label near the lower viewport edge by an actual slow drag;
        # the room card's lower portion is then clipped before it gains focus.
        delta = round(height - 80 - row['y'])
        if abs(delta) > 160:
            raise AssertionError('Observed room is too far from the partial-opener probe region')
        if delta:
            start_y = round(row['y'] - 90)  # Noninteractive rooms heading above the tile.
            self.device('shell', 'input', 'swipe', str(width * 3 // 4), str(start_y),
                        str(width * 3 // 4), str(start_y + delta), '700')
            time.sleep(.35)
            row, width, height, path = self.find(label)
        x, y = round(row['x'] + row['width'] / 2), round(row['y'] + row['height'] / 2)
        if not (0 <= x < width and 0 <= y < height):
            raise RuntimeError('Refuse a press outside the owned display')
        # A stationary real touch keeps DOWN active while screencap observes
        # the focus phase; UP still activates the ordinary public card action.
        child = subprocess.Popen([str(self.adb), '-s', self.serial, 'shell', 'input', 'touchscreen',
                                  'swipe', str(x), str(y), str(x), str(y), '2500'],
                                 stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        try:
            time.sleep(.35)
            labels, _, _, pressed_path = self.labels()
            focused = next((item for item in labels if normalized(item['label']) == normalized(label)), None)
            if focused is None:
                raise AssertionError('Pressed opener is missing before native touch release')
            if child.wait(timeout=8) != 0:
                raise AssertionError('Observed native press did not complete')
        finally:
            if child.poll() is None:
                child.terminate()
                child.wait(timeout=5)
        path.with_suffix('.press.json').write_text(json.dumps({'requested': label, 'before_down': row,
            'while_pressed': focused, 'press_capture': pressed_path.name, 'x': x, 'y': y,
            'display': [width, height]}, indent=2) + '\n')
        return row, focused

    def format(self, width, height, name):
        self.device('shell', 'wm', 'size', f'{width}x{height}')
        deadline, attempt = time.monotonic() + 20, 0
        while True:
            attempt += 1
            _, actual_width, actual_height = self.capture(f'{name}-resize-{attempt:02d}')
            if (actual_width, actual_height) == (width, height):
                # WindowManager applies the display mode and then Slint receives
                # its surface resize; allow the latter to render before capture.
                time.sleep(.75)
                path, actual_width, actual_height = self.capture(name)
                if (actual_width, actual_height) != (width, height):
                    raise AssertionError('Android display changed after its observed resize')
                return path, actual_width, actual_height
            if time.monotonic() >= deadline:
                raise AssertionError('Requested Android display format was not applied')
            time.sleep(.2)
