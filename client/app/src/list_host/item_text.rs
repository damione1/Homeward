//! The one place that reads the sub-item and quantity conventions out of a
//! task summary. The task service stores a flat, ordered list of summaries:
//! a sub-item is a summary that starts with [`NESTED_PREFIX`], and a quantity
//! is a trailing `×N` / `xN` word. Both stay readable in any other client;
//! nothing else in the application looks at the raw text for them.

/// A sub-item summary starts with this marker. Other clients show it as a
/// plain dash, which still reads as an indented entry under the row above.
pub(crate) const NESTED_PREFIX: &str = "- ";

/// A summary parsed once at the projection boundary.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) struct ItemSummary<'a> {
    pub nested: bool,
    /// The summary without the nesting marker and the quantity word.
    pub title: &'a str,
    /// The digits of the trailing quantity word, when the summary ends with one.
    pub quantity: Option<&'a str>,
}

pub(crate) fn parse(summary: &str) -> ItemSummary<'_> {
    let (nested, body) = match summary.strip_prefix(NESTED_PREFIX) {
        Some(rest) => (true, rest),
        None => (false, summary),
    };
    let body = body.trim();
    let (title, quantity) = match body.rsplit_once(' ') {
        Some((head, word)) if !head.trim().is_empty() => match quantity_digits(word) {
            Some(digits) => (head.trim_end(), Some(digits)),
            None => (body, None),
        },
        Some(_) | None => (body, None),
    };
    ItemSummary {
        nested,
        title,
        quantity,
    }
}

/// `×2`, `x2` or `X2` with one to four digits; anything else is part of the title.
fn quantity_digits(word: &str) -> Option<&str> {
    let digits = word
        .strip_prefix('×')
        .or_else(|| word.strip_prefix('x'))
        .or_else(|| word.strip_prefix('X'))?;
    let count = digits.len();
    if (1..=4).contains(&count) && digits.bytes().all(|byte| byte.is_ascii_digit()) {
        Some(digits)
    } else {
        None
    }
}

/// The badge text for a parsed quantity.
pub(crate) fn quantity_badge(digits: &str) -> String {
    format!("×{digits}")
}

/// The same summary marked as a sub-item; unchanged when it already is one.
pub(crate) fn nested(summary: &str) -> String {
    if summary.starts_with(NESTED_PREFIX) {
        summary.to_owned()
    } else {
        format!("{NESTED_PREFIX}{summary}")
    }
}

/// The same summary returned to the top level; unchanged when it already is.
pub(crate) fn top_level(summary: &str) -> String {
    summary
        .strip_prefix(NESTED_PREFIX)
        .unwrap_or(summary)
        .to_owned()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_plain_summary_is_a_top_level_title_without_quantity() {
        assert_eq!(
            parse("Flour"),
            ItemSummary {
                nested: false,
                title: "Flour",
                quantity: None
            }
        );
    }

    #[test]
    fn the_nesting_marker_and_the_quantity_word_are_split_off_the_title() {
        assert_eq!(
            parse("- Instant yeast ×2"),
            ItemSummary {
                nested: true,
                title: "Instant yeast",
                quantity: Some("2")
            }
        );
        assert_eq!(parse("Eggs x12").quantity, Some("12"));
        assert_eq!(parse("Eggs X12").title, "Eggs");
        assert_eq!(quantity_badge("12"), "×12");
    }

    #[test]
    fn a_quantity_needs_a_title_before_it_and_at_most_four_digits() {
        assert_eq!(parse("x2").title, "x2");
        assert_eq!(parse("x2").quantity, None);
        assert_eq!(parse("Rice x12345").quantity, None);
        assert_eq!(parse("Rice x12345").title, "Rice x12345");
        assert_eq!(parse("Xbox").quantity, None);
        assert_eq!(parse("Size x").quantity, None);
    }

    #[test]
    fn nesting_round_trips_and_never_doubles_the_marker() {
        assert_eq!(nested("Flour"), "- Flour");
        assert_eq!(nested("- Flour"), "- Flour");
        assert_eq!(top_level("- Flour"), "Flour");
        assert_eq!(top_level("Flour"), "Flour");
        assert_eq!(top_level("-Flour"), "-Flour");
    }
}
