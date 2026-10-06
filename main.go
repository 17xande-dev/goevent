// Command goevent is event registration and ticketing — public event pages, a
// registration form, payment through South African gateways, and an admin — in
// one binary.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/17xande-dev/goevent/internal/auth"
	"github.com/17xande-dev/goevent/internal/blob"
	"github.com/17xande-dev/goevent/internal/config"
	"github.com/17xande-dev/goevent/internal/db"
	"github.com/17xande-dev/goevent/internal/events"
	"github.com/17xande-dev/goevent/internal/handler"
	"github.com/17xande-dev/goevent/internal/middleware"
	"github.com/17xande-dev/goevent/internal/outbox"
	"github.com/17xande-dev/goevent/internal/payment"
	"github.com/17xande-dev/goevent/internal/payment/payfast"
	"github.com/17xande-dev/goevent/internal/payment/snapscan"
	"github.com/17xande-dev/goevent/internal/registrations"
	"github.com/17xande-dev/mailer"
	"github.com/17xande-dev/mailer/msauth"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	migrateOnly := flag.Bool("migrate", false, "apply pending migrations and exit")
	migrateStatus := flag.Bool("migrate-status", false, "print migration status and exit")
	checkConfig := flag.Bool("check-config", false, "validate the full server configuration and exit")
	flag.Parse()

	// Migration jobs need database access, not the payment or mail secrets.
	// An explicit full configuration check takes precedence over these modes.
	load := config.Load
	if (*migrateOnly || *migrateStatus) && !*checkConfig {
		load = config.LoadTool
	}
	cfg, err := load()
	if err != nil {
		return err
	}

	log := newLogger(cfg.LogLevel, cfg.LogFormat)
	slog.SetDefault(log)

	if *checkConfig {
		fmt.Println("config: ok")
		return nil
	}

	// Signals cancel this context, which unblocks the wait below and triggers
	// a graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if *migrateStatus {
		return printMigrationStatus(ctx, pool, log)
	}
	if err := db.Migrate(ctx, pool, log); err != nil {
		return err
	}
	if *migrateOnly {
		return nil
	}

	users := auth.NewStore(pool)
	if err := ensureSetupToken(ctx, users, cfg.SetupToken, log); err != nil {
		return err
	}

	gateways, err := newGateways(cfg, log)
	if err != nil {
		return err
	}
	mail, err := newMailer(cfg, log)
	if err != nil {
		return err
	}
	// Image storage is built before the templates because a template resolves an
	// event's image key through it.
	images, err := newBlobStorage(cfg, log)
	if err != nil {
		return err
	}

	// Assets and templates are both read once at startup, so a broken override
	// fails the boot rather than the first request that happens to hit it. The
	// overrides are still validated here when THEME_RELOAD is on — a theme that is
	// broken before the first request should still fail the boot.
	handler.SetStaticDir(cfg.StaticDir)
	if err := handler.CheckAssets(); err != nil {
		return err
	}
	if cfg.StaticDir != "" {
		log.Info("static assets may be overridden from disk", "dir", cfg.StaticDir)
	}
	tmpl, err := handler.ParseTemplates(cfg.TemplateDir, images)
	if err != nil {
		return err
	}
	if cfg.ThemeReload {
		// Loud, because it is a per-request cost and a production deployment that
		// has it on by accident should say so in its logs.
		log.Warn("THEME_RELOAD is on: templates and assets are re-read on every request; development only",
			"template_dir", cfg.TemplateDir, "static_dir", cfg.StaticDir)
		handler.SetAssetReload(true)
		tmpl.SetReload(true)
	}
	outboxKey, signer, err := deriveKeys(cfg.SecretKey)
	if err != nil {
		return err
	}
	queue, err := outbox.New(pool, outboxKey)
	if err != nil {
		return err
	}
	regs := registrations.NewStore(pool)
	h := handler.New(handler.Deps{
		Config:        cfg,
		Log:           log,
		Tmpl:          tmpl,
		Events:        events.NewStore(pool),
		Registrations: regs,
		Signer:        signer,
		Gateways:      gateways,
		Mail:          mail,
		Outbox:        queue,
		Images:        images,
		Users:         users,
	})
	// Expired admin sessions and lapsed registration holds are swept in-process,
	// on this context, so the sweeps stop with the server rather than outliving it.
	startSessionCleanup(ctx, users, log)
	startHoldExpiry(ctx, regs, log)
	workerCtx, stopWorker := context.WithCancel(ctx)
	waitWorker := h.StartMailWorker(workerCtx)
	defer func() { stopWorker(); waitWorker() }()

	srv := &http.Server{
		Addr:              net.JoinHostPort("", cfg.Port),
		Handler:           routes(cfg, h, gateways, users, pool, log),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", srv.Addr, "site", cfg.SiteName, "currency", cfg.Currency)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// ensureSetupToken makes sure a deployment with no administrators has a way to get its
// first one, and says how in the log.
//
// The claim flow replaces both alternatives a bootstrap normally picks between: a
// fixed default credential, which is a CVE class and worse in a project published
// for others to copy, and an unguarded first-run wizard, which is a race anybody
// who finds the deployment before its operator does can win. The cost is one
// `docker compose logs`.
//
// Supplied and generated tokens differ in one way only: a supplied one is never
// printed, because it is already wherever the deploy keeps its secrets and a log
// line is a worse place for it to also be.
func ensureSetupToken(ctx context.Context, users *auth.Store, supplied string, log *slog.Logger) error {
	n, err := users.Count(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		// Claimed. Nothing to issue, and admin_setup keeps its consumed row for
		// ever so this stays true across restarts and even if every account is
		// later disabled.
		return nil
	}

	token := supplied
	if token == "" {
		if token, err = auth.NewToken(); err != nil {
			return err
		}
	}

	stored, err := users.CreateSetupToken(ctx, token)
	if err != nil {
		return err
	}
	if !stored {
		// A token is already there and unclaimed. admin_setup holds a single row,
		// so this one was not stored and the one that was cannot be changed by a
		// restart — which is deliberate: a browser may be sitting on a
		// half-finished setup page holding it.
		//
		// Which of the two situations this is matters to the operator, so ask
		// rather than guess. A supplied token that does not match is the trap: it
		// looks configured, it is in the environment, and /admin/setup will refuse
		// it with nothing anywhere to say why.
		if supplied != "" {
			matches, err := users.CheckSetupToken(ctx, supplied)
			if err != nil {
				return err
			}
			if !matches {
				log.Warn("SETUP_TOKEN does not match the setup token already issued, and is being ignored",
					"visit", "/admin/setup",
					"note", "the issued token is stored only as a hash and cannot be recovered or replaced; "+
						"claim the admin with the token from an earlier log line, or "+
						"DELETE FROM admin_setup to have SETUP_TOKEN take effect on the next start")
				return nil
			}
			log.Info("no administrator exists; SETUP_TOKEN will claim the first account",
				"visit", "/admin/setup")
			return nil
		}
		log.Warn("no administrator exists and a setup token has already been issued",
			"visit", "/admin/setup",
			"note", "the token was printed when it was issued; look further back in this log, "+
				"or DELETE FROM admin_setup to have a new one issued on the next start")
		return nil
	}
	if supplied != "" {
		log.Info("no administrator exists; SETUP_TOKEN will claim the first account",
			"visit", "/admin/setup")
		return nil
	}
	// Deliberately one line and deliberately loud: this is the only place the
	// token ever exists in plain text, and an operator has to be able to find it.
	log.Warn("no administrator exists. Visit /admin/setup and use this one-time setup token",
		"setup_token", token)
	return nil
}

// deriveKeys turns the one configured secret into the keys each use needs: the
// outbox's AES key (as the hex it takes) and the link and ticket signer. Each is
// an HMAC of the secret under its own label, so no two uses ever share key
// material and a key leaked from one cannot be used for another.
func deriveKeys(secretHex string) (outboxHex string, signer registrations.Signer, err error) {
	secret, err := hex.DecodeString(secretHex)
	if err != nil || len(secret) != 32 {
		return "", registrations.Signer{}, errors.New("config: SECRET_KEY must be 64 hexadecimal characters")
	}
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("goevent/outbox/v1"))
	return hex.EncodeToString(m.Sum(nil)), registrations.NewSigner(secret), nil
}

func printMigrationStatus(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	status, err := db.Status(ctx, pool, log)
	if err != nil {
		return err
	}
	for _, s := range status {
		applied := "pending"
		if !s.AppliedAt.IsZero() {
			applied = s.AppliedAt.Format(time.RFC3339)
		}
		fmt.Printf("%-6d %-10s %-25s %s\n", s.Source.Version, s.State, applied, s.Source.Path)
	}
	return nil
}

// newGateways builds every payment gateway this deployment has configured, in
// the order the checkout will offer them.
//
// This is the one place a gateway is chosen. Everything downstream sees only
// payment.Registry and payment.Gateway, which is what keeps "add a gateway" to a
// package and a few lines here.
func newGateways(cfg config.Config, log *slog.Logger) (payment.Registry, error) {
	var gateways []payment.Gateway

	if cfg.PayFast.Configured() {
		g, err := newPayFast(cfg, log)
		if err != nil {
			return payment.Registry{}, err
		}
		gateways = append(gateways, g)
	}
	if cfg.SnapScan.Configured() {
		g, err := newSnapScan(cfg, log)
		if err != nil {
			return payment.Registry{}, err
		}
		gateways = append(gateways, g)
	}
	if len(gateways) == 0 {
		// Allowed: a deployment running only free events, or taking cash and EFT
		// that an organiser records by hand, has no gateway to configure. Paid
		// online registration is then simply not offered.
		log.Warn("no payment gateway is configured: paid tickets can only be settled by an administrator recording cash or EFT",
			"enable", "set PAYFAST_MERCHANT_ID or SNAPSCAN_SNAP_CODE")
		return payment.Registry{}, nil
	}

	// A gateway settling in a currency events are not priced in would take the
	// right number in the wrong money. Discovering that at the first checkout,
	// after a registration row already exists, is worse than at boot.
	for _, g := range gateways {
		if g.Currency() != cfg.Currency {
			return payment.Registry{}, fmt.Errorf("config: CURRENCY is %q, but %s settles in %s only",
				cfg.Currency, g.Label(), g.Currency())
		}
	}

	return payment.NewRegistry(gateways...)
}

func newPayFast(cfg config.Config, log *slog.Logger) (payment.Gateway, error) {
	// The gateway's URLs are derived from BASE_URL, because three URLs that have
	// to agree with each other and with the deployment are three chances to get
	// one wrong. NotifyURL is the exception: PayFast's own servers have to reach
	// it, which during development means a tunnel's hostname rather than whatever
	// BASE_URL says.
	notify := cfg.BaseURL + "/payments/payfast/callback"
	if cfg.PayFast.NotifyURL != "" {
		notify = cfg.PayFast.NotifyURL
	}

	return payfast.New(payfast.Config{
		MerchantID:       cfg.PayFast.MerchantID,
		MerchantKey:      cfg.PayFast.MerchantKey,
		Passphrase:       cfg.PayFast.Passphrase,
		Sandbox:          cfg.PayFast.Sandbox,
		ReturnURL:        cfg.BaseURL + "/checkout/success",
		CancelURL:        cfg.BaseURL + "/checkout/cancel",
		NotifyURL:        notify,
		AllowedCIDRs:     cfg.PayFast.AllowedCIDRs,
		AllowAnySourceIP: cfg.PayFast.AllowAnySourceIP,
		Log:              log,
	})
}

// newSnapScan builds the SnapScan gateway.
//
// Note what is absent: there is no sandbox switch, because SnapScan has no
// sandbox. Configuring it at all means real payments, which is the opposite of
// PAYFAST_SANDBOX's safe default and is why snapscan.New says so in the log at
// startup.
//
// SnapScan's notification URL is configured on the merchant account by SnapScan
// support rather than sent with each payment, so there is no notify URL here.
// It must point at BASE_URL + /payments/snapscan/callback, which on a laptop
// means a tunnel.
func newSnapScan(cfg config.Config, log *slog.Logger) (payment.Gateway, error) {
	return snapscan.New(snapscan.Config{
		SnapCode:       cfg.SnapScan.SnapCode,
		APIKey:         cfg.SnapScan.APIKey,
		WebhookAuthKey: cfg.SnapScan.WebhookAuthKey,
		ValidationKey:  cfg.SnapScan.ValidationKey,
		SuccessURL:     cfg.BaseURL + "/checkout/success",
		FailURL:        cfg.BaseURL + "/checkout/cancel",
		Log:            log,
	})
}

// newMailer builds the mail sender.
//
// Mail is required: a confirmation carries the attendee's tickets. The encrypted outbox
// handles temporary failures; config.Load refuses a missing transport at boot.
func newMailer(cfg config.Config, log *slog.Logger) (mailer.Sender, error) {
	// config.Load refuses to boot without SMTP or Graph, so this is unreachable
	// through main. It is kept as an error rather than deleted because newMailer
	// is the only thing standing between an unconfigured relay and a silently
	// dropped receipt, and a second reader of this function should not have to
	// go and check that somebody else already refused.
	//
	// mailer.Discard still exists for tests and for an adopter assembling their own
	// main with different rules. It is no longer reachable from this one.
	if !cfg.SMTP.Configured() && !cfg.Graph.Configured() {
		return nil, fmt.Errorf("config: SMTP_HOST and EMAIL_FROM, or Graph, are required")
	}

	// Graph wins when configured. The two are not layered — a deployment picks
	// one app registration and one permission grant, not both — so there is no
	// reason to build an SMTP sender at all once Graph is in play.
	if cfg.Graph.Configured() {
		tokens, err := msauth.New(cfg.Graph.TenantID, cfg.Graph.ClientID, cfg.Graph.ClientSecret,
			msauth.WithScope(msauth.ScopeGraph))
		if err != nil {
			return nil, err
		}
		sender, err := mailer.NewGraphSender(mailer.GraphConfig{
			TokenSource: tokens,
			From:        cfg.Graph.From,
			ReplyTo:     cfg.SMTP.ReplyTo,
		})
		if err != nil {
			return nil, err
		}
		log.Info("email configured", "transport", "graph", "from", cfg.Graph.From,
			"notify", cfg.NotifyEmail)
		return sender, nil
	}

	policy, err := mailer.ParseTLSPolicy(cfg.SMTP.TLS)
	if err != nil {
		return nil, err
	}
	if policy == mailer.TLSNone {
		log.Warn("SMTP TLS is disabled: credentials and registration details go over the network in the clear",
			"host", cfg.SMTP.Host)
	}

	// XOAUTH2 when an app registration is configured, which is how an Exchange
	// Online mailbox is reached; a password otherwise. The token source is built
	// here rather than dialled: a slow or unreachable identity provider at boot
	// is not a reason to refuse to start, for the same reason the relay
	// itself is not.
	var tokens mailer.TokenSource
	auth := "password"
	if cfg.SMTP.OAuth.Configured() {
		tokens, err = msauth.New(
			cfg.SMTP.OAuth.TenantID, cfg.SMTP.OAuth.ClientID, cfg.SMTP.OAuth.ClientSecret)
		if err != nil {
			return nil, err
		}
		auth = "xoauth2"
	} else if cfg.SMTP.Username == "" {
		auth = "none"
	}

	sender, err := mailer.NewSMTPSender(mailer.SMTPConfig{
		Host:        cfg.SMTP.Host,
		Port:        cfg.SMTP.Port,
		Username:    cfg.SMTP.Username,
		Password:    cfg.SMTP.Password,
		TokenSource: tokens,
		From:        cfg.SMTP.From,
		ReplyTo:     cfg.SMTP.ReplyTo,
		TLS:         policy,
	})
	if err != nil {
		return nil, err
	}
	log.Info("email configured", "transport", "smtp", "host", cfg.SMTP.Host, "port", cfg.SMTP.Port,
		"from", cfg.SMTP.From, "tls", policy, "auth", auth, "notify", cfg.NotifyEmail)
	return sender, nil
}

// newBlobStorage builds object storage for event images.
func newBlobStorage(cfg config.Config, log *slog.Logger) (blob.Storage, error) {
	// A directory this server serves itself: one binary, one volume, working
	// photographs, and no object storage to run. Not for a deployment behind a load
	// balancer or one that scales to zero — two instances do not share a directory.
	if cfg.ImageDir != "" {
		storage, err := blob.NewDisk(cfg.ImageDir)
		if err != nil {
			return nil, err
		}
		log.Info("event images stored on disk", "dir", storage.Dir(), "served_at", blob.ImagePrefix,
			"note", "a single instance with a persistent volume; use BLOB_* for anything scaled out")
		return storage, nil
	}

	if !cfg.Blob.Configured() {
		// Unreachable through main for the same reason as the mail branch above:
		// config.Load requires one image backend or the other. blob.Unconfigured
		// remains for tests and for an adopter's own main.
		return nil, fmt.Errorf("config: IMAGE_DIR or the BLOB_* variables are required")
	}

	storage, err := blob.NewS3(blob.S3Config{
		Endpoint:      cfg.Blob.Endpoint,
		Bucket:        cfg.Blob.Bucket,
		AccessKey:     cfg.Blob.AccessKey,
		SecretKey:     cfg.Blob.SecretKey,
		Region:        cfg.Blob.Region,
		UseTLS:        cfg.Blob.UseTLS,
		PublicBaseURL: cfg.Blob.PublicBaseURL,
	})
	if err != nil {
		return nil, err
	}
	if !cfg.Blob.UseTLS {
		log.Warn("object storage TLS is disabled: credentials go over the network in the clear",
			"endpoint", cfg.Blob.Endpoint)
	}
	log.Info("object storage configured", "endpoint", cfg.Blob.Endpoint,
		"bucket", cfg.Blob.Bucket, "public_base", cfg.Blob.PublicBaseURL)
	return storage, nil
}

func routes(cfg config.Config, h *handler.Handler, gateways payment.Registry, users *auth.Store, pool *pgxpool.Pool, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz(pool, log))

	h.RegisterPublic(mux)

	// Disk-backed images are served by this server, from its own origin — which
	// is why they need no CSP allowance beyond 'self'. A bucket-backed deployment
	// serves its own and this route does not exist.
	if cfg.ImageDir != "" {
		if err := h.RegisterImages(mux, cfg.ImageDir); err != nil {
			// The directory was checked at startup by blob.NewDisk, so reaching this
			// means it changed underneath us between then and now.
			log.Error("cannot serve event images", "dir", cfg.ImageDir, "error", err)
		}
	}

	// Everything that changes state is mounted here, behind CSRF protection and
	// the cookie nosurf needs to set for it.
	// The gateway callback is mounted here and not on the first-party handler, so
	// it sits outside CSRF protection by not being in the group — a payment
	// provider cannot carry a token. It authenticates itself instead; see
	// internal/handler/webhook.go.
	h.RegisterPayments(mux)

	firstParty := h.FirstPartyHandler(middleware.RequireAdmin(users, log))
	mux.Handle("/admin/", firstParty)
	mux.Handle(handler.RegisterPath, firstParty)

	// Security headers wrap everything, including 404s and /healthz. Each
	// gateway declares what its own hand-over needs: a form target for one that
	// posts, an image origin for one that shows a hosted QR code. A gateway whose
	// origin is missing here has its hand-over refused by the browser and nowhere
	// else, which is why this is derived rather than configured.
	gatewayForms, gatewayImages := gateways.CSP()
	policy := middleware.Policy{
		FrameAncestors: cfg.EmbedOrigins,
		FormActions:    gatewayForms,
		// Only on an https deployment: a browser ignores HSTS over plain HTTP
		// anyway, and sending it from localhost would pin a rule that makes the
		// next plain-HTTP project on this port unreachable.
		HSTS: cfg.CookieSecure,
	}
	policy.ImgSources = gatewayImages
	if cfg.Blob.Configured() {
		// The origin, not the base URL: a CSP source carrying a path matches that
		// path exactly, which would permit the bucket root and refuse every image
		// under it. See Blob.PublicOrigin.
		policy.ImgSources = append(policy.ImgSources, cfg.Blob.PublicOrigin())
	}
	// A hosted font service, if one is configured. Empty by default: the theme uses
	// the system font stack, which needs no origin allowed at all.
	policy.FontSources = cfg.FontOrigins
	// RequestID first, because Chain reads outermost-first: everything inside it —
	// including the rate limiter and the security headers, both of which can answer
	// a request on their own — then runs with an id already in the context to log
	// against.
	return middleware.Chain(mux, middleware.RequestID, middleware.SecurityHeaders(policy))
}

// healthz reports readiness, including the database, so a container platform
// can gate traffic on it.
func healthz(pool *pgxpool.Pool, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if err := pool.Ping(ctx); err != nil {
			log.Error("healthz: database unreachable", "error", err)
			http.Error(w, "database unreachable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("ok\n"))
	}
}

func newLogger(level, format string) *slog.Logger {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: l}
	if format == "gcp" {
		// Google Cloud Logging reads "severity" and "message"; slog writes "level"
		// and "msg". Without the rename every line files as DEFAULT severity, so a
		// `severity>=ERROR` filter matches nothing and an alert on the error rate
		// never fires — the failure is silent in the direction that matters.
		//
		// Renaming rather than duplicating, because a line carrying both is a line
		// that shows the message twice in the console.
		opts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) > 0 {
				return a
			}
			switch a.Key {
			case slog.LevelKey:
				a.Key = "severity"
			case slog.MessageKey:
				a.Key = "message"
			}
			return a
		}
	}
	// JSON to stdout: the one log format every managed platform ingests without
	// configuration.
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}
