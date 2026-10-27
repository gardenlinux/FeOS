// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! What pods do not get of SVCB and HTTPS records (RFC 9460): `ech`, so
//! that clients send the server name in the clear (ADR 0013).

use hickory_proto::rr::rdata::svcb::{Mandatory, SvcParamKey, SvcParamValue, SVCB};
use hickory_proto::rr::rdata::HTTPS;
use hickory_proto::rr::{RData, Record};

/// The parameters that SVCB and HTTPS records lose.
const STRIPPED: &[SvcParamKey] = &[SvcParamKey::EchConfigList];

/// Removes `ech` from the SVCB and HTTPS records among `records`, and from
/// their `mandatory` lists. Returns whether a record changed.
pub fn strip(records: &mut [Record]) -> bool {
    let mut changed = false;
    for record in records {
        let svcb = match &mut record.data {
            RData::HTTPS(HTTPS(svcb)) | RData::SVCB(svcb) => svcb,
            _ => continue,
        };
        if svcb
            .svc_params
            .iter()
            .any(|(key, _)| STRIPPED.contains(key))
        {
            *svcb = without(svcb, STRIPPED);
            changed = true;
        }
    }
    changed
}

fn without(svcb: &SVCB, keys: &[SvcParamKey]) -> SVCB {
    let params = svcb
        .svc_params
        .iter()
        .filter(|(key, _)| !keys.contains(key))
        .filter_map(|(key, value)| match value {
            // A list of mandatory keys must not be empty.
            SvcParamValue::Mandatory(Mandatory(mandatory)) => {
                let rest: Vec<_> = mandatory
                    .iter()
                    .filter(|key| !keys.contains(key))
                    .copied()
                    .collect();
                (!rest.is_empty()).then_some((*key, SvcParamValue::Mandatory(Mandatory(rest))))
            }
            _ => Some((*key, value.clone())),
        })
        .collect();
    SVCB::new(svcb.svc_priority, svcb.target_name.clone(), params)
}

#[cfg(test)]
mod tests {
    use super::*;
    use hickory_proto::op::{Message, MessageType, OpCode};
    use hickory_proto::rr::rdata::svcb::{Alpn, EchConfigList, IpHint};
    use hickory_proto::rr::rdata::{A, AAAA};
    use hickory_proto::rr::Name;

    fn https(params: Vec<(SvcParamKey, SvcParamValue)>) -> Record {
        let name = Name::from_ascii("example.com.").unwrap();
        let svcb = SVCB::new(1, Name::root(), params);
        Record::from_rdata(name, 300, RData::HTTPS(HTTPS(svcb)))
    }

    fn params(record: &Record) -> Vec<(SvcParamKey, SvcParamValue)> {
        match &record.data {
            RData::HTTPS(HTTPS(svcb)) => svcb.svc_params.clone(),
            _ => panic!("not an HTTPS record"),
        }
    }

    #[test]
    fn removes_ech() {
        let alpn = (
            SvcParamKey::Alpn,
            SvcParamValue::Alpn(Alpn(vec!["h2".into(), "h3".into()])),
        );
        let ipv4hint = (
            SvcParamKey::Ipv4Hint,
            SvcParamValue::Ipv4Hint(IpHint(vec![A([1, 2, 3, 4].into())])),
        );
        let ipv6hint = (
            SvcParamKey::Ipv6Hint,
            SvcParamValue::Ipv6Hint(IpHint(vec![AAAA("2001:db8::1".parse().unwrap())])),
        );
        let mut records = vec![https(vec![
            (
                SvcParamKey::Mandatory,
                SvcParamValue::Mandatory(Mandatory(vec![
                    SvcParamKey::Alpn,
                    SvcParamKey::EchConfigList,
                ])),
            ),
            alpn.clone(),
            ipv4hint.clone(),
            (
                SvcParamKey::EchConfigList,
                SvcParamValue::EchConfigList(EchConfigList(vec![0, 4, 0xfe, 0x0d, 0, 0])),
            ),
            ipv6hint.clone(),
        ])];
        assert!(strip(&mut records));
        // Pods have both families: the address hints stay.
        assert_eq!(
            params(&records[0]),
            [
                (
                    SvcParamKey::Mandatory,
                    SvcParamValue::Mandatory(Mandatory(vec![SvcParamKey::Alpn]))
                ),
                alpn,
                ipv4hint,
                ipv6hint
            ]
        );

        // What is left goes onto the wire and back.
        let mut message = Message::new(0, MessageType::Query, OpCode::Query);
        message.add_answers(records.clone());
        let decoded = Message::from_vec(&message.to_vec().unwrap()).unwrap();
        assert_eq!(decoded.answers, records);
    }

    #[test]
    fn drops_a_mandatory_list_left_empty() {
        let mut records = vec![https(vec![
            (
                SvcParamKey::Mandatory,
                SvcParamValue::Mandatory(Mandatory(vec![SvcParamKey::EchConfigList])),
            ),
            (
                SvcParamKey::EchConfigList,
                SvcParamValue::EchConfigList(EchConfigList(vec![0, 0])),
            ),
            (SvcParamKey::Port, SvcParamValue::Port(8443)),
        ])];
        assert!(strip(&mut records));
        assert_eq!(
            params(&records[0]),
            [(SvcParamKey::Port, SvcParamValue::Port(8443))]
        );
    }

    #[test]
    fn leaves_other_records_alone() {
        let port = vec![(SvcParamKey::Port, SvcParamValue::Port(8443))];
        let a = Record::from_rdata(Name::root(), 60, RData::A(A([1, 2, 3, 4].into())));
        let mut records = vec![https(port.clone()), a.clone()];
        assert!(!strip(&mut records));
        assert_eq!(params(&records[0]), port);
        assert_eq!(records[1], a);
    }
}
