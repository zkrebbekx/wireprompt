// Command wireprompt is a local recording proxy for LLM API traffic — a
// Wireshark for your AI tools. See README.md for usage.
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/zkrebbekx/wireprompt/internal/api"
	"github.com/zkrebbekx/wireprompt/internal/capture"
	"github.com/zkrebbekx/wireprompt/internal/pricing"
	"github.com/zkrebbekx/wireprompt/internal/runner"
	"github.com/zkrebbekx/wireprompt/internal/store"
)

var version = "0.1.0-dev"

const usage = `wireprompt — see every prompt, token and dollar your AI tools spend

Usage:
  wireprompt serve [-addr :9091] [-db PATH] [-upstream name=url ...]
  wireprompt run   [-addr 127.0.0.1:9091] [-session NAME] -- <command> [args...]
  wireprompt stats [-db PATH] [-by model|session|day] [-since 24h]
  wireprompt version

serve   start the recording proxy + web UI
run     wrap a command so its LLM traffic is captured (starts serve if needed)
stats   print cost/token rollups from the local database
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "stats":
		err = cmdStats(os.Args[2:])
	case "version":
		fmt.Println("wireprompt", version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "wireprompt:", err)
		os.Exit(1)
	}
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

func newServer(dbPath string, extra map[string]string) (http.Handler, *store.Store, error) {
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
	table, err := pricing.Load()
	if err != nil {
		st.Close()
		return nil, nil, err
	}
	routes, err := capture.DefaultRoutes(extra)
	if err != nil {
		st.Close()
		return nil, nil, err
	}
	feed := api.NewFeed()
	proxy := capture.New(st, table, routes, feed.Publish)

	mux := http.NewServeMux()
	api.New(st, feed).Register(mux)
	mux.Handle("/", proxy)
	return mux, st, nil
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":9091", "listen address")
	dbPath := fs.String("db", "", "database path (default ~/.wireprompt/wireprompt.db)")
	extra := upstreamFlags{}
	fs.Var(extra, "upstream", "extra openai-compatible upstream as name=url (repeatable)")
	fs.Parse(args)

	handler, st, err := newServer(*dbPath, extra)
	if err != nil {
		return err
	}
	defer st.Close()
	fmt.Printf("wireprompt %s listening on %s\n", version, *addr)
	fmt.Printf("  ui:        http://localhost%s/\n", uiSuffix(*addr))
	fmt.Printf("  anthropic: http://localhost%s/anthropic\n", uiSuffix(*addr))
	fmt.Printf("  openai:    http://localhost%s/openai/v1\n", uiSuffix(*addr))
	return http.ListenAndServe(*addr, handler)
}

func uiSuffix(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return addr
	}
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return ":" + port
	}
	return addr
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:9091", "proxy address")
	session := fs.String("session", "", "session name (default: command name + timestamp)")
	dbPath := fs.String("db", "", "database path (default ~/.wireprompt/wireprompt.db)")
	fs.Parse(args)

	rest := fs.Args()
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return fmt.Errorf("run: no command given; usage: wireprompt run -- <command> [args...]")
	}
	if *session == "" {
		*session = runner.DefaultSession(rest[0])
	}

	// Reuse a running server if one is up; otherwise start one in-process.
	if !healthy(*addr) {
		handler, st, err := newServer(*dbPath, nil)
		if err != nil {
			return err
		}
		defer st.Close()
		ln, err := net.Listen("tcp", *addr)
		if err != nil {
			return fmt.Errorf("listen %s: %w", *addr, err)
		}
		go http.Serve(ln, handler)
		fmt.Fprintf(os.Stderr, "wireprompt: started proxy on %s (ui: http://%s/)\n", *addr, *addr)
	}

	fmt.Fprintf(os.Stderr, "wireprompt: session %q → http://%s/\n", *session, *addr)
	code, err := runner.Run(*addr, *session, rest[0], rest[1:])
	if err != nil {
		return err
	}
	os.Exit(code)
	return nil
}

func healthy(addr string) bool {
	c := http.Client{Timeout: 500 * time.Millisecond}
	resp, err := c.Get("http://" + addr + "/api/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func cmdStats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	dbPath := fs.String("db", "", "database path (default ~/.wireprompt/wireprompt.db)")
	by := fs.String("by", "model", "group by: model, session or day")
	since := fs.Duration("since", 0, "only include requests newer than this (e.g. 24h; 0 = all)")
	fs.Parse(args)

	if *dbPath == "" {
		var err error
		*dbPath, err = store.DefaultPath()
		if err != nil {
			return err
		}
	}
	st, err := store.Open(*dbPath)
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
	fmt.Printf("%-42s %8s %12s %12s %12s %10s\n", strings.ToUpper(*by), "REQS", "INPUT", "OUTPUT", "CACHED", "COST")
	var tot store.StatRow
	for _, r := range rows {
		fmt.Printf("%-42s %8d %12d %12d %12d %10s\n", r.Key, r.Requests,
			r.InputTokens, r.OutputTokens, r.CacheReadTokens+r.CacheWriteTokens, usd(r.CostUSD))
		tot.Requests += r.Requests
		tot.InputTokens += r.InputTokens
		tot.OutputTokens += r.OutputTokens
		tot.CacheReadTokens += r.CacheReadTokens + r.CacheWriteTokens
		tot.CostUSD += r.CostUSD
	}
	fmt.Printf("%-42s %8d %12d %12d %12d %10s\n", "TOTAL", tot.Requests,
		tot.InputTokens, tot.OutputTokens, tot.CacheReadTokens, usd(tot.CostUSD))
	return nil
}

func usd(v float64) string {
	if v >= 0.1 {
		return fmt.Sprintf("$%.2f", v)
	}
	return fmt.Sprintf("$%.4f", v)
}
