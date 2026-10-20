package agent

import (
	"encoding/json"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

type AgentMode string

const (
	ModePrimary  AgentMode = "primary"
	ModeSubagent AgentMode = "subagent"
	ModeAll      AgentMode = "all"
)

func (m AgentMode) valid() bool {
	switch m {
	case ModePrimary, ModeSubagent, ModeAll:
		return true
	}
	return false
}

func ParseAgentMode(raw string) (AgentMode, bool) {
	m := AgentMode(strings.ToLower(strings.TrimSpace(raw)))
	return m, m.valid()
}

type SessionStatus string

const (
	SessionOpen   SessionStatus = "open"
	SessionClosed SessionStatus = "closed"
)

func (s SessionStatus) valid() bool { return s == SessionOpen || s == SessionClosed }

func ParseSessionStatus(raw string) (SessionStatus, bool) {
	s := SessionStatus(strings.ToLower(strings.TrimSpace(raw)))
	return s, s.valid()
}

// ArchiveFilter selects which slice of the session list a query reads.
type ArchiveFilter string

const (
	ActiveOnly   ArchiveFilter = "active"
	ArchivedOnly ArchiveFilter = "archived"
	AllSessions  ArchiveFilter = "all"
)

func (f ArchiveFilter) valid() bool {
	switch f {
	case "", ActiveOnly, ArchivedOnly, AllSessions:
		return true
	}
	return false
}

type TurnRole string

const (
	TurnUser      TurnRole = "user"
	TurnAssistant TurnRole = "assistant"
	TurnSystem    TurnRole = "system"
	TurnTool      TurnRole = "tool"
)

func (r TurnRole) valid() bool {
	switch r {
	case TurnUser, TurnAssistant, TurnSystem, TurnTool:
		return true
	}
	return false
}

func ParseTurnRole(raw string) (TurnRole, bool) {
	r := TurnRole(strings.ToLower(strings.TrimSpace(raw)))
	return r, r.valid()
}

type TurnStatus string

const (
	TurnStreaming TurnStatus = "streaming"
	TurnComplete  TurnStatus = "complete"
	TurnError     TurnStatus = "error"
	TurnAborted   TurnStatus = "aborted"
)

func (s TurnStatus) valid() bool {
	switch s {
	case TurnStreaming, TurnComplete, TurnError, TurnAborted:
		return true
	}
	return false
}

func ParseTurnStatus(raw string) (TurnStatus, bool) {
	s := TurnStatus(strings.ToLower(strings.TrimSpace(raw)))
	return s, s.valid()
}

type RunStatus string

const (
	RunQueued    RunStatus = "queued"
	RunRunning   RunStatus = "running"
	RunCompleted RunStatus = "completed"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

func (s RunStatus) valid() bool {
	switch s {
	case RunQueued, RunRunning, RunCompleted, RunFailed, RunCancelled:
		return true
	}
	return false
}

func ParseRunStatus(raw string) (RunStatus, bool) {
	s := RunStatus(strings.ToLower(strings.TrimSpace(raw)))
	return s, s.valid()
}

func (s RunStatus) Live() bool { return s == RunQueued || s == RunRunning }

func (s RunStatus) Terminal() bool {
	return s == RunCompleted || s == RunFailed || s == RunCancelled
}

type PermissionEffect string

const (
	EffectAllow PermissionEffect = "allow"
	EffectAsk   PermissionEffect = "ask"
	EffectDeny  PermissionEffect = "deny"
)

func (e PermissionEffect) valid() bool {
	return e == EffectAllow || e == EffectAsk || e == EffectDeny
}

type PermissionRule struct {
	Action   string           `json:"action"`
	Resource string           `json:"resource"`
	Effect   PermissionEffect `json:"effect"`
}

type Ruleset []PermissionRule

func NormalizeRuleset(in Ruleset) (Ruleset, error) {
	if len(in) > maxPermissionRules {
		return nil, invalid("an agent cannot carry more than 100 permission rules")
	}
	out := make(Ruleset, 0, len(in))
	for _, rule := range in {
		rule.Action = strings.TrimSpace(rule.Action)
		rule.Resource = strings.TrimSpace(rule.Resource)
		if rule.Action == "" || utf8.RuneCountInString(rule.Action) > 100 {
			return nil, invalid("every permission rule needs an action of at most 100 characters")
		}
		if rule.Resource == "" || utf8.RuneCountInString(rule.Resource) > 500 {
			return nil, invalid("every permission rule needs a resource pattern of at most 500 characters")
		}
		if !rule.Effect.valid() {
			return nil, invalid("a permission effect must be allow, ask or deny")
		}
		out = append(out, rule)
	}
	return out, nil
}

// Decide applies the last matching rule, which is the precedence OpenCode
// documents: a narrower rule written after a broad one wins without needing a
// specificity ordering. No match means ask.
func (r Ruleset) Decide(action, resource string) PermissionEffect {
	for i := len(r) - 1; i >= 0; i-- {
		if r[i].Action != action && r[i].Action != "*" {
			continue
		}
		if resourceMatches(r[i].Resource, resource) {
			return r[i].Effect
		}
	}
	return EffectAsk
}

func resourceMatches(pattern, resource string) bool {
	if pattern == "*" || pattern == "**" {
		return true
	}
	if pattern == resource {
		return true
	}
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		return resource == prefix || strings.HasPrefix(resource, prefix+"/")
	}
	ok, err := path.Match(pattern, resource)
	return err == nil && ok
}

type Part struct {
	Type   string          `json:"type"`
	Text   string          `json:"text,omitempty"`
	Tool   string          `json:"tool,omitempty"`
	CallID string          `json:"callId,omitempty"`
	Status string          `json:"status,omitempty"`
	Agent  string          `json:"agent,omitempty"`
	Model  string          `json:"model,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
}

var knownPartTypes = map[string]bool{
	"text": true, "reasoning": true, "tool": true, "file": true, "step": true,
	"agent": true, "model": true, "snapshot": true, "patch": true,
}

// NormalizeParts validates every part and derives the searchable body. The body
// is only the text a reader would call the message; reasoning and tool payloads
// stay out of search so a model's private deliberation cannot be found as if it
// had been said.
func NormalizeParts(parts []Part) ([]Part, string, error) {
	if len(parts) > maxPartsPerTurn {
		return nil, "", invalid("a turn cannot carry more than 200 parts")
	}
	out := make([]Part, 0, len(parts))
	var body strings.Builder

	for _, part := range parts {
		part.Type = strings.ToLower(strings.TrimSpace(part.Type))
		if !knownPartTypes[part.Type] {
			return nil, "", invalid("that is not a kind of turn part this tool stores")
		}

		switch part.Type {
		case "text", "reasoning":
			if strings.TrimSpace(part.Text) == "" {
				return nil, "", invalid("a text part needs some text in it")
			}
			if utf8.RuneCountInString(part.Text) > maxPartTextRunes {
				return nil, "", invalid("one part of that turn is too long")
			}
			if part.Type == "text" {
				if body.Len() > 0 {
					body.WriteString("\n")
				}
				body.WriteString(strings.TrimSpace(part.Text))
			}
		case "tool":
			if strings.TrimSpace(part.Tool) == "" {
				return nil, "", invalid("a tool part needs the tool it names")
			}
		case "file":
			if len(part.Data) == 0 {
				return nil, "", invalid("a file part needs its data (a path or a URL)")
			}
		case "agent":
			if strings.TrimSpace(part.Agent) == "" {
				return nil, "", invalid("an agent part needs the agent it names")
			}
		case "model":
			if strings.TrimSpace(part.Model) == "" {
				return nil, "", invalid("a model part needs the model it names")
			}
		case "snapshot", "patch":
			if len(part.Data) == 0 {
				return nil, "", invalid("that part needs its data")
			}
		}

		if len(part.Data) > 0 {
			var object map[string]any
			if err := json.Unmarshal(part.Data, &object); err != nil {
				return nil, "", invalid("a part's data must be a JSON object")
			}
		}
		out = append(out, part)
	}

	derived := strings.TrimSpace(body.String())
	if utf8.RuneCountInString(derived) > maxTurnBodyRunes {
		return nil, "", invalid("that turn carries too much text")
	}
	return out, derived, nil
}

type Tokens struct {
	Prompt     int64 `json:"prompt"`
	Completion int64 `json:"completion"`
	CostMicros int64 `json:"costMicros"`
}

func (t Tokens) delta(previous Tokens) Tokens {
	return Tokens{
		Prompt:     t.Prompt - previous.Prompt,
		Completion: t.Completion - previous.Completion,
		CostMicros: t.CostMicros - previous.CostMicros,
	}
}

type Profile struct {
	ID ID `json:"id"`

	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`

	Mode    AgentMode `json:"mode"`
	Builtin bool      `json:"builtin"`

	SystemPrompt *string  `json:"systemPrompt,omitempty"`
	Model        *string  `json:"model,omitempty"`
	Temperature  *float64 `json:"temperature,omitempty"`

	Tools       []string       `json:"tools"`
	Permissions Ruleset        `json:"permissions"`
	Metadata    map[string]any `json:"metadata"`

	ArchivedAt *time.Time `json:"archivedAt,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type Session struct {
	ID ID `json:"id"`

	ParentID *ID `json:"parentId,omitempty"`
	RootID   *ID `json:"rootId,omitempty"`
	Depth    int `json:"depth"`

	Title     string  `json:"title"`
	AgentID   ID      `json:"agentId"`
	AgentSlug string  `json:"agentSlug"`
	Model     *string `json:"model,omitempty"`
	Workspace *string `json:"workspace,omitempty"`
	Project   *string `json:"project,omitempty"`

	Metadata    map[string]any `json:"metadata"`
	Permissions Ruleset        `json:"permissions"`

	Status       SessionStatus `json:"status"`
	NextSequence int64         `json:"nextSequence"`
	TurnCount    int           `json:"turnCount"`
	Tokens       Tokens        `json:"tokens"`

	ForkedFromID       *ID    `json:"forkedFromId,omitempty"`
	ForkedFromSequence *int64 `json:"forkedFromSequence,omitempty"`

	ArchivedAt     *time.Time `json:"archivedAt,omitempty"`
	LastActivityAt time.Time  `json:"lastActivityAt"`
	ClosedAt       *time.Time `json:"closedAt,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
}

type Turn struct {
	ID        ID    `json:"id"`
	SessionID ID    `json:"sessionId"`
	Sequence  int64 `json:"sequence"`

	Role   TurnRole   `json:"role"`
	Status TurnStatus `json:"status"`

	ParentTurnID *ID `json:"parentTurnId,omitempty"`

	Parts []Part  `json:"parts"`
	Body  *string `json:"body,omitempty"`

	Model   *string `json:"model,omitempty"`
	AgentID *ID     `json:"agentId,omitempty"`
	RunID   *ID     `json:"runId,omitempty"`

	Tokens Tokens `json:"tokens"`

	ClientID *string `json:"clientId,omitempty"`

	RedactedAt *time.Time `json:"redactedAt,omitempty"`

	CreatedAt   time.Time  `json:"createdAt"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
}

func (t Turn) IsRedacted() bool { return t.RedactedAt != nil }

type Run struct {
	ID        ID  `json:"id"`
	SessionID ID  `json:"sessionId"`
	AgentID   *ID `json:"agentId,omitempty"`

	Status RunStatus `json:"status"`
	Model  *string   `json:"model,omitempty"`

	ChildSessionID *ID `json:"childSessionId,omitempty"`

	Steps  int     `json:"steps"`
	Tokens Tokens  `json:"tokens"`
	Error  *string `json:"error,omitempty"`

	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
}

type RunStats struct {
	Total     int    `json:"total"`
	Queued    int    `json:"queued"`
	Running   int    `json:"running"`
	Completed int    `json:"completed"`
	Failed    int    `json:"failed"`
	Cancelled int    `json:"cancelled"`
	Steps     int64  `json:"steps"`
	Tokens    Tokens `json:"tokens"`
}

type SessionQuery struct {
	ParentID  *ID
	RootID    *ID
	Status    *SessionStatus
	AgentSlug *string
	Archived  ArchiveFilter
}

type SessionPage struct {
	Items   []Session
	HasNext bool
}

type TurnQuery struct {
	SessionID     ID
	AfterSequence int64
	Limit         int
	Role          *TurnRole
}

type TurnSearch struct {
	Text      string
	SessionID *ID
	Role      *TurnRole
	AgentSlug *string
	Limit     int
}

type TurnPage struct {
	Items      []Turn
	HasMore    bool
	NextSeq    int64
	TotalCount *int
}

type CreateProfileParams struct {
	Slug        string
	Name        string
	Description string
	Mode        AgentMode

	SystemPrompt *string
	Model        *string
	Temperature  *float64

	Tools       []string
	Permissions Ruleset
	Metadata    map[string]any
}

type UpdateProfileParams struct {
	ID ID

	Name        *string
	Description *string
	Mode        *AgentMode

	SystemPrompt *string
	Model        *string
	Temperature  *float64

	Tools       *[]string
	Permissions *Ruleset
	Metadata    *map[string]any
}

type CreateSessionParams struct {
	ParentID *ID

	Title     string
	AgentSlug string
	Model     *string
	Workspace *string
	Project   *string

	Metadata    map[string]any
	Permissions *Ruleset
}

type UpdateSessionParams struct {
	ID ID

	Title    *string
	Model    *string
	Metadata *map[string]any

	Permissions *Ruleset
}

type ForkSessionParams struct {
	ID ID

	Title *string

	AtSequence *int64
}

type AppendTurnParams struct {
	SessionID ID

	Role   TurnRole
	Status TurnStatus

	ParentTurnID *ID
	RunID        *ID

	Parts []Part

	Model   *string
	AgentID *ID

	Tokens   Tokens
	ClientID *string
}

type CompleteTurnParams struct {
	ID     ID
	Status TurnStatus
	Parts  []Part
	Tokens *Tokens
}

type StartRunParams struct {
	SessionID ID
	AgentID   *ID
	Model     *string
}

type FinishRunParams struct {
	ID ID

	Status         RunStatus
	Steps          int
	Tokens         Tokens
	Error          *string
	ChildSessionID *ID
}

type ImportTurn struct {
	Sequence int64
	Role     TurnRole
	Status   TurnStatus

	Parts []Part
	Model *string

	Tokens Tokens

	CreatedAt   time.Time
	CompletedAt *time.Time
}

type ImportSessionParams struct {
	ParentID *ID

	Title     string
	AgentSlug string
	Model     *string
	Workspace *string
	Project   *string

	Metadata    map[string]any
	Permissions Ruleset
	Turns       []ImportTurn
}

type Export struct {
	Version    int       `json:"version"`
	ExportedAt time.Time `json:"exportedAt"`

	Session Session `json:"session"`

	Turns []ExportTurn `json:"turns"`
}

type ExportTurn struct {
	Sequence int64      `json:"sequence"`
	Role     TurnRole   `json:"role"`
	Status   TurnStatus `json:"status"`

	Parts []Part  `json:"parts"`
	Body  *string `json:"body,omitempty"`

	Model   *string `json:"model,omitempty"`
	AgentID *ID     `json:"agentId,omitempty"`

	Tokens Tokens `json:"tokens"`

	CreatedAt   time.Time  `json:"createdAt"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
}

const (
	maxTitleRunes        = 200
	maxSlugLength        = 64
	maxDescriptionRunes  = 2000
	maxModelLength       = 200
	maxPathLength        = 500
	maxPartsPerTurn      = 200
	maxPartTextRunes     = 16000
	maxTurnBodyRunes     = 32000
	maxPermissionRules   = 100
	maxMetadataBytes     = 64 * 1024
	maxExportTurns       = 10000
	maxSessionDepth      = 32
	maxClientIDLength    = 200
	maxSearchNeedleRunes = 3
)

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

func normalizeSlug(raw string) (string, error) {
	slug := strings.ToLower(strings.TrimSpace(raw))
	if len(slug) > maxSlugLength || !slugPattern.MatchString(slug) {
		return "", invalid("an agent slug is lowercase letters, digits and dashes, starting with a letter or digit")
	}
	return slug, nil
}

func normalizeTitle(raw string) (string, error) {
	title := strings.TrimSpace(raw)
	if title == "" || utf8.RuneCountInString(title) > maxTitleRunes {
		return "", invalid("a session title needs 1 to 200 characters")
	}
	return title, nil
}

func optionalText(raw *string, max int, what string) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	value := strings.TrimSpace(*raw)
	if value == "" {
		return nil, nil
	}
	if max > 0 && utf8.RuneCountInString(value) > max {
		return nil, invalid("that %s is too long", what)
	}
	return &value, nil
}

func normalizeMetadata(in map[string]any) (map[string]any, error) {
	if in == nil {
		return map[string]any{}, nil
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, invalid("the metadata is not a JSON object")
	}
	if len(raw) > maxMetadataBytes {
		return nil, invalid("the metadata is larger than 64 KiB")
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out, nil
}

func normalizeClientID(raw *string) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	value := strings.TrimSpace(*raw)
	if value == "" {
		return nil, nil
	}
	if utf8.RuneCountInString(value) > maxClientIDLength {
		return nil, invalid("a client id is too long")
	}
	return &value, nil
}

func validateTemperature(value *float64) error {
	if value == nil {
		return nil
	}
	if *value < 0 || *value > 2 {
		return invalid("a temperature must be between 0 and 2")
	}
	return nil
}

func validateTokens(t Tokens) error {
	if t.Prompt < 0 || t.Completion < 0 || t.CostMicros < 0 {
		return invalid("token counts cannot be negative")
	}
	return nil
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func nullableBody(body string) *string {
	if body == "" {
		return nil
	}
	return &body
}

type rowScanner interface {
	Scan(dest ...any) error
}
