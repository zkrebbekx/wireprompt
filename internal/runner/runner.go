// Package runner wraps arbitrary commands so their LLM traffic flows through
// the local wireprompt proxy, by injecting provider base-URL environment
// variables pointing at the proxy with a session id encoded in the path.
package runner

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// Env returns the environment overrides that route a child process's LLM
// traffic through the proxy at addr (host:port) under the given session.
func Env(addr, session string) []string {
	base := fmt.Sprintf("http://%s/s/%s", addr, session)
	return []string{
		// Anthropic SDKs append /v1/... to the base URL.
		"ANTHROPIC_BASE_URL=" + base + "/anthropic",
		// OpenAI SDKs treat the base URL as including /v1.
		"OPENAI_BASE_URL=" + base + "/openai/v1",
		"OPENAI_API_BASE=" + base + "/openai/v1",
		// Gemini CLI and google-genai SDKs.
		"GOOGLE_GEMINI_BASE_URL=" + base + "/gemini",
	}
}

// DefaultSession derives a readable session id from the wrapped command.
func DefaultSession(command string) string {
	return fmt.Sprintf("%s-%s", filepath.Base(command), time.Now().Format("0102-150405"))
}

// Run executes the command with proxy env injected, wiring the current
// process's stdio through, and returns the child's exit code. SIGINT is
// delivered to the child by the terminal's process group; SIGTERM is
// forwarded explicitly so wrapped commands shut down cleanly.
func Run(addr, session string, command string, args []string) (int, error) {
	cmd := exec.Command(command, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), Env(addr, session)...)
	if err := cmd.Start(); err != nil {
		return 1, err
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM)
	signal.Ignore(os.Interrupt) // child receives SIGINT from the terminal; parent survives to report
	defer signal.Reset(os.Interrupt)
	defer signal.Stop(sigs)
	go func() {
		for s := range sigs {
			cmd.Process.Signal(s)
		}
	}()

	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), nil
		}
		return 1, err
	}
	return 0, nil
}
