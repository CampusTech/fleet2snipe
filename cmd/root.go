// Package cmd contains the cobra command tree for fleet2snipe.
package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/CampusTech/fleet2snipe/config"
	"github.com/CampusTech/fleet2snipe/fleetapi"
	"github.com/CampusTech/fleet2snipe/images"
	"github.com/CampusTech/fleet2snipe/snipe"
	f2sync "github.com/CampusTech/fleet2snipe/sync"
)

var (
	// Cfg is the loaded configuration shared across subcommands.
	Cfg *config.Config
	// ConfigFile is the path to the YAML config file.
	ConfigFile string
	// Version is populated from main.go at build time.
	Version string

	verbose   bool
	debug     bool
	logFile   string
	logFormat string

	logFileFD *os.File
)

var log = logrus.New()

// LoadConfig reads the config file and applies CLI flag overrides + logging config.
func LoadConfig(cmd *cobra.Command) error {
	var err error
	Cfg, err = config.Load(ConfigFile)
	if err != nil {
		// config.Load already treats file-not-found as non-fatal. Anything that
		// reaches us here — parse error, permission denied, transform validation
		// failure — is a real problem and must surface, not be swallowed into
		// an empty Config (that turns "your YAML doesn't parse" into the much
		// more confusing "fleet.url is required").
		return fmt.Errorf("loading config: %w", err)
	}

	applyBoolFlag(cmd, "dry-run", &Cfg.Sync.DryRun)
	applyBoolFlag(cmd, "force", &Cfg.Sync.Force)
	applyBoolFlag(cmd, "update-only", &Cfg.Sync.UpdateOnly)
	applyBoolFlag(cmd, "use-cache", &Cfg.Sync.UseCache)
	applyStringFlag(cmd, "cache-dir", &Cfg.Sync.CacheDir)

	var level logrus.Level
	switch {
	case debug:
		level = logrus.DebugLevel
	case verbose:
		level = logrus.InfoLevel
	default:
		level = logrus.WarnLevel
	}
	setAllLogLevels(level)

	var formatter logrus.Formatter
	switch strings.ToLower(logFormat) {
	case "json":
		formatter = &logrus.JSONFormatter{}
	case "text", "":
		formatter = &logrus.TextFormatter{FullTimestamp: true}
	default:
		return fmt.Errorf("invalid --log-format %q: must be 'text' or 'json'", logFormat)
	}
	setAllLogFormatters(formatter)

	setAllLogOutputs(os.Stderr)
	if logFileFD != nil {
		_ = logFileFD.Close()
		logFileFD = nil
	}
	if logFile != "" {
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return fmt.Errorf("opening log file: %w", err)
		}
		logFileFD = f
		setAllLogOutputs(io.MultiWriter(os.Stderr, f))
	}
	return nil
}

func setAllLogLevels(l logrus.Level) {
	log.SetLevel(l)
	fleetapi.SetLogLevel(l)
	snipe.SetLogLevel(l)
	f2sync.SetLogLevel(l)
	images.SetLogLevel(l)
}

func setAllLogFormatters(f logrus.Formatter) {
	log.SetFormatter(f)
	fleetapi.SetLogFormatter(f)
	snipe.SetLogFormatter(f)
	f2sync.SetLogFormatter(f)
	images.SetLogFormatter(f)
}

func setAllLogOutputs(w io.Writer) {
	log.SetOutput(w)
	fleetapi.SetLogOutput(w)
	snipe.SetLogOutput(w)
	f2sync.SetLogOutput(w)
	images.SetLogOutput(w)
}

func applyBoolFlag(cmd *cobra.Command, name string, dst *bool) {
	if cmd.Flags().Changed(name) {
		*dst, _ = cmd.Flags().GetBool(name)
	}
}

func applyStringFlag(cmd *cobra.Command, name string, dst *string) {
	if cmd.Flags().Changed(name) {
		*dst, _ = cmd.Flags().GetString(name)
	}
}

// contextWithSignal returns a context cancelled on SIGINT/SIGTERM.
func contextWithSignal() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case sig := <-sigCh:
			log.Infof("received signal %v, shutting down...", sig)
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

func newFleetClient() (*fleetapi.Client, error) {
	log.Info("connecting to Fleet...")
	return fleetapi.NewClient(Cfg.Fleet.URL, Cfg.Fleet.Token, Cfg.Fleet.InsecureTLS)
}

func newSnipeClient() (*snipe.Client, error) {
	log.Info("connecting to Snipe-IT...")
	c, err := snipe.NewClient(Cfg.SnipeIT.URL, Cfg.SnipeIT.APIKey, Cfg.Sync.RateLimit)
	if err != nil {
		return nil, err
	}
	c.DryRun = Cfg.Sync.DryRun
	return c, nil
}

// Execute builds and runs the root cobra command.
func Execute() {
	root := &cobra.Command{
		Use:          "fleet2snipe",
		Short:        "Sync Fleet (fleetdm.com) hosts into Snipe-IT",
		Long:         "fleet2snipe pulls device inventory from Fleet and reconciles it into Snipe-IT. Runs as a one-shot CLI (sync) or as a long-running HTTP listener for Fleet automation webhooks (serve).",
		Version:      Version,
		SilenceUsage: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return LoadConfig(cmd)
		},
		PersistentPostRun: func(*cobra.Command, []string) {
			if logFileFD != nil {
				_ = logFileFD.Close()
			}
		},
	}

	root.PersistentFlags().StringVar(&ConfigFile, "config", "settings.yaml", "Path to YAML config file")
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Verbose output (INFO level)")
	root.PersistentFlags().BoolVarP(&debug, "debug", "d", false, "Debug output (DEBUG level)")
	root.PersistentFlags().StringVar(&logFile, "log-file", "", "Append log output to this file (in addition to stderr)")
	root.PersistentFlags().StringVar(&logFormat, "log-format", "text", "Log format: text or json")

	syncCmd := NewSyncCmd()
	setupCmd := NewSetupCmd()
	serveCmd := NewServeCmd()
	testCmd := NewTestCmd()

	// --dry-run shared by anything that writes.
	for _, c := range []*cobra.Command{syncCmd, setupCmd, serveCmd} {
		c.Flags().Bool("dry-run", false, "Simulate without making changes")
	}
	// --cache-dir shared by sync + setup.
	for _, c := range []*cobra.Command{syncCmd, setupCmd} {
		c.Flags().String("cache-dir", "", `Directory for cached responses (default ".cache")`)
	}

	root.AddCommand(syncCmd, setupCmd, serveCmd, testCmd)

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
