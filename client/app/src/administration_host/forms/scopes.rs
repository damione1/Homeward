//! Fixed presentation choices for the protocol's finite permission ceiling.
use home_wire_contract::home::auth::v1 as auth;

const FAMILIES: [auth::ScopeFamily; 13] = [
    auth::ScopeFamily::StateRegistry,
    auth::ScopeFamily::History,
    auth::ScopeFamily::Media,
    auth::ScopeFamily::Location,
    auth::ScopeFamily::Actions,
    auth::ScopeFamily::Dashboards,
    auth::ScopeFamily::Calendars,
    auth::ScopeFamily::Lists,
    auth::ScopeFamily::Logic,
    auth::ScopeFamily::Integrations,
    auth::ScopeFamily::GlobalConfiguration,
    auth::ScopeFamily::Accounts,
    auth::ScopeFamily::Backups,
];
const OPERATIONS: [auth::ScopeOperation; 3] = [
    auth::ScopeOperation::Read,
    auth::ScopeOperation::Write,
    auth::ScopeOperation::Administer,
];
#[derive(Clone, Copy, PartialEq, Eq)]
pub(in crate::administration_host) struct ScopeChoice {
    pub family: auth::ScopeFamily,
    pub operation: auth::ScopeOperation,
}
impl ScopeChoice {
    pub fn all() -> impl Iterator<Item = (i32, Self)> {
        FAMILIES
            .into_iter()
            .flat_map(|family| {
                OPERATIONS
                    .into_iter()
                    .map(move |operation| Self { family, operation })
            })
            .enumerate()
            .map(|(index, value)| {
                (
                    i32::try_from(index + 1).expect("finite scope choices"),
                    value,
                )
            })
    }
    pub fn parse(token: i32) -> Option<Self> {
        Self::all()
            .find(|(id, _)| *id == token)
            .map(|(_, value)| value)
    }
    pub fn wire(self) -> auth::Scope {
        auth::Scope {
            family: self.family.into(),
            operation: self.operation.into(),
            ..Default::default()
        }
    }
    pub fn label(self) -> String {
        let family = match self.family {
            auth::ScopeFamily::StateRegistry => "États et registre",
            auth::ScopeFamily::History => "Historique",
            auth::ScopeFamily::Media => "Médias",
            auth::ScopeFamily::Location => "Localisation",
            auth::ScopeFamily::Actions => "Actions",
            auth::ScopeFamily::Dashboards => "Dashboards",
            auth::ScopeFamily::Calendars => "Calendriers",
            auth::ScopeFamily::Lists => "Listes",
            auth::ScopeFamily::Logic => "Logique",
            auth::ScopeFamily::Integrations => "Intégrations",
            auth::ScopeFamily::GlobalConfiguration => "Configuration",
            auth::ScopeFamily::Accounts => "Comptes",
            auth::ScopeFamily::Backups => "Sauvegardes",
            auth::ScopeFamily::Unspecified => unreachable!("fixed scope choices"),
        };
        let operation = match self.operation {
            auth::ScopeOperation::Read => "lecture",
            auth::ScopeOperation::Write => "écriture",
            auth::ScopeOperation::Administer => "administration",
            auth::ScopeOperation::Unspecified => unreachable!("fixed operation choices"),
        };
        format!("{family} · {operation}")
    }
}
