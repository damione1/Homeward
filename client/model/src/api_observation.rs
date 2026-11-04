//! Process-local observation relations. Parsing grants no read authority.
use crate::api::{AdmissionError, HomeName, resource_name};

#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub struct RuntimeControllerName(String);
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub struct RuntimeRunName(String);
impl RuntimeControllerName {
    pub fn as_str(&self) -> &str {
        &self.0
    }
    pub fn home(&self) -> HomeName {
        HomeName::try_from(
            self.0
                .split("/runtimeControllers/")
                .next()
                .expect("admitted name"),
        )
        .expect("admitted home")
    }
}
impl TryFrom<&str> for RuntimeControllerName {
    type Error = AdmissionError;
    fn try_from(value: &str) -> Result<Self, Self::Error> {
        resource_name(value, &["homes", "runtimeControllers"])?;
        let (_, id) = value
            .split_once("/runtimeControllers/c-")
            .ok_or(AdmissionError::ResourceName)?;
        if id.len() != 52 {
            return Err(AdmissionError::ResourceName);
        }
        let mut bytes = [0u8; 32];
        let mut buffer = 0u16;
        let mut bits = 0;
        let mut offset = 0;
        for char in id.bytes() {
            let n = match char {
                b'a'..=b'z' => char - b'a',
                b'2'..=b'7' => char - b'2' + 26,
                _ => return Err(AdmissionError::ResourceName),
            };
            buffer = (buffer << 5) | u16::from(n);
            bits += 5;
            if bits >= 8 {
                bits -= 8;
                bytes[offset] = (buffer >> bits) as u8;
                offset += 1;
                buffer &= (1 << bits) - 1;
            }
        }
        if buffer != 0 || bytes[..16].iter().all(|v| *v == 0) || bytes[16..].iter().all(|v| *v == 0)
        {
            return Err(AdmissionError::ResourceName);
        }
        Ok(Self(value.to_owned()))
    }
}
impl RuntimeRunName {
    pub fn as_str(&self) -> &str {
        &self.0
    }
    pub fn controller(&self) -> RuntimeControllerName {
        RuntimeControllerName::try_from(self.0.split("/runs/").next().expect("admitted name"))
            .expect("admitted controller")
    }
}
impl TryFrom<&str> for RuntimeRunName {
    type Error = AdmissionError;
    fn try_from(value: &str) -> Result<Self, Self::Error> {
        resource_name(value, &["homes", "runtimeControllers", "runs"])?;
        let (controller, id) = value
            .split_once("/runs/r-")
            .ok_or(AdmissionError::ResourceName)?;
        RuntimeControllerName::try_from(controller)?;
        if id.starts_with('0')
            || !id
                .bytes()
                .all(|b| b.is_ascii_digit() || b.is_ascii_lowercase())
            || u64::from_str_radix(id, 36).ok().is_none_or(|v| v == 0)
        {
            return Err(AdmissionError::ResourceName);
        }
        Ok(Self(value.to_owned()))
    }
}
