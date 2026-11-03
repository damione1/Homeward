// SPDX-License-Identifier: GPL-3.0-or-later
//! Keep a cancelled or multi-finger sequence from becoming a release commit.
use std::collections::BTreeSet;

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub struct TouchId(pub u64);
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Phase {
    Started,
    Moved,
    Ended,
    Cancelled,
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Delivery {
    Propagate,
    CancelAndPropagate,
    CancelAndSuppress,
    Suppress,
}
#[derive(Debug, Default)]
enum Sequence {
    #[default]
    Idle,
    Single(TouchId),
    Suppressed {
        primary: Option<TouchId>,
        fingers: BTreeSet<TouchId>,
    },
}
#[derive(Debug, Default)]
pub struct TouchGuard {
    sequence: Sequence,
}
impl TouchGuard {
    pub fn observe(&mut self, id: TouchId, phase: Phase) -> Delivery {
        match &mut self.sequence {
            Sequence::Idle => match phase {
                Phase::Started => {
                    self.sequence = Sequence::Single(id);
                    Delivery::Propagate
                }
                Phase::Moved | Phase::Ended | Phase::Cancelled => Delivery::Suppress,
            },
            Sequence::Single(primary) => match phase {
                Phase::Started => {
                    self.sequence = Sequence::Suppressed {
                        primary: Some(*primary),
                        fingers: BTreeSet::from([*primary, id]),
                    };
                    Delivery::CancelAndSuppress
                }
                Phase::Moved => {
                    if *primary == id {
                        Delivery::Propagate
                    } else {
                        Delivery::Suppress
                    }
                }
                Phase::Ended | Phase::Cancelled => {
                    if *primary != id {
                        return Delivery::Suppress;
                    }
                    self.sequence = Sequence::Idle;
                    if phase == Phase::Cancelled {
                        Delivery::CancelAndPropagate
                    } else {
                        Delivery::Propagate
                    }
                }
            },
            Sequence::Suppressed { primary, fingers } => match phase {
                Phase::Started => {
                    fingers.insert(id);
                    Delivery::Suppress
                }
                Phase::Moved => Delivery::Suppress,
                Phase::Ended | Phase::Cancelled => {
                    fingers.remove(&id);
                    // Only the primary ever reached Slint. Let its end clean up
                    // TouchState, after cancelling any current pointer owner.
                    let delivery = if *primary == Some(id) {
                        *primary = None;
                        Delivery::CancelAndPropagate
                    } else {
                        Delivery::Suppress
                    };
                    if fingers.is_empty() {
                        self.sequence = Sequence::Idle;
                    }
                    delivery
                }
            },
        }
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn normal_and_cancelled_single_touch_are_distinct() {
        let mut guard = TouchGuard::default();
        let a = TouchId(1);
        assert_eq!(guard.observe(a, Phase::Started), Delivery::Propagate);
        assert_eq!(guard.observe(a, Phase::Moved), Delivery::Propagate);
        assert_eq!(guard.observe(a, Phase::Ended), Delivery::Propagate);
        assert_eq!(guard.observe(a, Phase::Started), Delivery::Propagate);
        assert_eq!(
            guard.observe(a, Phase::Cancelled),
            Delivery::CancelAndPropagate
        );
        assert_eq!(guard.observe(a, Phase::Ended), Delivery::Suppress);
        assert_eq!(guard.observe(a, Phase::Started), Delivery::Propagate);
    }
    #[test]
    fn all_release_orders_and_extra_fingers_stay_suppressed_until_empty() {
        for primary_first in [true, false] {
            let mut guard = TouchGuard::default();
            let a = TouchId(1);
            let b = TouchId(2);
            let c = TouchId(3);
            assert_eq!(guard.observe(a, Phase::Started), Delivery::Propagate);
            assert_eq!(
                guard.observe(b, Phase::Started),
                Delivery::CancelAndSuppress
            );
            assert_eq!(guard.observe(c, Phase::Started), Delivery::Suppress);
            for id in [a, b, c] {
                assert_eq!(guard.observe(id, Phase::Moved), Delivery::Suppress);
            }
            for id in if primary_first { [a, b] } else { [b, a] } {
                assert_eq!(
                    guard.observe(id, Phase::Ended),
                    if id == a {
                        Delivery::CancelAndPropagate
                    } else {
                        Delivery::Suppress
                    }
                );
            }
            assert_eq!(guard.observe(c, Phase::Ended), Delivery::Suppress);
            assert_eq!(guard.observe(a, Phase::Started), Delivery::Propagate);
        }
    }
}
