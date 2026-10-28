"""Package the small native Activity bridge using the existing Android SDK.

cargo-apk 0.10 packages native libraries but no DEX. Preserve its manifest/native
payload, add the bridge, align again, and sign with the same configured key.
"""
import os
from pathlib import Path
import subprocess
import tempfile
import tomllib
import zipfile


ACTIVITY_NAME = "ca.homeward.platform.HomewardActivity"


def signing(root, profile):
    name = 'dev' if profile == 'debug' else 'release'
    key_env = 'CARGO_APK_' + name.upper() + '_KEYSTORE'
    key = os.environ.get(key_env)
    password = os.environ.get(key_env + '_PASSWORD')
    if key:
        if password is None and name != 'dev':
            raise RuntimeError('Release keystore password is required')
        return Path(key), 'android' if password is None else password
    metadata = tomllib.loads((root / 'client/app/Cargo.toml').read_text())['package']['metadata']['android']
    configured = metadata.get('signing', {}).get(name)
    if configured:
        return root / 'client/app' / configured['path'], configured['keystore_password']
    if name != 'dev':
        raise RuntimeError('Explicit release signing configuration is required')
    user = Path(os.environ.get('ANDROID_USER_HOME', str(Path.home() / '.android')))
    return user / 'debug.keystore', 'android'


def package_activity(root, metadata, profile):
    android = Path(os.environ['ANDROID_HOME'])
    tools = android / 'build-tools/35.0.0'
    android_jar = android / 'platforms/android-35/android.jar'
    java = Path(os.environ['JAVA_HOME']) / 'bin/javac'
    target = Path(metadata['target_directory'])
    apk = target / profile / 'apk/homeward-dev.apk'
    key, password = signing(root, profile)
    env = dict(os.environ, HOMEWARD_APK_SIGNING_PASSWORD=password)

    def run(argv, **kwargs):
        return subprocess.run([str(arg) for arg in argv], check=True, **kwargs)

    def signer(path):
        output = run([tools / 'apksigner', 'verify', '--print-certs', path], capture_output=True, text=True).stdout
        digest = [line for line in output.splitlines() if 'certificate SHA-256 digest:' in line]
        if len(digest) != 1:
            raise RuntimeError('Expected one APK signer')
        return digest

    original_signer = signer(apk)
    with tempfile.TemporaryDirectory(prefix='native-activity-', dir=apk.parent) as stage:
        stage = Path(stage)
        classes = stage / 'classes'
        classes.mkdir()
        source = root / 'client/platform/android/ca/homeward/platform/HomewardActivity.java'
        run([java, '-source', '8', '-target', '8', '-Xlint:-options', '-encoding', 'UTF-8',
             '-classpath', android_jar, '-d', classes, source])
        run([tools / 'd8', '--min-api', '26', '--lib', android_jar,
             '--release' if profile == 'release' else '--debug', '--output', stage,
             *sorted(classes.rglob('*.class'))])
        dex = stage / 'classes.dex'
        if not dex.is_file():
            raise RuntimeError('Native Activity DEX missing')
        unsigned = stage / 'unsigned.apk'
        with zipfile.ZipFile(apk) as old, zipfile.ZipFile(unsigned, 'w') as new:
            if 'classes.dex' in old.namelist():
                raise RuntimeError('cargo-apk now supplies DEX; review bridge packaging')
            for item in old.infolist():
                signature = item.filename.upper()
                if signature.startswith('META-INF/') and (signature.endswith(('.SF', '.RSA', '.DSA', '.EC')) or signature == 'META-INF/MANIFEST.MF'):
                    continue
                new.writestr(item, old.read(item))
            new.write(dex, 'classes.dex', compress_type=zipfile.ZIP_DEFLATED)
        aligned = stage / 'homeward-dev.apk'
        run([tools / 'zipalign', '-P', '16', '-f', '4', unsigned, aligned])
        run([tools / 'apksigner', 'sign', '--ks', key, '--ks-pass', 'env:HOMEWARD_APK_SIGNING_PASSWORD', aligned], env=env)
        if signer(aligned) != original_signer:
            raise RuntimeError('Bridge packaging changed APK signer')
        run([tools / 'zipalign', '-c', '-P', '16', '4', aligned])
        # Compare the actual manifest and native libraries, not just filenames.
        with zipfile.ZipFile(apk) as old, zipfile.ZipFile(aligned) as new:
            for name in old.namelist():
                if name == 'AndroidManifest.xml' or name.startswith('lib/'):
                    if old.read(name) != new.read(name):
                        raise RuntimeError('Bridge packaging changed native payload')
        aligned.replace(apk)
        idsig = aligned.with_suffix('.apk.idsig')
        if not idsig.is_file():
            raise RuntimeError('Signed bridge APK has no v4 signature')
        idsig.replace(apk.with_suffix('.apk.idsig'))
