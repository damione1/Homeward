#!/usr/bin/env python3
"""Native-trust packaging boundaries, without compilers or device access."""
import hashlib
import io
import os
from pathlib import Path
from types import SimpleNamespace
import tempfile
import unittest
from unittest.mock import patch
import xml.etree.ElementTree as ET
import zipfile

import frontend_android_package as package


def locked(root):
    text = ''
    for name, (version, checksum) in package.NATIVE_TRUST_CRATES.items():
        text += (f'[[package]]\nname = "{name}"\nversion = "{version}"\n'
                 f'source = "registry+https://github.com/rust-lang/crates.io-index"\nchecksum = "{checksum}"\n')
    (root / 'Cargo.lock').write_text(text)


class NativeTrustTests(unittest.TestCase):
    def test_lock_upgrade_missing_duplicate_and_nonregistry_helper_fail_closed(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            locked(root)
            package.verify_native_trust_lock(root)
            original = (root / 'Cargo.lock').read_text()
            for invalid in (original.replace('0.2.0', '0.2.1'),
                            '[[package]]' + original.split('[[package]]')[1],
                            original + original,
                            original.replace(package.NATIVE_TRUST_CRATES['rustls-platform-verifier-android'][1], '0' * 64),
                            original.replace('registry+https://github.com/rust-lang/crates.io-index', 'git+https://example.invalid')):
                with self.subTest(invalid=invalid):
                    (root / 'Cargo.lock').write_text(invalid)
                    with self.assertRaisesRegex(RuntimeError, 'packaging pins'):
                        package.verify_native_trust_lock(root)

    def test_download_and_cached_bytes_are_verified_and_failed_download_is_not_published(self):
        body = b'reviewed dependency'
        artifact = package.Artifact('https://example.invalid/dependency.jar', hashlib.sha256(body).hexdigest())
        with tempfile.TemporaryDirectory() as temp:
            cache = Path(temp)
            response = io.BytesIO(body)
            response.url = artifact.url
            with patch.object(package.urllib.request, 'urlopen', return_value=response) as download:
                path = package.cached_artifact(artifact, cache)
                self.assertEqual(path.read_bytes(), body)
                self.assertEqual(package.cached_artifact(artifact, cache), path)
                download.assert_called_once()
            path.write_bytes(b'changed cached code')
            with patch.object(package.urllib.request, 'urlopen') as download:
                with self.assertRaisesRegex(RuntimeError, 'checksum mismatch'):
                    package.cached_artifact(artifact, cache)
                download.assert_not_called()
            path.unlink()
            response = io.BytesIO(b'unreviewed remote code')
            response.url = artifact.url
            with patch.object(package.urllib.request, 'urlopen', return_value=response):
                with self.assertRaisesRegex(RuntimeError, 'checksum mismatch'):
                    package.cached_artifact(artifact, cache)
            self.assertEqual(list(cache.iterdir()), [])

    def test_unreviewed_lock_is_refused_before_android_tools_or_downloads(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            locked(root)
            lock = root / 'Cargo.lock'
            lock.write_text(lock.read_text().replace('0.2.0', '0.2.1'))
            with patch.object(package.subprocess, 'run') as run, patch.object(package, 'cached_artifact') as download:
                with self.assertRaisesRegex(RuntimeError, 'packaging pins'):
                    package.package_activity(root, {}, 'debug')
                run.assert_not_called()
                download.assert_not_called()

    def test_existing_network_policy_is_not_overridden(self):
        application = ET.Element('application', {'keep': 'original'})
        package.add_native_trust_manifest(application)
        self.assertEqual(application.get('keep'), 'original')
        with self.assertRaisesRegex(RuntimeError, 'existing Android network security'):
            package.add_native_trust_manifest(application)

    def test_complete_jvm_inputs_and_exact_resources_preserve_native_payload_and_signer(self):
        self.package_fixture(change_native=False)

    def test_changed_native_payload_is_refused_before_replacing_original_apk(self):
        self.package_fixture(change_native=True)

    def package_fixture(self, change_native):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            locked(root)
            stage = root / 'target/debug/apk'
            stage.mkdir(parents=True)
            apk = stage / 'homeward-dev.apk'
            native = b'unchanged rust library'
            with zipfile.ZipFile(apk, 'w') as archive:
                archive.writestr('AndroidManifest.xml', b'original manifest')
                archive.writestr('lib/arm64-v8a/libhomeward_app.so', native)
                archive.writestr('assets/product', b'original asset')
                archive.writestr('META-INF/CERT.RSA', b'original signature')
            (stage / 'AndroidManifest.xml').write_text(
                '<manifest xmlns:android="http://schemas.android.com/apk/res/android" package="ca.homeward">'
                '<application android:label="Homeward"><activity android:name="OriginalActivity" /></application></manifest>')
            source = root / 'client/platform/android'
            source.mkdir(parents=True)
            (source / 'reception-service.xml').write_text(
                '<service xmlns:android="http://schemas.android.com/apk/res/android" '
                f'android:name="{package.RECEPTION_SERVICE}" android:exported="false" />')
            helper = root / package.TRUST_HELPER.filename
            with zipfile.ZipFile(helper, 'w') as archive:
                archive.writestr('classes.jar', b'helper classes')
                archive.writestr(package.TRUST_RESOURCE, b'<network-security-config/>')
                archive.writestr('lib/evil.so', b'must not be copied')
                archive.writestr('../escaped', b'must not be extracted')
            runtime = [root / artifact.filename for artifact in package.TRUST_RUNTIME]
            for path in runtime:
                path.write_bytes(b'pinned runtime fixture')
            artifacts = {package.TRUST_HELPER: helper, **dict(zip(package.TRUST_RUNTIME, runtime))}
            d8_inputs = []

            def run(arguments, **kwargs):
                args = [str(arg) for arg in arguments]
                tool = Path(args[0]).name
                if tool == 'javac':
                    (Path(args[args.index('-d') + 1]) / 'HomewardActivity.class').write_bytes(b'bridge')
                elif tool == 'd8':
                    d8_inputs.extend(args)
                    (Path(args[args.index('--output') + 1]) / 'classes.dex').write_bytes(b'complete dex')
                elif tool == 'aapt2' and args[1] == 'compile':
                    (Path(args[args.index('-o') + 1]) / 'xml_network_security_config.xml.flat').write_bytes(b'flat xml')
                elif tool == 'aapt2' and args[1] == 'link':
                    manifest = ET.parse(args[args.index('--manifest') + 1])
                    app = manifest.getroot().find('application')
                    self.assertEqual(app.get('{' + package.ANDROID_NAMESPACE + '}label'), 'Homeward')
                    self.assertEqual(app.find('activity').get('{' + package.ANDROID_NAMESPACE + '}name'), 'OriginalActivity')
                    self.assertEqual(app.get('{' + package.ANDROID_NAMESPACE + '}networkSecurityConfig'), '@xml/network_security_config')
                    with zipfile.ZipFile(args[args.index('-o') + 1], 'w') as archive:
                        archive.writestr('AndroidManifest.xml', b'linked manifest')
                        archive.writestr('resources.arsc', b'linked resource table')
                        archive.writestr(package.TRUST_RESOURCE, b'linked network policy')
                elif tool == 'zipalign' and '-f' in args:
                    with zipfile.ZipFile(args[-2]) as old, zipfile.ZipFile(args[-1], 'w') as new:
                        for entry in old.infolist():
                            data = old.read(entry)
                            if change_native and entry.filename.startswith('lib/'):
                                data = b'changed native bytes'
                            new.writestr(entry, data)
                elif tool == 'apksigner' and args[1] == 'sign':
                    Path(args[-1] + '.idsig').write_bytes(b'v4 signature')
                return SimpleNamespace(stdout='Signer #1 certificate SHA-256 digest: fixed-identity\n')

            env = {'ANDROID_HOME': str(root / 'sdk'), 'JAVA_HOME': str(root / 'java'),
                   'ANDROID_USER_HOME': str(root / 'user'), 'CARGO_APK_DEV_KEYSTORE': str(root / 'debug.keystore')}
            with patch.dict(os.environ, env), patch.object(package, 'cached_artifact', side_effect=lambda artifact, cache: artifacts[artifact]), patch.object(package.subprocess, 'run', side_effect=run):
                if change_native:
                    with self.assertRaisesRegex(RuntimeError, 'changed native payload'):
                        package.package_activity(root, {'target_directory': root / 'target'}, 'debug')
                else:
                    package.package_activity(root, {'target_directory': root / 'target'}, 'debug')
            for path in runtime:
                self.assertIn(str(path), d8_inputs)
            self.assertEqual({Path(arg).name for arg in d8_inputs if arg.endswith('.jar')},
                             {'android.jar', 'rustls-platform-verifier.jar', *(path.name for path in runtime)})
            with zipfile.ZipFile(apk) as archive:
                self.assertEqual(archive.read('lib/arm64-v8a/libhomeward_app.so'), native)
                self.assertEqual(archive.read('assets/product'), b'original asset')
                if not change_native:
                    self.assertEqual(archive.read(package.TRUST_RESOURCE), b'linked network policy')
                    self.assertNotIn('lib/evil.so', archive.namelist())
                    self.assertEqual(archive.read('classes.dex'), b'complete dex')
            self.assertFalse((root / 'escaped').exists())


if __name__ == '__main__':
    unittest.main()
