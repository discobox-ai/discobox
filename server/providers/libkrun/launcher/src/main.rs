#[cfg(not(all(target_os = "linux", target_arch = "x86_64")))]
compile_error!("discobox-krun currently supports only x86_64 Linux");

mod config;
mod krun;

use config::Config;
use serde::Serialize;
use std::env;
use std::fs::{self, File, OpenOptions};
use std::io::Write;
use std::net::Ipv4Addr;
use std::os::fd::AsRawFd;
use std::os::unix::fs::OpenOptionsExt;
use std::os::unix::process::CommandExt;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, ExitCode, Stdio};
use std::thread;
use std::time::{Duration, Instant};

const PASST_START_TIMEOUT: Duration = Duration::from_secs(5);

fn main() -> ExitCode {
    match run() {
        Ok(()) => ExitCode::SUCCESS,
        Err(err) => {
            eprintln!("discobox-krun: {err}");
            ExitCode::FAILURE
        }
    }
}

fn run() -> Result<(), String> {
    let (operation, config_path) = parse_args()?;
    let config = Config::from_path(&config_path)?;
    if operation == Operation::Validate {
        config.validate_disk_files()?;
        return validate_kvm();
    }

    config.prepare_runtime_dir()?;
    let _lock = acquire_lock(&config.runtime_dir)?;
    config.prepare_locked_runtime()?;
    write_identity(&config.runtime_dir)?;
    let passt = start_passt(&config)?;
    supervise_passt(passt);
    krun::run(&config)
}

fn validate_kvm() -> Result<(), String> {
    const KVM_GET_API_VERSION: libc::c_ulong = 0xAE00;
    const KVM_API_VERSION: libc::c_int = 12;

    let path = Path::new("/dev/kvm");
    let device = OpenOptions::new()
        .read(true)
        .write(true)
        .open(path)
        .map_err(|err| format!("open KVM device {}: {err}", path.display()))?;
    let version = unsafe { libc::ioctl(device.as_raw_fd(), KVM_GET_API_VERSION) };
    if version < 0 {
        return Err(format!(
            "query KVM API version from {}: {}",
            path.display(),
            std::io::Error::last_os_error()
        ));
    }
    if version != KVM_API_VERSION {
        return Err(format!(
            "KVM API version at {} is {version}, want {KVM_API_VERSION}",
            path.display()
        ));
    }
    Ok(())
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Operation {
    Run,
    Validate,
}

fn parse_args() -> Result<(Operation, PathBuf), String> {
    let mut args = env::args().skip(1);
    let operation = match args.next().as_deref() {
        Some("run") => Operation::Run,
        Some("validate") => Operation::Validate,
        Some("--version") => {
            println!("discobox-krun {}", env!("CARGO_PKG_VERSION"));
            std::process::exit(0);
        }
        _ => return Err("usage: discobox-krun <run|validate> --config <path>".to_owned()),
    };
    if args.next().as_deref() != Some("--config") {
        return Err("usage: discobox-krun <run|validate> --config <path>".to_owned());
    }
    let config = args
        .next()
        .map(PathBuf::from)
        .ok_or_else(|| "--config path is required".to_owned())?;
    if args.next().is_some() {
        return Err("unexpected trailing arguments".to_owned());
    }
    if !config.is_absolute() {
        return Err(format!(
            "configuration path {} must be absolute",
            config.display()
        ));
    }
    Ok((operation, config))
}

fn acquire_lock(runtime_dir: &Path) -> Result<File, String> {
    let path = runtime_dir.join("launcher.lock");
    let lock = OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .mode(0o600)
        .open(&path)
        .map_err(|err| format!("open launcher lock {}: {err}", path.display()))?;
    let result = unsafe { libc::flock(lock.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) };
    if result != 0 {
        let err = std::io::Error::last_os_error();
        return Err(format!(
            "lock runtime directory {}: {err}",
            runtime_dir.display()
        ));
    }
    Ok(lock)
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct ProcessIdentity {
    pid: u32,
    start_time_ticks: u64,
}

fn write_identity(runtime_dir: &Path) -> Result<(), String> {
    let identity = ProcessIdentity {
        pid: std::process::id(),
        start_time_ticks: process_start_time_ticks()?,
    };
    let path = runtime_dir.join("launcher.json");
    let mut file = OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(0o600)
        .open(&path)
        .map_err(|err| format!("open process identity {}: {err}", path.display()))?;
    serde_json::to_writer(&mut file, &identity)
        .map_err(|err| format!("write process identity {}: {err}", path.display()))?;
    file.write_all(b"\n")
        .map_err(|err| format!("finish process identity {}: {err}", path.display()))?;
    file.sync_all()
        .map_err(|err| format!("sync process identity {}: {err}", path.display()))
}

fn process_start_time_ticks() -> Result<u64, String> {
    let stat = fs::read_to_string("/proc/self/stat")
        .map_err(|err| format!("read /proc/self/stat: {err}"))?;
    let command_end = stat
        .rfind(')')
        .ok_or_else(|| "parse /proc/self/stat: missing command terminator".to_owned())?;
    let fields: Vec<_> = stat[command_end + 1..].split_whitespace().collect();
    fields
        .get(19)
        .ok_or_else(|| "parse /proc/self/stat: missing start time".to_owned())?
        .parse()
        .map_err(|err| format!("parse /proc/self/stat start time: {err}"))
}

fn start_passt(config: &Config) -> Result<Child, String> {
    let passt = config.passt_executable();
    let dns_host = host_ipv4_nameserver()?;
    let log_path = config.runtime_dir.join("passt.log");
    let mut command = Command::new(&passt);
    command
        .arg("--foreground")
        .arg("--one-off")
        .arg("--socket")
        .arg(&config.passt_socket)
        .arg("--tcp-ports")
        .arg("none")
        .arg("--udp-ports")
        .arg("none")
        .arg("--address")
        .arg("192.168.127.2")
        .arg("--netmask")
        .arg("255.255.255.0")
        .arg("--gateway")
        .arg("192.168.127.1")
        .arg("--dns")
        .arg("192.168.127.53")
        .arg("--dns-forward")
        .arg("192.168.127.53")
        .arg("--dns-host")
        .arg(dns_host.to_string())
        .arg("--map-host-loopback")
        .arg("none")
        .arg("--map-guest-addr")
        .arg("none")
        .arg("--no-map-gw")
        .arg("--ipv4-only")
        .arg("--log-file")
        .arg(&log_path)
        .arg("--log-size")
        .arg("1048576")
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null());

    unsafe {
        command.pre_exec(|| {
            if libc::prctl(libc::PR_SET_PDEATHSIG, libc::SIGTERM) != 0 {
                return Err(std::io::Error::last_os_error());
            }
            Ok(())
        });
    }

    let mut child = command
        .spawn()
        .map_err(|err| format!("start passt {}: {err}", passt.display()))?;
    let deadline = Instant::now() + PASST_START_TIMEOUT;
    loop {
        if config.passt_socket.exists() {
            return Ok(child);
        }
        if let Some(status) = child
            .try_wait()
            .map_err(|err| format!("wait for passt startup: {err}"))?
        {
            return Err(format!(
                "passt exited before creating {}: {status}",
                config.passt_socket.display()
            ));
        }
        if Instant::now() >= deadline {
            let _ = child.kill();
            let _ = child.wait();
            return Err(format!(
                "passt did not create {} within {:?}",
                config.passt_socket.display(),
                PASST_START_TIMEOUT
            ));
        }
        thread::sleep(Duration::from_millis(10));
    }
}

fn host_ipv4_nameserver() -> Result<Ipv4Addr, String> {
    let path = Path::new("/etc/resolv.conf");
    let contents = fs::read_to_string(path)
        .map_err(|err| format!("read host resolver configuration {}: {err}", path.display()))?;
    parse_first_ipv4_nameserver(&contents).ok_or_else(|| {
        format!(
            "host resolver configuration {} has no IPv4 nameserver",
            path.display()
        )
    })
}

fn parse_first_ipv4_nameserver(contents: &str) -> Option<Ipv4Addr> {
    contents.lines().find_map(|line| {
        let mut fields = line.split_whitespace();
        if fields.next()? != "nameserver" {
            return None;
        }
        fields.next()?.parse().ok()
    })
}

fn supervise_passt(mut passt: Child) {
    thread::spawn(move || {
        let message = match passt.wait() {
            Ok(status) => format!("passt exited unexpectedly: {status}"),
            Err(err) => format!("wait for passt: {err}"),
        };
        eprintln!("discobox-krun: {message}");
        unsafe {
            libc::_exit(1);
        }
    });
}

#[cfg(test)]
mod tests {
    use super::parse_first_ipv4_nameserver;
    use std::net::Ipv4Addr;

    #[test]
    fn parses_first_ipv4_nameserver() {
        let contents = "# generated\nnameserver ::1\nnameserver 10.255.255.254\n";
        assert_eq!(
            parse_first_ipv4_nameserver(contents),
            Some(Ipv4Addr::new(10, 255, 255, 254))
        );
    }
}
