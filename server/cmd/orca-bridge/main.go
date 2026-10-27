// orca-bridge hands Multica tasks to Orca's own agent launcher.
//
// The daemon spawns it through a custom runtime profile whose protocol family
// matches the wrapped CLI (e.g. runtime-type claude). The bridge lifts the
// initial prompt out of the daemon's stdin (skipping SDK control frames such
// as the hooks-init frame the daemon sends first), submits it through
// `orca agent launch`, and then answers the daemon with the minimal
// stream-json sequence (init + result) so the run completes cleanly.
//
// Semantics: the task runs as Orca's own interactive agent session — visible,
// steerable and continuable in Orca. Multica's run records that the task was
// handed over; the work itself lives in Orca. Supplement messages are not
// consumed. If Orca is unavailable the bridge falls back to running the
// wrapped CLI locally.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "orca-bridge:", err)
		os.Exit(3)
	}
}

func run() error {
	args := os.Args[1:]
	if len(args) == 0 {
		return fmt.Errorf("usage: orca-bridge [real-cli] [args...]")
	}
	// The real CLI path is optional in argv (a profile's fixed_args). When
	// absent, resolve it from ORCA_REAL_CLI or PATH — used only by the local
	// fallback and version probes.
	realCLI := ""
	cliArgs := args
	if _, err := os.Stat(args[0]); err == nil && filepath.IsAbs(args[0]) {
		realCLI = args[0]
		cliArgs = args[1:]
	}
	if realCLI == "" {
		if p := os.Getenv("ORCA_REAL_CLI"); p != "" {
			realCLI = p
		} else if p, err := exec.LookPath("claude"); err == nil {
			realCLI = p
		}
	}

	// Version probes are cheap and never user-visible: answer them locally.
	for _, a := range cliArgs {
		if a == "--version" || a == "-v" {
			if realCLI == "" {
				fmt.Fprintln(os.Stdout, "orca-bridge")
				return nil
			}
			return passthroughLocal(realCLI, []string{"--version"})
		}
	}

	// The daemon sends control frames (hooks init) before the user message;
	// lift the user message text and drop the rest of stdin.
	promptText, err := readInitialPrompt(os.Stdin)
	if err != nil {
		return fmt.Errorf("read initial prompt: %w", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve workdir: %w", err)
	}

	orcaCLI, err := findOrcaCLI()
	if err != nil {
		fmt.Fprintln(os.Stderr, "orca-bridge:", err, "— falling back to local execution")
		return passthroughLocal(realCLI, rearguedArgs(cliArgs, promptText))
	}

	anchor := os.Getenv("ORCA_ANCHOR_WORKTREE")
	if anchor == "" {
		anchor = cwd
	}
	agentID := os.Getenv("ORCA_AGENT_ID")
	if agentID == "" {
		agentID = "claude"
	}

	// Register the workdir under the matched project (best effort) so the
	// task surfaces under its repo in Orca.
	if projectID, perr := matchOrcaProject(orcaCLI, cwd); perr == nil {
		setupArgs := []string{
			"project", "setup-existing-folder",
			"--project", projectID,
			"--host", "local",
			"--path", cwd,
			"--kind", "git",
			"--json",
		}
		if out, err := runOrca(orcaCLI, setupArgs); err != nil {
			fmt.Fprintf(os.Stderr, "orca-bridge: setup-existing-folder notice (%v): %s\n", err, firstLine(out))
		}
	}

	// agent launch resolves the executing workspace from the caller's working
	// directory, so the CLI must run from inside the anchor workspace.
	// The launcher can stay attached to the freshly started interactive agent
	// instead of exiting; cap it and treat whatever it printed (or a matching
	// terminal tab) as success — by then the prompt is already submitted.
	launchArgs := []string{
		"agent", "launch",
		"--agent", agentID,
		"--worktree", "path:" + anchor,
		"--prompt", promptText,
		"--json",
	}
	launchOut, launchErr := runOrcaInDirTimeout(anchor, orcaCLI, launchArgs, 30*time.Second)
	if launchErr != nil && strings.TrimSpace(launchOut) == "" {
		fmt.Fprintf(os.Stderr, "orca-bridge: agent launch failed (%v): %s — falling back to local execution\n",
			launchErr, firstLine(launchOut))
		return passthroughLocal(realCLI, rearguedArgs(cliArgs, promptText))
	}

	handle, outcome := parseLaunch(launchOut)
	_ = os.MkdirAll(filepath.Join(cwd, ".orca-bridge"), 0o755)
	_ = os.WriteFile(filepath.Join(cwd, ".orca-bridge", "launch.txt"), []byte(strings.TrimSpace(launchOut)), 0o644)

	// Answer the daemon with the minimal claude stream-json sequence so the
	// run completes: init (carries the Orca handle as session id) + result.
	sessionID := handle
	if sessionID == "" {
		sessionID = "orca-launch"
	}
	fmt.Printf("{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":%s,\"cwd\":%s}\n",
		jsonString(sessionID), jsonString(cwd))
	fmt.Printf("{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":%s,\"session_id\":%s}\n",
		jsonString(fmt.Sprintf("Task handed to Orca agent (%s). Continue and review in Orca.", outcome)), jsonString(sessionID))
	return nil
}

// parseLaunch extracts the terminal handle (when the launch landed on a
// terminal surface) and the outcome kind from agent launch's JSON.
func parseLaunch(out string) (handle, outcome string) {
	var parsed struct {
		Result struct {
			Outcome struct {
				Kind   string `json:"kind"`
				Handle string `json:"handle"`
			} `json:"outcome"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err == nil {
		return parsed.Result.Outcome.Handle, parsed.Result.Outcome.Kind
	}
	return "", "unknown"
}

// jsonString escapes s for embedding inside a JSON string literal.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func hasVersionArg(args []string) bool {
	for _, a := range args {
		if a == "--version" || a == "-v" {
			return true
		}
	}
	return false
}

// rearguedArgs rewrites the daemon's argv for local fallback execution: the
// prompt text replaces the stdin protocol flags.
func rearguedArgs(cliArgs []string, promptText string) []string {
	out := make([]string, 0, len(cliArgs)+2)
	for i := 0; i < len(cliArgs); i++ {
		if cliArgs[i] == "--input-format" && i+1 < len(cliArgs) && cliArgs[i+1] == "stream-json" {
			i++
			continue
		}
		out = append(out, cliArgs[i])
	}
	for i, a := range out {
		if a == "-p" {
			res := append([]string{}, out[:i+1]...)
			res = append(res, promptText)
			res = append(res, out[i+1:]...)
			return ensureVerbose(res)
		}
	}
	return ensureVerbose(append(out, "-p", promptText))
}

// ensureVerbose adds --verbose when stream-json output is requested: claude
// rejects `--print --output-format stream-json` without it.
func ensureVerbose(args []string) []string {
	for i, a := range args {
		if a == "--output-format" && i+1 < len(args) && args[i+1] == "stream-json" {
			for _, v := range args {
				if v == "--verbose" {
					return args
				}
			}
			return append(args, "--verbose")
		}
	}
	return args
}

var lastStdinLine string

// readInitialPrompt consumes stdin lines until the stream-json user message
// (skipping control frames such as the SDK hooks init that the daemon sends
// first) and returns its text content.
func readInitialPrompt(r io.Reader) (string, error) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			for i := bytes.IndexByte(buf, '\n'); i >= 0; i = bytes.IndexByte(buf, '\n') {
				line := string(buf[:i])
				buf = append(buf[:0], buf[i+1:]...)
				if strings.TrimSpace(line) == "" {
					continue
				}
				lastStdinLine = line
				if !strings.Contains(line, `"type":"user"`) && !strings.Contains(line, `"type": "user"`) {
					continue
				}
				return extractPromptText(line)
			}
		}
		if err != nil {
			if len(buf) > 0 {
				lastStdinLine = string(buf)
				return extractPromptText(string(buf))
			}
			return "", fmt.Errorf("stdin closed before user message: %w", err)
		}
	}
}

func extractPromptText(line string) (string, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", fmt.Errorf("empty initial message")
	}
	var msg struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		return line, nil
	}
	content := msg.Message.Content
	if len(content) == 0 {
		content = msg.Content
	}
	if len(content) == 0 {
		return "", fmt.Errorf("initial message has no content")
	}
	var asText string
	if err := json.Unmarshal(content, &asText); err == nil {
		return asText, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &blocks); err == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type == "text" {
				b.WriteString(blk.Text)
			}
		}
		if b.Len() > 0 {
			return b.String(), nil
		}
	}
	return "", fmt.Errorf("unsupported content shape")
}

// passthroughLocal runs the wrapped CLI directly in this process context.
func passthroughLocal(realCLI string, args []string) error {
	if realCLI == "" {
		return fmt.Errorf("no wrapped CLI available for local fallback")
	}
	cmd := exec.Command(realCLI, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// matchOrcaProject resolves the Orca project that owns this workdir by
// matching the workdir's git origin against known project ids.
func matchOrcaProject(orcaCLI, cwd string) (string, error) {
	remote, err := exec.Command("git", "-C", cwd, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", fmt.Errorf("no git origin in workdir")
	}
	full, pathOnly := normalizeRepoURL(string(remote))
	if full == "" {
		return "", fmt.Errorf("unusable origin url")
	}
	out, err := runOrca(orcaCLI, []string{"project", "list", "--json"})
	if err != nil {
		return "", fmt.Errorf("orca project list: %v", err)
	}
	var parsed struct {
		Result struct {
			Projects []struct {
				ID string `json:"id"`
			} `json:"projects"`
		} `json:"result"`
		Projects []struct {
			ID string `json:"id"`
		} `json:"projects"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		return "", fmt.Errorf("parse project list: %v", err)
	}
	projects := parsed.Result.Projects
	if len(projects) == 0 {
		projects = parsed.Projects
	}
	for _, p := range projects {
		id := p.ID
		colon := strings.IndexByte(id, ':')
		if colon <= 0 {
			continue
		}
		kind, rest := id[:colon], id[colon+1:]
		if kind == "repo" {
			continue
		}
		restNorm := strings.ToLower(strings.Trim(rest, "/"))
		if restNorm == full || restNorm == pathOnly {
			return id, nil
		}
	}
	return "", fmt.Errorf("no Orca project matches origin %s", full)
}

// normalizeRepoURL reduces any git remote form to "host/path" (full) and
// "path" (path-only, host stripped), lowercased, without scheme, user,
// trailing .git or slashes.
func normalizeRepoURL(url string) (full, pathOnly string) {
	u := strings.TrimSpace(url)
	u = strings.TrimSuffix(u, ".git")
	lower := strings.ToLower(u)
	for _, scheme := range []string{"http://", "https://", "ssh://", "git://"} {
		if i := strings.Index(lower, scheme); i >= 0 {
			u = u[i+len(scheme):]
			lower = lower[i+len(scheme):]
		}
	}
	if i := strings.Index(lower, "@"); i >= 0 {
		u = u[i+1:]
		lower = lower[i+1:]
	}
	if i := strings.Index(lower, ":"); i >= 0 && !strings.Contains(lower[:i], "/") {
		u = u[:i] + "/" + u[i+1:]
		lower = strings.Replace(lower, ":", "/", 1)
	}
	u = strings.Trim(u, "/")
	lower = strings.Trim(lower, "/")
	full = lower
	if slash := strings.Index(lower, "/"); slash >= 0 {
		pathOnly = lower[slash+1:]
	}
	return full, pathOnly
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

func terminalTitle() string {
	agent := os.Getenv("MULTICA_AGENT_NAME")
	task := os.Getenv("MULTICA_TASK_ID")
	cwd, _ := os.Getwd()
	short := filepath.Base(cwd)
	parts := []string{"Multica"}
	if agent != "" {
		parts = append(parts, agent)
	}
	if task != "" {
		if len(task) > 8 {
			task = task[:8]
		}
		parts = append(parts, task)
	}
	if short != "." && short != "" {
		parts = append(parts, short)
	}
	return strings.Join(parts, " | ")
}

func findOrcaCLI() (string, error) {
	if p := os.Getenv("ORCA_CLI"); p != "" {
		return p, nil
	}
	if p, err := exec.LookPath("orca.exe"); err == nil {
		return p, nil
	}
	if p, err := exec.LookPath("orca"); err == nil {
		return p, nil
	}
	local := os.Getenv("LOCALAPPDATA")
	if local != "" {
		c := filepath.Join(local, "Programs", "orca", "resources", "bin", "orca.exe")
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("orca CLI not found (set ORCA_CLI or install Orca)")
}

func runOrca(orcaCLI string, args []string) (string, error) {
	return runOrcaInDir("", orcaCLI, args)
}

func runOrcaInDir(dir, orcaCLI string, args []string) (string, error) {
	return runOrcaInDirTimeout(dir, orcaCLI, args, 0)
}

func runOrcaInDirTimeout(dir, orcaCLI string, args []string, timeout time.Duration) (string, error) {
	ctx := context.Background()
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, orcaCLI, args...)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "ORCA_NO_COLOR=1")
	err := cmd.Run()
	if cancel != nil {
		cancel()
	}
	if err != nil && out.Len() == 0 {
		return errOut.String(), err
	}
	return out.String(), err
}
