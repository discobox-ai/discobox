use serde::Deserialize;
use std::collections::HashSet;
use std::fs;
use std::os::unix::fs::{FileTypeExt, PermissionsExt};
use std::path::{Component, Path, PathBuf};

pub const CONFIG_VERSION: u32 = 2;

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq)]
#[serde(rename_all = "camelCase")]
pub enum VsockDirection {
    GuestConnects,
    HostConnects,
}

#[derive(Clone, Debug, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct VsockMapping {
    pub name: String,
    pub port: u32,
    pub socket: PathBuf,
    pub direction: VsockDirection,
}

#[derive(Clone, Debug, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub struct Config {
    pub version: u32,
    pub pool_id: String,
    pub runtime_dir: PathBuf,
    pub kernel_image: PathBuf,
    pub root_disk: PathBuf,
    pub data_disk: PathBuf,
    pub cache_disk: PathBuf,
    pub passt_socket: PathBuf,
    pub console_log: PathBuf,
    pub vcpus: u8,
    #[serde(rename = "memoryMiB")]
    pub memory_mib: u32,
    pub mac_address: String,
    #[serde(default)]
    pub passt_path: Option<PathBuf>,
    #[serde(default)]
    pub vsock: Vec<VsockMapping>,
}

impl Config {
    pub fn from_path(path: &Path) -> Result<Self, String> {
        let data = fs::read(path)
            .map_err(|err| format!("read configuration {}: {err}", path.display()))?;
        let config: Self = serde_json::from_slice(&data)
            .map_err(|err| format!("decode configuration {}: {err}", path.display()))?;
        config.validate()?;
        Ok(config)
    }

    pub fn validate(&self) -> Result<(), String> {
        if self.version != CONFIG_VERSION {
            return Err(format!(
                "unsupported configuration version {}, want {CONFIG_VERSION}",
                self.version
            ));
        }
        if self.pool_id.trim().is_empty() {
            return Err("poolId is required".to_owned());
        }
        if self.vcpus == 0 {
            return Err("vcpus must be greater than zero".to_owned());
        }
        if self.memory_mib < 256 {
            return Err("memoryMiB must be at least 256".to_owned());
        }

        for (name, path) in [
            ("runtimeDir", &self.runtime_dir),
            ("kernelImage", &self.kernel_image),
            ("rootDisk", &self.root_disk),
            ("dataDisk", &self.data_disk),
            ("cacheDisk", &self.cache_disk),
            ("passtSocket", &self.passt_socket),
            ("consoleLog", &self.console_log),
        ] {
            validate_absolute_path(name, path)?;
        }

        if !is_beneath(&self.runtime_dir, &self.passt_socket) {
            return Err("passtSocket must be beneath runtimeDir".to_owned());
        }
        if !is_beneath(&self.runtime_dir, &self.console_log) {
            return Err("consoleLog must be beneath runtimeDir".to_owned());
        }

        let mut names = HashSet::new();
        let mut ports = HashSet::new();
        let mut sockets = HashSet::new();
        for mapping in &self.vsock {
            if mapping.name.trim().is_empty() {
                return Err("vsock mapping name is required".to_owned());
            }
            if !names.insert(mapping.name.as_str()) {
                return Err(format!("duplicate vsock mapping name {:?}", mapping.name));
            }
            if mapping.port < 1024 {
                return Err(format!(
                    "vsock mapping {:?} uses privileged port {}",
                    mapping.name, mapping.port
                ));
            }
            if !ports.insert(mapping.port) {
                return Err(format!("duplicate vsock port {}", mapping.port));
            }
            validate_absolute_path("vsock socket", &mapping.socket)?;
            if !sockets.insert(mapping.socket.as_path()) {
                return Err(format!(
                    "duplicate vsock socket {}",
                    mapping.socket.display()
                ));
            }
            if mapping.direction == VsockDirection::HostConnects
                && !is_beneath(&self.runtime_dir, &mapping.socket)
            {
                return Err(format!(
                    "host-listening vsock socket {} must be beneath runtimeDir",
                    mapping.socket.display()
                ));
            }
        }

        parse_mac_address(&self.mac_address)?;
        Ok(())
    }

    pub fn prepare_runtime_dir(&self) -> Result<(), String> {
        if self.runtime_dir.exists() {
            let metadata = fs::symlink_metadata(&self.runtime_dir).map_err(|err| {
                format!(
                    "inspect runtime directory {}: {err}",
                    self.runtime_dir.display()
                )
            })?;
            if !metadata.is_dir() || metadata.file_type().is_symlink() {
                return Err(format!(
                    "runtimeDir {} must be a real directory",
                    self.runtime_dir.display()
                ));
            }
        } else {
            fs::create_dir_all(&self.runtime_dir).map_err(|err| {
                format!(
                    "create runtime directory {}: {err}",
                    self.runtime_dir.display()
                )
            })?;
        }
        fs::set_permissions(&self.runtime_dir, fs::Permissions::from_mode(0o700)).map_err(
            |err| {
                format!(
                    "set runtime directory permissions {}: {err}",
                    self.runtime_dir.display()
                )
            },
        )?;
        Ok(())
    }

    pub fn validate_disk_files(&self) -> Result<(), String> {
        for (name, path) in [
            ("kernelImage", &self.kernel_image),
            ("rootDisk", &self.root_disk),
            ("dataDisk", &self.data_disk),
            ("cacheDisk", &self.cache_disk),
        ] {
            let metadata = fs::metadata(path)
                .map_err(|err| format!("inspect {name} {}: {err}", path.display()))?;
            if !metadata.is_file() {
                return Err(format!("{name} {} must be a regular file", path.display()));
            }
        }
        Ok(())
    }

    pub fn prepare_locked_runtime(&self) -> Result<(), String> {
        self.validate_disk_files()?;
        remove_stale_socket(&self.passt_socket)?;
        for mapping in &self.vsock {
            if mapping.direction == VsockDirection::HostConnects {
                remove_stale_socket(&mapping.socket)?;
            }
        }
        Ok(())
    }

    pub fn passt_executable(&self) -> PathBuf {
        self.passt_path
            .clone()
            .unwrap_or_else(|| PathBuf::from(option_env!("PASST_PATH").unwrap_or("passt")))
    }

    pub fn mac_address(&self) -> Result<[u8; 6], String> {
        parse_mac_address(&self.mac_address)
    }
}

fn validate_absolute_path(name: &str, path: &Path) -> Result<(), String> {
    if !path.is_absolute() {
        return Err(format!("{name} {} must be absolute", path.display()));
    }
    if path
        .components()
        .any(|component| component == Component::ParentDir)
    {
        return Err(format!("{name} {} must not contain '..'", path.display()));
    }
    Ok(())
}

fn is_beneath(parent: &Path, child: &Path) -> bool {
    child != parent && child.starts_with(parent)
}

fn remove_stale_socket(path: &Path) -> Result<(), String> {
    let metadata = match fs::symlink_metadata(path) {
        Ok(metadata) => metadata,
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(err) => return Err(format!("inspect socket {}: {err}", path.display())),
    };
    if !metadata.file_type().is_socket() {
        return Err(format!(
            "refusing to replace non-socket path {}",
            path.display()
        ));
    }
    fs::remove_file(path).map_err(|err| format!("remove stale socket {}: {err}", path.display()))
}

fn parse_mac_address(value: &str) -> Result<[u8; 6], String> {
    let parts: Vec<_> = value.split(':').collect();
    if parts.len() != 6 {
        return Err(format!("invalid macAddress {value:?}"));
    }
    let mut mac = [0_u8; 6];
    for (index, part) in parts.into_iter().enumerate() {
        if part.len() != 2 {
            return Err(format!("invalid macAddress {value:?}"));
        }
        mac[index] =
            u8::from_str_radix(part, 16).map_err(|_| format!("invalid macAddress {value:?}"))?;
    }
    if mac[0] & 0x01 != 0 {
        return Err("macAddress must be unicast".to_owned());
    }
    if mac[0] & 0x02 == 0 {
        return Err("macAddress must be locally administered".to_owned());
    }
    Ok(mac)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn valid_config() -> Config {
        Config {
            version: CONFIG_VERSION,
            pool_id: "pool_123".to_owned(),
            runtime_dir: PathBuf::from("/run/user/1000/discobox/pool_123"),
            kernel_image: PathBuf::from("/var/lib/discobox/images/vmlinux"),
            root_disk: PathBuf::from("/var/lib/discobox/images/root.qcow2"),
            data_disk: PathBuf::from("/var/lib/discobox/vms/pool_123/data.raw"),
            cache_disk: PathBuf::from("/var/lib/discobox/vms/pool_123/cache.raw"),
            passt_socket: PathBuf::from("/run/user/1000/discobox/pool_123/passt.sock"),
            console_log: PathBuf::from("/run/user/1000/discobox/pool_123/console.log"),
            vcpus: 2,
            memory_mib: 4096,
            mac_address: "02:00:00:00:00:01".to_owned(),
            passt_path: None,
            vsock: vec![
                VsockMapping {
                    name: "control-plane".to_owned(),
                    port: 3001,
                    socket: PathBuf::from("/run/user/1000/discobox/control-plane.sock"),
                    direction: VsockDirection::GuestConnects,
                },
                VsockMapping {
                    name: "pool-agent".to_owned(),
                    port: 3002,
                    socket: PathBuf::from("/run/user/1000/discobox/pool_123/pool-agent.sock"),
                    direction: VsockDirection::HostConnects,
                },
            ],
        }
    }

    #[test]
    fn valid_manifest_is_accepted() {
        valid_config().validate().unwrap();
    }

    #[test]
    fn go_manifest_memory_key_is_accepted() {
        let config: Config = serde_json::from_str(
            r#"{
                "version": 2,
                "poolId": "pool_123",
                "runtimeDir": "/run/user/1000/discobox/pool_123",
                "kernelImage": "/var/lib/discobox/images/vmlinux",
                "rootDisk": "/var/lib/discobox/images/root.qcow2",
                "dataDisk": "/var/lib/discobox/vms/pool_123/data.raw",
                "cacheDisk": "/var/lib/discobox/vms/pool_123/cache.raw",
                "passtSocket": "/run/user/1000/discobox/pool_123/passt.sock",
                "consoleLog": "/run/user/1000/discobox/pool_123/console.log",
                "vcpus": 2,
                "memoryMiB": 4096,
                "macAddress": "02:00:00:00:00:01",
                "vsock": []
            }"#,
        )
        .unwrap();

        assert_eq!(config.memory_mib, 4096);
        config.validate().unwrap();
    }

    #[test]
    fn duplicate_vsock_ports_are_rejected() {
        let mut config = valid_config();
        config.vsock[1].port = config.vsock[0].port;
        assert!(
            config
                .validate()
                .unwrap_err()
                .contains("duplicate vsock port")
        );
    }

    #[test]
    fn host_listener_must_be_private_to_runtime() {
        let mut config = valid_config();
        config.vsock[1].socket = PathBuf::from("/tmp/pool-agent.sock");
        assert!(
            config
                .validate()
                .unwrap_err()
                .contains("must be beneath runtimeDir")
        );
    }

    #[test]
    fn mac_must_be_local_unicast() {
        let mut config = valid_config();
        config.mac_address = "01:00:00:00:00:01".to_owned();
        assert!(config.validate().unwrap_err().contains("unicast"));

        config.mac_address = "00:00:00:00:00:01".to_owned();
        assert!(
            config
                .validate()
                .unwrap_err()
                .contains("locally administered")
        );
    }
}
