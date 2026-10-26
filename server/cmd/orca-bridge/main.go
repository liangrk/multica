// orca-bridge is a transparent bridge between the Multica daemon and a real
// agent CLI running inside an Orca-managed terminal tab.
//
// The daemon spawns it through a custom runtime profile whose protocol family
// matches the wrapped CLI (e.g. runtime-type claude), with the real CLI path
// as the profile's fixed_args, so argv looks like:
//
//	orca-bridge.exe <real-cli> <cli args...>
//
// The bridge lifts the initial prompt out of the daemon's stdin (first
// stream-json user message), passes it as a -p argument instead, and submits
// the wrapped CLI to an Orca terminal inside the task workdir. The CLI's
// stdout is teed to a capture file that the bridge tails back to the daemon
// byte-for-byte; a sentinel file carries the exit code. Supplement messages
// (steer) are not supported: the terminal CLI has no daemon-attached stdin.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	bridgeDirName = ".orca-bridge"
	pollInterval  = 300 * time.Millisecond
)

// envPrefixWhitelist is forwarded into the Orca terminal command. The terminal
// inherits Orca's own environment, not the daemon's task environment, so
// task-scoped variables the wrapped CLI depends on must be re-declared.
var envPrefixWhitelist = []string{
	"MULTICA_",
	"CLAUDE_CODE_",
	"ANTHROPIC_",
	"CLAUDE_",
	"TMPDIR",
}

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
	// absent, resolve it from ORCA_REAL_CLI or PATH — the daemon spawns the
	// bridge from an environment where the wrapped CLI is installed.
	realCLI := ""
	cliArgs := args
	if _, err := os.Stat(args[0]); err == nil && filepath.IsAbs(args[0]) {
		realCLI = args[0]
		cliArgs = args[1:]
	} else if !strings.ContainsAny(args[0], `/-`) && !hasVersionArg(args) {
		if p, err := exec.LookPath(args[0]); err == nil {
			realCLI = p
			cliArgs = args[1:]
		}
	}
	if realCLI == "" {
		if p := os.Getenv("ORCA_REAL_CLI"); p != "" {
			realCLI = p
		} else if p, err := exec.LookPath("claude"); err == nil {
			realCLI = p
		} else {
			return fmt.Errorf("wrapped CLI not found (set ORCA_REAL_CLI or add claude to PATH)")
		}
	}

	// Version probes are frequent, cheap, and never user-visible: answer them
	// locally instead of opening a terminal tab for each probe.
	if hasVersionArg(cliArgs) {
		return passthroughLocal(realCLI, cliArgs)
	}

	// The daemon sends the initial user message as the first stdin line; lift
	// its text out and pass it as the -p argument. Anything after the first
	// line (supplements) is discarded.
	promptText, err := readInitialPrompt(os.Stdin)
	if err != nil {
		// Dump the raw first line for diagnosis before failing.
		cwd0, _ := os.Getwd()
		if cwd0 != "" {
			_ = os.MkdirAll(filepath.Join(cwd0, bridgeDirName), 0o755)
			_ = os.WriteFile(filepath.Join(cwd0, bridgeDirName, "last-stdin-line.txt"), []byte(lastStdinLine), 0o644)
		}
		return fmt.Errorf("read initial prompt: %w", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve workdir: %w", err)
	}

	bridgeDir := filepath.Join(cwd, bridgeDirName)
	if err := os.MkdirAll(bridgeDir, 0o755); err != nil {
		return fmt.Errorf("create bridge dir: %w", err)
	}
	outFile := filepath.Join(bridgeDir, "capture-out.jsonl")
	errFile := filepath.Join(bridgeDir, "capture-err.txt")
	exitFile := filepath.Join(bridgeDir, "exit.code")
	_ = os.Remove(exitFile)

	orcaCLI, err := findOrcaCLI()
	if err != nil {
		fmt.Fprintln(os.Stderr, "orca-bridge:", err, "— falling back to local execution")
		return passthroughLocal(realCLI, rearguedArgs(cliArgs, promptText))
	}

	projectID, perr := matchOrcaProject(orcaCLI, cwd)
	if perr == nil {
		// Register the task workdir under the matched project so the task
		// shows up under its repo in Orca. Best-effort only: the terminal
		// below targets the anchor workspace and cd's into the workdir, so
		// registration is metadata, not a requirement.
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

	command := buildTerminalCommand(realCLI, cliArgs, promptText, bridgeDir)
	// The terminal may open in an anchor workspace (see below); the script
	// re-enters the task workdir first so the CLI always runs there.
	scriptBody := fmt.Sprintf("Set-Location -LiteralPath '%s'\r\n%s\r\n", pwshQuote(cwd), command)
	scriptPath := filepath.Join(bridgeDir, "bridge-run.ps1")
	if err := os.WriteFile(scriptPath, []byte(scriptBody), 0o644); err != nil {
		return fmt.Errorf("write bridge script: %w", err)
	}
	// A one-line terminal command avoids PSReadLine long-paste issues: the
	// whole pipeline lives in the script file, executed by a clean pwsh.
	terminalCommand := fmt.Sprintf("pwsh -NoProfile -ExecutionPolicy Bypass -File '%s'", pwshQuote(scriptPath))
	title := terminalTitle()
	// Anchor worktree: a freshly imported folder workspace may not have a
	// ready shell host, so the operator can point the terminal at any already
	// working workspace; the script cd's into the task workdir regardless.
	anchor := os.Getenv("ORCA_ANCHOR_WORKTREE")
	if anchor == "" {
		anchor = cwd
	}
	createArgs := []string{
		"terminal", "create",
		"--worktree", "path:" + anchor,
		"--title", title,
		"--command", terminalCommand,
		"--json",
	}
	// terminal create resolves the executing workspace from the caller's
	// working directory (the --worktree flag does not relocate it), so the
	// CLI itself must run from inside the anchor workspace.
	handleRaw, err := runOrcaInDir(anchor, orcaCLI, createArgs)
	if err != nil {
		return fmt.Errorf("orca terminal create: %v\noutput: %s", err, strings.TrimSpace(handleRaw))
	}
	_ = os.WriteFile(filepath.Join(bridgeDir, "terminal.txt"), []byte(strings.TrimSpace(handleRaw)), 0o644)

	if tailUntilExit(outFile, errFile, exitFile, os.Stdout, os.Stderr) {
		forwardExit(exitFile)
	}
	return fmt.Errorf("exit sentinel never appeared")
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
// initial prompt text replaces the stdin protocol flags.
func rearguedArgs(cliArgs []string, promptText string) []string {
	return rewriteArgsForPrompt(cliArgs, promptText)
}

// rewriteArgsForPrompt removes the stream-json stdin protocol pair and injects
// the prompt text after -p. If no -p exists, "-p <text>" is appended.
func rewriteArgsForPrompt(cliArgs []string, promptText string) []string {
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

// readInitialPrompt consumes stdin lines until the stream-json user message
// (skipping control frames such as the SDK hooks init that the daemon sends
// first) and returns its text content. Content may be a plain string or an
// array of blocks with "text" fields. The rest of stdin is discarded.
var lastStdinLine string

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
					continue // control frame (hooks init etc.) — skip
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
		// Not JSON — treat the line itself as the prompt.
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
// Used for --version probes: the daemon expects the answer on stdout and a
// probe must not open visible terminal tabs.
func passthroughLocal(realCLI string, args []string) error {
	cmd := exec.Command(realCLI, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// matchOrcaProject resolves the Orca project that owns this workdir by
// matching the workdir's git origin against known project ids. Ids look like
// "git:<host>/<path>", "github:<owner>/<name>" or "repo:<uuid>"; only the
// repo-hosting forms are matchable.
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
	// scp form: git@host:path -> host/path
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

// envInjection renders pwsh variable assignments for the whitelisted task
// environment variables so the terminal-spawned CLI sees the same values the
// daemon would have given a direct child.
func envInjection() string {
	var b strings.Builder
	for _, k := range os.Environ() {
		eq := strings.IndexByte(k, '=')
		if eq <= 0 {
			continue
		}
		key, val := k[:eq], k[eq+1:]
		matched := false
		for _, p := range envPrefixWhitelist {
			if strings.HasPrefix(key, p) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		b.WriteString(fmt.Sprintf("$env:%s = '%s'; ", key, pwshQuote(val)))
	}
	return b.String()
}

func pwshQuote(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// buildTerminalCommand assembles the script body typed into the Orca terminal:
// re-enter the workdir, inject env, then run the real CLI with the prompt as
// an argument, stdout teed to the capture file the bridge tails, stderr to its
// own file, and the exit code dropped into the sentinel file.
func buildTerminalCommand(realCLI string, cliArgs []string, promptText, bridgeDir string) string {
	finalArgs := rewriteArgsForPrompt(cliArgs, promptText)
	var b strings.Builder
	b.WriteString(envInjection())
	fmt.Fprintf(&b, "& '%s'", pwshQuote(realCLI))
	for _, a := range finalArgs {
		fmt.Fprintf(&b, " '%s'", pwshQuote(a))
	}
	fmt.Fprintf(&b, " 2> '%s'", pwshQuote(filepath.Join(bridgeDir, "capture-err.txt")))
	fmt.Fprintf(&b, " | Tee-Object -FilePath '%s'", pwshQuote(filepath.Join(bridgeDir, "capture-out.jsonl")))
	fmt.Fprintf(&b, "; $LASTEXITCODE | Set-Content -LiteralPath '%s' -Encoding ascii",
		pwshQuote(filepath.Join(bridgeDir, "exit.code")))
	return b.String()
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
	cmd := exec.Command(orcaCLI, args...)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "ORCA_NO_COLOR=1")
	err := cmd.Run()
	if err != nil && out.Len() == 0 {
		// Surface the CLI's own stderr on failure; on success it is noise
		// (crashpad banners) that would corrupt JSON parsing.
		return errOut.String(), err
	}
	return out.String(), err
}

// tailUntilExit polls the capture files and mirrors new bytes to the daemon
// until the sentinel exit-code file shows up. Returns true when the sentinel
// was seen.
func tailUntilExit(outFile, errFile, exitFile string, stdout, stderr io.Writer) bool {
	outPos, errPos := int64(0), int64(0)
	for {
		outPos = pump(outFile, &outPos, stdout)
		errPos = pump(errFile, &errPos, stderr)
		if _, err := os.Stat(exitFile); err == nil {
			pump(outFile, &outPos, stdout)
			pump(errFile, &errPos, stderr)
			return true
		}
		time.Sleep(pollInterval)
	}
}

func pump(path string, pos *int64, w io.Writer) int64 {
	f, err := os.Open(path)
	if err != nil {
		return *pos
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() < *pos {
		return *pos
	}
	if _, err := f.Seek(*pos, io.SeekStart); err != nil {
		return *pos
	}
	n, _ := io.Copy(w, f)
	return *pos + n
}

// forwardExit reads the sentinel exit code and terminates the bridge with the
// same code so the daemon records the real CLI outcome.
func forwardExit(exitFile string) {
	// Give the terminal a moment to finish flushing Tee-Object buffers.
	time.Sleep(500 * time.Millisecond)
	code := 1
	if b, err := os.ReadFile(exitFile); err == nil {
		fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &code)
	}
	os.Exit(code)
}
