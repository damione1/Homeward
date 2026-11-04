use buffa::Message;
use home_wire_contract::{
    authorization_contract as policy,
    home::{api::v2 as auth, auth::v1 as native},
};
use std::path::Path;

fn decode<M: Message + Default>(name: &str) -> M {
    let bytes = std::fs::read(
        Path::new("../../contracts/authorization_golden").join(format!("{name}.bin")),
    )
    .unwrap();
    let value = M::decode_from_slice(&bytes).unwrap();
    assert_eq!(value.encode_to_vec(), bytes, "fixture wire drift: {name}");
    value
}
#[test]
fn nha050_native_termination_cross_language_fixtures() {
    // Termination is a current native owner detail, independent of retired
    // public policy RPCs. The historical corpus remains byte-for-byte intact.
    let index = std::fs::read_to_string("../../contracts/authorization_golden/index.tsv").unwrap();
    let mut count = 0;
    for line in index.lines() {
        let fields: Vec<_> = line.split('\t').collect();
        assert_eq!(fields.len(), 3);
        if fields[1] != "AuthorizationTermination" {
            continue;
        }
        let accepted =
            policy::admit_termination(&decode::<native::AuthorizationTermination>(fields[0]))
                .is_ok();
        assert_eq!(accepted, fields[2] == "true", "{}", fields[0]);
        count += 1;
    }
    assert!(count > 0);
}
fn request() -> auth::ReplaceAuthorizationPolicyRequest {
    auth::ReplaceAuthorizationPolicyRequest {
        name: "homes/h/accounts/a/authorizationPolicy".into(),
        yaml_source: b"schema: homeward.authorization/1\naccount_id: native-opaque\ngrants: []\n"
            .to_vec(),
        precondition: Some(
            auth::replace_authorization_policy_request::Precondition::ExpectedEtag(
                "opaque-cas".into(),
            ),
        ),
        ..Default::default()
    }
}
#[test]
fn nha203_policy_boundaries_unknown_and_snapshot() {
    let mut value = request();
    value.yaml_source = vec![b'x'; policy::MAX_SOURCE_BYTES];
    assert!(policy::admit_replace_request(&value).is_ok());
    value.yaml_source.push(b'x');
    assert!(policy::admit_replace_request(&value).is_err());
    value.yaml_source = vec![0xff];
    assert!(policy::admit_replace_request(&value).is_err());
    let mut encoded = request().encode_to_vec();
    encoded.extend_from_slice(&[0x98, 0x06, 0x01]);
    assert!(
        policy::admit_replace_request(
            &auth::ReplaceAuthorizationPolicyRequest::decode_from_slice(&encoded).unwrap()
        )
        .is_err()
    );
    value = request();
    let admitted = policy::admit_replace_request(&value).unwrap();
    value.yaml_source.clear();
    assert_eq!(admitted.wire().yaml_source, request().yaml_source);
    for precondition in [
        None,
        Some(auth::replace_authorization_policy_request::Precondition::ExpectedEtag(String::new())),
    ] {
        let mut invalid = request();
        invalid.precondition = precondition;
        assert!(policy::admit_replace_request(&invalid).is_err());
    }
    let mut absent = request();
    absent.precondition = Some(
        auth::replace_authorization_policy_request::Precondition::ExpectAbsent(Default::default()),
    );
    assert!(policy::admit_replace_request(&absent).is_ok());
}
