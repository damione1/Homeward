#!/usr/bin/env python3
"""Owned API35 emulator OS adapter proof; no Rust-owner/TLS or physical verdict.

Invoke through frontend.py after a current default-development-signed debug APK build.
"""
import hashlib
import json
import os
from pathlib import Path
import re
import runpy
import shutil
import subprocess
import time
import xml.etree.ElementTree as ET
import zipfile

from frontend_android_package import ACTIVITY_NAME

ROOT = Path(__file__).resolve().parents[1]
CONFIG = json.loads((ROOT / 'frontend.local.json').read_text())
TRANSPORT = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-transport.py'))
PACKAGE = 'ca.homeward.frontend.dev'
PROBE_PACKAGE = 'ca.homeward.qualification.notifications'
PERMISSION = 'android.permission.POST_NOTIFICATIONS'
SDK = Path(os.environ['ANDROID_HOME'])
TOOLS = SDK / 'build-tools/35.0.0'
ANDROID_JAR = SDK / 'platforms/android-35/android.jar'
DEVICE = TRANSPORT['device']


def run(*args, **kwargs):
    return subprocess.run([str(arg) for arg in args], check=True, **kwargs)


def screenshot(path):
    path.write_bytes(subprocess.check_output([
        str(TRANSPORT['ADB']), '-s', TRANSPORT['SERIAL'], 'exec-out', 'screencap', '-p']))


def allow_permission(proof):
    DEVICE('shell', 'uiautomator', 'dump', '/sdcard/homeward-permission.xml', check=False)
    result = DEVICE('shell', 'cat', '/sdcard/homeward-permission.xml', check=False)
    if result.returncode:
        return False
    try:
        tree = ET.fromstring(result.stdout)
    except ET.ParseError:
        return False
    button = next((node for node in tree.iter('node') if node.get('resource-id') ==
                   'com.android.permissioncontroller:id/permission_allow_button'), None)
    if button is None:
        return False
    bounds = list(map(int, re.findall(r'\d+', button.get('bounds', ''))))
    if len(bounds) != 4:
        raise RuntimeError('Malformed OS permission button bounds')
    (proof / 'permission.xml').write_text(result.stdout)
    screenshot(proof / 'permission.png')
    DEVICE('shell', 'input', 'tap', (bounds[0] + bounds[2]) // 2, (bounds[1] + bounds[3]) // 2)
    print('EXPLICIT_OS_ALLOW_TAPPED', flush=True)
    return True


def build_probe(proof):
    classes = proof / 'classes'
    classes.mkdir()
    source = proof / 'NotificationAdapterProbe.java'
    shutil.copyfile(ROOT / 'qualification/frontend/notifications-android/NotificationAdapterProbe.java', source)
    shutil.copyfile(__file__, proof / 'probe-driver.py')
    run(Path(os.environ['JAVA_HOME']) / 'bin/javac', '-source', '8', '-target', '8',
        '-Xlint:-options', '-classpath', ANDROID_JAR, '-d', classes, source,
        ROOT / 'client/platform/android/ca/homeward/platform/HomewardActivity.java')
    run(TOOLS / 'd8', '--min-api', '26', '--lib', ANDROID_JAR, '--debug', '--output', proof,
        *sorted((classes / 'ca/homeward/qualification').glob('*.class')))
    manifest = proof / 'AndroidManifest.xml'
    manifest.write_text(f'''<manifest xmlns:android="http://schemas.android.com/apk/res/android" package="{PROBE_PACKAGE}">
<uses-sdk android:minSdkVersion="26" android:targetSdkVersion="35"/>
<application android:label="Homeward OS qualification"/>
<instrumentation android:name="ca.homeward.qualification.NotificationAdapterProbe" android:targetPackage="{PACKAGE}"/>
</manifest>''')
    unsigned = proof / 'unsigned.apk'
    run(TOOLS / 'aapt', 'package', '-f', '-M', manifest, '-I', ANDROID_JAR, '-F', unsigned)
    with zipfile.ZipFile(unsigned, 'a') as apk:
        apk.write(proof / 'classes.dex', 'classes.dex')
    probe_apk = proof / 'probe.apk'
    run(TOOLS / 'zipalign', '-f', '4', unsigned, probe_apk)
    key = Path(os.environ['ANDROID_USER_HOME']) / 'debug.keystore'
    env = dict(os.environ, HOMEWARD_PROBE_KEY_PASSWORD='android')
    run(TOOLS / 'apksigner', 'sign', '--ks', key, '--ks-pass',
        'env:HOMEWARD_PROBE_KEY_PASSWORD', probe_apk, env=env)
    return probe_apk


def qualify():
    proof = Path(CONFIG['frontend_cache']) / 'nhf023' / ('android-os-adapter-' + str(time.time_ns()))
    proof.mkdir(mode=0o700)
    probe_apk = build_probe(proof)
    app = proof / 'Homeward.apk'
    shutil.copy2(Path(os.environ['CARGO_TARGET_DIR']) / 'debug/apk/homeward-dev.apk', app)
    print('BOOT_OWNED_EMULATOR ' + str(proof), flush=True)
    with TRANSPORT['android'](proof):
        # The context has verified this exact owned AVD/API/ABI before any
        # installation/reset/touch. No physical serial can reach these calls.
        installed = []
        instrument = None
        try:
            for name in (PACKAGE, PROBE_PACKAGE):
                existing = DEVICE('shell', 'pm', 'path', name, check=False)
                if existing.returncode == 0 and 'package:' in existing.stdout:
                    raise RuntimeError('Preserve existing app ' + name)
            DEVICE('install', '--no-streaming', app)
            installed.append(PACKAGE)
            # Android may restore notification channels/permission on reinstall.
            # Reset only the app installed by this invocation on the owned VM.
            cleared = DEVICE('shell', 'pm', 'clear', PACKAGE)
            if cleared.stdout.strip() != 'Success':
                raise RuntimeError('Owned fixture app data not cleared')
            DEVICE('shell', 'pm', 'revoke', PACKAGE, PERMISSION, check=False)
            DEVICE('shell', 'pm', 'clear-permission-flags', PACKAGE, PERMISSION, 'user-set', 'user-fixed')
            DEVICE('install', '--no-streaming', probe_apk)
            installed.append(PROBE_PACKAGE)
            DEVICE('logcat', '-c')
            output = proof / 'instrumentation.log'
            with output.open('w') as log:
                instrument = subprocess.Popen([
                    str(TRANSPORT['ADB']), '-s', TRANSPORT['SERIAL'], 'shell', 'am', 'instrument',
                    '-w', PROBE_PACKAGE + '/ca.homeward.qualification.NotificationAdapterProbe'],
                    stdout=log, stderr=subprocess.STDOUT)
                until = time.monotonic() + 60
                allowed = False
                center_captured = False
                while time.monotonic() < until and instrument.poll() is None:
                    progress = output.read_text()
                    if 'AWAITING_EXPLICIT_OS_PERMISSION' in progress and not allowed:
                        allowed = allow_permission(proof)
                    if 'OS_CENTER_PASS' in progress and not center_captured:
                        DEVICE('shell', 'cmd', 'statusbar', 'expand-notifications')
                        time.sleep(.3)
                        screenshot(proof / 'os-center.png')
                        DEVICE('shell', 'cmd', 'statusbar', 'collapse')
                        center_captured = True
                    time.sleep(.25)
                code = instrument.wait(timeout=10)
            (proof / 'logcat.txt').write_text(DEVICE('logcat', '-d').stdout)
            text = output.read_text()
            print(text, flush=True)
            if not (code == 0 and allowed and center_captured and
                    'PASS_OS_ADAPTER_ONLY' in text and 'failure=' not in text):
                raise RuntimeError('OS adapter proof failed: ' + str(output))
            DEVICE('shell', 'am', 'start', '-n', PACKAGE + '/' + ACTIVITY_NAME)
            time.sleep(1)
            screenshot(proof / 'native-login.png')
            if not DEVICE('shell', 'pidof', PACKAGE).stdout.strip():
                raise RuntimeError('Native Activity did not remain alive')
            with app.open('rb') as stream:
                apk_hash = hashlib.file_digest(stream, 'sha256').hexdigest()
            result = {
                'status': 'PASS_OS_ADAPTER_ONLY',
                'scope': 'Java NotificationManager API35 emulator; native Activity/library loads and opaque click intent accepted; JNI-owner/TLS/physical/background/performance not qualified',
                'apk_sha256': apk_hash,
                'read_only_query': True, 'explicit_permission_dialog': True,
                'two_present_in_os_center': True, 'targeted_removal_preserves_other_message': True,
                'sources': str(proof / 'NotificationAdapterProbe.java'),
                'physical_device': False, 'real_device_actions': 0,
            }
            (proof / 'result.json').write_text(json.dumps(result, indent=2) + '\n')
            print('PASS ' + str(proof), flush=True)
        finally:
            if instrument is not None and instrument.poll() is None:
                instrument.terminate()
                instrument.wait(timeout=10)
            for name in reversed(installed):
                DEVICE('shell', 'am', 'force-stop', name, check=False)
                DEVICE('uninstall', name, check=False)


if __name__ == '__main__':
    qualify()
