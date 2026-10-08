// Command syncwatch is a read-only monitor for Syncthing deployments.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "time/tzdata" // time zones in minimal containers

	"github.com/ivarstudios/syncwatch/internal/app"
	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/probe"
	"github.com/ivarstudios/syncwatch/internal/settings"
	"github.com/ivarstudios/syncwatch/internal/web"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

// defaultListen is loopback only, so a plain "syncwatch serve" isn't reachable
// from other machines by accident. The Docker image sets SYNCWATCH_LISTEN=:8080
// and the published port decides who can reach it.
const defaultListen = "127.0.0.1:8080"

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string) bool {
	b, _ := strconv.ParseBool(os.Getenv(key))
	return b
}

// splitList splits a comma-separated list, dropping empty items.
func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func defaultDataDir() string {
	if st, err := os.Stat("/data"); err == nil && st.IsDir() {
		return "/data"
	}
	return "data"
}

func newLogger(level, format string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lv}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}

func usage() {
	fmt.Fprintf(os.Stderr, `syncwatch %s — read-only dashboard, weekly report and urgent pings for Syncthing

Usage:
  syncwatch [serve] [flags]          run the monitor (default)
  syncwatch probe [flags]            API probe; writes a findings report
  syncwatch gate [flags]             read-only gate in front of one Syncthing (holds its API key; see README)
  syncwatch reset-admin-password     set a new admin password from stdin
  syncwatch import-config FILE       replace the configuration with a YAML file
  syncwatch export-config            print the configuration as YAML (no secrets)
  syncwatch healthcheck              exit 0 if the local server answers /healthz
  syncwatch version

Run "syncwatch <command> -h" for the flags of a command.
`, version)
}

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	// Test setups with a fake Discord; never needed in production.
	config.AllowAnyWebhook = envBool("SYNCWATCH_ALLOW_ANY_WEBHOOK")
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "gate":
		err = runGate(args)
	case "probe":
		err = runProbe(args)
	case "reset-admin-password":
		err = resetPassword(args)
	case "import-config":
		err = importConfig(args)
	case "export-config":
		err = exportConfig(args)
	case "healthcheck":
		err = healthcheck(args)
	case "version", "--version":
		fmt.Println("syncwatch", version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "syncwatch:", err)
		os.Exit(1)
	}
}

func dataFlags(fs *flag.FlagSet) (*string, *string) {
	data := fs.String("data", env("SYNCWATCH_DATA", defaultDataDir()), "data directory (env SYNCWATCH_DATA)")
	key := fs.String("secret-key", "", "secrets key; default: env SYNCWATCH_SECRET_KEY or <data>/secret.key")
	return data, key
}

func secretKey(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv("SYNCWATCH_SECRET_KEY")
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	data, key := dataFlags(fs)
	listen := fs.String("listen", env("SYNCWATCH_LISTEN", defaultListen), "listen address, e.g. 192.168.1.10:8080 (env SYNCWATCH_LISTEN; the default is loopback only)")
	cfgFile := fs.String("config", env("SYNCWATCH_CONFIG", ""), "bootstrap YAML imported on first start (env SYNCWATCH_CONFIG; default <data>/syncwatch.yaml)")
	level := fs.String("log-level", env("SYNCWATCH_LOG_LEVEL", "info"), "debug, info, warn or error (env SYNCWATCH_LOG_LEVEL)")
	format := fs.String("log-format", env("SYNCWATCH_LOG_FORMAT", "text"), "text or json (env SYNCWATCH_LOG_FORMAT)")
	tlsCert := fs.String("tls-cert", env("SYNCWATCH_TLS_CERT", ""), "serve HTTPS with this certificate file (env SYNCWATCH_TLS_CERT)")
	tlsKey := fs.String("tls-key", env("SYNCWATCH_TLS_KEY", ""), "key file for --tls-cert (env SYNCWATCH_TLS_KEY)")
	tlsSelf := fs.Bool("tls-self-signed", envBool("SYNCWATCH_TLS_SELF_SIGNED"), "serve HTTPS with a self-signed certificate kept in the data directory (env SYNCWATCH_TLS_SELF_SIGNED)")
	tlsHosts := fs.String("tls-hosts", env("SYNCWATCH_TLS_HOSTS", ""), "extra names or IP addresses for the self-signed certificate, comma-separated, e.g. the host's LAN address (env SYNCWATCH_TLS_HOSTS)")
	_ = fs.Parse(args)
	if *cfgFile == "" {
		*cfgFile = *data + string(os.PathSeparator) + "syncwatch.yaml"
	}
	log := newLogger(*level, *format)
	slog.SetDefault(log)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return app.Run(ctx, app.Options{
		DataDir: *data, Listen: *listen, ConfigFile: *cfgFile, SecretKey: secretKey(*key),
		AdminPassword: os.Getenv("SYNCWATCH_ADMIN_PASSWORD"), Version: version, Log: log,
		TLSCert: *tlsCert, TLSKey: *tlsKey, TLSSelfSigned: *tlsSelf, TLSHosts: splitList(*tlsHosts),
	})
}

// runGate serves the read-only gate: it holds Syncthing's API key and
// gives syncwatch a token for the allowlisted GET endpoints only.
func runGate(args []string) error {
	fs := flag.NewFlagSet("gate", flag.ExitOnError)
	data := fs.String("data", env("SYNCWATCH_DATA", defaultDataDir()), "data directory for the gate's token, certificate and Syncthing pin (env SYNCWATCH_DATA)")
	listen := fs.String("listen", env("SYNCWATCH_LISTEN", "127.0.0.1:8385"), "listen address; syncwatch must reach it, e.g. 192.168.1.10:8385 (env SYNCWATCH_LISTEN)")
	syncthing := fs.String("syncthing", env("SYNCWATCH_GATE_SYNCTHING", "https://127.0.0.1:8384"), "Syncthing's GUI/API URL as the gate reaches it (env SYNCWATCH_GATE_SYNCTHING)")
	keyFile := fs.String("syncthing-key-file", env("SYNCWATCH_GATE_SYNCTHING_KEY_FILE", ""), "file holding Syncthing's API key (env SYNCWATCH_GATE_SYNCTHING_KEY_FILE; or set SYNCWATCH_GATE_SYNCTHING_KEY)")
	cfgXML := fs.String("syncthing-config", env("SYNCWATCH_GATE_SYNCTHING_CONFIG", ""), "Syncthing's config.xml, to read the API key from (env SYNCWATCH_GATE_SYNCTHING_CONFIG)")
	tlsCert := fs.String("tls-cert", env("SYNCWATCH_TLS_CERT", ""), "serve with this certificate instead of a self-signed one (env SYNCWATCH_TLS_CERT)")
	tlsKey := fs.String("tls-key", env("SYNCWATCH_TLS_KEY", ""), "key file for --tls-cert (env SYNCWATCH_TLS_KEY)")
	tlsHosts := fs.String("tls-hosts", env("SYNCWATCH_TLS_HOSTS", ""), "extra names or addresses for the self-signed certificate (env SYNCWATCH_TLS_HOSTS)")
	level := fs.String("log-level", env("SYNCWATCH_LOG_LEVEL", "info"), "debug, info, warn or error (env SYNCWATCH_LOG_LEVEL)")
	_ = fs.Parse(args)
	log := newLogger(*level, env("SYNCWATCH_LOG_FORMAT", "text"))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return app.RunGate(ctx, app.GateOptions{
		DataDir: *data, Listen: *listen, Syncthing: *syncthing,
		KeyFile: *keyFile, Key: os.Getenv("SYNCWATCH_GATE_SYNCTHING_KEY"), ConfigXML: *cfgXML,
		Token:   os.Getenv("SYNCWATCH_GATE_TOKEN"),
		TLSCert: *tlsCert, TLSKey: *tlsKey, TLSHosts: splitList(*tlsHosts), Log: log,
	})
}

func runProbe(args []string) error {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	var o probe.Options
	var shares stringList
	fs.StringVar(&o.Name, "name", "server", "server name for the report")
	fs.StringVar(&o.URL, "url", "", "Syncthing GUI/API URL, e.g. https://192.168.3.10:8384")
	fs.StringVar(&o.APIKey, "api-key", os.Getenv("SYNCWATCH_PROBE_API_KEY"), "API key (or env SYNCWATCH_PROBE_API_KEY)")
	fs.StringVar(&o.Out, "out", "docs/api-findings.md", "report file to write (appends a section per server)")
	fs.StringVar(&o.Record, "record", "", "directory to save raw (secret-free) responses as test fixtures")
	fs.DurationVar(&o.EventTimeout, "event-timeout", 50*time.Second, "long-poll timeout to test against the VPN idle timeout")
	fs.DurationVar(&o.EventWait, "event-wait", 2*time.Minute, "how long to listen for events")
	fs.StringVar(&o.Pin, "pin", "", "expected certificate fingerprint (hex SHA-256), if known")
	fs.Var(&shares, "share", "share root to browse (repeatable); default: parents of folder paths")
	fs.StringVar(&o.ProjectPattern, "project-pattern", "", "project_pattern from your structure rules, to estimate the structure scan's load")
	fs.IntVar(&o.ScanDepth, "scan-depth", 3, "nested_scan_depth for the estimate")
	fs.IntVar(&o.ScanBudget, "scan-budget", 20000, "most directory listings the estimate may make (it extrapolates beyond)")
	_ = fs.Parse(args)
	o.Shares = shares
	if o.URL == "" || o.APIKey == "" {
		return fmt.Errorf("probe needs --url and --api-key")
	}
	log := newLogger("info", "text")
	return probe.Run(context.Background(), o, log)
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func resetPassword(args []string) error {
	fs := flag.NewFlagSet("reset-admin-password", flag.ExitOnError)
	data, key := dataFlags(fs)
	_ = fs.Parse(args)
	fmt.Fprint(os.Stderr, "New admin password: ")
	pw, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && pw == "" {
		return fmt.Errorf("reading password: %w", err)
	}
	pw = strings.TrimRight(pw, "\r\n")
	if len([]rune(pw)) < web.MinPasswordLength {
		return fmt.Errorf("the password must be at least %d characters", web.MinPasswordLength)
	}
	st, err := app.OpenStore(*data, secretKey(*key))
	if err != nil {
		return err
	}
	defer st.Close()
	h, err := web.HashPassword(pw)
	if err != nil {
		return err
	}
	if err := st.SetSetting(web.KeyAdminHash, h); err != nil {
		return err
	}
	if err := st.DeleteSessions(""); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Admin password changed; all sessions were logged out.")
	return nil
}

func importConfig(args []string) error {
	fs := flag.NewFlagSet("import-config", flag.ExitOnError)
	data, key := dataFlags(fs)
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: syncwatch import-config [--data DIR] FILE")
	}
	st, err := app.OpenStore(*data, secretKey(*key))
	if err != nil {
		return err
	}
	defer st.Close()
	c, n, err := app.ImportFile(st, fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Imported %d server(s) and %d secret(s). Restart syncwatch to apply.\n", len(c.Servers), n)
	if n > 0 {
		fmt.Fprintf(os.Stderr, "The secrets are stored encrypted now, but %s still contains them in plaintext: delete it.\n", fs.Arg(0))
	}
	if need := settings.NeedKey(st, c); len(need) > 0 {
		fmt.Fprintf(os.Stderr, "Enter the API key again for %s: an API key is only sent to the URL it was entered for.\n", strings.Join(need, ", "))
	}
	return nil
}

func exportConfig(args []string) error {
	fs := flag.NewFlagSet("export-config", flag.ExitOnError)
	data, key := dataFlags(fs)
	_ = fs.Parse(args)
	st, err := app.OpenStore(*data, secretKey(*key))
	if err != nil {
		return err
	}
	defer st.Close()
	c, err := settings.Load(st)
	if err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("no configuration stored yet")
	}
	b, err := c.ExportYAML()
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(b)
	return err
}

// healthURL is where the local server answers /healthz: the listen address
// (loopback if it listens on all interfaces) and http or https as configured.
func healthURL() string {
	scheme := "http"
	if os.Getenv("SYNCWATCH_TLS_CERT") != "" || envBool("SYNCWATCH_TLS_SELF_SIGNED") {
		scheme = "https"
	}
	host, port, err := net.SplitHostPort(env("SYNCWATCH_LISTEN", defaultListen))
	if err != nil {
		host, port = "127.0.0.1", "8080"
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return scheme + "://" + net.JoinHostPort(host, port) + "/healthz"
}

func healthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	url := fs.String("url", env("SYNCWATCH_HEALTHCHECK_URL", healthURL()), "health URL")
	_ = fs.Parse(args)
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		// Only checks that the local server answers; the dashboard's own
		// certificate may well be self-signed.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // local health probe, sends nothing
	}}
	resp, err := c.Get(*url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned HTTP %d", resp.StatusCode)
	}
	return nil
}
