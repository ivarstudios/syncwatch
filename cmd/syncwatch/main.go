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
	"strings"
	"syscall"
	"time"

	_ "time/tzdata" // time zones in minimal containers

	"github.com/ivarstudios/syncwatch/internal/app"
	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/probe"
	"github.com/ivarstudios/syncwatch/internal/settings"
	"github.com/ivarstudios/syncwatch/internal/stclient"
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

// envBool reads a yes/no variable: true/false, 1/0, yes/no or on/off in any
// case; unset means false. Anything else is an error naming the variable, so
// a misspelt SYNCWATCH_TLS_SELF_SIGNED can't silently mean plain HTTP.
func envBool(key string) (bool, error) {
	v := os.Getenv(key)
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "t", "true", "yes", "on":
		return true, nil
	case "", "0", "f", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("%s must be true or false, not %q", key, v)
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
	fmt.Fprintf(os.Stderr, `syncwatch %s — read-only dashboard, weekly report and urgent messages for Syncthing

Usage:
  syncwatch [serve] [flags]          run the monitor (default)
  syncwatch probe [flags]            test the API of a server (writes a probe report)
  syncwatch gate [flags]             read-only gate next to a server (keeps its API key, see README)
  syncwatch reset-admin-password     set a new administrator password from stdin
  syncwatch import-config FILE       replace the configuration with a YAML file
  syncwatch export-config            print the configuration as YAML (no secrets)
  syncwatch healthcheck              exit 0 if syncwatch on this host answers /healthz
  syncwatch version

Run "syncwatch <command> -h" for the flags of a command.
`, version)
}

// command picks the subcommand: the first argument, or serve when it is a
// flag. --version and -h/--help work without a command.
func command(args []string) (string, []string) {
	if len(args) == 0 {
		return "serve", nil
	}
	switch args[0] {
	case "-version", "--version":
		return "version", args[1:]
	case "-h", "-help", "--help":
		return "help", args[1:]
	}
	if strings.HasPrefix(args[0], "-") {
		return "serve", args
	}
	return args[0], args[1:]
}

func main() {
	cmd, args := command(os.Args[1:])
	// Test setups with a fake Discord; never needed in production.
	anyWebhook, err := envBool("SYNCWATCH_ALLOW_ANY_WEBHOOK")
	if err != nil {
		fmt.Fprintln(os.Stderr, "syncwatch:", err)
		os.Exit(1)
	}
	config.AllowAnyWebhook = anyWebhook
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
	case "version":
		fmt.Println("syncwatch", version)
	case "help":
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
	key := fs.String("secret-key", "", "secret key (env SYNCWATCH_SECRET_KEY, default <data>/secret.key)")
	return data, key
}

func secretKey(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv("SYNCWATCH_SECRET_KEY")
}

func serve(args []string) error {
	selfSigned, err := envBool("SYNCWATCH_TLS_SELF_SIGNED")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	data, key := dataFlags(fs)
	listen := fs.String("listen", env("SYNCWATCH_LISTEN", defaultListen), "listen address, for example 192.168.1.10:8080 (env SYNCWATCH_LISTEN, default loopback only)")
	cfgFile := fs.String("config", env("SYNCWATCH_CONFIG", ""), "bootstrap file that syncwatch imports on first start (env SYNCWATCH_CONFIG, default <data>/syncwatch.yaml)")
	level := fs.String("log-level", env("SYNCWATCH_LOG_LEVEL", "info"), "debug, info, warn or error (env SYNCWATCH_LOG_LEVEL)")
	format := fs.String("log-format", env("SYNCWATCH_LOG_FORMAT", "text"), "text or json (env SYNCWATCH_LOG_FORMAT)")
	tlsCert := fs.String("tls-cert", env("SYNCWATCH_TLS_CERT", ""), "serve HTTPS with this certificate file (env SYNCWATCH_TLS_CERT)")
	tlsKey := fs.String("tls-key", env("SYNCWATCH_TLS_KEY", ""), "private key file for --tls-cert (env SYNCWATCH_TLS_KEY)")
	tlsSelf := fs.Bool("tls-self-signed", selfSigned, "serve HTTPS with a self-signed certificate in the data directory (env SYNCWATCH_TLS_SELF_SIGNED)")
	tlsHosts := fs.String("tls-hosts", env("SYNCWATCH_TLS_HOSTS", ""), "more names or IP addresses for the self-signed certificate, comma-separated, for example the LAN address (env SYNCWATCH_TLS_HOSTS)")
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
	o, level := gateOptions(args)
	o.Log = newLogger(level, env("SYNCWATCH_LOG_FORMAT", "text"))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return app.RunGate(ctx, o)
}

// gateOptions parses the gate's flags. The gate has its own listen
// variable: the image sets SYNCWATCH_LISTEN for serve, and a gate
// container must not inherit it.
func gateOptions(args []string) (o app.GateOptions, level string) {
	fs := flag.NewFlagSet("gate", flag.ExitOnError)
	data := fs.String("data", env("SYNCWATCH_DATA", defaultDataDir()), "data directory for the gate token, certificate and pinned Syncthing fingerprint (env SYNCWATCH_DATA)")
	listen := fs.String("listen", env("SYNCWATCH_GATE_LISTEN", "127.0.0.1:8385"), "listen address that syncwatch can reach, for example 192.168.1.10:8385 (env SYNCWATCH_GATE_LISTEN)")
	syncthing := fs.String("syncthing", env("SYNCWATCH_GATE_SYNCTHING", "https://127.0.0.1:8384"), "API URL of Syncthing as the gate reaches it (env SYNCWATCH_GATE_SYNCTHING)")
	keyFile := fs.String("syncthing-key-file", env("SYNCWATCH_GATE_SYNCTHING_KEY_FILE", ""), "file with the Syncthing API key (env SYNCWATCH_GATE_SYNCTHING_KEY_FILE, or the key in SYNCWATCH_GATE_SYNCTHING_KEY)")
	cfgXML := fs.String("syncthing-config", env("SYNCWATCH_GATE_SYNCTHING_CONFIG", ""), "Syncthing config.xml to read the API key from (env SYNCWATCH_GATE_SYNCTHING_CONFIG)")
	roots := fs.String("browse-roots", env("SYNCWATCH_GATE_BROWSE_ROOTS", ""), "more directories that the gate token can list, comma-separated (env SYNCWATCH_GATE_BROWSE_ROOTS, default the parent directories of the Syncthing folders)")
	tlsCert := fs.String("tls-cert", env("SYNCWATCH_TLS_CERT", ""), "serve with this certificate instead of a self-signed one (env SYNCWATCH_TLS_CERT)")
	tlsKey := fs.String("tls-key", env("SYNCWATCH_TLS_KEY", ""), "private key file for --tls-cert (env SYNCWATCH_TLS_KEY)")
	tlsHosts := fs.String("tls-hosts", env("SYNCWATCH_TLS_HOSTS", ""), "more names or IP addresses for the self-signed certificate, comma-separated (env SYNCWATCH_TLS_HOSTS)")
	lvl := fs.String("log-level", env("SYNCWATCH_LOG_LEVEL", "info"), "debug, info, warn or error (env SYNCWATCH_LOG_LEVEL)")
	_ = fs.Parse(args)
	return app.GateOptions{
		DataDir: *data, Listen: *listen, Syncthing: *syncthing,
		KeyFile: *keyFile, Key: os.Getenv("SYNCWATCH_GATE_SYNCTHING_KEY"), ConfigXML: *cfgXML,
		Token: os.Getenv("SYNCWATCH_GATE_TOKEN"), BrowseRoots: splitList(*roots),
		TLSCert: *tlsCert, TLSKey: *tlsKey, TLSHosts: splitList(*tlsHosts),
	}, *lvl
}

func runProbe(args []string) error {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	var o probe.Options
	var shares stringList
	fs.StringVar(&o.Name, "name", "server", "server name for the probe report")
	fs.StringVar(&o.URL, "url", "", "API URL of the server, for example https://192.168.3.10:8384")
	fs.StringVar(&o.APIKey, "api-key", os.Getenv("SYNCWATCH_PROBE_API_KEY"), "API key of the server (env SYNCWATCH_PROBE_API_KEY)")
	fs.StringVar(&o.Out, "out", "docs/api-findings.md", "report file to write (the probe appends a section for each server)")
	fs.StringVar(&o.Record, "record", "", "directory to save the raw API responses (without secrets) as test fixtures")
	fs.DurationVar(&o.EventTimeout, "event-timeout", 50*time.Second, "long-poll timeout to test against the VPN idle timeout")
	fs.DurationVar(&o.EventWait, "event-wait", 2*time.Minute, "how long to listen for events")
	fs.StringVar(&o.Pin, "pin", "", "certificate fingerprint to accept (hex SHA-256, upper case and colons permitted), if known")
	fs.Var(&shares, "share", "share root to browse, repeatable (default: the parent directories of the folder paths)")
	fs.StringVar(&o.ProjectPattern, "project-pattern", "", "project_pattern from your structure rules, to estimate the load of the structure scan")
	fs.IntVar(&o.ScanDepth, "scan-depth", 3, "nested_scan_depth for the estimate")
	fs.IntVar(&o.ScanBudget, "scan-budget", 20000, "limit of directory listings for the estimate (the probe calculates the rest)")
	_ = fs.Parse(args)
	o.Shares = shares
	if o.URL == "" || o.APIKey == "" {
		return fmt.Errorf("the probe needs --url and --api-key")
	}
	pin, err := stclient.NormalizePin(o.Pin)
	if err != nil {
		return fmt.Errorf("--pin: %w", err)
	}
	o.Pin = pin
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
	// Before asking for a password: a wrong --data is reported at once.
	st, err := app.OpenExistingStore(*data, secretKey(*key))
	if err != nil {
		return err
	}
	defer st.Close()
	fmt.Fprint(os.Stderr, "New administrator password: ")
	pw, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && pw == "" {
		return fmt.Errorf("cannot read the password: %w", err)
	}
	pw = strings.TrimRight(pw, "\r\n")
	if len([]rune(pw)) < web.MinPasswordLength {
		return fmt.Errorf("the password must have at least %d characters", web.MinPasswordLength)
	}
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
	// The server that runs sends a settings notice (a server that does not
	// run sends it when it starts).
	if err := app.RecordPasswordReset(st); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "The new administrator password is set. syncwatch logged out all sessions.")
	return nil
}

func importConfig(args []string) error {
	fs := flag.NewFlagSet("import-config", flag.ExitOnError)
	data, key := dataFlags(fs)
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: syncwatch import-config [--data DIR] FILE")
	}
	st, err := app.OpenExistingStore(*data, secretKey(*key))
	if err != nil {
		return err
	}
	defer st.Close()
	c, n, err := app.ImportFile(st, fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "syncwatch imported %s and %s. A syncwatch that runs uses the new configuration in less than 10 seconds. If syncwatch does not run, it uses the new configuration on the next start.\n", plural(len(c.Servers), "server", "servers"), plural(n, "secret", "secrets"))
	if n > 0 {
		fmt.Fprintf(os.Stderr, "syncwatch stores the secrets encrypted. The file %s still contains them unencrypted. Delete the file.\n", fs.Arg(0))
	}
	if need := settings.NeedKey(st, c); len(need) > 0 {
		fmt.Fprintf(os.Stderr, "Type the API key again for %s. syncwatch sends an API key only to the URL that you typed it for.\n", strings.Join(need, ", "))
	}
	return nil
}

func exportConfig(args []string) error {
	fs := flag.NewFlagSet("export-config", flag.ExitOnError)
	data, key := dataFlags(fs)
	_ = fs.Parse(args)
	st, err := app.OpenExistingStore(*data, secretKey(*key))
	if err != nil {
		return err
	}
	defer st.Close()
	c, err := settings.Load(st)
	if err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("there is no stored configuration")
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
	// A bad value stops serve itself, so the check can't pass either way.
	if selfSigned, _ := envBool("SYNCWATCH_TLS_SELF_SIGNED"); os.Getenv("SYNCWATCH_TLS_CERT") != "" || selfSigned {
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
		return fmt.Errorf("the health check returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// plural writes a count with its noun: "1 server", "3 servers".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
