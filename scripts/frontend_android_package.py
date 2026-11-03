"""Package the small native Activity bridge using the existing Android SDK.

cargo-apk0.10 has no DEX or Service/property support. Preserve its generated
manifest attributes and native payload, append the private service and pinned
native trust helper, and re-sign with the same configured identity after alignment.
"""
from dataclasses import dataclass
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile
import tomllib
import zipfile
import urllib.request
import xml.etree.ElementTree as ET


ACTIVITY_NAME = "ca.homeward.platform.HomewardActivity"
RECEPTION_SERVICE = "ca.homeward.platform.HomewardReceptionService"
ANDROID_NAMESPACE = 'http://schemas.android.com/apk/res/android'
TRUST_RESOURCE = 'res/xml/network_security_config.xml'

# Reviewed together: the JVM helper version is the *android* crate version,
# not the verifier crate version. Reject lockfile upgrades until these pins and
# the helper manifest/resource/runtime dependencies have been reviewed again.
NATIVE_TRUST_CRATES = {
    'rustls-platform-verifier': ('0.7.1', '1167586491e2b18b8bfbb293e8180ec17c201c4f076d7cb3070ca964e7598f98'),
    'rustls-platform-verifier-android': ('0.2.0', 'eec689c0bc40ff2458a5977b6619cb718087084a18e02a131c599b62d05e1a5f'),
}
VERIFIER_MAVEN = ('https://raw.githubusercontent.com/rustls/rustls-platform-verifier/'
                  '1aa691352a5e0c215210dfc9a3c2f2078639dc91/android-release-support/maven/'
                  'org/rustls/rustls-platform-verifier/0.2.0/')
MAVEN_CENTRAL = 'https://repo.maven.apache.org/maven2/'


@dataclass(frozen=True)
class Artifact:
    url: str
    sha256: str

    @property
    def filename(self):
        return self.url.rsplit('/', 1)[1]


TRUST_HELPER = Artifact(VERIFIER_MAVEN + 'rustls-platform-verifier-0.2.0.aar',
                        'aa021794230fbc2f0be355999e2cf67398dd563de066a2e17001ffbd0b69101b')
# The helper POM omits dependencies. The release's android/gradle/
# libraries.versions.toml and android/rustls-platform-verifier/build.gradle
# require kotlin-stdlib-jdk7:2.2.10. Its Maven POM dependency closure is stdlib
# 2.2.10 and annotations 13.0 (dependencyManagement entries are not dependencies).
TRUST_RUNTIME = (
    Artifact(MAVEN_CENTRAL + 'org/jetbrains/kotlin/kotlin-stdlib-jdk7/2.2.10/kotlin-stdlib-jdk7-2.2.10.jar',
             'cf95eccc6cc784dff479dee0b8834f3584b93292cc5d91972e82f5d7b9e13e3b'),
    Artifact(MAVEN_CENTRAL + 'org/jetbrains/kotlin/kotlin-stdlib/2.2.10/kotlin-stdlib-2.2.10.jar',
             '9c67cc79efd6b9215b49d2a4308f5f3433537376c7c88e89bdd6729bd096e61a'),
    Artifact(MAVEN_CENTRAL + 'org/jetbrains/annotations/13.0/annotations-13.0.jar',
             'ace2a10dc8e2d5fd34925ecac03e4988b2c0f851650c94b8cef49ba1bd111478'),
)


def verify_native_trust_lock(root):
    packages = tomllib.loads((root / 'Cargo.lock').read_text())['package']
    for name, (version, checksum) in NATIVE_TRUST_CRATES.items():
        matches = [package for package in packages if package['name'] == name]
        if (len(matches) != 1 or matches[0].get('version') != version
                or matches[0].get('checksum') != checksum
                or matches[0].get('source') != 'registry+https://github.com/rust-lang/crates.io-index'):
            raise RuntimeError(f'Review Android native trust packaging pins for {name} in Cargo.lock')


def cached_artifact(artifact, cache):
    cache.mkdir(parents=True, exist_ok=True)
    path = cache / artifact.filename
    if path.exists():
        if hashlib.sha256(path.read_bytes()).hexdigest() != artifact.sha256:
            raise RuntimeError(f'Android dependency checksum mismatch: {path}')
        return path
    # Never publish a partial or unverified download to the shared cache.
    # Versions/URLs/hashes are fixed above; no remote metadata selects code.
    with tempfile.NamedTemporaryFile(prefix=artifact.filename + '.', dir=cache, delete=False) as output:
        temporary = Path(output.name)
        try:
            with urllib.request.urlopen(artifact.url, timeout=30) as response:
                if not response.url.startswith('https://'):
                    raise RuntimeError('Android dependency download left HTTPS')
                body = response.read(8 * 1024 * 1024 + 1)
            if len(body) > 8 * 1024 * 1024 or hashlib.sha256(body).hexdigest() != artifact.sha256:
                raise RuntimeError(f'Android dependency checksum mismatch: {artifact.filename}')
            output.write(body)
            output.close()
            temporary.replace(path)
        finally:
            temporary.unlink(missing_ok=True)
    return path


def prepare_native_trust(root, stage):
    verify_native_trust_lock(root)
    user = Path(os.environ.get('ANDROID_USER_HOME', str(Path.home() / '.android')))
    cache = user / 'cache/homeward-maven'
    helper = cached_artifact(TRUST_HELPER, cache)
    runtime = [cached_artifact(artifact, cache) for artifact in TRUST_RUNTIME]
    with zipfile.ZipFile(helper) as archive:
        # Extract selected files only. No native libraries, arbitrary resources,
        # or manifest fragments from the AAR can enter the application.
        jar = stage / 'rustls-platform-verifier.jar'
        jar.write_bytes(archive.read('classes.jar'))
        resource = stage / TRUST_RESOURCE
        resource.parent.mkdir(parents=True)
        resource.write_bytes(archive.read(TRUST_RESOURCE))
    return [jar, *runtime], resource


def add_native_trust_manifest(application):
    name = '{' + ANDROID_NAMESPACE + '}networkSecurityConfig'
    if application.get(name) is not None:
        raise RuntimeError('Review existing Android network security configuration before adding native trust')
    # Merge only the helper's reviewed application attribute. The original SDK,
    # permissions and application attributes continue to come from cargo-apk.
    application.set(name, '@xml/network_security_config')


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
    verify_native_trust_lock(root)
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
        trust_jars, trust_resource = prepare_native_trust(root, stage)
        source = root / 'client/platform/android/ca/homeward/platform'
        run([java, '-source', '8', '-target', '8', '-Xlint:-options', '-encoding', 'UTF-8',
             '-classpath', android_jar, '-d', classes,
             source / 'HomewardActivity.java', source / 'HomewardReceptionService.java'])
        run([tools / 'd8', '--min-api', '26', '--lib', android_jar,
             '--release' if profile == 'release' else '--debug', '--output', stage,
             *sorted(classes.rglob('*.class')), *trust_jars])
        dex = stage / 'classes.dex'
        if not dex.is_file() or sorted(stage.glob('classes*.dex')) != [dex]:
            raise RuntimeError('Expected one native Activity/trust DEX')
        # cargo-apk0.10 cannot declare Service/property elements. Preserve its
        # generated XML and append this reviewed private service and the helper's
        # network-security resource. No existing attribute is overridden.
        namespace = ANDROID_NAMESPACE
        ET.register_namespace('android', namespace)
        manifest = ET.parse(apk.parent / 'AndroidManifest.xml')
        application = manifest.getroot().find('application')
        if application is None or application.findall('service'):
            raise RuntimeError('Unexpected generated application/services')
        add_native_trust_manifest(application)
        fragment = ET.parse(root / 'client/platform/android/reception-service.xml').getroot()
        if fragment.tag != 'service' or fragment.get('{' + namespace + '}name') != RECEPTION_SERVICE:
            raise RuntimeError('Unexpected reception service fragment')
        application.append(fragment)
        manifest_path = stage / 'AndroidManifest.xml'
        manifest.write(manifest_path, encoding='utf-8', xml_declaration=True)
        compiled = stage / 'compiled'
        compiled.mkdir()
        run([tools / 'aapt2', 'compile', trust_resource, '-o', compiled])
        resources = stage / 'manifest.apk'
        run([tools / 'aapt2', 'link', '-o', resources, '--manifest', manifest_path, '-I', android_jar,
             compiled / 'xml_network_security_config.xml.flat'])
        with zipfile.ZipFile(resources) as linked:
            if set(linked.namelist()) != {'AndroidManifest.xml', 'resources.arsc', TRUST_RESOURCE}:
                raise RuntimeError('Unexpected resources from native trust packaging')
            compiled_manifest = linked.read('AndroidManifest.xml')
            linked_resources = {name: linked.read(name) for name in ('resources.arsc', TRUST_RESOURCE)}
        unsigned = stage / 'unsigned.apk'
        with zipfile.ZipFile(apk) as old, zipfile.ZipFile(unsigned, 'w') as new:
            if any(name.endswith('.dex') for name in old.namelist()):
                raise RuntimeError('cargo-apk now supplies DEX; review bridge packaging')
            if 'resources.arsc' in old.namelist() or any(name.startswith('res/') for name in old.namelist()):
                raise RuntimeError('Review native trust packaging before introducing application resources')
            for item in old.infolist():
                signature = item.filename.upper()
                if signature.startswith('META-INF/') and (signature.endswith(('.SF', '.RSA', '.DSA', '.EC')) or signature == 'META-INF/MANIFEST.MF'):
                    continue
                new.writestr(item, compiled_manifest if item.filename == 'AndroidManifest.xml' else old.read(item))
            new.write(dex, 'classes.dex', compress_type=zipfile.ZIP_DEFLATED)
            for name, data in linked_resources.items():
                new.writestr(name, data, compress_type=zipfile.ZIP_STORED if name == 'resources.arsc' else zipfile.ZIP_DEFLATED)
        aligned = stage / 'homeward-dev.apk'
        run([tools / 'zipalign', '-P', '16', '-f', '4', unsigned, aligned])
        run([tools / 'apksigner', 'sign', '--ks', key, '--ks-pass', 'env:HOMEWARD_APK_SIGNING_PASSWORD', aligned], env=env)
        if signer(aligned) != original_signer:
            raise RuntimeError('Bridge packaging changed APK signer')
        run([tools / 'zipalign', '-c', '-P', '16', '4', aligned])
        # Only the reviewed manifest, DEX and helper resources may change.
        with zipfile.ZipFile(apk) as old, zipfile.ZipFile(aligned) as new:
            if new.read('AndroidManifest.xml') != compiled_manifest:
                raise RuntimeError('Bridge packaging changed the prepared service manifest')
            for name, data in linked_resources.items():
                if new.read(name) != data:
                    raise RuntimeError('Bridge packaging changed native trust resources')
            for name in old.namelist():
                if name.startswith('lib/'):
                    if old.read(name) != new.read(name):
                        raise RuntimeError('Bridge packaging changed native payload')
        aligned.replace(apk)
        idsig = aligned.with_suffix('.apk.idsig')
        if not idsig.is_file():
            raise RuntimeError('Signed bridge APK has no v4 signature')
        idsig.replace(apk.with_suffix('.apk.idsig'))
