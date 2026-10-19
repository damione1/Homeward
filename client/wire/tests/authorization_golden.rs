use buffa::Message;
use home_wire_contract::{authorization_contract as policy, home::auth::v1 as auth};
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
fn nha050_policy_cross_language_fixtures() {
    let index = std::fs::read_to_string("../../contracts/authorization_golden/index.tsv").unwrap();
    for line in index.lines() {
        let fields: Vec<_> = line.split('\t').collect();
        assert_eq!(fields.len(), 3);
        let accepted = match fields[1] {
            "ReplacePolicyRequest" => {
                policy::admit_replace_request(&decode::<auth::ReplacePolicyRequest>(fields[0]))
                    .is_ok()
            }
            "GetPolicyResponse" => {
                policy::admit_get_response(&decode::<auth::GetPolicyResponse>(fields[0])).is_ok()
            }
            "AuthorizationTermination" => {
                policy::admit_termination(&decode::<auth::AuthorizationTermination>(fields[0]))
                    .is_ok()
            }
            "AuthorizationPolicyError" => {
                policy::admit_error(&decode::<auth::AuthorizationPolicyError>(fields[0])).is_ok()
            }
            _ => panic!("unhandled fixture"),
        };
        assert_eq!(accepted, fields[2] == "true", "{}", fields[0]);
    }
}
#[test]
fn nha050_policy_boundaries_unknown_and_snapshot() {
    let mut request: auth::ReplacePolicyRequest = decode("replace-exact");
    request.yaml_source = vec![b'x'; policy::MAX_SOURCE_BYTES];
    assert!(policy::admit_replace_request(&request).is_ok());
    request.yaml_source.push(b'x');
    assert!(policy::admit_replace_request(&request).is_err());
    request.yaml_source = vec![0xff];
    assert!(policy::admit_replace_request(&request).is_err());
    let mut encoded = decode::<auth::ReplacePolicyRequest>("replace-exact").encode_to_vec();
    encoded.extend_from_slice(&[0x98, 0x06, 0x01]);
    assert!(
        policy::admit_replace_request(
            &auth::ReplacePolicyRequest::decode_from_slice(&encoded).unwrap()
        )
        .is_err()
    );
    let mut response: auth::GetPolicyResponse = decode("get-absent");
    let admitted = policy::admit_get_response(&response).unwrap();
    response.result = None;
    assert!(admitted.wire().result.is_some());
}
