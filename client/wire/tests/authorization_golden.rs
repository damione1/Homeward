use buffa::Message;
use home_wire_contract::{authorization_contract as policy, home::api::v2 as auth};
// Historical authorization fixtures are preserved by the corpus integrity
// manifest. Retired native transport DTOs have no current decoder here.
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
    policy::admit_replace_request(&value).unwrap();
    assert_eq!(value.yaml_source, request().yaml_source);
    value.yaml_source.clear();
    assert!(policy::admit_replace_request(&value).is_err());
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
