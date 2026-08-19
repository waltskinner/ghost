package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// OpenCodeClient drives the `opencode` CLI as a subprocess LLM, the way
// CLIClient drives `claude -p`. It bills to whatever provider opencode is
// configured with, so it works with no ANTHROPIC_API_KEY and no `claude`
// binary. It implements the same Reflect/Classify shapes as CLIClient
// (reflector / Provider), so it substitutes for the direct API client in the
// reflect tier.
type OpenCodeClient struct {
	binary string
}

// NewOpenCodeClient creates an OpenCodeClient that invokes `opencode` on PATH.
func NewOpenCodeClient() *OpenCodeClient {
	return &OpenCodeClient{binary: "opencode"}
}

// Reflect satisfies reflection's reflector interface (see
// internal/reflection/tier_haiku.go). TokenUsage is always zero: subscription
// calls have no per-token API cost to record.
func (c *OpenCodeClient) Reflect(ctx context.Context, prompt string) (string, TokenUsage, error) {
	text, err := c.run(ctx, prompt)
	return text, TokenUsage{}, err
}

// Classify satisfies the Provider interface (see internal/ai/provider.go).
// opencode run has no --system-prompt flag, so the system prompt is joined into
// the message — the same join anthropicClient.Classify performs.
func (c *OpenCodeClient) Classify(ctx context.Context, systemPrompt, userContent string) (string, error) {
	return c.run(ctx, systemPrompt+"\n\n"+userContent)
}

func (c *OpenCodeClient) run(ctx context.Context, prompt string) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
	}
	// --format json emits a JSON-lines stream; --pure skips plugins. The prompt
	// is the last argument. cmd.Dir is a neutral temp dir so the subprocess does
	// not load the repo's CLAUDE.md/AGENTS.md, project opencode.json, or git
	// context — the reflect prompt is self-contained. subprocessEnv (below)
	// additionally points XDG_CONFIG_HOME at a fresh empty dir so the child does
	// not load the user's global opencode config (which would start Ghost's own
	// MCP server against this process's SQLite DB) and strips ANTHROPIC_API_KEY.
	args := []string{"run", "--format", "json", "--pure", prompt}
	cmd := exec.CommandContext(ctx, c.binary, args...)
	cmd.Dir = os.TempDir()
	env, cleanup, err := c.subprocessEnv()
	if err != nil {
		return "", err
	}
	defer cleanup()
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("opencode run: %w: %s", err, stderr.String())
	}
	return parseOpenCodeOutput(stdout.String()), nil
}

// subprocessEnv builds the environment for the opencode subprocess: it points
// XDG_CONFIG_HOME at a fresh empty dir (so the child loads no global opencode
// config — no Ghost MCP server, no user plugins) and strips ANTHROPIC_API_KEY,
// mirroring CLIClient's stripAPIKey. The child consequently runs on opencode's
// built-in default model rather than the user's configured `model` key, which
// is an acceptable, documented trade for not re-opening this process's DB.
func (c *OpenCodeClient) subprocessEnv() ([]string, func(), error) {
	scratch, err := os.MkdirTemp("", "ghost-opencode-")
	if err != nil {
		return nil, nil, err
	}
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "XDG_CONFIG_HOME=") {
			continue // replaced by the scrub dir below
		}
		env = append(env, kv)
	}
	env = append(env, "XDG_CONFIG_HOME="+scratch)
	return stripAPIKey(env), func() { _ = os.RemoveAll(scratch) }, nil
}

// opencodeEvent is the minimal shape of one JSON line in `opencode run --format
// json` output. Everything else in the line is ignored.
type opencodeEvent struct {
	Type string `json:"type"`
	Part struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"part"`
}

// parseOpenCodeOutput concatenates the text events from an opencode JSON-lines
// stream into the model's answer, ignoring step_start/step_finish/reasoning and
// any unparseable lines.
func parseOpenCodeOutput(raw string) string {
	var sb strings.Builder
	sc := bufio.NewScanner(strings.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var ev opencodeEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		if ev.Type == "text" && ev.Part.Type == "text" {
			sb.WriteString(ev.Part.Text)
		}
	}
	return sb.String()
}
