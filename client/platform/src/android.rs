use std::sync::{Arc, Mutex};

use android_activity::AndroidApp;
use base64::{Engine as _, engine::general_purpose::STANDARD};
use home_client_session::{CredentialKey, SecureStore, StoreFailure};
use jni::{
    JNIEnv, JavaVM,
    objects::{GlobalRef, JByteArray, JObject, JString, JValue},
};
use zeroize::Zeroizing;

const MAGIC: &[u8; 8] = b"HWStore1";

/// OS Keystore AES/GCM encrypts the record; only ciphertext goes into private
/// SharedPreferences. Every write/remove checks Editor.commit's boolean.
/// An AndroidApp is required: a shell ELF cannot impersonate the app identity.
pub struct NativeStore {
    vm: Arc<JavaVM>,
    application: GlobalRef,
    namespace: String,
    serial: Mutex<()>,
    _lease: std::fs::File,
}

#[derive(Clone)]
pub struct NativeStoreContext {
    vm: Arc<JavaVM>,
    application: GlobalRef,
    directory: std::path::PathBuf,
}

impl NativeStoreContext {
    pub fn new(app: &AndroidApp) -> Result<Self, StoreFailure> {
        // SAFETY: AndroidApp owns the process VM and activity reference. Take
        // our own Application global reference before releasing this borrow.
        let vm = unsafe { JavaVM::from_raw(app.vm_as_ptr().cast()) }
            .map_err(|_| StoreFailure::Unavailable)?;
        let application = {
            let mut env = vm
                .attach_current_thread()
                .map_err(|_| StoreFailure::Unavailable)?;
            // SAFETY: the borrowed activity reference remains valid while app
            // is borrowed, and JObject itself does not delete that reference.
            let activity = unsafe { JObject::from_raw(app.activity_as_ptr().cast()) };
            let result = env.with_local_frame(8, |env| -> jni::errors::Result<_> {
                let application = env
                    .call_method(
                        &activity,
                        "getApplicationContext",
                        "()Landroid/content/Context;",
                        &[],
                    )?
                    .l()?;
                env.new_global_ref(application)
            });
            if env.exception_check().unwrap_or(true) {
                let _ = env.exception_clear();
                return Err(StoreFailure::Unavailable);
            }
            result.map_err(|_| StoreFailure::Unavailable)?
        };
        Ok(Self {
            vm: Arc::new(vm),
            application,
            directory: app.internal_data_path().ok_or(StoreFailure::Unavailable)?,
        })
    }
    pub fn data_directory(&self) -> &std::path::Path {
        &self.directory
    }
    pub fn store(&self, key: CredentialKey) -> Result<NativeStore, StoreFailure> {
        Ok(NativeStore {
            vm: self.vm.clone(),
            application: self.application.clone(),
            namespace: format!("ca.homeward.session.v1.{}", key.identifier()),
            serial: Mutex::new(()),
            _lease: crate::lease::acquire(&self.directory, key)?,
        })
    }
}

impl NativeStore {
    pub fn new(key: CredentialKey, app: &AndroidApp) -> Result<Self, StoreFailure> {
        NativeStoreContext::new(app)?.store(key)
    }

    fn with_env<T>(
        &self,
        operation: impl FnOnce(&mut JNIEnv<'_>) -> jni::errors::Result<T>,
    ) -> Result<T, StoreFailure> {
        let _serial = self.serial.lock().map_err(|_| StoreFailure::Unavailable)?;
        let mut env = self
            .vm
            .attach_current_thread()
            .map_err(|_| StoreFailure::Unavailable)?;
        let result = env.with_local_frame(64, operation);
        if env.exception_check().unwrap_or(true) {
            let _ = env.exception_clear();
            return Err(StoreFailure::Unavailable);
        }
        result.map_err(|_| StoreFailure::Unavailable)
    }

    fn preferences<'a>(&self, env: &mut JNIEnv<'a>) -> jni::errors::Result<JObject<'a>> {
        let name = env.new_string(&self.namespace)?;
        env.call_method(
            self.application.as_obj(),
            "getSharedPreferences",
            "(Ljava/lang/String;I)Landroid/content/SharedPreferences;",
            &[JValue::Object(name.as_ref()), JValue::Int(0)],
        )?
        .l()
    }

    fn keystore<'a>(&self, env: &mut JNIEnv<'a>) -> jni::errors::Result<JObject<'a>> {
        let provider = env.new_string("AndroidKeyStore")?;
        let store = env
            .call_static_method(
                "java/security/KeyStore",
                "getInstance",
                "(Ljava/lang/String;)Ljava/security/KeyStore;",
                &[JValue::Object(provider.as_ref())],
            )?
            .l()?;
        env.call_method(
            &store,
            "load",
            "(Ljava/io/InputStream;[C)V",
            &[
                JValue::Object(&JObject::null()),
                JValue::Object(&JObject::null()),
            ],
        )?;
        Ok(store)
    }

    fn key<'a>(&self, env: &mut JNIEnv<'a>, create: bool) -> jni::errors::Result<JObject<'a>> {
        let store = self.keystore(env)?;
        let alias = env.new_string(&self.namespace)?;
        let key = env
            .call_method(
                &store,
                "getKey",
                "(Ljava/lang/String;[C)Ljava/security/Key;",
                &[
                    JValue::Object(alias.as_ref()),
                    JValue::Object(&JObject::null()),
                ],
            )?
            .l()?;
        if !key.is_null() || !create {
            return Ok(key);
        }
        let builder = env.new_object(
            "android/security/keystore/KeyGenParameterSpec$Builder",
            "(Ljava/lang/String;I)V",
            &[JValue::Object(alias.as_ref()), JValue::Int(3)],
        )?;
        for (method, value) in [
            ("setBlockModes", "GCM"),
            ("setEncryptionPaddings", "NoPadding"),
        ] {
            let array = env.new_object_array(1, "java/lang/String", JObject::null())?;
            let value = env.new_string(value)?;
            env.set_object_array_element(&array, 0, value)?;
            env.call_method(
                &builder,
                method,
                "([Ljava/lang/String;)Landroid/security/keystore/KeyGenParameterSpec$Builder;",
                &[JValue::Object(array.as_ref())],
            )?;
        }
        env.call_method(
            &builder,
            "setKeySize",
            "(I)Landroid/security/keystore/KeyGenParameterSpec$Builder;",
            &[JValue::Int(256)],
        )?;
        let spec = env
            .call_method(
                &builder,
                "build",
                "()Landroid/security/keystore/KeyGenParameterSpec;",
                &[],
            )?
            .l()?;
        let algorithm = env.new_string("AES")?;
        let provider = env.new_string("AndroidKeyStore")?;
        let generator = env
            .call_static_method(
                "javax/crypto/KeyGenerator",
                "getInstance",
                "(Ljava/lang/String;Ljava/lang/String;)Ljavax/crypto/KeyGenerator;",
                &[
                    JValue::Object(algorithm.as_ref()),
                    JValue::Object(provider.as_ref()),
                ],
            )?
            .l()?;
        env.call_method(
            &generator,
            "init",
            "(Ljava/security/spec/AlgorithmParameterSpec;)V",
            &[JValue::Object(&spec)],
        )?;
        env.call_method(&generator, "generateKey", "()Ljavax/crypto/SecretKey;", &[])?
            .l()
    }

    fn cipher<'a>(
        &self,
        env: &mut JNIEnv<'a>,
        key: &JObject<'a>,
        iv: Option<&[u8]>,
    ) -> jni::errors::Result<JObject<'a>> {
        let transformation = env.new_string("AES/GCM/NoPadding")?;
        let cipher = env
            .call_static_method(
                "javax/crypto/Cipher",
                "getInstance",
                "(Ljava/lang/String;)Ljavax/crypto/Cipher;",
                &[JValue::Object(transformation.as_ref())],
            )?
            .l()?;
        if let Some(iv) = iv {
            let iv = env.byte_array_from_slice(iv)?;
            let parameters = env.new_object(
                "javax/crypto/spec/GCMParameterSpec",
                "(I[B)V",
                &[JValue::Int(128), JValue::Object(iv.as_ref())],
            )?;
            env.call_method(
                &cipher,
                "init",
                "(ILjava/security/Key;Ljava/security/spec/AlgorithmParameterSpec;)V",
                &[
                    JValue::Int(2),
                    JValue::Object(key),
                    JValue::Object(&parameters),
                ],
            )?;
        } else {
            // The OS provider generates a fresh unpredictable IV for each save.
            env.call_method(
                &cipher,
                "init",
                "(ILjava/security/Key;)V",
                &[JValue::Int(1), JValue::Object(key)],
            )?;
        }
        let aad = env.byte_array_from_slice(self.namespace.as_bytes())?;
        env.call_method(
            &cipher,
            "updateAAD",
            "([B)V",
            &[JValue::Object(aad.as_ref())],
        )?;
        Ok(cipher)
    }
}

impl SecureStore for NativeStore {
    fn load(&self) -> Result<Option<Zeroizing<Vec<u8>>>, StoreFailure> {
        self.with_env(|env| {
            let file = self.preferences(env)?;
            let field = env.new_string("record")?;
            let text = env
                .call_method(
                    &file,
                    "getString",
                    "(Ljava/lang/String;Ljava/lang/String;)Ljava/lang/String;",
                    &[
                        JValue::Object(field.as_ref()),
                        JValue::Object(&JObject::null()),
                    ],
                )?
                .l()?;
            if text.is_null() {
                return Ok(Ok(None));
            }
            let text: String = env.get_string(&JString::from(text))?.into();
            if text.len() > 64 * 1024 {
                return Ok(Err(StoreFailure::Corrupt));
            }
            let bytes = match STANDARD.decode(text) {
                Ok(bytes) if bytes.len() >= 36 && &bytes[..8] == MAGIC => bytes,
                _ => return Ok(Err(StoreFailure::Corrupt)),
            };
            let key = self.key(env, false)?;
            if key.is_null() {
                return Ok(Err(StoreFailure::Corrupt));
            }
            let cipher = self.cipher(env, &key, Some(&bytes[8..20]))?;
            let input = env.byte_array_from_slice(&bytes[20..])?;
            let output = env
                .call_method(
                    &cipher,
                    "doFinal",
                    "([B)[B",
                    &[JValue::Object(input.as_ref())],
                )?
                .l()?;
            let plain = Zeroizing::new(env.convert_byte_array(JByteArray::from(output))?);
            if plain.len() > 32 * 1024 {
                return Ok(Err(StoreFailure::Corrupt));
            }
            Ok(Ok(Some(plain)))
        })?
    }

    fn save(&self, record: &[u8]) -> Result<(), StoreFailure> {
        if record.len() > 32 * 1024 {
            return Err(StoreFailure::Corrupt);
        }
        let committed = self.with_env(|env| {
            let key = self.key(env, true)?;
            let cipher = self.cipher(env, &key, None)?;
            let input = env.byte_array_from_slice(record)?;
            let output = env
                .call_method(
                    &cipher,
                    "doFinal",
                    "([B)[B",
                    &[JValue::Object(input.as_ref())],
                )?
                .l()?;
            let ciphertext = env.convert_byte_array(JByteArray::from(output))?;
            let iv = env.call_method(&cipher, "getIV", "()[B", &[])?.l()?;
            let iv = env.convert_byte_array(JByteArray::from(iv))?;
            if iv.len() != 12 {
                return Ok(false);
            }
            let mut blob = MAGIC.to_vec();
            blob.extend_from_slice(&iv);
            blob.extend_from_slice(&ciphertext);
            let encoded = env.new_string(STANDARD.encode(blob))?;
            let field = env.new_string("record")?;
            let file = self.preferences(env)?;
            let edit = env
                .call_method(
                    &file,
                    "edit",
                    "()Landroid/content/SharedPreferences$Editor;",
                    &[],
                )?
                .l()?;
            env.call_method(
                &edit,
                "putString",
                "(Ljava/lang/String;Ljava/lang/String;)Landroid/content/SharedPreferences$Editor;",
                &[
                    JValue::Object(field.as_ref()),
                    JValue::Object(encoded.as_ref()),
                ],
            )?;
            env.call_method(&edit, "commit", "()Z", &[])?.z()
        })?;
        if committed {
            Ok(())
        } else {
            Err(StoreFailure::Unavailable)
        }
    }

    fn clear(&self) -> Result<(), StoreFailure> {
        let committed = self.with_env(|env| {
            let file = self.preferences(env)?;
            let edit = env
                .call_method(
                    &file,
                    "edit",
                    "()Landroid/content/SharedPreferences$Editor;",
                    &[],
                )?
                .l()?;
            let field = env.new_string("record")?;
            env.call_method(
                &edit,
                "remove",
                "(Ljava/lang/String;)Landroid/content/SharedPreferences$Editor;",
                &[JValue::Object(field.as_ref())],
            )?;
            if !env.call_method(&edit, "commit", "()Z", &[])?.z()? {
                return Ok(false);
            }
            let store = self.keystore(env)?;
            let alias = env.new_string(&self.namespace)?;
            env.call_method(
                &store,
                "deleteEntry",
                "(Ljava/lang/String;)V",
                &[JValue::Object(alias.as_ref())],
            )?;
            Ok(true)
        })?;
        if committed {
            Ok(())
        } else {
            Err(StoreFailure::Unavailable)
        }
    }
}
