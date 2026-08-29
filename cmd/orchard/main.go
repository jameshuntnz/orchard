// Command orchard serves internal build distribution over a tailnet.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"tailscale.com/tsnet"

	"github.com/jameshuntnz/orchard/internal/api"
	"github.com/jameshuntnz/orchard/internal/config"
	"github.com/jameshuntnz/orchard/internal/store"
	"github.com/jameshuntnz/orchard/internal/web"
)

// version is stamped at build time with -ldflags "-X main.version=…".
var version = ""

func main() {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	switch cmd {
	case "serve":
		if err := serve(args); err != nil {
			fmt.Fprintln(os.Stderr, "orchard:", err)
			os.Exit(1)
		}
	case "version":
		fmt.Println(buildVersion())
	default:
		fmt.Fprintf(os.Stderr, "orchard: unknown command %q (want: serve, version)\n", cmd)
		os.Exit(2)
	}
}

func buildVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "dev"
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	// devAddr serves everything over plain HTTP on a local address, skipping tsnet, for
	// looking at the pages during development. It is a flag rather than an environment
	// variable so the documented configuration surface stays exactly as designed.
	devAddr := fs.String("dev-addr", "", "serve plain HTTP here instead of joining the tailnet (development only)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := config.Prepare(cfg.StateDir); err != nil {
		return err
	}

	st := store.New(cfg.StateDir, log)
	st.CleanTmp() // staging left by a process that died mid-publish

	renderer, err := web.New()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &api.Server{
		Store:     st,
		Web:       renderer,
		Log:       log,
		Token:     cfg.Token,
		MaxUpload: cfg.MaxUploadBytes,
		Version:   buildVersion(),
	}

	var (
		browseLn net.Listener
		peer     func(*http.Request) string
	)

	if *devAddr != "" {
		browseLn, err = net.Listen("tcp", *devAddr)
		if err != nil {
			return err
		}
		srv.BaseURL = cfg.BaseURL
		if srv.BaseURL == "" {
			srv.BaseURL = "http://" + *devAddr
		}
		log.Warn("development mode: serving plain HTTP, not joining the tailnet",
			"addr", *devAddr, "baseUrl", srv.BaseURL)
	} else {
		ts := &tsnet.Server{
			Hostname: cfg.Hostname,
			Dir:      cfg.StateDir + "/" + config.TSNetSubdir,
			AuthKey:  os.Getenv("TS_AUTHKEY"),
			Logf:     func(format string, a ...any) { log.Debug("tsnet: " + fmt.Sprintf(format, a...)) },
		}
		defer ts.Close()

		status, err := ts.Up(ctx)
		if err != nil {
			return fmt.Errorf("join tailnet: %w", err)
		}
		name := strings.TrimSuffix(status.Self.DNSName, ".")

		srv.BaseURL = cfg.BaseURL
		if srv.BaseURL == "" {
			srv.BaseURL = "https://" + name
		}

		// Tailscale provisions a publicly trusted Let's Encrypt certificate for this
		// name. iOS's install daemon requires exactly that; a self-signed certificate on
		// a LAN address will not work (DESIGN §4.2).
		browseLn, err = ts.ListenTLS("tcp", ":443")
		if err != nil {
			return fmt.Errorf("listen on tailnet: %w", err)
		}

		lc, err := ts.LocalClient()
		if err != nil {
			return err
		}
		peer = func(r *http.Request) string {
			who, err := lc.WhoIs(r.Context(), r.RemoteAddr)
			if err != nil || who == nil {
				return ""
			}
			if who.UserProfile != nil && who.UserProfile.LoginName != "" {
				return who.UserProfile.LoginName
			}
			if who.Node != nil {
				return strings.TrimSuffix(who.Node.Name, ".")
			}
			return ""
		}

		log.Info("joined tailnet", "name", name, "baseUrl", srv.BaseURL)
	}

	// Timeouts are explicit on both listeners: a default http.Server has none, which is a
	// slow-loris waiting to happen. ReadHeaderTimeout is short; the body and write limits
	// are generous enough for a several-hundred-megabyte IPA to move over a tunnel
	// (DESIGN §4.4).
	browse := &http.Server{
		Handler:           api.Logging(srv.TailnetMux(), log, "tailnet", peer),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Minute,
		WriteTimeout:      60 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}

	errs := make(chan error, 2)
	go func() { errs <- browse.Serve(browseLn) }()

	var upload *http.Server
	if cfg.UploadAddr != "" {
		ln, err := net.Listen("tcp", cfg.UploadAddr)
		if err != nil {
			return fmt.Errorf("listen for uploads: %w", err)
		}
		upload = &http.Server{
			Handler:           api.Logging(srv.UploadMux(), log, "upload", nil),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       60 * time.Minute,
			WriteTimeout:      5 * time.Minute,
			IdleTimeout:       2 * time.Minute,
		}
		go func() { errs <- upload.Serve(ln) }()
		log.Info("upload listener bound: writes only, bearer token required", "addr", cfg.UploadAddr)
	}

	if cfg.MaxBuildAge > 0 {
		go ageSweeper(ctx, st, cfg.MaxBuildAge, log)
	}

	select {
	case err := <-errs:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = browse.Shutdown(shutdownCtx)
	if upload != nil {
		_ = upload.Shutdown(shutdownCtx)
	}
	return nil
}

// ageSweeper is the backstop for a consumer that stops calling in. It is off unless
// ORCHARD_MAX_BUILD_AGE_DAYS is set, and it logs loudly when it acts (DESIGN §11).
func ageSweeper(ctx context.Context, st *store.Store, max time.Duration, log *slog.Logger) {
	log.Warn("age fallback is enabled: builds older than this are removed regardless of branch state",
		"maxAgeDays", int(max.Hours()/24))
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		st.SweepAge(max, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
