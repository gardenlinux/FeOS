// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0
mod kernel_stats;

use anyhow::Result;
use clap::builder::ArgPredicate;
use clap::{ArgGroup, Args, Subcommand};
use feos_proto::host_service::{
    host_service_client::HostServiceClient, AdjustTimeRequest, FreezeFilesystemRequest,
    GetCpuInfoRequest, GetMemoryBreakdownRequest, GetNetworkConfigRequest, GetNetworkInfoRequest,
    GetTimeRequest, GetVersionInfoRequest, HostnameRequest, MemoryBreakdown, MemoryRequest,
    NetworkRoute, PingRequest, PreparePauseRequest, RebootRequest, SetNetworkConfigRequest,
    SetTimeRequest, ShutdownRequest, StreamFeosLogsRequest, StreamKernelLogsRequest,
    ThawFilesystemRequest, UpgradeFeosBinaryRequest,
};
use prost_types::Timestamp;
use std::time::{SystemTime, UNIX_EPOCH};
use tokio_stream::StreamExt;
use tonic::transport::Channel;

use crate::host_commands::kernel_stats::get_kernel_stats;

#[derive(Args, Debug)]
pub struct HostArgs {
    #[arg(
        short,
        long,
        global = true,
        env = "FEOS_ADDRESS",
        default_value = "unix:/run/feos/api.sock"
    )]
    pub address: String,

    #[command(subcommand)]
    command: HostCommand,
}

#[derive(Subcommand, Debug)]
pub enum HostCommand {
    /// Get the host machine's hostname
    Hostname,
    /// Display detailed memory information from /proc/meminfo
    Memory,
    /// Display CPU information from /proc/cpuinfo
    CpuInfo,
    /// Display CPU usage statistics from /proc/stat
    KernelStats,
    /// Display network interface statistics
    NetworkInfo,
    /// Upgrade the FeOS binary from a remote URL
    Upgrade {
        #[arg(long, required = true, help = "URL to fetch the new FeOS binary from")]
        url: String,
        #[arg(
            long,
            required = true,
            help = "Hex-encoded SHA256 checksum for verification"
        )]
        sha256_sum: String,
    },
    /// Stream kernel logs from /dev/kmsg
    Klogs,
    /// Stream logs from the internal FeOS logger
    Flogs,
    /// Shutdown the host machine
    Shutdown,
    /// Reboot the host machine
    Reboot,
    /// Get kernel and FeOS version information
    VersionInfo,
    /// Check that FeOS answers; prints the ping count, uptime and boot id
    Ping,
    /// Set the host's clock; defaults to the local time of this machine
    SetTime {
        #[arg(help = "Seconds since the Unix epoch")]
        unix_seconds: Option<i64>,
    },
    /// Read the clock, and how far it is from this machine's
    GetTime,
    /// Correct the clock by an offset, as feosd does once a minute (ADR
    /// 0027): over 100 ms it is stepped, otherwise slewed
    AdjustTime {
        #[arg(
            allow_hyphen_values = true,
            help = "How far the clock is ahead, in nanoseconds; negative if behind"
        )]
        offset_ns: i64,
    },
    /// Push a static network configuration: an IPv6 address, an IPv4
    /// address or both
    ///
    /// A family left out is not configured. A pod gets both, each with the
    /// node's address on its link as gateway: fe80::1 and 169.254.0.1.
    #[command(group(
        ArgGroup::new("addresses").required(true).multiple(true).args(["ipv4", "ipv6"])
    ))]
    SetNetwork {
        #[arg(long, default_value = "eth0")]
        interface: String,
        #[arg(
            long,
            help = "IPv4 address with prefix, e.g. 169.254.16.2/32 or 192.168.100.10/24"
        )]
        ipv4: Option<String>,
        #[arg(
            long,
            requires = "ipv4",
            default_value_if("ipv4", ArgPredicate::IsPresent, "169.254.0.1"),
            help = "IPv4 default gateway on the interface's link, also outside the \
                    address's prefix; 169.254.0.1 if not given"
        )]
        gateway: Option<String>,
        #[arg(long, help = "IPv6 address with prefix, e.g. fd00:fe05::2/128")]
        ipv6: Option<String>,
        #[arg(
            long,
            requires = "ipv6",
            default_value_if("ipv6", ArgPredicate::IsPresent, "fe80::1"),
            help = "IPv6 default gateway on the interface's link; fe80::1 if not given"
        )]
        ipv6_gateway: Option<String>,
        #[arg(long = "dns", help = "Resolver; repeat for more than one")]
        dns: Vec<String>,
        #[arg(long, default_value = "")]
        hostname: String,
        #[arg(
            long = "route",
            help = "A prefix via its family's gateway, in place of the family's default \
                    route, e.g. fc00::/7; repeat for more than one"
        )]
        routes: Vec<String>,
        #[arg(
            long,
            default_value_t = 0,
            help = "The routes' MTU; 0 is the interface's"
        )]
        route_mtu: u32,
    },
    /// Show the last pushed network configuration
    GetNetwork,
    /// Freeze a filesystem (default: the data root)
    Freeze {
        #[arg(default_value = "")]
        mountpoint: String,
    },
    /// Thaw a frozen filesystem (default: the data root)
    Thaw {
        #[arg(default_value = "")]
        mountpoint: String,
    },
    /// Show who holds the memory a snapshot would write
    MemoryBreakdown {
        #[arg(long, help = "Include the resident memory of every process")]
        processes: bool,
    },
    /// Empty memory before a pause: sync and drop caches, optionally more
    PreparePause {
        #[arg(long)]
        compact: bool,
        #[arg(long)]
        zero_free_pages: bool,
        #[arg(long, default_value_t = 0)]
        settle_seconds: u32,
    },
}

pub async fn handle_host_command(args: HostArgs) -> Result<()> {
    let mut client = HostServiceClient::new(crate::transport::channel(&args.address).await?);

    match args.command {
        HostCommand::Hostname => get_hostname(&mut client).await?,
        HostCommand::Memory => get_memory(&mut client).await?,
        HostCommand::CpuInfo => get_cpu_info(&mut client).await?,
        HostCommand::KernelStats => get_kernel_stats(&mut client).await?,
        HostCommand::NetworkInfo => get_network_info(&mut client).await?,
        HostCommand::Upgrade { url, sha256_sum } => {
            upgrade_feos(&mut client, url, sha256_sum).await?
        }
        HostCommand::Klogs => stream_klogs(&mut client).await?,
        HostCommand::Flogs => stream_flogs(&mut client).await?,
        HostCommand::Shutdown => shutdown_host(&mut client).await?,
        HostCommand::Reboot => reboot_host(&mut client).await?,
        HostCommand::VersionInfo => get_version_info(&mut client).await?,
        HostCommand::Ping => ping(&mut client).await?,
        HostCommand::SetTime { unix_seconds } => set_time(&mut client, unix_seconds).await?,
        HostCommand::GetTime => get_time(&mut client).await?,
        HostCommand::AdjustTime { offset_ns } => {
            let response = client
                .adjust_time(AdjustTimeRequest { offset_ns })
                .await?
                .into_inner();
            println!(
                "{}; frequency {} ppb",
                if response.stepped {
                    "stepped"
                } else {
                    "slewing"
                },
                response.frequency_ppb
            );
        }
        HostCommand::SetNetwork {
            interface,
            ipv4,
            gateway,
            ipv6,
            ipv6_gateway,
            dns,
            hostname,
            routes,
            route_mtu,
        } => {
            let gateway = gateway.unwrap_or_default();
            let ipv6_gateway = ipv6_gateway.unwrap_or_default();
            let routes = routes
                .into_iter()
                .map(|destination| NetworkRoute {
                    gateway: match destination.contains(':') {
                        true => ipv6_gateway.clone(),
                        false => gateway.clone(),
                    },
                    destination,
                    mtu: route_mtu,
                })
                .collect();
            let request = SetNetworkConfigRequest {
                interface,
                ipv4_cidr: ipv4.unwrap_or_default(),
                gateway,
                dns,
                hostname,
                ipv6_cidr: ipv6.unwrap_or_default(),
                ipv6_gateway,
                routes,
            };
            set_network(&mut client, request).await?
        }
        HostCommand::GetNetwork => get_network(&mut client).await?,
        HostCommand::Freeze { mountpoint } => {
            let request = FreezeFilesystemRequest { mountpoint };
            let device = client.freeze_filesystem(request).await?.into_inner().device;
            println!("Frozen ({device})");
        }
        HostCommand::Thaw { mountpoint } => {
            let request = ThawFilesystemRequest { mountpoint };
            let device = client.thaw_filesystem(request).await?.into_inner().device;
            println!("Thawed ({device})");
        }
        HostCommand::MemoryBreakdown { processes } => {
            let request = GetMemoryBreakdownRequest {
                with_processes: processes,
                ..Default::default()
            };
            let breakdown = client.get_memory_breakdown(request).await?.into_inner();
            print_memory_breakdown(&breakdown);
        }
        HostCommand::PreparePause {
            compact,
            zero_free_pages,
            settle_seconds,
        } => {
            let request = PreparePauseRequest {
                compact,
                zero_free_pages,
                settle_seconds,
                ..Default::default()
            };
            let response = client.prepare_pause(request).await?.into_inner();
            for step in &response.steps {
                println!("{step}");
            }
            if let (Some(before), Some(after)) = (response.before, response.after) {
                println!(
                    "MemFree: {} -> {} MiB in {} ms",
                    before.mem_free_kb / 1024,
                    after.mem_free_kb / 1024,
                    response.duration_ms
                );
            }
        }
    }

    Ok(())
}

async fn get_hostname(client: &mut HostServiceClient<Channel>) -> Result<()> {
    let request = HostnameRequest {};
    let response = client.hostname(request).await?.into_inner();
    println!("{}", response.hostname);
    Ok(())
}

async fn get_memory(client: &mut HostServiceClient<Channel>) -> Result<()> {
    let request = MemoryRequest {};
    let response = client.get_memory(request).await?.into_inner();

    if let Some(mem_info) = response.mem_info {
        println!("{:<20} {:>15} kB", "Key", "Value");
        println!("{:-<20} {:-<16}", "", "");
        println!("{:<20} {:>15} kB", "MemTotal:", mem_info.memtotal);
        println!("{:<20} {:>15} kB", "MemFree:", mem_info.memfree);
        println!("{:<20} {:>15} kB", "MemAvailable:", mem_info.memavailable);
        println!("{:<20} {:>15} kB", "Buffers:", mem_info.buffers);
        println!("{:<20} {:>15} kB", "Cached:", mem_info.cached);
        println!("{:<20} {:>15} kB", "SwapCached:", mem_info.swapcached);
        println!("{:<20} {:>15} kB", "Active:", mem_info.active);
        println!("{:<20} {:>15} kB", "Inactive:", mem_info.inactive);
        println!("{:<20} {:>15} kB", "Active(anon):", mem_info.activeanon);
        println!("{:<20} {:>15} kB", "Inactive(anon):", mem_info.inactiveanon);
        println!("{:<20} {:>15} kB", "Active(file):", mem_info.activefile);
        println!("{:<20} {:>15} kB", "Inactive(file):", mem_info.inactivefile);
        println!("{:<20} {:>15} kB", "Unevictable:", mem_info.unevictable);
        println!("{:<20} {:>15} kB", "Mlocked:", mem_info.mlocked);
        println!("{:<20} {:>15} kB", "SwapTotal:", mem_info.swaptotal);
        println!("{:<20} {:>15} kB", "SwapFree:", mem_info.swapfree);
        println!("{:<20} {:>15} kB", "Dirty:", mem_info.dirty);
        println!("{:<20} {:>15} kB", "Writeback:", mem_info.writeback);
        println!("{:<20} {:>15} kB", "AnonPages:", mem_info.anonpages);
        println!("{:<20} {:>15} kB", "Mapped:", mem_info.mapped);
        println!("{:<20} {:>15} kB", "Shmem:", mem_info.shmem);
        println!("{:<20} {:>15} kB", "Slab:", mem_info.slab);
        println!("{:<20} {:>15} kB", "SReclaimable:", mem_info.sreclaimable);
        println!("{:<20} {:>15} kB", "SUnreclaim:", mem_info.sunreclaim);
        println!("{:<20} {:>15} kB", "KernelStack:", mem_info.kernelstack);
        println!("{:<20} {:>15} kB", "PageTables:", mem_info.pagetables);
        println!("{:<20} {:>15} kB", "NFS_Unstable:", mem_info.nfsunstable);
        println!("{:<20} {:>15} kB", "Bounce:", mem_info.bounce);
        println!("{:<20} {:>15} kB", "WritebackTmp:", mem_info.writebacktmp);
        println!("{:<20} {:>15} kB", "CommitLimit:", mem_info.commitlimit);
        println!("{:<20} {:>15} kB", "Committed_AS:", mem_info.committedas);
        println!("{:<20} {:>15} kB", "VmallocTotal:", mem_info.vmalloctotal);
        println!("{:<20} {:>15} kB", "VmallocUsed:", mem_info.vmallocused);
        println!("{:<20} {:>15} kB", "VmallocChunk:", mem_info.vmallocchunk);
        println!(
            "{:<20} {:>15} kB",
            "HardwareCorrupted:", mem_info.hardwarecorrupted
        );
        println!("{:<20} {:>15} kB", "AnonHugePages:", mem_info.anonhugepages);
        println!(
            "{:<20} {:>15} kB",
            "ShmemHugePages:", mem_info.shmemhugepages
        );
        println!(
            "{:<20} {:>15} kB",
            "ShmemPmdMapped:", mem_info.shmempmdmapped
        );
        println!("{:<20} {:>15} kB", "CmaTotal:", mem_info.cmatotal);
        println!("{:<20} {:>15} kB", "CmaFree:", mem_info.cmafree);
        println!(
            "{:<20} {:>15} kB",
            "HugePages_Total:", mem_info.hugepagestotal
        );
        println!(
            "{:<20} {:>15} kB",
            "HugePages_Free:", mem_info.hugepagesfree
        );
        println!(
            "{:<20} {:>15} kB",
            "HugePages_Rsvd:", mem_info.hugepagesrsvd
        );
        println!(
            "{:<20} {:>15} kB",
            "HugePages_Surp:", mem_info.hugepagessurp
        );
        println!("{:<20} {:>15} kB", "Hugepagesize:", mem_info.hugepagesize);
        println!("{:<20} {:>15} kB", "DirectMap4k:", mem_info.directmap4k);
        println!("{:<20} {:>15} kB", "DirectMap2m:", mem_info.directmap2m);
        println!("{:<20} {:>15} kB", "DirectMap1G:", mem_info.directmap1g);
    } else {
        println!("No memory information received from the host.");
    }
    Ok(())
}

async fn get_cpu_info(client: &mut HostServiceClient<Channel>) -> Result<()> {
    let request = GetCpuInfoRequest {};
    let response = client.get_cpu_info(request).await?.into_inner();

    if response.cpu_info.is_empty() {
        println!("No CPU information received from the host.");
        return Ok(());
    }

    for (i, cpu) in response.cpu_info.iter().enumerate() {
        println!("--- Processor {} ---", cpu.processor);
        println!("{:<20}: {}", "Vendor ID", cpu.vendor_id);
        println!("{:<20}: {}", "Model Name", cpu.model_name);
        println!("{:<20}: {}", "CPU Family", cpu.cpu_family);
        println!("{:<20}: {}", "Model", cpu.model);
        println!("{:<20}: {}", "Stepping", cpu.stepping);
        println!("{:<20}: {:.3} MHz", "CPU MHz", cpu.cpu_mhz);
        println!("{:<20}: {}", "Cache Size", cpu.cache_size);
        println!("{:<20}: {}", "Physical ID", cpu.physical_id);
        println!("{:<20}: {}", "Core ID", cpu.core_id);
        println!("{:<20}: {}", "CPU Cores", cpu.cpu_cores);
        println!("{:<20}: {}", "Siblings", cpu.siblings);
        println!("{:<20}: {}", "Address Sizes", cpu.address_sizes);
        println!("{:<20}: {:.2}", "BogoMIPS", cpu.bogo_mips);
        if i < response.cpu_info.len() - 1 {
            println!();
        }
    }

    Ok(())
}

async fn get_network_info(client: &mut HostServiceClient<Channel>) -> Result<()> {
    let request = GetNetworkInfoRequest {};
    let response = client.get_network_info(request).await?.into_inner();

    if response.devices.is_empty() {
        println!("No network devices found on the host.");
        return Ok(());
    }

    for dev in response.devices {
        println!("Interface: {}", dev.name);
        println!("  RX");
        println!("    Bytes:    {:>15}", dev.rx_bytes);
        println!("    Packets:  {:>15}", dev.rx_packets);
        println!("    Errors:   {:>15}", dev.rx_errors);
        println!("    Dropped:  {:>15}", dev.rx_dropped);
        println!("  TX");
        println!("    Bytes:    {:>15}", dev.tx_bytes);
        println!("    Packets:  {:>15}", dev.tx_packets);
        println!("    Errors:   {:>15}", dev.tx_errors);
        println!("    Dropped:  {:>15}", dev.tx_dropped);
        println!();
    }

    Ok(())
}

async fn stream_klogs(client: &mut HostServiceClient<Channel>) -> Result<()> {
    println!("Streaming kernel logs... Press Ctrl+C to stop.");
    let request = StreamKernelLogsRequest {};
    let mut stream = client.stream_kernel_logs(request).await?.into_inner();

    while let Some(entry_res) = stream.next().await {
        match entry_res {
            Ok(entry) => {
                println!("{}", entry.message);
            }
            Err(status) => {
                eprintln!("Error in kernel log stream: {status}");
                break;
            }
        }
    }

    Ok(())
}

async fn stream_flogs(client: &mut HostServiceClient<Channel>) -> Result<()> {
    println!("Streaming FeOS logs... Press Ctrl+C to stop.");
    let request = StreamFeosLogsRequest {};
    let mut stream = client.stream_fe_os_logs(request).await?.into_inner();

    while let Some(entry_res) = stream.next().await {
        match entry_res {
            Ok(entry) => {
                let ts = entry
                    .timestamp
                    .map(|t| {
                        chrono::DateTime::from_timestamp(t.seconds, t.nanos as u32)
                            .unwrap_or_default()
                            .to_rfc3339()
                    })
                    .unwrap_or_default();
                println!(
                    "[{ts} {:<5} {}] {}",
                    entry.level, entry.target, entry.message
                );
            }
            Err(status) => {
                eprintln!("Error in FeOS log stream: {status}");
                break;
            }
        }
    }
    Ok(())
}

async fn upgrade_feos(
    client: &mut HostServiceClient<Channel>,
    url: String,
    sha256_sum: String,
) -> Result<()> {
    println!("Requesting FeOS upgrade from URL: {url}");
    println!("Expected SHA256: {sha256_sum}");

    let request = UpgradeFeosBinaryRequest { url, sha256_sum };

    client.upgrade_feos_binary(request).await?;

    println!("Upgrade request accepted by host.");

    Ok(())
}

async fn get_version_info(client: &mut HostServiceClient<Channel>) -> Result<()> {
    println!("Requesting version information...");
    let request = GetVersionInfoRequest {};
    let response = client.get_version_info(request).await?.into_inner();
    println!("FeOS Version:    {}", response.feos_version);
    println!("Kernel Version:  {}", response.kernel_version);
    Ok(())
}

async fn shutdown_host(client: &mut HostServiceClient<Channel>) -> Result<()> {
    println!("Requesting host shutdown...");
    let request = ShutdownRequest {};
    client.shutdown(request).await?;
    println!("Shutdown command sent successfully. Connection will be lost.");
    Ok(())
}

async fn reboot_host(client: &mut HostServiceClient<Channel>) -> Result<()> {
    println!("Requesting host reboot...");
    let request = RebootRequest {};
    client.reboot(request).await?;
    println!("Reboot command sent successfully. Connection will be lost.");
    Ok(())
}

async fn ping(client: &mut HostServiceClient<Channel>) -> Result<()> {
    let response = client.ping(PingRequest {}).await?.into_inner();
    println!(
        "pong: tick={} uptime={}s boot_id={}",
        response.tick, response.uptime_seconds, response.boot_id
    );
    Ok(())
}

async fn set_time(
    client: &mut HostServiceClient<Channel>,
    unix_seconds: Option<i64>,
) -> Result<()> {
    let time = match unix_seconds {
        Some(seconds) => Timestamp { seconds, nanos: 0 },
        None => {
            let now = SystemTime::now().duration_since(UNIX_EPOCH)?;
            Timestamp {
                seconds: now.as_secs() as i64,
                nanos: now.subsec_nanos() as i32,
            }
        }
    };
    let seconds = time.seconds;
    client.set_time(SetTimeRequest { time: Some(time) }).await?;
    println!("Clock set to {seconds}");
    Ok(())
}

/// Prints the time and how far it is from this machine's clock: of four
/// calls, the one with the shortest round trip, against its middle.
async fn get_time(client: &mut HostServiceClient<Channel>) -> Result<()> {
    let mut best: Option<(Timestamp, i128, std::time::Duration)> = None;
    for _ in 0..4 {
        let sent = SystemTime::now();
        let response = client.get_time(GetTimeRequest {}).await?.into_inner();
        let round_trip = SystemTime::now().duration_since(sent).unwrap_or_default();
        if best
            .as_ref()
            .is_some_and(|(_, _, best)| *best <= round_trip)
        {
            continue;
        }
        let time = response.time.unwrap_or_default();
        let middle = (sent + round_trip / 2).duration_since(UNIX_EPOCH)?;
        let offset_ns = i128::from(time.seconds) * 1_000_000_000 + i128::from(time.nanos)
            - middle.as_nanos() as i128;
        best = Some((time, offset_ns, round_trip));
    }
    let Some((time, offset_ns, round_trip)) = best else {
        return Ok(());
    };
    match chrono::DateTime::from_timestamp(time.seconds, time.nanos as u32) {
        Some(datetime) => print!("{}", datetime.to_rfc3339()),
        None => print!("{}.{:09}", time.seconds, time.nanos),
    }
    println!(
        " ({:+.3} ms from this machine's clock, round trip {:.3} ms)",
        offset_ns as f64 / 1e6,
        round_trip.as_secs_f64() * 1e3
    );
    Ok(())
}

async fn set_network(
    client: &mut HostServiceClient<Channel>,
    request: SetNetworkConfigRequest,
) -> Result<()> {
    let response = client.set_network_config(request).await?.into_inner();
    println!("Network configured.");
    for failure in response.resolver_failed {
        println!(
            "Container {}: resolvers not linked: {}",
            failure.container_id, failure.reason
        );
    }
    Ok(())
}

async fn get_network(client: &mut HostServiceClient<Channel>) -> Result<()> {
    let response = client
        .get_network_config(GetNetworkConfigRequest {})
        .await?
        .into_inner();
    if !response.configured {
        println!("No network configuration pushed since boot.");
        return Ok(());
    }
    println!("Interface:    {}", response.interface);
    println!("IPv6:         {}", response.ipv6_cidr);
    println!("IPv6 gateway: {}", response.ipv6_gateway);
    println!("IPv4:         {}", response.ipv4_cidr);
    println!("IPv4 gateway: {}", response.gateway);
    println!("DNS:          {}", response.dns.join(", "));
    println!("Hostname:     {}", response.hostname);
    for route in &response.routes {
        match route.mtu {
            0 => println!("Route:        {} via {}", route.destination, route.gateway),
            mtu => println!(
                "Route:        {} via {}, MTU {mtu}",
                route.destination, route.gateway
            ),
        }
    }
    Ok(())
}

fn print_memory_breakdown(m: &MemoryBreakdown) {
    let mib = |kb: u64| kb as f64 / 1024.0;
    println!(
        "System RAM {:.0} MiB = MemTotal {:.0} MiB + kernel image {:.1} MiB + memmap/reserved {:.1} MiB",
        mib(m.system_ram_kb),
        mib(m.mem_total_kb),
        mib(m.kernel_image_kb),
        mib(m.memmap_reserved_kb)
    );
    println!(
        "Free {:.0} MiB, available {:.0} MiB, cached {:.1} MiB, anon {:.1} MiB, unreclaimable {:.1} MiB",
        mib(m.mem_free_kb),
        mib(m.mem_available_kb),
        mib(m.cached_kb),
        mib(m.anon_pages_kb),
        mib(m.unreclaimable_kb)
    );
    for process in &m.processes {
        println!(
            "{:>7} {:<20} {:>8.1} MiB",
            process.pid,
            process.name,
            mib(process.rss_kb)
        );
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use clap::Parser;

    /// With the host's own arguments: clap checks that no name is taken twice.
    #[derive(Parser)]
    struct Cli {
        #[command(flatten)]
        host: HostArgs,
    }

    fn addresses(args: &[&str]) -> Option<[Option<String>; 4]> {
        let cli = Cli::try_parse_from([["host", "set-network"].as_slice(), args].concat()).ok()?;
        let HostCommand::SetNetwork {
            ipv4,
            gateway,
            ipv6,
            ipv6_gateway,
            ..
        } = cli.host.command
        else {
            panic!("not a set-network");
        };
        Some([ipv4, gateway, ipv6, ipv6_gateway])
    }

    #[test]
    fn set_network_takes_either_family_or_both() {
        let some = |s: &str| Some(s.to_string());
        assert_eq!(
            addresses(&["--ipv6", "fd00::2/128"]),
            Some([None, None, some("fd00::2/128"), some("fe80::1")])
        );
        assert_eq!(
            addresses(&["--ipv6", "fd00::2/128", "--ipv6-gateway", "fd00::1"]),
            Some([None, None, some("fd00::2/128"), some("fd00::1")])
        );
        assert_eq!(
            addresses(&["--ipv4", "10.0.0.2/24", "--gateway", "10.0.0.1"]),
            Some([some("10.0.0.2/24"), some("10.0.0.1"), None, None])
        );
        assert!(addresses(&[
            "--ipv4",
            "10.0.0.2/24",
            "--gateway",
            "10.0.0.1",
            "--ipv6",
            "fd00::2/128"
        ])
        .is_some());
        // A pod's: the gateways are the node's on the link.
        assert_eq!(
            addresses(&["--ipv4", "169.254.16.2/32", "--ipv6", "fd00::2/128"]),
            Some([
                some("169.254.16.2/32"),
                some("169.254.0.1"),
                some("fd00::2/128"),
                some("fe80::1")
            ])
        );
        assert_eq!(addresses(&[]), None);
        assert_eq!(addresses(&["--gateway", "10.0.0.1"]), None);
        assert_eq!(addresses(&["--ipv6-gateway", "fe80::1"]), None);
    }
}
