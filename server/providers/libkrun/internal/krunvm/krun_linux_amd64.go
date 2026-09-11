package krunvm

import (
	"fmt"
	"runtime"
	"syscall"

	"github.com/ebitengine/purego"
)

// libkrun's C API, bound at run time rather than at link time.
//
// dlopen is the point (ADR 0062 §9). The server binary is one artifact on every
// Linux machine, and only the machines that actually enable this provider need
// libkrun installed at all; a user who never configures a libkrun pool never
// needs the library present, and nothing about the build changes when they do.
// purego is what makes that possible without cgo, and it is already how this
// project loads a native library (endpoint's iroh binding).
//
// The library is loaded in the launcher child, never in the server process, so
// a fault inside libkrun takes down one pool's VM rather than the control plane.
const (
	// defaultSoname is what a distribution or Nix package installs. The
	// manifest can name an absolute path instead.
	defaultSoname = "libkrun.so.1"

	krunDiskFormatRaw   = 0
	krunKernelFormatELF = 1
	// krunSyncFull is write-through: a guest fsync reaches the host's disk.
	// The alternative loses a pool's Docker state to a host crash, which is the
	// one failure the data disk exists to survive.
	krunSyncFull = 2

	krunFeatureNet = 0
	krunFeatureBlk = 1
)

type krunLibrary struct {
	createCtx            func() int32
	freeCtx              func(ctx uint32) int32
	setVMConfig          func(ctx uint32, vcpus uint8, ramMiB uint32) int32
	setKernel            func(ctx uint32, kernelPath string, format uint32, initramfs *byte, cmdline *byte) int32
	addDisk              func(ctx uint32, blockID string, diskPath string, format uint32, readOnly bool, directIO bool, syncMode uint32) int32
	setRootDiskRemount   func(ctx uint32, device string, fstype string, options string) int32
	setWorkdir           func(ctx uint32, workdir string) int32
	setExec              func(ctx uint32, execPath string, argv []*byte, envp []*byte) int32
	addNetUnixstream     func(ctx uint32, path string, fd int32, mac []byte, features uint32, flags uint32) int32
	disableImplicitVSOCK func(ctx uint32) int32
	addVSOCK             func(ctx uint32, tsiFeatures uint32) int32
	addVSOCKPort         func(ctx uint32, port uint32, path string, listen bool) int32
	setConsoleOutput     func(ctx uint32, path string) int32
	hasFeature           func(feature uint64) int32
	startEnter           func(ctx uint32) int32
}

// openLibrary dlopens libkrun and binds the entry points this launcher uses.
//
// RTLD_GLOBAL, not RTLD_LOCAL: libkrun dlopens libkrunfw for the guest kernel
// payload and resolves symbols against the global namespace to do it.
func openLibrary(path string) (*krunLibrary, error) {
	if path == "" {
		path = defaultSoname
	}
	handle, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w; put libkrun on the dynamic loader's path (nix develop .#libkrun sets LD_LIBRARY_PATH) or name it in the provider's libkrunPath", path, err)
	}
	lib := &krunLibrary{}
	for name, target := range map[string]any{
		"krun_create_ctx":             &lib.createCtx,
		"krun_free_ctx":               &lib.freeCtx,
		"krun_set_vm_config":          &lib.setVMConfig,
		"krun_set_kernel":             &lib.setKernel,
		"krun_add_disk3":              &lib.addDisk,
		"krun_set_root_disk_remount":  &lib.setRootDiskRemount,
		"krun_set_workdir":            &lib.setWorkdir,
		"krun_set_exec":               &lib.setExec,
		"krun_add_net_unixstream":     &lib.addNetUnixstream,
		"krun_disable_implicit_vsock": &lib.disableImplicitVSOCK,
		"krun_add_vsock":              &lib.addVSOCK,
		"krun_add_vsock_port2":        &lib.addVSOCKPort,
		"krun_set_console_output":     &lib.setConsoleOutput,
		"krun_has_feature":            &lib.hasFeature,
		"krun_start_enter":            &lib.startEnter,
	} {
		symbol, err := purego.Dlsym(handle, name)
		if err != nil || symbol == 0 {
			return nil, fmt.Errorf("%s does not export %s; it is too old for this server", path, name)
		}
		purego.RegisterFunc(target, symbol)
	}
	return lib, nil
}

// boot configures a VM from the manifest and enters it. It never returns nil:
// krun_start_enter takes over the process and only comes back on failure.
func boot(cfg Config, lib *krunLibrary) error {
	if err := lib.requireFeature(krunFeatureBlk, "virtio-block"); err != nil {
		return err
	}
	if err := lib.requireFeature(krunFeatureNet, "virtio-net"); err != nil {
		return err
	}

	raw := lib.createCtx()
	if raw < 0 {
		return krunError("krun_create_ctx", raw)
	}
	ctx := uint32(raw)
	entered := false
	defer func() {
		if !entered {
			lib.freeCtx(ctx)
		}
	}()

	if err := check("krun_set_vm_config", lib.setVMConfig(ctx, uint8(cfg.VCPUs), uint32(cfg.MemoryMiB))); err != nil {
		return err
	}
	// No initramfs and no command line: the kernel is built with every driver
	// this guest has, and libkrun's default command line already names the
	// console and the root device it sets up below.
	if err := check("krun_set_kernel", lib.setKernel(ctx, cfg.KernelImage, krunKernelFormatELF, nil, nil)); err != nil {
		return err
	}

	// Disk order is the guest's contract: root, data, cache become /dev/vda,
	// /dev/vdb, /dev/vdc, and discobox-mount-storage addresses them by those
	// names. All three are raw — the guest image publishes a raw ext4 root
	// (ADR 0062 §8), so nothing here reads QCOW2.
	for _, disk := range []struct {
		id       string
		path     string
		readOnly bool
	}{
		{"vda", cfg.RootDisk, true},
		{"vdb", cfg.DataDisk, false},
		{"vdc", cfg.CacheDisk, false},
	} {
		if err := check("krun_add_disk3("+disk.id+")",
			lib.addDisk(ctx, disk.id, disk.path, krunDiskFormatRaw, disk.readOnly, false, krunSyncFull)); err != nil {
			return err
		}
	}
	if err := check("krun_set_root_disk_remount", lib.setRootDiskRemount(ctx, "/dev/vda", "ext4", "ro")); err != nil {
		return err
	}
	if err := check("krun_set_workdir", lib.setWorkdir(ctx, "/")); err != nil {
		return err
	}

	// libkrun's argv holds only the arguments after argv[0]; exec_path becomes
	// argv[0] in the guest. Passing /sbin/init here as well would make systemd
	// parse it as a runlevel, and an empty list is encoded by libkrun as one
	// empty positional argument. /sbin/init takes a single SysV runlevel, so
	// this asks for multi-user.
	argv := cStrings("3")
	env := cStrings(
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"container=discobox-krun",
		"KRUN_INIT_PID1=1",
	)
	var pinner runtime.Pinner
	defer pinner.Unpin()
	pin(&pinner, argv)
	pin(&pinner, env)
	if err := check("krun_set_exec", lib.setExec(ctx, "/sbin/init", argv, env)); err != nil {
		return err
	}

	if err := check("krun_set_console_output", lib.setConsoleOutput(ctx, cfg.ConsoleLog)); err != nil {
		return err
	}

	mac, err := cfg.MAC()
	if err != nil {
		return err
	}
	macBytes := mac[:]
	pinner.Pin(&macBytes[0])
	if err := check("krun_add_net_unixstream", lib.addNetUnixstream(ctx, cfg.PasstSocket, -1, macBytes, 0, 0)); err != nil {
		return err
	}

	// The implicit VSOCK device libkrun adds is TSI, which terminates guest
	// sockets on the host's network stack. This guest must not reach the host
	// that way (ADR 0013): every port it uses is an explicit mapping onto a
	// Unix socket, and passt is the only path out.
	if err := check("krun_disable_implicit_vsock", lib.disableImplicitVSOCK(ctx)); err != nil {
		return err
	}
	if err := check("krun_add_vsock", lib.addVSOCK(ctx, 0)); err != nil {
		return err
	}
	for _, mapping := range cfg.VSOCK {
		if err := check("krun_add_vsock_port2("+mapping.Name+")",
			lib.addVSOCKPort(ctx, mapping.Port, mapping.Socket, mapping.Direction == HostConnects)); err != nil {
			return err
		}
	}

	// krun_start_enter consumes this process: the vCPUs run on its threads and
	// it exits when the guest does. That is the whole reason the launcher is a
	// separate process rather than a goroutine in the server.
	entered = true
	return krunError("krun_start_enter", lib.startEnter(ctx))
}

func (l *krunLibrary) requireFeature(feature uint64, name string) error {
	switch result := l.hasFeature(feature); result {
	case 1:
		return nil
	case 0:
		return fmt.Errorf("the installed libkrun was built without %s support", name)
	default:
		return krunError("krun_has_feature", result)
	}
}

// cStrings renders a NULL-terminated C string array. The trailing nil entry is
// how libkrun finds the end; without it the C side walks off the slice.
func cStrings(values ...string) []*byte {
	out := make([]*byte, 0, len(values)+1)
	for _, value := range values {
		buffer := append([]byte(value), 0)
		out = append(out, &buffer[0])
	}
	return append(out, nil)
}

// pin keeps a C string array and everything it points at where it is for as
// long as libkrun might read it. The array is Go memory holding Go pointers,
// which the cgo rules forbid handing to C precisely because the collector
// cannot see them; pinning is what makes it legal rather than merely lucky.
func pin(pinner *runtime.Pinner, values []*byte) {
	pinner.Pin(&values[0])
	for _, value := range values {
		if value != nil {
			pinner.Pin(value)
		}
	}
}

func check(name string, result int32) error {
	if result < 0 {
		return krunError(name, result)
	}
	return nil
}

// krunError renders libkrun's negative-errno return convention.
func krunError(name string, code int32) error {
	if code < 0 {
		return fmt.Errorf("%s: %w (%d)", name, syscall.Errno(-code), code)
	}
	return fmt.Errorf("%s: unexpected return code %d", name, code)
}
