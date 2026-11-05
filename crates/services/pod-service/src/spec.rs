// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//! A pod's spec: what is valid, and the microVM it becomes.

use crate::network::{self, Attachment};
use crate::volumes;
use feos_proto::microvm::{
    BalloonConfig, CpuConfig, DataDiskConfig, GuestNetworkConfig, GuestRoute, MemoryConfig,
    NetConfig, TapConfig, VmConfig,
};
use feos_proto::pod_service::{PodHttpBindings, PodSpec, Resources};
use feos_utils::filesystem::volume as feos_volume;
use feos_utils::pod_id::PodId;
use std::collections::{BTreeMap, HashMap, HashSet};
use std::net::Ipv4Addr;
use tonic::Status;

const DEFAULT_VCPUS: u32 = 1;
const DEFAULT_MEMORY_MIB: u64 = 512;
/// Firecracker's limit.
const MAX_VCPUS: u32 = 32;
/// Less does not hold the guest and an image.
const MIN_MEMORY_MIB: u64 = 128;
/// The pod's interface in the guest.
const INTERFACE: &str = "eth0";
/// A pod's volumes, each a drive: Firecracker's microVM has interrupts for
/// some 19 devices, of which the balloon, the disk, the network and vsock
/// take four.
const MAX_VOLUMES: usize = 8;
/// What a pod's metadata may take, as JSON: Firecracker keeps 50 KiB,
/// feosd's part beside it a few hundred bytes.
const MAX_METADATA_BYTES: usize = 32 * 1024;
const MAX_METADATA_KEY: usize = 128;
/// What a pod's HTTP bindings' clear part may hold (ADR 0028): names, and
/// the interception CAs' certificates.
const MAX_HTTP_NAMES: usize = 64;
const MAX_CA_PEMS: usize = 8;
const MAX_CA_PEM_BYTES: usize = 64 * 1024;
const MAX_HTTP_VERSION: usize = 128;

/// Checks a pod's ID, which also names its microVM and its paths: a short
/// one, or the UUID of a pod made before short IDs (ADR 0019).
pub fn check_id(pod_id: &str) -> Result<(), Status> {
    PodId::parse(pod_id)
        .map(|_| ())
        .map_err(|e| Status::invalid_argument(e.to_string()))
}

/// Checks a spec and fills in the defaults, so that two creates of a pod
/// compare as they will run.
pub fn normalize(mut spec: PodSpec) -> Result<PodSpec, Status> {
    let resources = spec.resources.get_or_insert_with(Resources::default);
    if resources.vcpus == 0 {
        resources.vcpus = DEFAULT_VCPUS;
    }
    if resources.memory_mib == 0 {
        resources.memory_mib = DEFAULT_MEMORY_MIB;
    }
    if resources.vcpus > MAX_VCPUS {
        return Err(Status::invalid_argument(format!(
            "a pod has at most {MAX_VCPUS} vCPUs"
        )));
    }
    if resources.memory_mib < MIN_MEMORY_MIB {
        return Err(Status::invalid_argument(format!(
            "a pod has at least {MIN_MEMORY_MIB} MiB of memory"
        )));
    }

    let mut names = HashSet::new();
    for container in &spec.containers {
        if !valid_name(&container.name) {
            return Err(Status::invalid_argument(format!(
                "invalid container name {:?}: 1 to 64 letters, digits, \"-\", \"_\" and \".\"",
                container.name
            )));
        }
        if !names.insert(container.name.as_str()) {
            return Err(Status::invalid_argument(format!(
                "two containers are named {:?}",
                container.name
            )));
        }
        if container.image_ref.is_empty() {
            return Err(Status::invalid_argument(format!(
                "container {:?} has no image",
                container.name
            )));
        }
    }
    check_volumes(&spec)?;
    check_metadata(&spec)?;
    if let Some(bindings) = &spec.http_bindings {
        check_http_bindings(&spec, bindings)?;
    }

    Ok(spec)
}

/// Checks the clear part of a pod's HTTP bindings (ADR 0028): names that
/// are DNS labels, intercepted hosts that are host names, each once and in
/// one of the two only, CAs that are PEM certificates, and a network to
/// reach them on.
pub fn check_http_bindings(spec: &PodSpec, bindings: &PodHttpBindings) -> Result<(), Status> {
    let named = bindings.names.len() + bindings.intercept_hosts.len();
    if named > MAX_HTTP_NAMES {
        return Err(Status::invalid_argument(format!(
            "a pod has at most {MAX_HTTP_NAMES} HTTP bindings' names"
        )));
    }
    if named > 0 && !has_network(spec) {
        return Err(Status::invalid_argument(
            "a pod without a network reaches no HTTP bindings",
        ));
    }
    let mut seen = HashSet::new();
    for name in &bindings.names {
        if !feos_volume::valid_name(name) {
            return Err(Status::invalid_argument(format!(
                "invalid HTTP binding name {name:?}: 1 to 63 lowercase letters, digits and \"-\", beginning and ending with a letter or a digit"
            )));
        }
        if !seen.insert(name.as_str()) {
            return Err(Status::invalid_argument(format!(
                "HTTP binding name {name:?} is named twice"
            )));
        }
    }
    for host in &bindings.intercept_hosts {
        if !valid_host(host) {
            return Err(Status::invalid_argument(format!(
                "invalid intercepted host {host:?}: a lowercase host name of DNS labels, without the final dot"
            )));
        }
        if !seen.insert(host.as_str()) {
            return Err(Status::invalid_argument(format!(
                "HTTP binding name {host:?} is named twice"
            )));
        }
    }
    if bindings.intercept_ca_pems.len() > MAX_CA_PEMS {
        return Err(Status::invalid_argument(format!(
            "a pod has at most {MAX_CA_PEMS} interception CAs"
        )));
    }
    let size: usize = bindings.intercept_ca_pems.iter().map(String::len).sum();
    if size > MAX_CA_PEM_BYTES {
        return Err(Status::invalid_argument(format!(
            "the interception CAs take {size} bytes, more than {MAX_CA_PEM_BYTES}"
        )));
    }
    for (index, ca) in bindings.intercept_ca_pems.iter().enumerate() {
        if !is_certificate_pem(ca) {
            return Err(Status::invalid_argument(format!(
                "interception CA {index} is not one PEM block CERTIFICATE"
            )));
        }
    }
    if bindings.version.len() > MAX_HTTP_VERSION || bindings.version.chars().any(char::is_control) {
        return Err(Status::invalid_argument(format!(
            "the HTTP bindings' version is at most {MAX_HTTP_VERSION} printable characters"
        )));
    }
    Ok(())
}

/// A host name of lowercase DNS labels, without the final dot, which no
/// address is.
fn valid_host(host: &str) -> bool {
    host.len() <= 253
        && host.parse::<std::net::IpAddr>().is_err()
        && host.split('.').all(feos_volume::valid_name)
}

/// Whether `pem` is one PEM block of a certificate: its armor and base64
/// in between, nothing else but blank lines.
fn is_certificate_pem(pem: &str) -> bool {
    const BEGIN: &str = "-----BEGIN CERTIFICATE-----";
    const END: &str = "-----END CERTIFICATE-----";
    let mut lines = pem.lines().map(str::trim).filter(|line| !line.is_empty());
    if lines.next() != Some(BEGIN) {
        return false;
    }
    let mut body = 0;
    for line in lines.by_ref() {
        if line == END {
            return body > 0 && lines.next().is_none();
        }
        if !line
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'+' | b'/' | b'='))
        {
            return false;
        }
        body += line.len();
    }
    false
}

/// The spec without its HTTP bindings' clear part, which SetPodHttpBindings
/// changes: what two creates of a pod compare.
pub fn without_http_bindings(spec: &PodSpec) -> PodSpec {
    PodSpec {
        http_bindings: None,
        ..spec.clone()
    }
}

/// Checks what the pod's metadata service is to serve: keys that are
/// names in its paths, and no more than [`MAX_METADATA_BYTES`]; a pod
/// without a network has no metadata service.
fn check_metadata(spec: &PodSpec) -> Result<(), Status> {
    if spec.metadata.is_empty() {
        return Ok(());
    }
    if !has_network(spec) {
        return Err(Status::invalid_argument(
            "a pod without a network has no metadata service, and no metadata",
        ));
    }
    if let Some(key) = spec.metadata.keys().find(|key| !valid_metadata_key(key)) {
        return Err(Status::invalid_argument(format!(
            "invalid metadata key {key:?}: 1 to {MAX_METADATA_KEY} letters, digits, \"-\", \"_\" and \".\""
        )));
    }
    let size = serde_json::to_string(&spec.metadata).map_or(usize::MAX, |json| json.len());
    if size > MAX_METADATA_BYTES {
        return Err(Status::invalid_argument(format!(
            "the metadata takes {size} bytes as JSON, more than {MAX_METADATA_BYTES}"
        )));
    }
    Ok(())
}

/// A key of the metadata, which is a name in the metadata service's paths.
fn valid_metadata_key(key: &str) -> bool {
    (1..=MAX_METADATA_KEY).contains(&key.len())
        && key != "."
        && key != ".."
        && key
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.'))
}

/// Checks the pod's volumes and its containers' mounts of them: at most
/// [`MAX_VOLUMES`], each under a name of its own and named once, and each
/// mount of a volume of the pod at a path of its own.
fn check_volumes(spec: &PodSpec) -> Result<(), Status> {
    if spec.volumes.len() > MAX_VOLUMES {
        return Err(Status::invalid_argument(format!(
            "a pod has at most {MAX_VOLUMES} volumes"
        )));
    }
    let mut names = HashSet::new();
    let mut ids = HashSet::new();
    for volume in &spec.volumes {
        if !feos_volume::valid_name(&volume.name) {
            return Err(Status::invalid_argument(format!(
                "invalid volume name {:?}: 1 to 63 lowercase letters, digits and \"-\", beginning and ending with a letter or a digit",
                volume.name
            )));
        }
        volumes::check_id(&volume.volume_id)?;
        if !names.insert(volume.name.as_str()) {
            return Err(Status::invalid_argument(format!(
                "two volumes are named {:?}",
                volume.name
            )));
        }
        if !ids.insert(volume.volume_id.as_str()) {
            return Err(Status::invalid_argument(format!(
                "volume {} is named twice",
                volume.volume_id
            )));
        }
    }
    for container in &spec.containers {
        let mut paths = HashSet::new();
        for mount in &container.volume_mounts {
            if !names.contains(mount.volume.as_str()) {
                return Err(Status::invalid_argument(format!(
                    "container {:?} mounts volume {:?}, which the pod does not have",
                    container.name, mount.volume
                )));
            }
            if !feos_volume::valid_mount_path(&mount.path) || !paths.insert(mount.path.as_str()) {
                return Err(Status::invalid_argument(format!(
                    "container {:?} cannot mount volume {:?} at {:?}: an absolute path other than \"/\", once",
                    container.name, mount.volume, mount.path
                )));
            }
        }
    }
    Ok(())
}

/// The IDs of the pod's volumes, in the order of its spec.
pub fn volume_ids(spec: &PodSpec) -> Vec<String> {
    spec.volumes
        .iter()
        .map(|volume| volume.volume_id.clone())
        .collect()
}

/// Whether the pod has a network: unless its spec says none.
pub fn has_network(spec: &PodSpec) -> bool {
    !spec.network.as_ref().is_some_and(|network| network.none)
}

/// A container's environment with OpenTelemetry's variables for the node's
/// OTLP endpoint at `otlp` (docs/architecture/traces.md, "The
/// environment"), each unless the container's own has it; in a pod with a
/// network alone. The service's name is the pod's `ferun.object`, else its
/// `ferun.job`, of its `labels`.
pub fn otel_env(
    env: &HashMap<String, String>,
    spec: &PodSpec,
    labels: &HashMap<String, String>,
    otlp: Ipv4Addr,
) -> HashMap<String, String> {
    let mut env = env.clone();
    if !has_network(spec) {
        return env;
    }
    let (_, http) = network::OTLP_PORTS;
    let service = ["ferun.object", "ferun.job"]
        .iter()
        .filter_map(|key| labels.get(*key))
        .find(|name| !name.is_empty());
    let defaults = [
        (
            "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
            Some(format!("http://{otlp}:{http}/v1/traces")),
        ),
        (
            "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL",
            Some("http/protobuf".to_string()),
        ),
        ("OTEL_SERVICE_NAME", service.cloned()),
    ];
    for (name, value) in defaults {
        if let Some(value) = value {
            env.entry(name.to_string()).or_insert(value);
        }
    }
    env
}

/// The container names the guest takes as container IDs.
fn valid_name(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= 64
        && name != "."
        && name != ".."
        && name
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.'))
}

/// The pod's microVM: the newest guest installed on the node, with a
/// balloon that reports free pages, so that a suspend writes what the
/// guest uses, the pod's disk if it has one started and its volumes'
/// disks after it, in the order of its spec, and the pod's TAP device and
/// addresses if it has a network (ADR 0010): an IPv6 /128 behind the node
/// at `fe80::1`, an IPv4 /32 behind `169.254.0.1`, which is on the link
/// although outside the /32, the node's resolver, and the metadata
/// service at `169.254.169.254`.
pub fn vm_config(
    pod_id: &str,
    spec: &PodSpec,
    attachment: Option<&Attachment>,
    disk: Option<&str>,
    volumes: &[String],
) -> VmConfig {
    let resources = spec.resources.unwrap_or_default();
    let mut config = VmConfig {
        cpus: Some(CpuConfig {
            vcpu_count: resources.vcpus,
        }),
        memory: Some(MemoryConfig {
            size_mib: resources.memory_mib,
            balloon: Some(BalloonConfig {
                amount_mib: 0,
                deflate_on_oom: true,
                free_page_reporting: true,
                stats_interval_s: 0,
            }),
        }),
        data_disk: disk.map(|path| DataDiskConfig {
            path: path.to_string(),
        }),
        volumes: volumes
            .iter()
            .map(|path| DataDiskConfig { path: path.clone() })
            .collect(),
        ..Default::default()
    };
    if let Some(attachment) = attachment {
        config.net.push(NetConfig {
            device_id: INTERFACE.to_string(),
            tap: Some(TapConfig {
                tap_name: attachment.tap.clone(),
            }),
            mac_address: network::mac_string(attachment.mac),
        });
        config.guest_network = Some(GuestNetworkConfig {
            interface: INTERFACE.to_string(),
            ipv4_cidr: format!("{}/32", attachment.ipv4),
            gateway: network::GATEWAY4.to_string(),
            ipv6_cidr: format!("{}/128", attachment.ipv6),
            ipv6_gateway: network::GATEWAY.to_string(),
            dns: vec![attachment.dns.to_string()],
            hostname: pod_id.to_string(),
            routes: attachment
                .routes
                .iter()
                .map(|route| GuestRoute {
                    destination: route.destination.to_string(),
                    gateway: route.gateway.to_string(),
                    mtu: route.mtu,
                })
                .collect(),
        });
        config.metadata_json = metadata_json(pod_id, spec, attachment);
    }
    config
}

/// What the pod's metadata service serves: feosd's part under "feos", the
/// pod's ID and its addresses on this node, and the spec's under
/// "metadata". The keys are in order, so that the microVM's configuration
/// compares the same as long as the pod and its addresses are.
fn metadata_json(pod_id: &str, spec: &PodSpec, attachment: &Attachment) -> String {
    let metadata: BTreeMap<&String, &String> = spec.metadata.iter().collect();
    serde_json::json!({
        "feos": {
            "pod-id": pod_id,
            "ipv6": attachment.ipv6.to_string(),
            "ipv4": attachment.ipv4.to_string(),
        },
        "metadata": metadata,
    })
    .to_string()
}

#[cfg(test)]
mod tests {
    use super::*;
    use feos_proto::pod_service::{ContainerSpec, PodNetwork, PodVolume, VolumeMount};

    fn container(name: &str) -> ContainerSpec {
        ContainerSpec {
            name: name.to_string(),
            image_ref: "docker.io/library/busybox:1.36".to_string(),
            ..Default::default()
        }
    }

    fn spec(containers: Vec<ContainerSpec>) -> PodSpec {
        PodSpec {
            containers,
            ..Default::default()
        }
    }

    const CA: &str =
        "-----BEGIN CERTIFICATE-----\nMIIBszCCAVmgAwIBAgIU\nAA==\n-----END CERTIFICATE-----\n";

    #[test]
    fn http_bindings_name_labels_and_hosts_once_with_pem_cas() {
        let bindings = |names: &[&str], hosts: &[&str], cas: &[&str]| PodHttpBindings {
            names: names.iter().map(|n| n.to_string()).collect(),
            intercept_hosts: hosts.iter().map(|h| h.to_string()).collect(),
            intercept_ca_pems: cas.iter().map(|c| c.to_string()).collect(),
            version: "3".to_string(),
        };
        let pod = spec(vec![container("main")]);
        let good = bindings(&["erp", "pay-1"], &["api.erp.example"], &[CA, CA]);
        check_http_bindings(&pod, &good).unwrap();
        let created = normalize(PodSpec {
            http_bindings: Some(good.clone()),
            ..pod.clone()
        })
        .unwrap();
        assert_eq!(created.http_bindings, Some(good));
        check_http_bindings(&pod, &PodHttpBindings::default()).unwrap();

        for bad in [
            bindings(&["ERP"], &[], &[]),
            bindings(&["-erp"], &[], &[]),
            bindings(&["erp.example"], &[], &[]),
            bindings(&["erp", "erp"], &[], &[]),
            bindings(&["erp"], &["erp"], &[]),
            bindings(&[], &["Api.example"], &[]),
            bindings(&[], &["api.example."], &[]),
            bindings(&[], &["10.0.0.1"], &[]),
            bindings(&[], &["a..b"], &[]),
            bindings(&[], &[], &["not a certificate"]),
            bindings(
                &[],
                &[],
                &["-----BEGIN PRIVATE KEY-----\nAA==\n-----END PRIVATE KEY-----"],
            ),
            bindings(&[], &[], &[&format!("{CA}{CA}")]),
        ] {
            assert!(check_http_bindings(&pod, &bad).is_err(), "{bad:?}");
        }
        // A pod without a network reaches none.
        let isolated = PodSpec {
            network: Some(PodNetwork {
                none: true,
                ..Default::default()
            }),
            ..pod.clone()
        };
        assert!(check_http_bindings(&isolated, &bindings(&["erp"], &[], &[])).is_err());
        // Two creates compare without them.
        let other = PodSpec {
            http_bindings: Some(bindings(&["crm"], &[], &[])),
            ..created.clone()
        };
        assert_eq!(
            without_http_bindings(&created),
            without_http_bindings(&other)
        );
    }

    #[test]
    fn defaults_are_filled_in() {
        let spec = normalize(spec(vec![container("main")])).unwrap();
        assert_eq!(
            spec.resources,
            Some(Resources {
                vcpus: 1,
                memory_mib: 512
            })
        );
        assert_eq!(normalize(spec.clone()).unwrap(), spec);
    }

    #[test]
    fn bad_specs_are_refused() {
        assert!(normalize(spec(vec![container("main"), container("main")])).is_err());
        assert!(normalize(spec(vec![container("a/b")])).is_err());
        assert!(normalize(spec(vec![container("..")])).is_err());
        let mut no_image = container("main");
        no_image.image_ref.clear();
        assert!(normalize(spec(vec![no_image])).is_err());
        let mut small = spec(vec![container("main")]);
        small.resources = Some(Resources {
            vcpus: 1,
            memory_mib: 64,
        });
        assert!(normalize(small).is_err());
    }

    #[test]
    fn a_pod_has_a_network_unless_it_says_none() {
        let mut spec = spec(vec![]);
        assert!(has_network(&spec));
        spec.network = Some(PodNetwork {
            none: false,
            ..Default::default()
        });
        assert!(has_network(&spec));
        spec.network = Some(PodNetwork {
            none: true,
            ..Default::default()
        });
        assert!(!has_network(&spec));
    }

    #[test]
    fn a_pod_may_come_without_containers() {
        assert!(normalize(spec(vec![])).is_ok());
    }

    #[test]
    fn pod_ids_are_short_or_uuids() {
        assert!(check_id("k7m2q9x4c1b").is_ok());
        assert!(check_id("11111111-2222-3333-4444-555555555555").is_ok());
        assert!(check_id("../etc").is_err());
        assert!(check_id("").is_err());
        // A digit first, a letter Crockford leaves out, upper case.
        assert!(check_id("07m2q9x4c1b").is_err());
        assert!(check_id("k7m2q9x4c1u").is_err());
        assert!(check_id("K7M2Q9X4C1B").is_err());
    }

    #[test]
    fn the_network_becomes_eth0() {
        let spec = normalize(spec(vec![container("main")])).unwrap();
        let id = "11111111-2222-3333-4444-555555555555";
        let attachment = Attachment {
            tap: network::tap_name(id),
            mac: network::mac(id),
            ipv6: "fd00:fe05:1::9".parse().unwrap(),
            ipv4: "169.254.16.9".parse().unwrap(),
            dns: "169.254.0.53".parse().unwrap(),
            routes: network::routes("fd00:fe05:1::9".parse().unwrap(), 1420),
        };
        let config = vm_config(id, &spec, Some(&attachment), None, &[]);
        assert_eq!(config.net[0].tap.as_ref().unwrap().tap_name, "pod-11111111");
        assert_eq!(config.net[0].mac_address, "06:00:11:11:11:11");
        let guest = config.guest_network.unwrap();
        assert_eq!(guest.interface, "eth0");
        assert_eq!(guest.ipv6_cidr, "fd00:fe05:1::9/128");
        assert_eq!(guest.ipv6_gateway, "fe80::1");
        assert_eq!(
            (guest.ipv4_cidr.as_str(), guest.gateway.as_str()),
            ("169.254.16.9/32", "169.254.0.1")
        );
        assert_eq!(guest.dns, ["169.254.0.53"]);
        assert_eq!(guest.hostname, id);
        // A ULA: no default route over IPv6.
        assert_eq!(
            guest.routes,
            [GuestRoute {
                destination: "fc00::/7".to_string(),
                gateway: "fe80::1".to_string(),
                mtu: 1420,
            }]
        );
        assert_eq!(config.memory.unwrap().size_mib, 512);

        let config = vm_config(id, &spec, None, None, &[]);
        assert!(config.net.is_empty() && config.guest_network.is_none());
        assert!(config.metadata_json.is_empty());
    }

    #[test]
    fn the_metadata_has_the_pods_identity_and_the_specs() {
        let mut spec = normalize(spec(vec![])).unwrap();
        spec.metadata = [
            ("zone".to_string(), "b".to_string()),
            ("role".to_string(), "web".to_string()),
        ]
        .into();
        let id = "11111111-2222-3333-4444-555555555555";
        let attachment = Attachment {
            tap: network::tap_name(id),
            mac: network::mac(id),
            ipv6: "fd00:fe05:1::9".parse().unwrap(),
            ipv4: "169.254.16.9".parse().unwrap(),
            dns: "169.254.0.53".parse().unwrap(),
            routes: Vec::new(),
        };
        let config = vm_config(id, &spec, Some(&attachment), None, &[]);
        assert_eq!(
            config.metadata_json,
            r#"{"feos":{"ipv4":"169.254.16.9","ipv6":"fd00:fe05:1::9","pod-id":"11111111-2222-3333-4444-555555555555"},"metadata":{"role":"web","zone":"b"}}"#
        );
        // The same pod with the same addresses, the same configuration.
        for _ in 0..8 {
            let again = vm_config(id, &spec.clone(), Some(&attachment), None, &[]);
            assert_eq!(again.metadata_json, config.metadata_json);
        }
    }

    #[test]
    fn metadata_takes_names_a_network_and_little_room() {
        let with = |key: &str, value: &str| {
            let mut spec = spec(vec![]);
            spec.metadata = [(key.to_string(), value.to_string())].into();
            spec
        };
        assert!(normalize(with("role", "web")).is_ok());
        assert!(normalize(with("a-b_c.1", &"x".repeat(16 * 1024))).is_ok());
        for bad in ["", ".", "..", "a/b", "a b", "ä", &"k".repeat(129)] {
            assert!(normalize(with(bad, "v")).is_err(), "{bad:?}");
        }
        assert!(normalize(with("role", &"x".repeat(MAX_METADATA_BYTES))).is_err());
        // Escaped, a value takes more room.
        assert!(normalize(with("role", &"\u{1}".repeat(MAX_METADATA_BYTES / 4))).is_err());
        let mut none = with("role", "web");
        none.network = Some(PodNetwork {
            none: true,
            ..Default::default()
        });
        assert!(normalize(none).is_err());
    }

    #[test]
    fn the_disk_is_the_data_disk() {
        let spec = normalize(spec(vec![])).unwrap();
        let id = "11111111-2222-3333-4444-555555555555";
        let link = format!("/var/lib/feos/disk/live/{id}/disk");
        let config = vm_config(id, &spec, None, Some(&link), &[]);
        assert_eq!(config.data_disk.unwrap().path, link);
        assert!(vm_config(id, &spec, None, None, &[]).data_disk.is_none());
        let volumes = ["/a".to_string(), "/b".to_string()];
        let config = vm_config(id, &spec, None, Some(&link), &volumes);
        let paths: Vec<_> = config.volumes.iter().map(|v| v.path.as_str()).collect();
        assert_eq!(paths, ["/a", "/b"]);
    }

    fn volume(name: &str, volume_id: &str) -> PodVolume {
        PodVolume {
            name: name.to_string(),
            volume_id: volume_id.to_string(),
        }
    }

    fn mount(volume: &str, path: &str) -> VolumeMount {
        VolumeMount {
            volume: volume.to_string(),
            path: path.to_string(),
            read_only: false,
        }
    }

    const A: &str = "aaaaaaaa-0000-0000-0000-000000000001";
    const B: &str = "bbbbbbbb-0000-0000-0000-000000000002";

    #[test]
    fn volumes_are_named_once_and_mounted_by_name() {
        let with = |volumes: Vec<PodVolume>, mounts: Vec<VolumeMount>| {
            let mut main = container("main");
            main.volume_mounts = mounts;
            let mut spec = spec(vec![main]);
            spec.volumes = volumes;
            normalize(spec)
        };
        let good = with(
            vec![volume("db", A), volume("logs", B)],
            vec![mount("db", "/var/lib/db"), mount("logs", "/logs")],
        )
        .unwrap();
        assert_eq!(volume_ids(&good), [A, B]);
        // A volume no container mounts is fine.
        assert!(with(vec![volume("db", A)], vec![]).is_ok());

        for (volumes, mounts) in [
            (vec![volume("db", A), volume("db", B)], vec![]),
            (vec![volume("db", A), volume("logs", A)], vec![]),
            (vec![volume("DB", A)], vec![]),
            (vec![volume("db", "not-a-uuid")], vec![]),
            (vec![volume("db", &A.to_uppercase())], vec![]),
            (vec![volume("db", A)], vec![mount("logs", "/logs")]),
            (vec![volume("db", A)], vec![mount("db", "/")]),
            (vec![volume("db", A)], vec![mount("db", "relative")]),
            (
                vec![volume("db", A), volume("logs", B)],
                vec![mount("db", "/data"), mount("logs", "/data")],
            ),
        ] {
            let status = with(volumes.clone(), mounts.clone()).unwrap_err();
            assert_eq!(
                status.code(),
                tonic::Code::InvalidArgument,
                "{volumes:?} {mounts:?}"
            );
        }
        let many = (0..9)
            .map(|i| {
                volume(
                    &format!("v{i}"),
                    &format!("aaaaaaaa-0000-0000-0000-00000000000{i}"),
                )
            })
            .collect();
        assert!(with(many, vec![]).is_err());
    }

    #[test]
    fn a_pod_with_a_network_gets_otels_variables_its_own_win() {
        let otlp = Ipv4Addr::new(169, 254, 169, 1);
        let map = |pairs: &[(&str, &str)]| -> HashMap<String, String> {
            pairs
                .iter()
                .map(|(k, v)| (k.to_string(), v.to_string()))
                .collect()
        };
        let spec = PodSpec::default();
        let service = map(&[("ferun.object", "web"), ("ferun.job", "j1")]);
        assert_eq!(
            otel_env(&map(&[("A", "1")]), &spec, &service, otlp),
            map(&[
                ("A", "1"),
                (
                    "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
                    "http://169.254.169.1:4318/v1/traces"
                ),
                ("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "http/protobuf"),
                ("OTEL_SERVICE_NAME", "web"),
            ])
        );
        // A job's pod is named after its job; one without either has none.
        let job = map(&[("ferun.job", "j1")]);
        let env = otel_env(&HashMap::new(), &spec, &job, otlp);
        assert_eq!(env["OTEL_SERVICE_NAME"], "j1");
        let env = otel_env(&HashMap::new(), &spec, &HashMap::new(), otlp);
        assert!(!env.contains_key("OTEL_SERVICE_NAME"));
        assert_eq!(env.len(), 2);
        // The container's own are kept.
        let own = map(&[
            ("OTEL_SERVICE_NAME", "mine"),
            ("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "grpc"),
        ]);
        let env = otel_env(&own, &spec, &service, otlp);
        assert_eq!(env["OTEL_SERVICE_NAME"], "mine");
        assert_eq!(env["OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"], "grpc");
        assert_eq!(env.len(), 3);
        // A pod without a network gets nothing.
        let none = PodSpec {
            network: Some(feos_proto::pod_service::PodNetwork {
                none: true,
                ..Default::default()
            }),
            ..Default::default()
        };
        assert_eq!(otel_env(&own, &none, &service, otlp), own);
    }
}
