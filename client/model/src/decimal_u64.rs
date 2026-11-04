//! Exact native positions, ordered numerically and encoded as canonical decimal.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Ord, PartialOrd, Hash)]
pub struct DecimalU64(u64);
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct InvalidDecimalU64;
impl TryFrom<&str> for DecimalU64 {
    type Error = InvalidDecimalU64;
    fn try_from(value: &str) -> Result<Self, Self::Error> {
        if value.is_empty()
            || value.len() > 20
            || (value.len() > 1 && value.starts_with('0'))
            || !value.bytes().all(|v| v.is_ascii_digit())
        {
            return Err(InvalidDecimalU64);
        }
        value
            .parse::<u64>()
            .map(Self)
            .map_err(|_| InvalidDecimalU64)
    }
}
impl DecimalU64 {
    pub const fn from_native(value: u64) -> Self {
        Self(value)
    }
    pub const fn value(self) -> u64 {
        self.0
    }
}
impl core::fmt::Display for DecimalU64 {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        self.0.fmt(f)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn canonical_full_width_and_numeric_order() {
        for value in [0, 9, 10, i64::MAX as u64, u64::MAX] {
            let encoded = DecimalU64::from_native(value).to_string();
            assert_eq!(
                DecimalU64::try_from(encoded.as_str()).unwrap().value(),
                value
            );
        }
        for text in [
            "",
            "00",
            "01",
            "-1",
            "+1",
            " 1",
            "1 ",
            "1.0",
            "18446744073709551616",
        ] {
            assert!(DecimalU64::try_from(text).is_err(), "{text}");
        }
        assert!(DecimalU64::try_from("9").unwrap() < DecimalU64::try_from("10").unwrap());
    }
}
