// Command buttercams is the single ButterCams executable. Its role is chosen
// by a sub-command (spec 4):
//
//	buttercams service        the Windows service (run by the service manager)
//	buttercams console        the same guardian in the foreground, for development
//	buttercams worker --id N  a recorder worker (started by the guardian)
//	buttercams mover          the copy and retention process (started by the guardian)
//	buttercams media          the live view and playback server (started by the guardian)
//	buttercams rebuild-index  rebuild the index from the files on disk
//	buttercams setup ...      installer helpers (first configuration, service registration)
//	buttercams version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/jxburros/buttercams/internal/api"
	"github.com/jxburros/buttercams/internal/config"
	"github.com/jxburros/buttercams/internal/guardian"
	"github.com/jxburros/buttercams/internal/index"
	"github.com/jxburros/buttercams/internal/logx"
	"github.com/jxburros/buttercams/internal/media"
	"github.com/jxburros/buttercams/internal/mover"
	"github.com/jxburros/buttercams/internal/naming"
	"github.com/jxburros/buttercams/internal/version"
	"github.com/jxburros/buttercams/internal/winsvc"
	"github.com/jxburros/buttercams/internal/worker"
)

const usage = `ButterCams %s

Usage:
  buttercams service                     run as the Windows service
  buttercams console [--data-root DIR]   run in the foreground (development)
  buttercams rebuild-index [--data-root DIR]
                                         rebuild the index from the recordings on disk
                                         (stop the service first)
  buttercams setup init ...              create the first configuration and administrator
  buttercams setup install-service ...   register and start the Windows service
  buttercams setup uninstall-service ... remove the Windows service
  buttercams setup test-destination ...  test a storage destination
  buttercams version

Run "buttercams setup <command> -h" for the options of a setup command.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, version.Version)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "service":
		err = runService(os.Args[2:])
	case "console":
		err = runConsole(os.Args[2:])
	case "worker":
		err = runWorker(os.Args[2:])
	case "mover":
		err = runMover()
	case "media":
		err = runMedia()
	case "rebuild-index":
		err = runRebuildIndex(os.Args[2:])
	case "setup":
		err = runSetup(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("ButterCams", version.Version)
	case "help", "--help", "-h":
		fmt.Printf(usage, version.Version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		fmt.Fprintf(os.Stderr, usage, version.Version)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "buttercams:", err)
		os.Exit(1)
	}
}

func runService(args []string) error {
	fs := flag.NewFlagSet("service", flag.ExitOnError)
	dataRoot := fs.String("data-root", "", "data root (default: the one chosen at install)")
	fs.Parse(args)
	if !winsvc.IsWindowsService() {
		return errors.New(`"service" is started by the Windows service manager; use "buttercams console" to run in the foreground`)
	}
	opts := guardian.Options{DataRoot: winsvc.ResolveDataRoot(*dataRoot)}
	return winsvc.RunService(func(ctx context.Context) error { return guardian.Run(ctx, opts) })
}

func runConsole(args []string) error {
	fs := flag.NewFlagSet("console", flag.ExitOnError)
	dataRoot := fs.String("data-root", "", "data root (default: registry, BUTTERCAMS_DATA_ROOT, or the platform default)")
	fs.Parse(args)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return guardian.Run(ctx, guardian.Options{DataRoot: winsvc.ResolveDataRoot(*dataRoot), Console: true})
}

// Children ignore Ctrl+C: the guardian receives it and tells them to
// finalize their files and stop.
func ignoreInterrupt() { signal.Ignore(os.Interrupt) }

func runWorker(args []string) error {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	id := fs.Int("id", 0, "worker number")
	fs.Parse(args)
	if *id <= 0 {
		return errors.New("worker needs --id N; workers are started by the guardian")
	}
	ignoreInterrupt()
	log := logx.NewChild(fmt.Sprintf("worker:%d", *id))
	return worker.Run(context.Background(), *id, os.Stdin, os.Stdout, log)
}

func runMover() error {
	ignoreInterrupt()
	return mover.Run(context.Background(), os.Stdin, os.Stdout, logx.NewChild("mover"))
}

func runMedia() error {
	ignoreInterrupt()
	return media.Run(context.Background(), os.Stdin, os.Stdout, logx.NewChild("media"))
}

func validateEnv() config.Env { return config.Env{IsRemoteDrive: winsvc.IsRemoteDrive} }

// runRebuildIndex rebuilds the index while the service is stopped (spec 10.4).
func runRebuildIndex(args []string) error {
	fs := flag.NewFlagSet("rebuild-index", flag.ExitOnError)
	dataRootFlag := fs.String("data-root", "", "data root")
	fs.Parse(args)
	root := winsvc.ResolveDataRoot(*dataRootFlag)

	unlock, err := winsvc.LockFile(filepath.Join(root, guardian.LockName))
	if err != nil {
		if errors.Is(err, winsvc.ErrLocked) {
			return errors.New("the ButterCams service is running. Stop it first, or use the Rebuild index button on the System page")
		}
		return err
	}
	defer unlock()

	res, err := config.NewStore(root, validateEnv()).Load(filepath.Join(root, "staging"))
	if err != nil {
		return err
	}
	if res.Created {
		return fmt.Errorf("no configuration found in %s", root)
	}
	cfg := res.Config
	db, err := index.Open(filepath.Join(root, index.FileName))
	if err != nil {
		return err
	}
	defer db.Close()
	for _, cam := range cfg.Cameras {
		db.UpsertCamera(cam.ID, cam.Name)
	}
	fmt.Println("Scanning staging and", len(cfg.Destinations), "destination(s) ...")
	last := 0
	out, err := db.Rebuild(context.Background(), guardian.Roots(cfg, guardian.OldStagingPaths(db)), guardian.WantDest(cfg), func(n int) {
		if n-last >= 500 {
			last = n
			fmt.Printf("  %d files\n", n)
		}
	})
	if err != nil {
		return err
	}
	fmt.Printf("Index rebuilt: %d recordings in %d files.\n", out.Segments, out.Files)
	for _, id := range out.Offline {
		fmt.Printf("  could not be reached and was left as it was: %s\n", id)
	}
	for _, p := range out.Skipped {
		fmt.Printf("  could not be read: %s\n", p)
	}
	return nil
}

// ---------------------------------------------------------------------- setup

func runSetup(args []string) error {
	if len(args) == 0 {
		return errors.New("setup needs a command: init, install-service, uninstall-service or test-destination")
	}
	switch args[0] {
	case "init":
		return setupInit(args[1:])
	case "install-service":
		return setupInstallService(args[1:])
	case "uninstall-service":
		return setupUninstallService(args[1:])
	case "test-destination":
		return setupTestDestination(args[1:])
	}
	return fmt.Errorf("unknown setup command %q", args[0])
}

// envDefault fills an empty flag from an environment variable.
func envDefault(flagValue *string, name string) {
	if *flagValue == "" {
		*flagValue = os.Getenv(name)
	}
}

type destFlags struct {
	typ, path, user, password, name *string
	capGB                           *float64
}

func addDestFlags(fs *flag.FlagSet) destFlags {
	return destFlags{
		typ:      fs.String("dest-type", "", `first destination: "local" or "unc"`),
		path:     fs.String("dest-path", "", `first destination path, for example E:\Recordings or \\nas01\cameras`),
		user:     fs.String("dest-user", "", "user name for a network share (optional)"),
		password: fs.String("dest-password", "", "password for a network share (optional)"),
		name:     fs.String("dest-name", "", "display name of the first destination"),
		capGB:    fs.Float64("dest-cap-gb", 0, "storage limit of the first destination in GB"),
	}
}

func (d destFlags) destination() (config.Destination, string, bool) {
	if strings.TrimSpace(*d.path) == "" {
		return config.Destination{}, "", false
	}
	typ := *d.typ
	if typ == "" {
		typ = config.DestLocal
		if config.IsUNC(*d.path) {
			typ = config.DestUNC
		}
	}
	name := *d.name
	if name == "" {
		name = "Recordings"
	}
	dest := config.Destination{ID: config.Slugify(name), Name: name, Type: typ, Path: strings.TrimSpace(*d.path),
		CapGB: *d.capGB, Enabled: true}
	if dest.ID == "" || dest.ID == "staging" {
		dest.ID = "recordings"
	}
	if typ == config.DestUNC {
		dest.Username = *d.user
	}
	return dest, *d.password, true
}

// setupInit prepares a data root: registry entry, folders, config.json, the
// first administrator and the HTTPS certificate. It is safe to run again on
// an upgrade: whatever already exists is kept.
func setupInit(args []string) error {
	fs := flag.NewFlagSet("setup init", flag.ExitOnError)
	dataRootFlag := fs.String("data-root", "", "data root (default "+winsvc.DefaultDataRoot+")")
	staging := fs.String("staging", "", `staging folder (default <data root>\staging)`)
	port := fs.Int("port", config.DefaultPort, "web port")
	adminUser := fs.String("admin-user", "", "name of the first administrator")
	adminPassword := fs.String("admin-password", "", "password of the first administrator")
	df := addDestFlags(fs)
	fs.Parse(args)
	// The installer hands passwords over in the environment so that they do
	// not appear on a command line.
	envDefault(adminPassword, "BUTTERCAMS_ADMIN_PASSWORD")
	envDefault(df.password, "BUTTERCAMS_DEST_PASSWORD")

	root := winsvc.ResolveDataRoot(*dataRootFlag)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("creating the data root: %w", err)
	}
	if err := winsvc.WriteDataRoot(root); err != nil {
		return fmt.Errorf("storing the data root in the registry: %w", err)
	}

	store := config.NewStore(root, validateEnv())
	stagingPath := *staging
	if stagingPath == "" {
		stagingPath = filepath.Join(root, "staging")
	}
	res, err := store.Load(stagingPath)
	if err != nil {
		return err
	}
	if res.Created {
		cfg := res.Config
		cfg.Staging.Path = stagingPath
		cfg.Server.Port = *port
		if dest, password, ok := df.destination(); ok {
			if dest.CapGB <= 0 {
				return errors.New("the first destination needs a storage limit: --dest-cap-gb")
			}
			if password != "" && dest.Username != "" {
				enc, err := config.Protect(password)
				if err != nil {
					return err
				}
				dest.PasswordEnc = enc
			}
			cfg.Destinations = append(cfg.Destinations, dest)
		}
		if err := store.Save(cfg); err != nil {
			return err
		}
		if err := os.MkdirAll(cfg.Staging.Path, 0o755); err != nil {
			return fmt.Errorf("creating the staging folder: %w", err)
		}
		fmt.Println("Created", store.Path())
	} else {
		fmt.Println("Kept the existing", store.Path())
	}

	users, err := api.OpenUserStore(root)
	if err != nil {
		return err
	}
	switch {
	case users.Count() > 0:
		fmt.Println("Kept the existing user accounts")
	case *adminUser == "" || *adminPassword == "":
		return errors.New("the first administrator is needed: --admin-user and --admin-password")
	default:
		if _, err := users.Create(*adminUser, *adminPassword, api.RoleAdmin); err != nil {
			return err
		}
		fmt.Println("Created administrator", *adminUser)
	}

	if err := api.EnsureCertificate(root); err != nil {
		return fmt.Errorf("creating the HTTPS certificate: %w", err)
	}
	fmt.Println("Data root ready:", root)
	return nil
}

func setupInstallService(args []string) error {
	fs := flag.NewFlagSet("setup install-service", flag.ExitOnError)
	account := fs.String("account", "", `account to run as (default LocalSystem), for example CORP\svc-buttercams`)
	password := fs.String("password", "", "password of that account")
	dataRootFlag := fs.String("data-root", "", "data root")
	noStart := fs.Bool("no-start", false, "register the service without starting it")
	fs.Parse(args)
	envDefault(password, "BUTTERCAMS_SERVICE_PASSWORD")

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	root := winsvc.ResolveDataRoot(*dataRootFlag)
	if *account != "" {
		if err := winsvc.GrantLogonAsService(*account); err != nil {
			return fmt.Errorf(`granting "Log on as a service" to %s: %w`, *account, err)
		}
	}
	if err := winsvc.InstallService(winsvc.ServiceConfig{ExePath: exe, Account: *account, Password: *password}); err != nil {
		return err
	}
	port := config.DefaultPort
	if res, err := config.NewStore(root, validateEnv()).Load(filepath.Join(root, "staging")); err == nil {
		port = res.Config.Server.Port
	}
	if err := winsvc.AddFirewallRule(port); err != nil {
		fmt.Fprintln(os.Stderr, "warning: the firewall rule could not be added:", err)
	}
	if !*noStart {
		if err := winsvc.StartService(); err != nil {
			return fmt.Errorf("starting the service: %w", err)
		}
	}
	fmt.Println("Service", winsvc.ServiceName, "installed")
	return nil
}

// setupUninstallService removes the service. With --remove-data it also
// removes ButterCams' own files from the data root. Recordings on
// destinations are never touched, and in the staging folder only files that
// ButterCams wrote are removed (spec 13, rule 7).
func setupUninstallService(args []string) error {
	fs := flag.NewFlagSet("setup uninstall-service", flag.ExitOnError)
	removeData := fs.Bool("remove-data", false, "also remove the data root (settings, database, logs, staging recordings)")
	dataRootFlag := fs.String("data-root", "", "data root")
	fs.Parse(args)

	root := winsvc.ResolveDataRoot(*dataRootFlag)
	if err := winsvc.UninstallService(); err != nil {
		return err
	}
	winsvc.RemoveFirewallRule()
	fmt.Println("Service", winsvc.ServiceName, "removed")
	if !*removeData {
		fmt.Println("The data root was kept:", root)
		return nil
	}
	staging := filepath.Join(root, "staging")
	if res, err := config.NewStore(root, validateEnv()).Load(staging); err == nil && !res.Created {
		staging = res.Config.Staging.Path
	}
	removeRecordings(staging)
	for _, pattern := range []string{config.FileName + "*", api.UsersFile + "*", index.FileName + "*", guardian.LockName} {
		matches, _ := filepath.Glob(filepath.Join(root, pattern))
		for _, m := range matches {
			os.Remove(m)
		}
	}
	os.RemoveAll(filepath.Join(root, "logs"))
	os.RemoveAll(filepath.Join(root, api.CertDir))
	os.Remove(staging)
	os.Remove(root) // only succeeds when nothing else is left in it
	winsvc.DeleteRegistry()
	fmt.Println("ButterCams data removed from", root)
	return nil
}

// removeRecordings deletes ButterCams recordings from a staging folder and
// then any folders that became empty. Anything else is left alone.
func removeRecordings(staging string) {
	cams, _ := os.ReadDir(staging)
	for _, cam := range cams {
		if !cam.IsDir() || !naming.ValidID(cam.Name()) {
			continue
		}
		days, _ := os.ReadDir(filepath.Join(staging, cam.Name()))
		for _, day := range days {
			dir := filepath.Join(staging, cam.Name(), day.Name())
			files, _ := os.ReadDir(dir)
			for _, f := range files {
				if p, ok := naming.Parse(f.Name()); ok && p.CameraID == cam.Name() {
					os.Remove(filepath.Join(dir, f.Name()))
				}
			}
			os.Remove(dir)
		}
		os.Remove(filepath.Join(staging, cam.Name()))
	}
}

// setupTestDestination is the installer's Test button (spec 13).
func setupTestDestination(args []string) error {
	fs := flag.NewFlagSet("setup test-destination", flag.ExitOnError)
	df := addDestFlags(fs)
	fs.Parse(args)
	envDefault(df.password, "BUTTERCAMS_DEST_PASSWORD")
	dest, password, ok := df.destination()
	if !ok {
		return errors.New("give the destination with --dest-path")
	}
	dest.CapGB = 1
	cfg := config.Default(filepath.Join(os.TempDir(), "buttercams-staging"))
	cfg.Destinations = []config.Destination{dest}
	if err := config.Validate(cfg, validateEnv()); err != nil {
		var ve *config.ValidationError
		if errors.As(err, &ve) {
			for _, f := range ve.Fields {
				if strings.HasPrefix(f.Field, "destinations[0].") {
					return errors.New(f.Message)
				}
			}
		}
	}
	res, err := mover.TestDestination(dest, password)
	if err != nil {
		return err
	}
	fmt.Printf("OK: %.1f GB free of %.1f GB\n", float64(res.FreeBytes)/(1<<30), float64(res.TotalBytes)/(1<<30))
	return nil
}
