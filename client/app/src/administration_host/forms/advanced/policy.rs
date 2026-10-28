use super::*;
use home_wire_contract::authorization_contract;

#[derive(Clone)]
pub(in crate::administration_host) struct PolicyCommand {
    pub account: AccountId,
    pub response: auth::GetPolicyResponse,
}
pub(in crate::administration_host) struct PolicyDraft {
    account: AccountId,
    expected: auth::replace_policy_request::Precondition,
    source: Zeroizing<String>,
}
#[derive(Clone, Copy)]
#[repr(i32)]
pub(in crate::administration_host) enum Preset {
    None = 1,
    ListsRead = 2,
    ListsReadWrite = 3,
    OwnDashboards = 4,
}
impl Preset {
    fn parse(token: i32) -> Option<Self> {
        match token {
            1 => Some(Self::None),
            2 => Some(Self::ListsRead),
            3 => Some(Self::ListsReadWrite),
            4 => Some(Self::OwnDashboards),
            _ => None,
        }
    }
    fn label(self) -> &'static str {
        match self {
            Self::None => "Remplacer par aucun droit configurable",
            Self::ListsRead => "Remplacer par lecture des listes",
            Self::ListsReadWrite => "Remplacer par lecture/écriture des listes",
            Self::OwnDashboards => "Remplacer par lecture des dashboards personnels",
        }
    }
    fn source(self, account: &AccountId) -> String {
        let grants = match self {
            Self::None => " []\n",
            Self::ListsRead => {
                "\n  - scope: {family: lists, operation: read}\n    objects: {lists: {all: {}}}\n"
            }
            Self::ListsReadWrite => {
                "\n  - scope: {family: lists, operation: read}\n    objects: {lists: {all: {}}}\n  - scope: {family: lists, operation: write}\n    objects: {lists: {all: {}}}\n"
            }
            Self::OwnDashboards => {
                "\n  - scope: {family: dashboards, operation: read}\n    objects: {dashboards: {own: {}}}\n"
            }
        };
        format!(
            "schema: {}\naccount_id: {}\ngrants:{grants}",
            authorization_contract::POLICY_SCHEMA,
            serde_json::to_string(account.as_str()).expect("opaque id JSON string")
        )
    }
}
impl PolicyDraft {
    pub fn open(command: &PolicyCommand) -> Result<Self, &'static str> {
        authorization_contract::admit_get_response(&command.response)
            .map_err(|_| "La politique reçue n’est pas prise en charge.")?;
        let (expected, source) = match &command.response.result {
            Some(auth::get_policy_response::Result::Present(value)) => {
                if value.account_id.value != command.account.as_str() {
                    return Err("La politique ne correspond pas au compte sélectionné.");
                }
                (
                    auth::replace_policy_request::Precondition::Expected(
                        value
                            .version
                            .as_option()
                            .ok_or("Version absente.")?
                            .clone()
                            .into(),
                    ),
                    String::from_utf8(value.yaml_source.clone())
                        .map_err(|_| "La source n’est pas UTF-8.")?,
                )
            }
            Some(auth::get_policy_response::Result::Absent(_)) => (
                auth::replace_policy_request::Precondition::ExpectAbsent(Default::default()),
                Preset::None.source(&command.account),
            ),
            None => return Err("Politique absente de la réponse."),
        };
        Ok(Self {
            account: command.account.clone(),
            expected,
            source: Zeroizing::new(source),
        })
    }
    pub fn fields(&self) -> Vec<ProAdminField> {
        vec![
            field(
                Field::PolicySource,
                "Politique de droits YAML",
                &self.source,
                true,
                true,
            ),
            field(
                Field::Identity,
                "Compte concerné",
                self.account.as_str(),
                false,
                false,
            ),
        ]
    }
    pub fn choices(&self) -> Vec<ProAdminChoice> {
        [
            Preset::None,
            Preset::ListsRead,
            Preset::ListsReadWrite,
            Preset::OwnDashboards,
        ]
        .into_iter()
        .map(|preset| {
            choice(
                ChoiceField::PolicyPreset,
                preset as i32,
                preset.label(),
                false,
            )
        })
        .collect()
    }
    pub fn edit(&mut self, token: i32, value: String) -> Result<(), &'static str> {
        if Field::parse(token) != Some(Field::PolicySource) {
            return Ok(());
        }
        if value.len() > authorization_contract::MAX_SOURCE_BYTES || value.contains('\0') {
            return Err("La politique est limitée à 1 Mio de texte UTF-8.");
        }
        self.source = Zeroizing::new(value);
        Ok(())
    }
    pub fn choose(&mut self, field: i32, option: i32) -> Result<(), &'static str> {
        if ChoiceField::parse(field) == Some(ChoiceField::PolicyPreset)
            && let Some(preset) = Preset::parse(option)
        {
            self.source = Zeroizing::new(preset.source(&self.account));
        }
        Ok(())
    }
    pub fn submit(&self) -> Result<Operation, &'static str> {
        let request = auth::ReplacePolicyRequest {
            account_id: auth::AccountId {
                value: self.account.as_str().into(),
                ..Default::default()
            }
            .into(),
            yaml_source: self.source.as_bytes().to_vec(),
            precondition: Some(self.expected.clone()),
            ..Default::default()
        };
        authorization_contract::admit_replace_request(&request)
            .map_err(|_| "La source ou la version de politique est invalide.")?;
        Ok(Operation::Policy(request))
    }
}
