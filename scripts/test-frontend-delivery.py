#!/usr/bin/env python3
"""Source export and refusal boundaries; no compiler, app or device started."""
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec=importlib.util.spec_from_file_location('frontend_delivery',Path(__file__).with_name('frontend-delivery.py'))
delivery=importlib.util.module_from_spec(spec)
spec.loader.exec_module(delivery)


class DeliveryBoundaries(unittest.TestCase):
    def test_clean_export_keeps_internal_licenses_omits_ignored_keys_and_refuses_later_changes(self):
        with tempfile.TemporaryDirectory() as temp:
            base=Path(temp)
            roots={source:base/source.value for source in delivery.Source}
            for root in roots.values():
                root.mkdir()
                (root/'LICENSE').write_text('Fixture license\n')
                (root/'code.txt').write_text('Validated source\n')
                (root/'.gitignore').write_text('signing.keystore\nfrontend.local.json\n')
                (root/'signing.keystore').write_bytes(b'PRIVATE TEST SIGNING MATERIAL MUST NOT BE EXPORTED')
                (root/'crates/component').mkdir(parents=True)
                (root/'crates/component/LICENSE').symlink_to('../../LICENSE')
                subprocess.run(['git','init','-q',root],check=True)
                subprocess.run(['git','-C',root,'add','.'],check=True)
                subprocess.run(['git','-C',root,'-c','user.name=Fixture','-c','user.email=fixture@example.invalid','commit','-qm','Fixture'],check=True)
            home=roots[delivery.Source.HOMEWARD]
            (home/'frontend.local.json').write_text(json.dumps(dict(free_kit=str(roots[delivery.Source.FREE]),pro_kit=str(roots[delivery.Source.PRO]))))
            stage=base/'delivery'
            with patch.object(delivery,'ROOT',home):
                delivery.stage_sources(stage)
                delivery.verify_sources(stage)
                for source in delivery.Source:
                    exported=stage/'sources'/source.value
                    self.assertFalse((exported/'signing.keystore').exists())
                    self.assertFalse((exported/'frontend.local.json').exists())
                    self.assertEqual((exported/'crates/component/LICENSE').read_text(),'Fixture license\n')
                code=stage/'sources/homeward/code.txt'
                code.write_text('Unqualified modified source\n')
                with self.assertRaisesRegex(RuntimeError,'Source tree changed'):
                    delivery.verify_sources(stage)
                code.write_text('Validated source\n')
                artifacts=stage/'artifacts';artifacts.mkdir()
                binary=artifacts/'Homeward.bin';binary.write_bytes(b'validated installable bytes')
                (artifacts/'SHA256SUMS').write_text(delivery.sha(binary)+'  Homeward.bin\n')
                delivery.verify_artifacts(stage)
                binary.write_bytes(b'changed installable bytes')
                with self.assertRaisesRegex(RuntimeError,'Artifact checksum mismatch'):
                    delivery.verify_artifacts(stage)

    def test_source_links_cannot_read_outside_the_export(self):
        with tempfile.TemporaryDirectory() as temp:
            base=Path(temp);root=base/'source';root.mkdir()
            secret=base/'outside-secret';secret.write_text('outside private data')
            (root/'LICENSE').symlink_to('../outside-secret')
            with self.assertRaisesRegex(RuntimeError,'symlink escapes'):
                delivery.files(root)

    def test_existing_output_is_preserved(self):
        with tempfile.TemporaryDirectory() as temp:
            path=Path(temp)/'release';path.mkdir();existing=path/'user-file';existing.write_text('keep')
            with self.assertRaisesRegex(RuntimeError,'Preserve the existing output'):
                delivery.empty_destination(path)
            self.assertEqual(existing.read_text(),'keep')

    def test_embedded_signing_key_is_refused_before_running_a_build(self):
        with tempfile.TemporaryDirectory() as temp:
            stage=Path(temp);key=stage/'private.keystore';key.write_bytes(b'fixture')
            with patch.object(delivery,'execute') as execute:
                with self.assertRaisesRegex(RuntimeError,'outside source and delivery'):
                    delivery.android_app(stage,stage,dict(CARGO_APK_RELEASE_KEYSTORE=str(key),CARGO_APK_RELEASE_KEYSTORE_PASSWORD='fixture'))
                execute.assert_not_called()


if __name__=='__main__':unittest.main()
