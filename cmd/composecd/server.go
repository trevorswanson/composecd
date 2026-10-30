package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/trevorswanson/composetpl/pkg/registry"

	"github.com/trevorswanson/composecd/internal/api"
	"github.com/trevorswanson/composecd/internal/auth"
	"github.com/trevorswanson/composecd/internal/config"
	"github.com/trevorswanson/composecd/internal/controller"
	"github.com/trevorswanson/composecd/internal/gitcache"
	"github.com/trevorswanson/composecd/internal/hosts"
	"github.com/trevorswanson/composecd/internal/notify"
	"github.com/trevorswanson/composecd/internal/store"
)

func newServerCmd() *cobra.Command {
	var (
		file      string
		logLevel  string
		logFormat string
	)
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run the controller and its API",
		Long: `Run the controller: every refresh interval it fetches the root of the GitOps
repository, reads the Host and Application objects there, and reconciles each
Application with its composetpl release, syncing automated ones. It also serves
the REST API, the web UI on / and /healthz.

The configuration file (--config, $COMPOSECD_CONFIG) names the root and holds
credentials; see the README for its format.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			log, err := newLogger(logLevel, logFormat)
			if err != nil {
				return err
			}
			boot, err := config.LoadBootstrap(file)
			if err != nil {
				return err
			}
			return runServer(cmd.Context(), boot, log)
		},
	}
	cmd.Flags().StringVarP(&file, "config", "c", envOr("COMPOSECD_CONFIG", "composecd.yaml"), "configuration file [$COMPOSECD_CONFIG]")
	cmd.Flags().StringVar(&logLevel, "log-level", envOr("COMPOSECD_LOG_LEVEL", "info"), "debug, info, warn or error [$COMPOSECD_LOG_LEVEL]")
	cmd.Flags().StringVar(&logFormat, "log-format", envOr("COMPOSECD_LOG_FORMAT", "text"), "text or json [$COMPOSECD_LOG_FORMAT]")
	return cmd
}

func newLogger(level, format string) (*slog.Logger, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid --log-level %q", level)
	}
	opts := &slog.HandlerOptions{Level: l}
	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(os.Stderr, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stderr, opts)), nil
	}
	return nil, fmt.Errorf("invalid --log-format %q (want text or json)", format)
}

func runServer(parent context.Context, boot *config.Bootstrap, log *slog.Logger) error {
	if parent == nil {
		parent = context.Background()
	}
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	for _, bin := range []string{"git", "docker"} {
		if _, err := exec.LookPath(bin); err != nil {
			log.Warn("not found on PATH; composecd needs it", "binary", bin)
		}
	}
	if err := os.MkdirAll(boot.DataDir, 0o700); err != nil {
		return err
	}
	token, err := adminToken(boot, log)
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(boot.DataDir, "composecd.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	webhookSecret := ""
	if f := boot.Webhook.SecretFile; f != "" {
		data, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("reading webhook.secretFile: %w", err)
		}
		if webhookSecret = strings.TrimSpace(string(data)); webhookSecret == "" {
			return fmt.Errorf("webhook.secretFile %s is empty", f)
		}
	}
	notifier, err := notify.New(boot.Notifications, log)
	if err != nil {
		return err
	}
	go notifier.Run(ctx)

	backend := &hosts.CLI{DataDir: boot.DataDir, HelperImage: os.Getenv("COMPOSECD_HELPER_IMAGE")}
	ctrl := controller.New(controller.Options{
		Boot:     boot,
		Git:      gitcache.New(filepath.Join(boot.DataDir, "git"), boot.Credentials),
		Hosts:    hosts.NewRegistry(backend),
		Store:    st,
		Registry: registry.New(os.Stderr),
		Log:      log,
		Notify:   notifier,
		// Finds the Application that runs composecd itself, which it must not
		// sync from inside its own container.
		SelfContainer: controller.ContainerID(),
		// Starts the container that updates composecd itself.
		Helpers: backend,
	})
	authn := &auth.Authenticator{Store: st, AdminToken: token, Disabled: boot.Auth.Disabled}
	if boot.Auth.OIDC != nil {
		if authn.OIDC, err = auth.NewOIDC(*boot.Auth.OIDC); err != nil {
			return err
		}
	}
	if err := authn.Init(); err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              boot.Listen,
		Handler:           api.New(ctrl, api.Options{Auth: authn, WebhookSecret: webhookSecret, Version: version, Log: log}),
		ReadHeaderTimeout: 10 * time.Second,
		// Cancel SSE requests and docker log follows when the server stops.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("serving", "addr", boot.Listen, "version", version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
		close(errc)
	}()
	done := make(chan struct{})
	go func() {
		ctrl.Run(ctx)
		close(done)
	}()

	select {
	case err := <-errc:
		stop()
		<-done
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down; waiting for running operations")
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	<-done
	return nil
}

// adminToken returns the API token: from auth.tokenFile, or generated once into
// <dataDir>/admin-token.
func adminToken(boot *config.Bootstrap, log *slog.Logger) (string, error) {
	if boot.Auth.Disabled {
		log.Warn("authentication is disabled; anyone who can reach the API can deploy")
		return "", nil
	}
	if f := boot.Auth.TokenFile; f != "" {
		data, err := os.ReadFile(f)
		if err != nil {
			return "", fmt.Errorf("reading auth.tokenFile: %w", err)
		}
		token := strings.TrimSpace(string(data))
		if len(token) < 16 {
			return "", fmt.Errorf("auth.tokenFile %s: the token must be at least 16 characters", f)
		}
		return token, nil
	}
	path := filepath.Join(boot.DataDir, "admin-token")
	if data, err := os.ReadFile(path); err == nil {
		if token := strings.TrimSpace(string(data)); token != "" {
			return token, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	log.Info("generated the admin API token", "file", path)
	return token, nil
}
