use buffa::{EnumValue, Message};
use home_wire_contract::{home::solar::v1 as solar, solar_contract};

#[test]
fn nha105_go_protoc_rust_roundtrip_and_admission() {
    let root =
        std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("../../contracts/solar_golden");
    let index = std::fs::read_to_string(root.join("index.tsv")).unwrap();
    for line in index.lines() {
        let fields: Vec<_> = line.split('\t').collect();
        assert_eq!(fields.len(), 3);
        let bytes = std::fs::read(root.join(format!("{}.bin", fields[0]))).unwrap();
        let expected = fields[2] == "true";
        match fields[1] {
            "SolarValue" => {
                let value = solar::SolarValue::decode_from_slice(&bytes).unwrap();
                let admitted = solar_contract::admit_solar_value(&value);
                assert_eq!(admitted.is_ok(), expected, "{}", fields[0]);
                if let Ok(admitted) = admitted {
                    assert_eq!(admitted.into_wire().encode_to_vec(), bytes);
                }
                assert_eq!(value.encode_to_vec(), bytes);
            }
            "SolarEventValue" => {
                let value = solar::SolarEventValue::decode_from_slice(&bytes).unwrap();
                let admitted = solar_contract::admit_solar_event_value(&value);
                assert_eq!(admitted.is_ok(), expected, "{}", fields[0]);
                assert_eq!(value.encode_to_vec(), bytes);
                if fields[0] == "event-future" {
                    assert_eq!(value.event, EnumValue::Unknown(99));
                }
            }
            _ => panic!("unknown fixture message"),
        }
    }
}
