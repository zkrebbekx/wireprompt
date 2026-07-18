// Command wireprompt is a local recording proxy for LLM API traffic — a
// Wireshark for your AI tools. See README.md for usage.
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/zkrebbekx/wireprompt/internal/api"
	"github.com/zkrebbekx/wireprompt/internal/capture"
	"github.com/zkrebbekx/wireprompt/internal/config"
	"github.com/zkrebbekx/wireprompt/internal/demo"
	"github.com/zkrebbekx/wireprompt/internal/pricing"
	"github.com/zkrebbekx/wireprompt/internal/runner"
	"github.com/zkrebbekx/wireprompt/internal/store"
)

// version is stamped by goreleaser via -ldflags "-X main.version=...".
var version = "dev"

const usage = `wireprompt — see every prompt, token and dollar your AI tools spend

Usage:
  wireprompt serve  [-addr 127.0.0.1:9091] [-db PATH] [-token T] [-no-bodies] [-upstream name=url ...]
  wireprompt run    [-addr 127.0.0.1:9091] [-session NAME] -- <command> [args...]
  wireprompt stats  [-db PATH] [-by model|session|day] [-since 24h]
  wireprompt search [-db PATH] [-limit N] <query>
  wireprompt prune  [-db PATH] -older-than 720h [-bodies-only]
  wireprompt env    [-addr 127.0.0.1:9091] [-session NAME]
  wireprompt demo   [-db PATH]
  wireprompt version

serve   start the recording proxy + web UI
run     wrap a command so its LLM traffic is captured (starts serve if needed)
stats   print cost/token rollups from the local database
search  full-text search across captured prompts and responses
prune   delete (or strip bodies from) records older than a duration
env     print shell exports for routing a tool through the proxy manually
demo    load a sample agent session so the UI has something to show

Config file: ~/.config/wireprompt/config.json (flags override).
`

func main() {
	if len(os.Args) < 2 {
		// Bare `wireprompt` = the first-run path: start serving and open
		// the UI so install-to-aha is one word.
		openUISoon()
		if err := cmdServe(nil); err != nil {
			fmt.Fprintln(os.Stderr, "wireprompt:", err)
			os.Exit(1)
		}
		return
	}
	code, err := dispatch(os.Args[1], os.Args[2:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "wireprompt:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

// openUISoon opens the web UI in the default browser once the server is up.
func openUISoon() {
	go func() {
		time.Sleep(700 * time.Millisecond)
		url := "http://127.0.0.1:9091/"
		var cmd *exec.Cmd
		switch goruntime.GOOS {
		case "darwin":
			cmd = exec.Command("open", url)
		case "windows":
			cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
		default:
			cmd = exec.Command("xdg-open", url)
		}
		cmd.Run() // best effort
	}()
}

func dispatch(cmd string, args []string) (int, error) {
	switch cmd {
	case "serve":
		return 0, cmdServe(args)
	case "run":
		return cmdRun(args)
	case "stats":
		return 0, cmdStats(args)
	case "search":
		return 0, cmdSearch(args)
	case "prune":
		return 0, cmdPrune(args)
	case "env":
		return 0, cmdEnv(args)
	case "demo":
		return 0, cmdDemo(args)
	case "version":
		fmt.Println("wireprompt", version)
		return 0, nil
	case "-h", "--help", "help":
		fmt.Print(usage)
		return 0, nil
	}
	fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
	return 2, nil
}

type upstreamFlags map[string]string

func (u upstreamFlags) String() string { return "" }
func (u upstreamFlags) Set(v string) error {
	name, url, ok := strings.Cut(v, "=")
	if !ok {
		return fmt.Errorf("expected name=url, got %q", v)
	}
	u[name] = url
	return nil
}

// resolve merges config-file values with flag values (flags win).
func resolve(cfg *config.Config, addr, db, token string, noBodies bool, extra map[string]string) *config.Config {
	out := *cfg
	if addr != "" {
		out.Addr = addr
	}
	if out.Addr == "" {
		out.Addr = "127.0.0.1:9091"
	}
	if db != "" {
		out.DB = db
	}
	if token != "" {
		out.Token = token
	}
	if noBodies {
		out.NoBodies = true
	}
	if len(extra) > 0 {
		if out.Upstreams == nil {
			out.Upstreams = map[string]string{}
		}
		for k, v := range extra {
			out.Upstreams[k] = v
		}
	}
	return &out
}

func newServer(cfg *config.Config) (http.Handler, *store.Store, error) {
	dbPath := cfg.DB
	if dbPath == "" {
		var err error
		dbPath, err = store.DefaultPath()
		if err != nil {
			return nil, nil, err
		}
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return nil, nil, err
	}
	if cfg.RetentionDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -cfg.RetentionDays)
		// No VACUUM on the startup path — it blocks the database.
		if _, err := st.Prune(cutoff, false, false); err != nil {
			fmt.Fprintf(os.Stderr, "wireprompt: retention prune failed: %v\n", err)
		}
	}
	table, err := pricing.Load()
	if err != nil {
		st.Close()
		return nil, nil, err
	}
	routes, err := capture.DefaultRoutes(cfg.Upstreams)
	if err != nil {
		st.Close()
		return nil, nil, err
	}
	feed := api.NewFeed()
	proxy := capture.New(st, table, cfg, routes, feed.Publish)

	mux := http.NewServeMux()
	api.New(st, feed, proxy).Register(mux)
	mux.Handle("/", proxy)
	return api.Secure(mux, cfg.Token), st, nil
}

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func listen(cfg *config.Config) (net.Listener, http.Handler, *store.Store, error) {
	if !isLoopbackAddr(cfg.Addr) && cfg.Token == "" {
		return nil, nil, nil, fmt.Errorf(
			"refusing to bind %s without a token — the API exposes your prompt history; set -token or bind 127.0.0.1", cfg.Addr)
	}
	handler, st, err := newServer(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		st.Close()
		return nil, nil, nil, err
	}
	return ln, handler, st, nil
}

func serveOn(ln net.Listener, handler http.Handler) *http.Server {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: SSE streams are long-lived by design.
	}
	go srv.Serve(ln)
	return srv
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "", "listen address (default 127.0.0.1:9091)")
	dbPath := fs.String("db", "", "database path (default ~/.wireprompt/wireprompt.db)")
	token := fs.String("token", "", "require this token on all HTTP access (mandatory for non-loopback binds)")
	noBodies := fs.Bool("no-bodies", false, "never store request/response bodies")
	extra := upstreamFlags{}
	fs.Var(extra, "upstream", "extra openai-compatible upstream as name=url (repeatable)")
	fs.Parse(args)

	fileCfg, err := config.Load()
	if err != nil {
		return err
	}
	cfg := resolve(fileCfg, *addr, *dbPath, *token, *noBodies, extra)
	ln, handler, st, err := listen(cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	base := "http://" + displayAddr(cfg.Addr)
	fmt.Printf("wireprompt %s listening on %s\n", version, cfg.Addr)
	fmt.Printf("  ui:        %s/\n", base)
	fmt.Printf("  anthropic: %s/anthropic\n", base)
	fmt.Printf("  openai:    %s/openai/v1\n", base)
	fmt.Printf("  gemini:    %s/gemini\n", base)
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	return srv.Serve(ln)
}

// displayAddr turns a bind address into something clickable.
func displayAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return net.JoinHostPort(host, port)
}

func cmdRun(args []string) (int, error) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	addr := fs.String("addr", "", "proxy address (default 127.0.0.1:9091)")
	session := fs.String("session", "", "session name (default: command name + timestamp)")
	dbPath := fs.String("db", "", "database path (default ~/.wireprompt/wireprompt.db)")
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return 1, fmt.Errorf("run: no command given; usage: wireprompt run -- <command> [args...]")
	}
	if *session == "" {
		*session = runner.DefaultSession(rest[0])
	}

	fileCfg, err := config.Load()
	if err != nil {
		return 1, err
	}
	cfg := resolve(fileCfg, *addr, *dbPath, "", false, nil)

	// Reuse a running server if one is up; otherwise start one in-process.
	// The listen itself is the race-free claim on the port: if another run
	// grabbed it between the health check and here, fall back to using it.
	if !healthy(cfg.Addr, cfg.Token) {
		ln, handler, st, err := listen(cfg)
		if err == nil {
			defer st.Close()
			serveOn(ln, handler)
			fmt.Fprintf(os.Stderr, "wireprompt: started proxy on %s (ui: http://%s/)\n", cfg.Addr, displayAddr(cfg.Addr))
		} else if !healthy(cfg.Addr, cfg.Token) {
			return 1, err
		}
	}

	fmt.Fprintf(os.Stderr, "wireprompt: session %q → http://%s/\n", *session, displayAddr(cfg.Addr))
	return runner.Run(cfg.Addr, *session, rest[0], rest[1:])
}

func healthy(addr, token string) bool {
	c := http.Client{Timeout: 500 * time.Millisecond}
	req, err := http.NewRequest("GET", "http://"+addr+"/api/health", nil)
	if err != nil {
		return false
	}
	if token != "" {
		req.Header.Set("X-Wireprompt-Token", token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func openStore(dbPath string) (*store.Store, error) {
	if dbPath == "" {
		var err error
		dbPath, err = store.DefaultPath()
		if err != nil {
			return nil, err
		}
	}
	return store.Open(dbPath)
}

func cmdStats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	dbPath := fs.String("db", "", "database path")
	by := fs.String("by", "model", "group by: model, session or day")
	since := fs.Duration("since", 0, "only include requests newer than this (e.g. 24h; 0 = all)")
	fs.Parse(args)

	st, err := openStore(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	var sinceT time.Time
	if *since > 0 {
		sinceT = time.Now().Add(-*since)
	}
	rows, err := st.Stats(*by, sinceT)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("no captured requests yet — try: wireprompt run -- <your ai tool>")
		return nil
	}
	fmt.Printf("%-42s %8s %12s %12s %12s %10s %10s\n", strings.ToUpper(*by), "REQS", "INPUT", "OUTPUT", "CACHED", "COST", "SAVED")
	var tot store.StatRow
	for _, r := range rows {
		key := r.Key
		if r.Unpriced > 0 {
			key += " *"
		}
		fmt.Printf("%-42s %8d %12d %12d %12d %10s %10s\n", key, r.Requests,
			r.InputTokens, r.OutputTokens, r.CacheReadTokens+r.CacheWriteTokens,
			usd(r.CostUSD), usd(r.SavedUSD))
		tot.Requests += r.Requests
		tot.InputTokens += r.InputTokens
		tot.OutputTokens += r.OutputTokens
		tot.CacheReadTokens += r.CacheReadTokens + r.CacheWriteTokens
		tot.CostUSD += r.CostUSD
		tot.SavedUSD += r.SavedUSD
		tot.Unpriced += r.Unpriced
	}
	fmt.Printf("%-42s %8d %12d %12d %12d %10s %10s\n", "TOTAL", tot.Requests,
		tot.InputTokens, tot.OutputTokens, tot.CacheReadTokens, usd(tot.CostUSD), usd(tot.SavedUSD))
	if tot.Unpriced > 0 {
		fmt.Printf("\n* %d request(s) used models missing from the pricing table (counted as $0)\n", tot.Unpriced)
	}
	return nil
}

func cmdSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	dbPath := fs.String("db", "", "database path")
	limit := fs.Int("limit", 25, "max results")
	fs.Parse(args)
	query := strings.Join(fs.Args(), " ")
	if query == "" {
		return fmt.Errorf("search: no query given")
	}
	st, err := openStore(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	recs, err := st.List(store.ListOptions{Query: query, Limit: *limit})
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		fmt.Println("no matches")
		return nil
	}
	for _, r := range recs {
		fmt.Printf("#%-6d %s  %-14s %-30s %3d  %s\n", r.ID,
			r.StartedAt.Local().Format("2006-01-02 15:04:05"), r.Session, r.Model,
			r.Status, usd(r.CostUSD))
	}
	return nil
}

func cmdPrune(args []string) error {
	fs := flag.NewFlagSet("prune", flag.ExitOnError)
	dbPath := fs.String("db", "", "database path")
	olderThan := fs.Duration("older-than", 0, "delete records older than this (required, e.g. 720h)")
	bodiesOnly := fs.Bool("bodies-only", false, "keep records but strip stored bodies")
	fs.Parse(args)
	if *olderThan <= 0 {
		return fmt.Errorf("prune: -older-than is required (e.g. -older-than 720h)")
	}
	st, err := openStore(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	n, err := st.Prune(time.Now().Add(-*olderThan), *bodiesOnly, true)
	if err != nil {
		return err
	}
	verb := "deleted"
	if *bodiesOnly {
		verb = "stripped bodies from"
	}
	fmt.Printf("%s %d record(s)\n", verb, n)
	return nil
}

func cmdEnv(args []string) error {
	fs := flag.NewFlagSet("env", flag.ExitOnError)
	addr := fs.String("addr", "", "proxy address (default from config, then 127.0.0.1:9091)")
	session := fs.String("session", "default", "session name")
	fs.Parse(args)
	fileCfg, err := config.Load()
	if err != nil {
		return err
	}
	cfg := resolve(fileCfg, *addr, "", "", false, nil)
	for _, kv := range runner.Env(cfg.Addr, *session) {
		fmt.Println("export " + kv)
	}
	return nil
}

func cmdDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	dbPath := fs.String("db", "", "database path")
	fs.Parse(args)
	st, err := openStore(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	n, err := demo.Seed(st)
	if err != nil {
		return err
	}
	fmt.Printf("seeded %d demo requests in session %q — open the UI to explore\n", n, demo.Session)
	return nil
}

func usd(v float64) string {
	if v >= 0.1 {
		return fmt.Sprintf("$%.2f", v)
	}
	return fmt.Sprintf("$%.4f", v)
}
