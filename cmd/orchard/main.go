// Command orchard serves internal build distribution over a tailnet.
package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"tailscale.com/tsnet"

	"github.com/jameshuntnz/orchard/internal/api"
	"github.com/jameshuntnz/orchard/internal/config"
	"github.com/jameshuntnz/orchard/internal/install"
	"github.com/jameshuntnz/orchard/internal/store"
	"github.com/jameshuntnz/orchard/internal/update"
	"github.com/jameshuntnz/orchard/internal/version"
	"github.com/jameshuntnz/orchard/internal/web"
)

// stampedVersion is set with -ldflags for a one-off build. Normally the version comes
// from the constant the release workflow rewrites in source, which is what makes a
// binary's own account of itself survive being copied around.
var stampedVersion = ""

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
	case "install":
		if err := provision(args, true); err != nil {
			fmt.Fprintln(os.Stderr, "orchard:", err)
			os.Exit(1)
		}
	case "doctor":
		if err := provision(args, false); err != nil {
			fmt.Fprintln(os.Stderr, "orchard:", err)
			os.Exit(1)
		}
	case "update":
		if err := updateCmd(args); err != nil {
			fmt.Fprintln(os.Stderr, "orchard:", err)
			os.Exit(1)
		}
	case "firewall":
		if err := firewall(args); err != nil {
			fmt.Fprintln(os.Stderr, "orchard:", err)
			os.Exit(1)
		}
	case "version":
		fmt.Println(buildVersion())
	default:
		fmt.Fprintf(os.Stderr, "orchard: unknown command %q (want: serve, install, doctor, update, firewall, version)\n", cmd)
		os.Exit(2)
	}
}

func buildVersion() string {
	if stampedVersion != "" {
		return stampedVersion
	}
	return version.Current
}

// provision runs the install steps, applying fixes when apply is set. `doctor` is the
// same checks with apply false — one code path, so what it reports is what install acts
// on rather than a second opinion that can drift.
func provision(args []string, apply bool) error {
	name := "doctor"
	if apply {
		name = "install"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	prefix := fs.String("prefix", install.DefaultPrefix, "install root, owned by the service user")
	svcUser := fs.String("user", defaultServiceUser(), "account the service runs as")
	hostname := fs.String("hostname", install.DefaultHostname, "tsnet node name")
	uploadAddr := fs.String("upload-addr", "", "bind address for the CI upload listener, e.g. 0.0.0.0:8477")
	uploadAllow := fs.String("upload-allow", install.DefaultUploadAllow, "CIDRs allowed to use the upload listener")
	if err := fs.Parse(args); err != nil {
		return err
	}

	plan := install.Plan{
		Prefix:      *prefix,
		User:        *svcUser,
		Hostname:    *hostname,
		UploadAddr:  *uploadAddr,
		UploadAllow: *uploadAllow,
	}

	fmt.Printf("orchard %s %s\n  prefix %s, user %s\n\n", name, buildVersion(), plan.Prefix, plan.User)
	res := install.Run(context.Background(), plan.Steps(), apply, os.Stdout)
	fmt.Println()

	// Only things that are wrong fail the command. A step waiting on a person is
	// outstanding, not broken, and failing on it makes this impossible to use from a
	// script that goes on to do the next thing.
	if res.NeedsAttention() {
		return fmt.Errorf("%d step(s) need attention", res.Failed+res.Fixable)
	}
	switch {
	case res.Manual > 0:
		fmt.Println("Nothing failed. Re-run once the steps above marked \"you\" are done.")
	case apply:
		fmt.Printf("Done. The write token is in %s\n", plan.EnvFile())
	}
	return nil
}

// updateCmd is the same logic the timer runs, on demand — for when waiting six hours is
// not acceptable (DESIGN §14.1).
func updateCmd(args []string) error {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	check := fs.Bool("check", false, "report what is available without installing it")
	rollback := fs.Bool("rollback", false, "put the previous binary back")
	want := fs.String("version", "", "install this version instead of the newest")
	if err := fs.Parse(args); err != nil {
		return err
	}

	binary, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(binary); err == nil {
		binary = resolved
	}

	if *rollback {
		if err := update.Rollback(binary); err != nil {
			return err
		}
		fmt.Println("restored the previous binary; restart the service to run it")
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		// Updating needs only the update settings, but Load is the one place that
		// reads them, and a host with a broken configuration should not be quietly
		// replacing its own binary either.
		return err
	}
	if cfg.Update.InContainer {
		return errors.New("this is a container: the image tag is the unit of deployment, so updating means pulling a new tag")
	}

	u, err := update.New(update.Config{
		Enabled:  cfg.Update.Enabled,
		Repo:     cfg.Update.Repo,
		Channel:  cfg.Update.Channel,
		Interval: cfg.Update.Interval,
		Token:    cfg.Update.Token,
	}, buildVersion(), binary)
	if err != nil {
		return err
	}

	ctx := context.Background()
	var rel update.Release

	if *want != "" {
		v, err := update.ParseVersion(*want)
		if err != nil {
			return err
		}
		if rel, err = u.Find(ctx, v); err != nil {
			return err
		}
	} else {
		rel, err = u.Latest(ctx)
		if errors.Is(err, update.ErrNoUpdate) {
			fmt.Printf("%s is the newest on the %s channel\n", u.Current(), cfg.Update.Channel)
			return nil
		}
		if err != nil {
			return err
		}
	}

	if *check {
		fmt.Printf("running %s, available %s\n", u.Current(), rel.Version)
		return nil
	}

	fmt.Printf("fetching %s\n", update.AssetName(rel.Version))
	body, err := u.Fetch(ctx, rel)
	if err != nil {
		return err
	}
	if err := u.Install(body, rel.Version); err != nil {
		return err
	}
	fmt.Printf("installed %s; restart the service to run it\n", rel.Version)
	return nil
}

// firewall prints or installs the packet-filter rules for the upload listener.
//
// Separate from `install` on purpose: pf on a host is shared with whatever else uses it,
// and reloading it is not something an install should do as a side effect of putting a
// binary in place.
func firewall(args []string) error {
	fs := flag.NewFlagSet("firewall", flag.ExitOnError)
	apply := fs.Bool("apply", false, "write the anchor and load it (needs root)")
	uploadAddr := fs.String("upload-addr", "", "the listener being restricted, e.g. 0.0.0.0:8477")
	uploadAllow := fs.String("upload-allow", install.DefaultUploadAllow, "CIDRs allowed to reach it")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Default to what the service is actually configured with, so the rules cannot
	// drift from the allowlist they are meant to reinforce.
	if *uploadAddr == "" {
		*uploadAddr = os.Getenv("ORCHARD_UPLOAD_ADDR")
	}
	if v := os.Getenv("ORCHARD_UPLOAD_ALLOW"); v != "" && !isFlagSet(fs, "upload-allow") {
		*uploadAllow = v
	}
	if *uploadAddr == "" {
		return errors.New("no upload listener to restrict: pass --upload-addr or set ORCHARD_UPLOAD_ADDR")
	}

	return install.Firewall(context.Background(), install.Plan{
		UploadAddr:  *uploadAddr,
		UploadAllow: *uploadAllow,
	}, *apply, os.Stdout)
}

func isFlagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// defaultServiceUser prefers the human who invoked sudo over root, since running the
// service as root would defeat the point of it owning its own install directory.
func defaultServiceUser() string {
	if u := os.Getenv("SUDO_USER"); u != "" {
		return u
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "orchard"
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

	// Before anything is served: if an update installed a new binary and has not yet
	// been proven, prove it or put the old one back.
	if binary, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(binary); err == nil {
			binary = resolved
		}
		verifyUpdate(binary, st, renderer, log)
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
			Logf:     tsnetLogf(log),
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
		srv.UploadAllowed = cfg.AllowsUpload
		upload = &http.Server{
			// The source address is logged on every request here, not just on a
			// refusal. The allowlist covers the range virtualisation hands to guest
			// bridges, and seeing which address CI actually arrives from is what
			// turns a renumbered bridge into something noticed rather than a publish
			// that starts failing.
			Handler:           api.Logging(srv.UploadMux(), log, "upload", sourceAddr),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       60 * time.Minute,
			WriteTimeout:      5 * time.Minute,
			IdleTimeout:       2 * time.Minute,
		}
		go func() { errs <- upload.Serve(ln) }()

		if cfg.UploadAllowAny {
			log.Warn("upload listener accepts every source address: ORCHARD_UPLOAD_ALLOW is \"any\"",
				"addr", cfg.UploadAddr)
		} else {
			allowed := make([]string, 0, len(cfg.UploadAllow))
			for _, p := range cfg.UploadAllow {
				allowed = append(allowed, p.String())
			}
			log.Info("upload listener bound: writes only, bearer token required",
				"addr", cfg.UploadAddr, "allow", allowed)
		}
	}

	if cfg.MaxBuildAge > 0 {
		go ageSweeper(ctx, st, cfg.MaxBuildAge, log)
	}

	switch {
	case *devAddr != "":
		// Development mode replaces the binary's own idea of where it lives.
		srv.SetSelfUpdate(api.SelfUpdateStatus{Reason: "development mode"})
	case cfg.Update.InContainer:
		srv.SetSelfUpdate(api.SelfUpdateStatus{Reason: "container: the image tag is the unit of deployment"})
		log.Info("self-update disabled: this is a container, so the image tag is the unit of deployment")
	case !cfg.Update.Enabled:
		srv.SetSelfUpdate(api.SelfUpdateStatus{Reason: "disabled by configuration"})
		log.Info("self-update disabled by configuration")
	default:
		binary, err := os.Executable()
		if err != nil {
			srv.SetSelfUpdate(api.SelfUpdateStatus{Reason: "cannot determine the running binary"})
			log.Warn("self-update disabled: cannot determine the running binary", "err", err)
			break
		}
		if resolved, err := filepath.EvalSymlinks(binary); err == nil {
			binary = resolved
		}
		u, err := update.New(update.Config{
			Enabled:  cfg.Update.Enabled,
			Repo:     cfg.Update.Repo,
			Channel:  cfg.Update.Channel,
			Interval: cfg.Update.Interval,
			Token:    cfg.Update.Token,
		}, buildVersion(), binary)
		if err != nil {
			srv.SetSelfUpdate(api.SelfUpdateStatus{Reason: err.Error()})
			log.Warn("self-update disabled", "err", err)
			break
		}
		base := api.SelfUpdateStatus{Enabled: true, Channel: cfg.Update.Channel}
		srv.SetSelfUpdate(base)
		// Reporting the outcome of each check is what keeps "enabled" from being the
		// only thing /healthz knows, since the two can disagree for months.
		record := func(available string, checkErr error) {
			st := base
			now := time.Now().UTC()
			st.LastCheck = &now
			st.Available = available
			if checkErr != nil {
				st.LastError = checkErr.Error()
			}
			srv.SetSelfUpdate(st)
		}
		log.Info("self-update enabled", "repo", cmp.Or(cfg.Update.Repo, update.DefaultRepo),
			"channel", cfg.Update.Channel, "interval", cfg.Update.Interval.String())
		go updateLoop(ctx, u, cfg.Update.Interval, srv.PublishesInFlight, log, stop, record)
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

// tsnetLogf routes tsnet's chatter to Debug, which the default level drops, with one
// exception: on first run — no TS_AUTHKEY, no node key in the state directory — the only
// way to join the tailnet arrives through this callback as a login URL. Losing it leaves a
// service that has started, logged nothing, and is waiting for something no one can see.
func tsnetLogf(log *slog.Logger) func(string, ...any) {
	return func(format string, a ...any) {
		msg := strings.TrimSpace(fmt.Sprintf(format, a...))
		if strings.Contains(msg, "login.tailscale.com") || strings.Contains(msg, "To authenticate") {
			log.Warn("tailscale login required: open this URL to join the tailnet", "detail", msg)
			return
		}
		log.Debug("tsnet: " + msg)
	}
}

// verifyUpdate runs the self-check an update leaves behind, and puts the previous binary
// back if it fails.
//
// This is what makes rollback automatic: a bad release corrects itself on the restart it
// causes, instead of crash-looping until someone notices. It is viable only because
// nothing rewrites state on disk during an update, so the previous binary still
// understands everything it left behind (DESIGN §14.2).
func verifyUpdate(binary string, st *store.Store, renderer *web.Renderer, log *slog.Logger) {
	marker, pending := update.PendingMarker(binary)
	if !pending {
		return
	}

	if err := selfCheck(st, renderer); err != nil {
		log.Error("self-check failed after update; rolling back",
			"from", marker.PreviousVersion, "to", marker.NewVersion, "err", err)
		if rbErr := update.Rollback(binary); rbErr != nil {
			// Nothing else can be done from here, and continuing on a binary that
			// just failed its own check would be worse than exiting.
			log.Error("rollback failed", "err", rbErr)
		}
		os.Exit(1) // the supervisor starts what is now in place, which is the old one
	}

	if err := update.ClearMarker(binary); err != nil {
		log.Warn("could not clear the update marker", "err", err)
	}
	log.Info("update verified", "from", marker.PreviousVersion, "to", marker.NewVersion)
}

// selfCheck is deliberately the three things §14.1 names: bind a listener, read the state
// directory, render a page. Between them they exercise everything a release could break
// badly enough to be worth reverting for.
func selfCheck(st *store.Store, renderer *web.Renderer) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("cannot bind a listener: %w", err)
	}
	ln.Close()

	apps, err := st.ListApps()
	if err != nil {
		return fmt.Errorf("cannot read the state directory: %w", err)
	}

	var buf bytes.Buffer
	if len(apps) > 0 && len(apps[0].Builds) > 0 {
		return renderer.Install(&buf, web.InstallData{
			App:         apps[0].ID,
			Build:       apps[0].Builds[0],
			PageURL:     "https://example.invalid/",
			IPAURL:      "https://example.invalid/app.ipa",
			ManifestURL: "https://example.invalid/manifest.plist",
		})
	}
	// With no builds there is no install page to render, and the index is the most
	// this can honestly exercise.
	return renderer.Root(&buf, web.RootData{Apps: apps})
}

// updateLoop checks for a newer release on a timer and installs it.
//
// Exiting and letting the supervisor respawn is the whole mechanism: it needs no
// privilege beyond writing to the install directory the service already owns, so there
// is no sudo, no root daemon and no launchctl permission to arrange (DESIGN §14.1).
func updateLoop(ctx context.Context, u *update.Updater, interval time.Duration,
	inFlight func() int64, log *slog.Logger, done func(), record func(string, error)) {

	t := time.NewTicker(interval)
	defer t.Stop()
	// The first check runs immediately rather than an interval from now. Waiting six
	// hours to find out whether updating works at all would leave /healthz reporting
	// nothing for exactly the window in which a misconfiguration is most likely.
	for first := true; ; first = false {
		if !first {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}

		// An update that replaced the binary mid-publish would abort a transfer that
		// may have taken minutes. Deferring to the next check costs at most one
		// interval.
		if inFlight() > 0 {
			log.Info("deferring update check: a publish is in flight")
			continue
		}

		rel, err := u.Latest(ctx)
		if errors.Is(err, update.ErrNoUpdate) {
			record("", nil)
			continue
		}
		if err != nil {
			record("", err)
			log.Warn("update check failed", "err", err)
			continue
		}
		record(rel.Version.String(), nil)

		body, err := u.Fetch(ctx, rel)
		if err != nil {
			record(rel.Version.String(), err)
			log.Error("refusing the update", "version", rel.Version.String(), "err", err)
			continue
		}

		// Checked again: the download takes time, and a publish may have started
		// during it.
		if inFlight() > 0 {
			log.Info("deferring install: a publish started during the download")
			continue
		}
		if err := u.Install(body, rel.Version); err != nil {
			record(rel.Version.String(), err)
			log.Error("installing the update failed", "version", rel.Version.String(), "err", err)
			continue
		}

		log.Warn("installed a new version; draining and exiting for the supervisor to start it",
			"from", u.Current().String(), "to", rel.Version.String())
		done()
		return
	}
}

// sourceAddr is the peer identity for a listener with no tsnet underneath it.
func sourceAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
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
