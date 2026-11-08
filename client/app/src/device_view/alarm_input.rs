//! Ephemeral keypad state is owned by one admitted card generation. Slint sees
//! only digit counts; no code enters projection models, drafts or command IDs.
use super::*;
use zeroize::{Zeroize, Zeroizing};

#[derive(Default)]
pub(super) struct Input {
    pub operation: Option<wire::AlarmOperation>,
    pub digits: Zeroizing<Vec<u8>>,
}
impl DeviceStore {
    pub fn alarm_choice(
        &self,
        token: i32,
        generation: i32,
        revision: i32,
        choice: i32,
    ) -> Result<Option<DeviceIntent>, Refusal> {
        let mut state = self.0.lock().expect("device store mutex");
        let slot = active_slot(&state, token, generation, revision)?.clone();
        let Control::Alarm(alarm) = &slot.control else {
            return Err(Refusal::Unsupported);
        };
        let operation = *usize::try_from(choice)
            .ok()
            .and_then(|index| alarm.operations().get(index))
            .ok_or(Refusal::Unsupported)?;
        let input = state.alarm_inputs.entry(slot.generation).or_default();
        if input.operation != Some(operation) {
            input.digits.zeroize();
        }
        if alarm.requires_code(operation) {
            input.operation = Some(operation);
            Ok(None)
        } else {
            state.alarm_inputs.remove(&slot.generation);
            Ok(Some(DeviceIntent::Alarm(operation)))
        }
    }
    pub fn alarm_digit(
        &self,
        token: i32,
        generation: i32,
        revision: i32,
        digit: Option<i32>,
    ) -> Result<(), Refusal> {
        let mut state = self.0.lock().expect("device store mutex");
        let slot = active_slot(&state, token, generation, revision)?.clone();
        let Control::Alarm(alarm) = &slot.control else {
            return Err(Refusal::Unsupported);
        };
        if !alarm.has_code() {
            return Err(Refusal::Unsupported);
        }
        let input = state.alarm_inputs.entry(slot.generation).or_default();
        if input.operation.is_none() {
            let mut required = alarm
                .operations()
                .iter()
                .copied()
                .filter(|operation| alarm.requires_code(*operation));
            let first = required.next();
            if required.next().is_none() {
                input.operation = first;
            }
        }
        if input.operation.is_none() {
            return Err(Refusal::InvalidValue);
        }
        match digit {
            Some(digit) if (0..=9).contains(&digit) && input.digits.len() < 32 => {
                input.digits.push(b'0' + digit as u8)
            }
            None => {
                if let Some(last) = input.digits.last_mut() {
                    last.zeroize();
                }
                input.digits.pop();
            }
            Some(_) => return Err(Refusal::InvalidValue),
        }
        Ok(())
    }
    pub fn alarm_confirm(
        &self,
        token: i32,
        generation: i32,
        revision: i32,
    ) -> Result<DeviceIntent, Refusal> {
        let state = self.0.lock().expect("device store mutex");
        let slot = active_slot(&state, token, generation, revision)?;
        let input = state
            .alarm_inputs
            .get(&slot.generation)
            .filter(|input| !input.digits.is_empty())
            .ok_or(Refusal::InvalidValue)?;
        input
            .operation
            .map(DeviceIntent::Alarm)
            .ok_or(Refusal::InvalidValue)
    }
}
