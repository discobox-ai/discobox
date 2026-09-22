// Command discobox-macvm installs and boots a macOS guest under
// Virtualization.framework.
//
// It is a proof of concept that stands beside the vz provider rather than
// inside it (see the macvm package for why a pool cannot use one), and it is a
// separate binary because the framework's window owns the main thread of
// whatever opens it — something a server cannot give it.
//
// Like the server, it only works signed: `go tool task build:macvm`.
//
//	discobox-macvm fetch                    # ~20 GB, resumable
//	discobox-macvm install --name poc       # fetches too, if needed
//	discobox-macvm run --name poc --gui     # first boot: Setup Assistant
//	discobox-macvm run --name poc           # headless, once it has a user
//
// Then the fast path: boot the golden guest to idle, snapshot it on the way
// out, and hand out copy-on-write clones that resume into it.
//
//	discobox-macvm run --name poc --save-on-exit
//	discobox-macvm clone --from poc --name work1 --snapshot
//	discobox-macvm run --name work1 --restore
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"os/user"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/discobox-ai/discobox/server/internal/macvm"
	"github.com/discobox-ai/discobox/server/providers/vmsize"
)

const gib = 1024 * 1024 * 1024

func main() {
	// Held for the whole process because the GUI needs it: the framework's
	// window runs an NSApplication event loop on the thread that opens it, and
	// AppKit only accepts the main one.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "canceled")
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("a command is required")
	}
	switch args[0] {
	case "fetch":
		return fetchCommand(ctx, args[1:])
	case "install":
		return installCommand(ctx, args[1:])
	case "run":
		return runCommand(ctx, args[1:])
	case "clone":
		return cloneCommand(args[1:])
	case "info":
		return infoCommand(args[1:])
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `discobox-macvm — install and boot a macOS guest under Virtualization.framework

  fetch     download the latest restore image this host supports (~20 GB, resumable)
  install   create a guest and install macOS into it
  run       boot an installed guest, headless or in a window
  clone     copy a guest, copy-on-write, in milliseconds
  info      print a guest's state and metadata, and the latest restore image

Every command takes --root (default `+"`"+macvmRootPlaceholder+"`"+`) and most take --name.
`)
}

const macvmRootPlaceholder = "~/Library/Application Support/discobox/macvm"

// fetchCommand downloads the IPSW on its own. install does it too; this exists
// so the 20 GB can be started, and resumed, without committing to an install.
func fetchCommand(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("fetch", flag.ExitOnError)
	root := flags.String("root", "", "directory holding guests and the restore image")
	dest := flags.String("dest", "", "restore image path (default <root>/<the image's own file name>)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	path, url, err := restoreImagePath(*root, *dest)
	if err != nil {
		return err
	}
	if url == "" {
		if url, err = macvm.LatestRestoreImageURL(); err != nil {
			return err
		}
	}
	if _, err := os.Stat(path); err == nil {
		fmt.Printf("%s is already here; a partial download resumes, a complete one returns at once\n", path)
	}
	return fetchRestoreImage(ctx, path, url)
}

func fetchRestoreImage(ctx context.Context, path, url string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create restore image directory: %w", err)
	}
	fmt.Printf("restore image %s\n  -> %s\n", url, path)
	return macvm.FetchRestoreImage(ctx, path, func(fraction float64, current int64) {
		fmt.Printf("\rdownload %5.1f%%  %.1f GiB", fraction*100, float64(current)/gib)
	})
}

func installCommand(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("install", flag.ExitOnError)
	root := flags.String("root", "", "directory holding guests and the restore image")
	name := flags.String("name", "poc", "guest name, which is its directory name")
	ipsw := flags.String("ipsw", "", "restore image path (default: the latest this host supports, under <root>, downloaded if missing)")
	diskGiB := flags.Int64("disk-gib", 96, "disk size in GiB; sparse, and not growable from the host afterwards")
	cpus := flags.Uint("cpus", 0, "vCPUs (default: every vCPU, the rule every local VM provider shares), raised to the restore image's minimum")
	memoryGiB := flags.Uint64("memory-gib", 0, "memory in GiB (default: half the host's, the rule every local VM provider shares), raised to the restore image's minimum")
	asif := flags.Bool("asif", false, "create the disk as an ASIF image, Apple's recommended VM format (macOS 26+), instead of raw")
	force := flags.Bool("force", false, "delete an existing guest of this name first")
	if err := flags.Parse(args); err != nil {
		return err
	}

	bundle, err := macvm.OpenBundle(*root, *name)
	if err != nil {
		return err
	}
	if err := prepareBundleDir(bundle, *force); err != nil {
		return err
	}
	restorePath, url, err := restoreImagePath(*root, *ipsw)
	if err != nil {
		return err
	}
	if _, err := os.Stat(restorePath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat restore image: %w", err)
		}
		if url == "" {
			return fmt.Errorf("restore image %s does not exist", restorePath)
		}
		if err := fetchRestoreImage(ctx, restorePath, url); err != nil {
			return err
		}
	}

	// Neither is a reservation: vCPUs are scheduled, and guest memory is
	// allocated on the host only as the guest touches it.
	host := vmsize.Host()
	cpuCount, memoryBytes := *cpus, *memoryGiB*gib
	if cpuCount == 0 {
		cpuCount = uint(max(host.VCPUs, 1))
	}
	if memoryBytes == 0 {
		memoryBytes = uint64(max(host.MemoryMiB, 1)) << 20
	}
	fmt.Printf("installing into %s (%d vCPU, %d GiB)\n", bundle.Dir, cpuCount, memoryBytes/gib)
	err = macvm.Install(ctx, macvm.InstallOptions{
		Bundle:           bundle,
		RestoreImagePath: restorePath,
		DiskBytes:        *diskGiB * gib,
		Sparse:           *asif,
		CPUCount:         cpuCount,
		MemoryBytes:      memoryBytes,
		Progress: func(fraction float64) {
			fmt.Printf("\rinstall %5.1f%%", fraction*100)
		},
	})
	fmt.Println()
	if err != nil {
		return err
	}
	fmt.Printf("installed. On a macOS 27 guest the first boot can create the account itself:\n  discobox-macvm run --name %s --provision\nOtherwise the first boot needs a screen:\n  discobox-macvm run --name %s --gui\n", *name, *name)
	return nil
}

// prepareBundleDir refuses to install over a guest rather than overwriting one:
// the four files are a set, and a disk left beside a new machine identifier is
// a guest that boots into undefined behavior.
func prepareBundleDir(bundle macvm.Bundle, force bool) error {
	entries, err := os.ReadDir(bundle.Dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read %s: %w", bundle.Dir, err)
	}
	if len(entries) == 0 {
		return nil
	}
	if !force {
		return fmt.Errorf("%s already exists; pass --force to replace it", bundle.Dir)
	}
	if err := os.RemoveAll(bundle.Dir); err != nil {
		return fmt.Errorf("remove %s: %w", bundle.Dir, err)
	}
	return nil
}

func runCommand(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("run", flag.ExitOnError)
	root := flags.String("root", "", "directory holding guests and the restore image")
	name := flags.String("name", "poc", "guest name, which is its directory name")
	gui := flags.Bool("gui", false, "open a window on the guest's screen; required for Setup Assistant")
	recovery := flags.Bool("recovery", false, "boot into macOS Recovery")
	restore := flags.Bool("restore", false, "resume from the guest's saved state instead of booting")
	saveOnExit := flags.Bool("save-on-exit", false, "on exit, pause the guest and save its state instead of shutting it down")
	provision := flags.Bool("provision", false, "create the account on this first boot instead of Setup Assistant (macOS 27+ guest; password from $"+passwordEnv+" or --password-file)")
	username := flags.String("user", "", "account to provision (default: your username here, which on the first account also gives the same uid 501 and gid 20)")
	fullName := flags.String("full-name", "", "full name to provision (default: yours here)")
	passwordFile := flags.String("password-file", "", "file holding the password to provision")
	autoLogin := flags.Bool("auto-login", true, "log the provisioned account in at startup")
	remoteLogin := flags.Bool("ssh", true, "turn on Remote Login (SSH) for the provisioned account")
	cpus := flags.Uint("cpus", 0, "vCPUs (default: what the install recorded)")
	memoryGiB := flags.Uint64("memory-gib", 0, "memory in GiB (default: what the install recorded)")
	windowWidth := flags.Float64("window-width", 1280, "window width in points")
	windowHeight := flags.Float64("window-height", 800, "window height in points")
	displayWidth := flags.Int64("display-width", 1920, "guest display width in pixels")
	displayHeight := flags.Int64("display-height", 1200, "guest display height in pixels")
	displayPPI := flags.Int64("display-ppi", 80, "guest display pixels per inch; ~220 makes it Retina")
	var shares shareList
	flags.Var(&shares, "share", "host directory to export over virtiofs: [tag=]/path[:ro]; without a tag the guest automounts it at /Volumes/My Shared Files (repeatable)")
	if err := flags.Parse(args); err != nil {
		return err
	}

	bundle, err := macvm.OpenBundle(*root, *name)
	if err != nil {
		return err
	}
	metadata, err := bundle.ReadMetadata()
	if err != nil {
		return err
	}
	cpuCount := *cpus
	if cpuCount == 0 {
		cpuCount = metadata.CPUCount
	}
	memoryBytes := *memoryGiB * gib
	if memoryBytes == 0 {
		memoryBytes = metadata.MemoryBytes
	}

	if *restore {
		// The configuration has to match the one the state was saved with, so
		// the bundle's own numbers win over anything typed here.
		if (*cpus != 0 && *cpus != metadata.CPUCount) || (*memoryGiB != 0 && *memoryGiB*gib != metadata.MemoryBytes) {
			fmt.Printf("restore: using the saved size (%d vCPU, %d GiB), not the flags\n", metadata.CPUCount, metadata.MemoryBytes/gib)
		}
		cpuCount, memoryBytes = metadata.CPUCount, metadata.MemoryBytes
	}

	var provisioning *macvm.GuestProvisioning
	if *provision {
		provisioning, err = provisioningFromFlags(*username, *fullName, *passwordFile, *autoLogin, *remoteLogin)
		if err != nil {
			return err
		}
		if major := metadata.MacOSMajor(); major != 0 && major < 27 {
			fmt.Printf("warning: this guest is macOS %s; provisioning needs macOS 27 or newer and it will be ignored\n", metadata.MacOSVersion)
		}
		if metadata.ProvisionedUser != "" {
			fmt.Printf("warning: %q was already provisioned; a guest evaluates provisioning once, on its first boot after restore\n", metadata.ProvisionedUser)
		}
		fmt.Printf("provisioning %q (auto-login %v, SSH %v)\n", provisioning.Username, provisioning.AutoLogin, provisioning.RemoteLogin)
	}

	action := "booting"
	if *restore {
		action = "resuming"
	}
	fmt.Printf("%s %s (%s %s)\n", action, bundle.Dir, metadata.MacOSVersion, metadata.BuildVersion)
	if !*gui {
		fmt.Println("headless: nothing is printed by the guest — a macOS guest has no serial console")
	}
	if *saveOnExit {
		fmt.Println("a snapshot is the state plus the disk it was taken with: clone this guest to use it more than once")
	}
	return macvm.Run(ctx, macvm.RunOptions{
		Bundle:        bundle,
		CPUCount:      cpuCount,
		MemoryBytes:   memoryBytes,
		GUI:           *gui,
		WindowWidth:   *windowWidth,
		WindowHeight:  *windowHeight,
		DisplayWidth:  *displayWidth,
		DisplayHeight: *displayHeight,
		DisplayPPI:    *displayPPI,
		Recovery:      *recovery,
		Restore:       *restore,
		SaveOnExit:    *saveOnExit,
		Provision:     provisioning,
		Shares:        shares,
	})
}

// passwordEnv carries the provisioning password, so it appears in neither the
// shell history nor the process list the way a flag would.
const passwordEnv = "DISCOBOX_MACVM_PASSWORD" //nolint:gosec // The name of the variable that holds a password, not a password.

// provisioningFromFlags defaults the account to the person running this, which
// is the point: the guest's first account is uid 501 in staff, as the host's
// first account is, so the same name gives the same identity on both sides.
func provisioningFromFlags(username, fullName, passwordFile string, autoLogin, remoteLogin bool) (*macvm.GuestProvisioning, error) {
	if username == "" || fullName == "" {
		current, err := user.Current()
		if err != nil {
			return nil, fmt.Errorf("look up the current user: %w", err)
		}
		if username == "" {
			username = current.Username
		}
		if fullName == "" {
			fullName = current.Name
		}
		if fullName == "" {
			fullName = username
		}
	}
	password := os.Getenv(passwordEnv)
	if passwordFile != "" {
		data, err := os.ReadFile(passwordFile)
		if err != nil {
			return nil, fmt.Errorf("read password file: %w", err)
		}
		password = strings.TrimRight(string(data), "\r\n")
	}
	if password == "" {
		return nil, fmt.Errorf("provisioning needs a password: set $%s or pass --password-file", passwordEnv)
	}
	return &macvm.GuestProvisioning{
		FullName:    fullName,
		Username:    username,
		Password:    password,
		AutoLogin:   autoLogin,
		RemoteLogin: remoteLogin,
	}, nil
}

func cloneCommand(args []string) error {
	flags := flag.NewFlagSet("clone", flag.ExitOnError)
	root := flags.String("root", "", "directory holding guests and the restore image")
	from := flags.String("from", "poc", "guest to clone")
	name := flags.String("name", "", "name for the clone")
	snapshot := flags.Bool("snapshot", false, "carry the source's saved state, and with it its machine identifier, so the clone resumes instead of booting")
	if err := flags.Parse(args); err != nil {
		return err
	}
	source, err := macvm.OpenBundle(*root, *from)
	if err != nil {
		return err
	}
	dest, err := macvm.OpenBundle(*root, *name)
	if err != nil {
		return err
	}

	started := time.Now()
	if err := macvm.Clone(macvm.CloneOptions{Source: source, Dest: dest, Snapshot: *snapshot}); err != nil {
		return err
	}
	fmt.Printf("cloned %s -> %s in %s\n", source.Dir, dest.Dir, time.Since(started).Round(time.Millisecond))
	if *snapshot {
		fmt.Printf("it carries %s's machine identifier, so do not run both at once:\n  discobox-macvm run --name %s --restore\n", *from, *name)
	} else {
		fmt.Printf("it has a new machine identifier and cold boots:\n  discobox-macvm run --name %s\n", *name)
	}
	return nil
}

func infoCommand(args []string) error {
	flags := flag.NewFlagSet("info", flag.ExitOnError)
	root := flags.String("root", "", "directory holding guests and the restore image")
	name := flags.String("name", "poc", "guest name, which is its directory name")
	if err := flags.Parse(args); err != nil {
		return err
	}
	bundle, err := macvm.OpenBundle(*root, *name)
	if err != nil {
		return err
	}
	fmt.Printf("bundle:    %s\n", bundle.Dir)
	if err := bundle.Installed(); err != nil {
		fmt.Printf("installed: no (%v)\n", err)
	} else {
		fmt.Println("installed: yes")
	}
	metadata, err := bundle.ReadMetadata()
	if err != nil {
		return err
	}
	if metadata.BuildVersion != "" {
		fmt.Printf("macOS:     %s (%s), %d vCPU, %d GiB memory, %d GiB disk, installed %s\n",
			metadata.MacOSVersion, metadata.BuildVersion, metadata.CPUCount,
			metadata.MemoryBytes/gib, metadata.DiskBytes/gib, metadata.InstalledAt.Format("2006-01-02"))
	}
	if data, err := os.ReadFile(bundle.MACAddressPath()); err == nil {
		fmt.Printf("mac:       %s\n", strings.TrimSpace(string(data)))
	}
	if size, ok := bundle.HasState(); ok {
		fmt.Printf("snapshot:  %s (%.1f GiB)\n", bundle.StatePath(), float64(size)/gib)
	} else {
		fmt.Println("snapshot:  none")
	}
	if metadata.ProvisionedUser != "" {
		fmt.Printf("account:   %s (provisioned on first boot)\n", metadata.ProvisionedUser)
	}
	latest, url, err := restoreImagePath(*root, "")
	if err != nil {
		return err
	}
	fmt.Printf("latest:    %s\n", url)
	images, _ := filepath.Glob(filepath.Join(filepath.Dir(latest), "*.ipsw"))
	for _, image := range images {
		info, err := os.Stat(image)
		if err != nil {
			continue
		}
		marker := ""
		if image == latest {
			marker = "  <- latest"
		}
		fmt.Printf("ipsw:      %s (%.1f GiB)%s\n", image, float64(info.Size())/gib, marker)
	}
	if _, err := os.Stat(latest); err != nil {
		fmt.Printf("ipsw:      %s (not downloaded)\n", latest)
	}
	return nil
}

// restoreImagePath is a configured image as given, or else the latest this
// host supports, named as Apple names it. The name carries the version and
// build, so a host upgrade fetches a new image instead of installing the one
// it downloaded for the release before. A configured path has no URL: it is
// used as it is or not at all.
func restoreImagePath(root, configured string) (string, string, error) {
	if path := strings.TrimSpace(configured); path != "" {
		abs, err := filepath.Abs(path)
		return abs, "", err
	}
	dir := strings.TrimSpace(root)
	if dir == "" {
		dir = macvm.DefaultRoot()
	}
	url, err := macvm.LatestRestoreImageURL()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(dir, path.Base(url)), url, nil
}

// shareList parses repeated --share values as [tag=]/path[:ro].
type shareList []macvm.SharedDirectory

func (s *shareList) String() string {
	paths := make([]string, 0, len(*s))
	for _, share := range *s {
		paths = append(paths, share.HostPath)
	}
	return strings.Join(paths, ",")
}

func (s *shareList) Set(value string) error {
	share := macvm.SharedDirectory{HostPath: value}
	if tag, rest, ok := strings.Cut(share.HostPath, "="); ok {
		share.Tag, share.HostPath = tag, rest
	}
	if rest, ok := strings.CutSuffix(share.HostPath, ":ro"); ok {
		share.HostPath, share.ReadOnly = rest, true
	}
	path, err := filepath.Abs(share.HostPath)
	if err != nil {
		return fmt.Errorf("share %q: %w", value, err)
	}
	share.HostPath = path
	*s = append(*s, share)
	return nil
}
