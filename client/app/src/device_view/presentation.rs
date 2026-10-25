//! Transferable projection data. Slint's nested models use Rc and are created
//! only on the UI thread, after the cache publication fence has been checked.
use crate::{
    IconKind, OptionRow, ProActionPhase, ProControlAccess, ProDeviceChoice, ProDeviceKind,
    ProDeviceRow, ProDeviceValue, SelectState,
};
use slint::{Model, ModelRc, SharedString, VecModel};

fn nested<T: Clone + PartialEq + 'static>(rows: &[T], previous: Option<&ModelRc<T>>) -> ModelRc<T> {
    if let Some(previous) = previous
        && let Some(model) = previous.as_any().downcast_ref::<VecModel<T>>()
    {
        if model.row_count() != rows.len() {
            model.set_vec(rows.to_vec());
        } else {
            for (index, row) in rows.iter().enumerate() {
                if model.row_data(index).as_ref() != Some(row) {
                    model.set_row_data(index, row.clone());
                }
            }
        }
        previous.clone()
    } else {
        ModelRc::from(rows)
    }
}

macro_rules! card_row {
    ($($field:ident: $ty:ty),* $(,)?) => {
        #[derive(Clone,Default,Debug)]
        pub(crate) struct CardRow {
            $(pub $field: $ty,)*
            pub values: Vec<ProDeviceValue>,
            pub choices: Vec<ProDeviceChoice>,
            pub select_options: Vec<OptionRow>,
        }
        impl CardRow {
            pub fn ui(&self, previous: Option<&ProDeviceRow>) -> ProDeviceRow {
                let previous=previous.filter(|row|row.token==self.token && row.generation==self.generation);
                ProDeviceRow {
                    $($field:self.$field.to_owned(),)*
                    values:nested(self.values.as_slice(),previous.map(|row|&row.values)),
                    choices:nested(self.choices.as_slice(),previous.map(|row|&row.choices)),
                    select_options:nested(self.select_options.as_slice(),previous.map(|row|&row.select_options)),
                }
            }
        }
    }
}
card_row! {
    token:i32, generation:i32, kind:ProDeviceKind,
    title:SharedString, value:SharedString, unit:SharedString, quality:SharedString, freshness:SharedString,
    icon:IconKind, power_known:bool, power_on:bool,
    brightness_known:bool, brightness_percent:f32,
    brightness_minimum:f32, brightness_maximum:f32, brightness_step:f32,
    can_power:bool, can_brightness:bool, access:ProControlAccess,
    phase:ProActionPhase, action_label:SharedString, action_detail:SharedString,
    readings:SharedString, select_state:SelectState, command_unavailable:bool,
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn refreshing_nested_values_keeps_the_model_until_card_identity_changes() {
        let mut row = CardRow {
            token: 4,
            generation: 7,
            values: vec![ProDeviceValue {
                token: 0,
                lower: "20".into(),
                ..Default::default()
            }],
            ..Default::default()
        };
        let first = row.ui(None);
        row.values[0].lower = "-".into();
        let refreshed = row.ui(Some(&first));
        assert_eq!(
            first.values, refreshed.values,
            "a live editor must retain its model"
        );
        assert_eq!(refreshed.values.row_data(0).unwrap().lower, "-");
        row.generation += 1;
        let replaced = row.ui(Some(&refreshed));
        assert_ne!(
            replaced.values, refreshed.values,
            "an old gesture must not share the replacement editor"
        );
    }
}
