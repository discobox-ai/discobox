use crate::config::{Config, VsockDirection};
use std::ffi::{CString, c_char, c_int, c_uint};
use std::os::unix::ffi::OsStrExt;
use std::ptr;

const KRUN_DISK_FORMAT_RAW: c_uint = 0;
const KRUN_DISK_FORMAT_QCOW2: c_uint = 1;
const KRUN_KERNEL_FORMAT_ELF: c_uint = 1;
const KRUN_SYNC_FULL: c_uint = 2;
const KRUN_FEATURE_NET: u64 = 0;
const KRUN_FEATURE_BLK: u64 = 1;

#[link(name = "krun")]
unsafe extern "C" {
    fn krun_create_ctx() -> i32;
    fn krun_free_ctx(ctx_id: c_uint) -> i32;
    fn krun_set_vm_config(ctx_id: c_uint, num_vcpus: u8, ram_mib: c_uint) -> i32;
    fn krun_set_kernel(
        ctx_id: c_uint,
        kernel_path: *const c_char,
        kernel_format: c_uint,
        initramfs_path: *const c_char,
        cmdline: *const c_char,
    ) -> i32;
    fn krun_add_disk3(
        ctx_id: c_uint,
        block_id: *const c_char,
        disk_path: *const c_char,
        disk_format: c_uint,
        read_only: bool,
        direct_io: bool,
        sync_mode: c_uint,
    ) -> i32;
    fn krun_set_root_disk_remount(
        ctx_id: c_uint,
        device: *const c_char,
        fstype: *const c_char,
        options: *const c_char,
    ) -> i32;
    fn krun_set_workdir(ctx_id: c_uint, workdir_path: *const c_char) -> i32;
    fn krun_set_exec(
        ctx_id: c_uint,
        exec_path: *const c_char,
        argv: *const *const c_char,
        envp: *const *const c_char,
    ) -> i32;
    fn krun_add_net_unixstream(
        ctx_id: c_uint,
        path: *const c_char,
        fd: c_int,
        mac: *mut u8,
        features: c_uint,
        flags: c_uint,
    ) -> i32;
    fn krun_disable_implicit_vsock(ctx_id: c_uint) -> i32;
    fn krun_add_vsock(ctx_id: c_uint, tsi_features: c_uint) -> i32;
    fn krun_add_vsock_port2(ctx_id: c_uint, port: c_uint, path: *const c_char, listen: bool)
    -> i32;
    fn krun_set_console_output(ctx_id: c_uint, filepath: *const c_char) -> i32;
    fn krun_has_feature(feature: u64) -> i32;
    fn krun_start_enter(ctx_id: c_uint) -> i32;
}

pub fn run(config: &Config) -> Result<(), String> {
    require_feature(KRUN_FEATURE_BLK, "virtio-block")?;
    require_feature(KRUN_FEATURE_NET, "virtio-net")?;

    let raw_ctx = unsafe { krun_create_ctx() };
    if raw_ctx < 0 {
        return Err(krun_error("krun_create_ctx", raw_ctx));
    }
    let mut context = Context::new(raw_ctx as u32);
    let ctx = context.id;

    call("krun_set_vm_config", unsafe {
        krun_set_vm_config(ctx, config.vcpus, config.memory_mib)
    })?;

    let kernel_image = path_cstring(&config.kernel_image)?;
    call("krun_set_kernel", unsafe {
        krun_set_kernel(
            ctx,
            kernel_image.as_ptr(),
            KRUN_KERNEL_FORMAT_ELF,
            ptr::null(),
            ptr::null(),
        )
    })?;

    add_disk(ctx, "vda", &config.root_disk, KRUN_DISK_FORMAT_QCOW2, true)?;
    add_disk(ctx, "vdb", &config.data_disk, KRUN_DISK_FORMAT_RAW, false)?;
    add_disk(ctx, "vdc", &config.cache_disk, KRUN_DISK_FORMAT_RAW, false)?;

    let root_device = CString::new("/dev/vda").unwrap();
    let ext4 = CString::new("ext4").unwrap();
    let read_only = CString::new("ro").unwrap();
    call("krun_set_root_disk_remount", unsafe {
        krun_set_root_disk_remount(ctx, root_device.as_ptr(), ext4.as_ptr(), read_only.as_ptr())
    })?;

    let workdir = CString::new("/").unwrap();
    call("krun_set_workdir", unsafe {
        krun_set_workdir(ctx, workdir.as_ptr())
    })?;

    let init = CString::new("/sbin/init").unwrap();
    let multi_user_runlevel = CString::new("3").unwrap();
    // libkrun's argv contains only arguments after argv[0]; exec_path becomes
    // argv[0] inside the guest. Passing init here as well makes systemd parse
    // "/sbin/init" as a runlevel argument. An empty list is also encoded as an
    // empty positional argument by libkrun 1.19. /sbin/init accepts a single
    // SysV runlevel, so request the multi-user target with runlevel 3.
    let argv = [multi_user_runlevel.as_ptr(), ptr::null()];
    let environment = [
        CString::new("PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin").unwrap(),
        CString::new("container=discobox-krun").unwrap(),
        CString::new("KRUN_INIT_PID1=1").unwrap(),
    ];
    let envp = [
        environment[0].as_ptr(),
        environment[1].as_ptr(),
        environment[2].as_ptr(),
        ptr::null(),
    ];
    call("krun_set_exec", unsafe {
        krun_set_exec(ctx, init.as_ptr(), argv.as_ptr(), envp.as_ptr())
    })?;

    let console_log = path_cstring(&config.console_log)?;
    call("krun_set_console_output", unsafe {
        krun_set_console_output(ctx, console_log.as_ptr())
    })?;

    let passt_socket = path_cstring(&config.passt_socket)?;
    let mut mac = config.mac_address()?;
    call("krun_add_net_unixstream", unsafe {
        krun_add_net_unixstream(ctx, passt_socket.as_ptr(), -1, mac.as_mut_ptr(), 0, 0)
    })?;

    call("krun_disable_implicit_vsock", unsafe {
        krun_disable_implicit_vsock(ctx)
    })?;
    call("krun_add_vsock", unsafe { krun_add_vsock(ctx, 0) })?;
    for mapping in &config.vsock {
        let socket = path_cstring(&mapping.socket)?;
        call(&format!("krun_add_vsock_port2({})", mapping.name), unsafe {
            krun_add_vsock_port2(
                ctx,
                mapping.port,
                socket.as_ptr(),
                mapping.direction == VsockDirection::HostConnects,
            )
        })?;
    }

    context.consumed = true;
    let result = unsafe { krun_start_enter(ctx) };
    Err(krun_error("krun_start_enter", result))
}

fn require_feature(feature: u64, name: &str) -> Result<(), String> {
    let result = unsafe { krun_has_feature(feature) };
    match result {
        1 => Ok(()),
        0 => Err(format!("libkrun was built without required {name} support")),
        code => Err(krun_error("krun_has_feature", code)),
    }
}

fn add_disk(
    ctx: u32,
    id: &str,
    path: &std::path::Path,
    format: c_uint,
    read_only: bool,
) -> Result<(), String> {
    let id = CString::new(id).unwrap();
    let path = path_cstring(path)?;
    call("krun_add_disk3", unsafe {
        krun_add_disk3(
            ctx,
            id.as_ptr(),
            path.as_ptr(),
            format,
            read_only,
            false,
            KRUN_SYNC_FULL,
        )
    })
}

fn path_cstring(path: &std::path::Path) -> Result<CString, String> {
    CString::new(path.as_os_str().as_bytes())
        .map_err(|_| format!("path {} contains a NUL byte", path.display()))
}

fn call(name: &str, result: i32) -> Result<(), String> {
    if result < 0 {
        Err(krun_error(name, result))
    } else {
        Ok(())
    }
}

fn krun_error(name: &str, code: i32) -> String {
    if code < 0 {
        format!(
            "{name}: {} ({code})",
            std::io::Error::from_raw_os_error(-code)
        )
    } else {
        format!("{name}: unexpected return code {code}")
    }
}

struct Context {
    id: u32,
    consumed: bool,
}

impl Context {
    fn new(id: u32) -> Self {
        Self {
            id,
            consumed: false,
        }
    }
}

impl Drop for Context {
    fn drop(&mut self) {
        if !self.consumed {
            unsafe {
                krun_free_ctx(self.id);
            }
        }
    }
}
