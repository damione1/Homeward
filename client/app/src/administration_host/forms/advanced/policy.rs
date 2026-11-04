use super::*;
use home_wire_contract::authorization_contract;

#[derive(Clone)]
pub(in crate::administration_host) struct PolicyCommand {
    pub account: AccountName,
    pub response: auth::AuthorizationPolicy,
}
pub(in crate::administration_host) struct PolicyDraft {
    account: AccountName,
    expected: auth::replace_authorization_policy_request::Precondition,
    source: Zeroizing<String>,
    native_account: NativePolicyAccount,
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
            Self::None => "Replace With No Configurable Permissions",
            Self::ListsRead => "Replace With List Read Access",
            Self::ListsReadWrite => "Replace With List Read/Write Access",
            Self::OwnDashboards => "Replace With Personal Dashboard Read Access",
        }
    }
    fn source(self, account: &NativePolicyAccount) -> String {
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
            serde_json::to_string(&account.0).expect("opaque id JSON string")
        )
    }
}
impl PolicyDraft {
    pub fn open(command: &PolicyCommand) -> Result<Self, &'static str> {
        authorization_contract::admit_get_response(&command.response)
            .map_err(|_| "The received policy is unsupported.")?;
        let value = &command.response;
        if value.name != format!("{}/authorizationPolicy", command.account.as_str()) {
            return Err("The policy does not match the selected account.");
        }
        let expected = match value.origin {
            buffa::EnumValue::Known(auth::AuthorizationPolicyOrigin::Stored) => {
                auth::replace_authorization_policy_request::Precondition::ExpectedEtag(
                    value.etag.clone(),
                )
            }
            buffa::EnumValue::Known(auth::AuthorizationPolicyOrigin::Default) => {
                auth::replace_authorization_policy_request::Precondition::ExpectAbsent(
                    Default::default(),
                )
            }
            buffa::EnumValue::Known(auth::AuthorizationPolicyOrigin::Unspecified)
            | buffa::EnumValue::Unknown(_) => {
                return Err("The received policy origin is unsupported.");
            }
        };
        let source =
            String::from_utf8(value.yaml_source.clone()).map_err(|_| "The source is not UTF-8.")?;
        let native_account = NativePolicyAccount::from_source(&source)?;
        Ok(Self {
            account: command.account.clone(),
            expected,
            native_account,
            source: Zeroizing::new(source),
        })
    }
    pub fn fields(&self) -> Vec<ProAdminField> {
        vec![
            field(
                Field::PolicySource,
                "Permissions Policy YAML",
                &self.source,
                true,
                true,
            ),
            field(
                Field::Identity,
                "Affected Account",
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
            return Err("The policy is limited to 1 MiB of UTF-8 text.");
        }
        self.source = Zeroizing::new(value);
        Ok(())
    }
    pub fn choose(&mut self, field: i32, option: i32) -> Result<(), &'static str> {
        if ChoiceField::parse(field) == Some(ChoiceField::PolicyPreset)
            && let Some(preset) = Preset::parse(option)
        {
            self.source = Zeroizing::new(preset.source(&self.native_account));
        }
        Ok(())
    }
    pub fn submit(&self) -> Result<Operation, &'static str> {
        let request = auth::ReplaceAuthorizationPolicyRequest {
            name: format!("{}/authorizationPolicy", self.account.as_str()),
            yaml_source: self.source.as_bytes().to_vec(),
            precondition: Some(self.expected.clone()),
            ..Default::default()
        };
        authorization_contract::admit_replace_request(&request)
            .map_err(|_| "The source or policy version is invalid.")?;
        Ok(Operation::Policy(request))
    }
}

// This identity is admitted only from the native policy document, never derived
// from a public name or session. Presets replace grants in a new draft; neither
// opening the editor nor parsing this header rewrites the server's exact bytes.
struct NativePolicyAccount(String);
impl NativePolicyAccount {
    fn from_source(source: &str) -> Result<Self, &'static str> {
        #[derive(serde::Deserialize)]
        #[serde(deny_unknown_fields)]
        struct Header {
            schema: String,
            account_id: String,
            // Existing grants are discarded ONLY when the user selects a
            // complete replacement preset. Their authority remains server-side.
            grants: Vec<serde::de::IgnoredAny>,
        }
        if source.len() > authorization_contract::MAX_SOURCE_BYTES {
            return Err("The policy source is too large.");
        }
        let mut budget = serde_saphyr::Budget::default();
        budget.max_documents = 1;
        budget.max_events = 65_536;
        budget.max_nodes = 32_768;
        budget.max_depth = 32;
        budget.flow_nesting_limit = 32;
        budget.max_aliases = 0;
        budget.max_anchors = 0;
        budget.max_merge_keys = 0;
        budget.max_total_scalar_bytes = authorization_contract::MAX_SOURCE_BYTES;
        let mut options = serde_saphyr::Options::default();
        options.budget = Some(budget);
        options.duplicate_keys = serde_saphyr::DuplicateKeyPolicy::Error;
        options.merge_keys = serde_saphyr::MergeKeyPolicy::Error;
        options.reject_unsupported_tags = true;
        options.reject_tagged_field_names = true;
        options.with_snippet = false;
        let header: Header = serde_saphyr::from_str_with_options(source, options)
            .map_err(|_| "The policy source cannot be safely edited with presets.")?;
        if header.schema != authorization_contract::POLICY_SCHEMA
            || header.account_id.is_empty()
            || header.account_id.len() > 128
            || header.account_id.contains('\0')
            || header.grants.len() > 4096
        {
            return Err("The policy source identity or schema is unsupported.");
        }
        Ok(Self(header.account_id))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn opening_default_preserves_exact_source_and_presets_keep_native_identity() {
        let source = "# exact owner bytes\nschema: homeward.authorization/1\naccount_id: 'historical: opaque/é'\ngrants: []\n";
        let account = AccountName::try_from("homes/h/accounts/public-opaque").unwrap();
        let command = PolicyCommand {
            account,
            response: auth::AuthorizationPolicy {
                name: "homes/h/accounts/public-opaque/authorizationPolicy".into(),
                yaml_source: source.as_bytes().to_vec(),
                schema: authorization_contract::POLICY_SCHEMA.into(),
                syntax_profile: authorization_contract::POLICY_SYNTAX_PROFILE.into(),
                origin: auth::AuthorizationPolicyOrigin::Default.into(),
                ..Default::default()
            },
        };
        let mut draft = PolicyDraft::open(&command).unwrap();
        let Operation::Policy(unchanged) = draft.submit().unwrap() else {
            panic!("policy")
        };
        assert_eq!(unchanged.yaml_source, source.as_bytes());
        assert!(matches!(
            unchanged.precondition,
            Some(auth::replace_authorization_policy_request::Precondition::ExpectAbsent(_))
        ));
        draft
            .choose(ChoiceField::PolicyPreset as i32, Preset::ListsRead as i32)
            .unwrap();
        let Operation::Policy(replacement) = draft.submit().unwrap() else {
            panic!("policy")
        };
        assert_eq!(
            NativePolicyAccount::from_source(
                std::str::from_utf8(&replacement.yaml_source).unwrap()
            )
            .unwrap()
            .0,
            "historical: opaque/é"
        );
        assert_eq!(command.response.yaml_source, source.as_bytes());
    }

    #[test]
    fn presets_refuse_ambiguous_or_unbounded_policy_headers() {
        for source in [
            "schema: homeward.authorization/1\naccount_id: a\naccount_id: b\ngrants: []\n",
            "schema: homeward.authorization/1\naccount_id: a\ngrants: []\nfuture: true\n",
            "schema: homeward.authorization/1\naccount_id: &id a\ngrants: []\n",
            "schema: homeward.authorization/1\naccount_id: a\ngrants: []\n---\nschema: homeward.authorization/1\naccount_id: b\ngrants: []\n",
            "schema: future/2\naccount_id: a\ngrants: []\n",
        ] {
            assert!(NativePolicyAccount::from_source(source).is_err());
        }
        assert!(
            NativePolicyAccount::from_source(
                &"x".repeat(authorization_contract::MAX_SOURCE_BYTES + 1)
            )
            .is_err()
        );
    }
}
