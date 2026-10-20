package agent_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aungpwint/vibe-agent/agent"
)

func TestDecidePrefersTheLastMatchingRule(t *testing.T) {
	rules := agent.Ruleset{
		{Action: "*", Resource: "*", Effect: agent.EffectAllow},
		{Action: "bash", Resource: "git *", Effect: agent.EffectAsk},
		{Action: "bash", Resource: "git push *", Effect: agent.EffectDeny},
		{Action: "edit", Resource: "*", Effect: agent.EffectDeny},
		{Action: "edit", Resource: "src/**", Effect: agent.EffectAllow},
	}

	cases := []struct {
		action   string
		resource string
		want     agent.PermissionEffect
	}{
		{"read", "anything", agent.EffectAllow},
		{"bash", "git status", agent.EffectAsk},
		{"bash", "git push origin main", agent.EffectDeny},
		{"bash", "rm -rf build", agent.EffectAllow},
		{"edit", "README.md", agent.EffectDeny},
		{"edit", "src/main.go", agent.EffectAllow},
	}

	for _, tc := range cases {
		if got := rules.Decide(tc.action, tc.resource); got != tc.want {
			t.Errorf("Decide(%q, %q) = %q, want %q", tc.action, tc.resource, got, tc.want)
		}
	}

	if got := (agent.Ruleset{}).Decide("bash", "ls"); got != agent.EffectAsk {
		t.Errorf("an empty ruleset should ask, got %q", got)
	}
}

func TestNormalizePartsDerivesBodyFromTextOnly(t *testing.T) {
	parts, body, err := agent.NormalizeParts([]agent.Part{
		{Type: "text", Text: "  hello  "},
		{Type: "reasoning", Text: "hidden chain"},
		{Type: "text", Text: "world"},
	})
	if err != nil {
		t.Fatalf("NormalizeParts: %v", err)
	}
	if body != "hello\nworld" {
		t.Errorf("body = %q, want %q", body, "hello\nworld")
	}
	if len(parts) != 3 {
		t.Errorf("stored %d parts, want 3", len(parts))
	}
}

func TestNormalizePartsRejectsInvalidParts(t *testing.T) {
	cases := map[string][]agent.Part{
		"unknown type":    {{Type: "video"}},
		"empty text":      {{Type: "text", Text: "   "}},
		"tool without id": {{Type: "tool"}},
		"agent without id": {
			{Type: "agent"},
		},
		"file without data": {{Type: "file"}},
		"bad data":          {{Type: "file", Data: json.RawMessage(`{oops`)}},
	}

	for name, parts := range cases {
		if _, _, err := agent.NormalizeParts(parts); err == nil {
			t.Errorf("%s: NormalizeParts accepted an invalid part", name)
		}
	}
}

func TestNormalizePartsAcceptsFileWithObjectData(t *testing.T) {
	parts, _, err := agent.NormalizeParts([]agent.Part{
		{Type: "file", Data: json.RawMessage(`{"path":"internal/app/app.go"}`)},
	})
	if err != nil {
		t.Fatalf("NormalizeParts: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("stored %d parts, want 1", len(parts))
	}
}

func TestIDsSortStrictlyWithinOneMillisecond(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	var previous agent.ID
	for i := 0; i < 1000; i++ {
		id := agent.NewID(now)
		if _, err := agent.ParseID(string(id)); err != nil {
			t.Fatalf("minted an id that does not parse: %v", err)
		}
		if id <= previous {
			t.Fatalf("id %s does not sort after %s (iteration %d)", id, previous, i)
		}
		previous = id
	}
}

func TestParseIDRejectsNonCanonicalInput(t *testing.T) {
	id := agent.NewID(time.Now())
	parsed, err := agent.ParseID(strings.ToLower(string(id)))
	if err != nil {
		t.Fatalf("ParseID: %v", err)
	}
	if parsed != id {
		t.Errorf("ParseID returned %s, want %s", parsed, id)
	}

	if _, err := agent.ParseID("not-an-id"); !errors.Is(err, agent.ErrInvalidID) {
		t.Errorf("ParseID(not-an-id) error = %v, want ErrInvalidID", err)
	}
}

func TestParseEnums(t *testing.T) {
	if mode, ok := agent.ParseAgentMode(" Primary "); !ok || mode != agent.ModePrimary {
		t.Errorf("ParseAgentMode = %q, %t", mode, ok)
	}
	if _, ok := agent.ParseAgentMode("orchestrator"); ok {
		t.Error("ParseAgentMode accepted an unknown mode")
	}
	if role, ok := agent.ParseTurnRole("ASSISTANT"); !ok || role != agent.TurnAssistant {
		t.Errorf("ParseTurnRole = %q, %t", role, ok)
	}
	if status, ok := agent.ParseTurnStatus("streaming"); !ok || status != agent.TurnStreaming {
		t.Errorf("ParseTurnStatus = %q, %t", status, ok)
	}
	if status, ok := agent.ParseRunStatus("cancelled"); !ok || !status.Terminal() {
		t.Errorf("ParseRunStatus = %q, %t", status, ok)
	}
	if (agent.RunRunning).Terminal() {
		t.Error("a running run reported itself terminal")
	}
}
