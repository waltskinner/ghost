package ai

import (
	"context"
	"errors"
	"os/exec"
)

// errNoCLIBackend is returned when no subprocess LLM binary is available.
var errNoCLIBackend = errors.New("no CLI LLM backend available (install claude or opencode)")

// cliBackend is the shared capability of the two subprocess LLM clients.
type cliBackend interface {
	Reflect(ctx context.Context, prompt string) (string, TokenUsage, error)
	Classify(ctx context.Context, systemPrompt, userContent string) (string, error)
}

// CLIProvider resolves the best available CLI backend — claude first, then
// opencode — and delegates to it, so callers need no knowledge of which binary
// is installed. It satisfies reflection's reflector interface and ai.Provider,
// so it drops in wherever CLIClient is used.
type CLIProvider struct {
	backend cliBackend
	name    string
}

// NewCLIProvider picks the best backend available on PATH: claude if present,
// else opencode if present, else an unavailable provider.
func NewCLIProvider() *CLIProvider {
	if _, err := exec.LookPath("claude"); err == nil {
		return &CLIProvider{backend: NewCLIClient(), name: "cli"}
	}
	if _, err := exec.LookPath("opencode"); err == nil {
		return &CLIProvider{backend: NewOpenCodeClient(), name: "opencode"}
	}
	return &CLIProvider{backend: nil, name: "none"}
}

// Name reports which backend was selected ("cli", "opencode", or "none").
func (p *CLIProvider) Name() string { return p.name }

// Available reports whether any subprocess LLM backend is on PATH.
func (p *CLIProvider) Available() bool { return p.backend != nil }

func (p *CLIProvider) Reflect(ctx context.Context, prompt string) (string, TokenUsage, error) {
	if p.backend == nil {
		return "", TokenUsage{}, errNoCLIBackend
	}
	return p.backend.Reflect(ctx, prompt)
}

func (p *CLIProvider) Classify(ctx context.Context, systemPrompt, userContent string) (string, error) {
	if p.backend == nil {
		return "", errNoCLIBackend
	}
	return p.backend.Classify(ctx, systemPrompt, userContent)
}
