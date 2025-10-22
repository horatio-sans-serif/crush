package tools

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/shell"
)

type BashParams struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

type BashPermissionsParams struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

type BashResponseMetadata struct {
	StartTime        int64  `json:"start_time"`
	EndTime          int64  `json:"end_time"`
	Output           string `json:"output"`
	WorkingDirectory string `json:"working_directory"`
}
type bashTool struct {
	permissions permission.Service
	workingDir  string
	attribution *config.Attribution
}

const (
	BashToolName = "bash"

	DefaultTimeout  = 1 * 60 * 1000  // 1 minutes in milliseconds
	MaxTimeout      = 10 * 60 * 1000 // 10 minutes in milliseconds
	MaxOutputLength = 30000
	BashNoOutput    = "no output"
)

//go:embed bash.md
var bashDescription []byte

var bashDescriptionTpl = template.Must(
	template.New("bashDescription").
		Parse(string(bashDescription)),
)

type bashDescriptionData struct {
	MaxOutputLength    int
	AttributionStep    string
	AttributionExample string
	PRAttribution      string
}

func (b *bashTool) bashDescription() string {

	// Build attribution text based on settings
	var attributionStep, attributionExample, prAttribution string

	// Default to true if attribution is nil (backward compatibility)
	generatedWith := b.attribution == nil || b.attribution.GeneratedWith
	coAuthoredBy := b.attribution == nil || b.attribution.CoAuthoredBy

	// Build PR attribution
	if generatedWith {
		prAttribution = "💘 Generated with Crush"
	}

	if generatedWith || coAuthoredBy {
		var attributionParts []string
		if generatedWith {
			attributionParts = append(attributionParts, "💘 Generated with Crush")
		}
		if coAuthoredBy {
			attributionParts = append(attributionParts, "Co-Authored-By: Crush <crush@charm.land>")
		}

		if len(attributionParts) > 0 {
			attributionStep = fmt.Sprintf("4. Create the commit with a message ending with:\n%s", strings.Join(attributionParts, "\n"))

			attributionText := strings.Join(attributionParts, "\n ")
			attributionExample = fmt.Sprintf(`<example>
git commit -m "$(cat <<'EOF'
 Commit message here.

 %s
 EOF
)"</example>`, attributionText)
		}
	}

	if attributionStep == "" {
		attributionStep = "4. Create the commit with your commit message."
		attributionExample = `<example>
git commit -m "$(cat <<'EOF'
 Commit message here.
 EOF
)"</example>`
	}

	var out bytes.Buffer
	if err := bashDescriptionTpl.Execute(&out, bashDescriptionData{

		MaxOutputLength:    MaxOutputLength,
		AttributionStep:    attributionStep,
		AttributionExample: attributionExample,
		PRAttribution:      prAttribution,
	}); err != nil {
		// this should never happen.
		panic("failed to execute bash description template: " + err.Error())
	}
	return out.String()
}

func blockFuncs() []shell.BlockFunc {
	return []shell.BlockFunc{}
}

func NewBashTool(permission permission.Service, workingDir string, attribution *config.Attribution) BaseTool {
	// Set up command blocking on the persistent shell
	persistentShell := shell.GetPersistentShell(workingDir)
	persistentShell.SetBlockFuncs(blockFuncs())

	return &bashTool{
		permissions: permission,
		workingDir:  workingDir,
		attribution: attribution,
	}
}

func (b *bashTool) Name() string {
	return BashToolName
}

func (b *bashTool) Info() ToolInfo {
	return ToolInfo{
		Name:        BashToolName,
		Description: b.bashDescription(),
		Parameters: map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The command to execute",
			},
			"timeout": map[string]any{
				"type":        "number",
				"description": "Optional timeout in milliseconds (max 600000)",
			},
		},
		Required: []string{"command"},
	}
}

func (b *bashTool) Run(ctx context.Context, call ToolCall) (ToolResponse, error) {
	var params BashParams
	if err := json.Unmarshal([]byte(call.Input), &params); err != nil {
		return NewTextErrorResponse("invalid parameters"), nil
	}

	if params.Timeout > MaxTimeout {
		params.Timeout = MaxTimeout
	} else if params.Timeout <= 0 {
		params.Timeout = DefaultTimeout
	}

	if params.Command == "" {
		return NewTextErrorResponse("missing command"), nil
	}

	isSafeReadOnly := false
	cmdLower := strings.ToLower(params.Command)

	for _, safe := range safeCommands {
		if strings.HasPrefix(cmdLower, safe) {
			if len(cmdLower) == len(safe) || cmdLower[len(safe)] == ' ' || cmdLower[len(safe)] == '-' {
				isSafeReadOnly = true
				break
			}
		}
	}

	sessionID, messageID := GetContextValues(ctx)
	if sessionID == "" || messageID == "" {
		return ToolResponse{}, fmt.Errorf("session ID and message ID are required for executing shell command")
	}
	if !isSafeReadOnly {
		shell := shell.GetPersistentShell(b.workingDir)
		p := b.permissions.Request(
			permission.CreatePermissionRequest{
				SessionID:   sessionID,
				Path:        shell.GetWorkingDir(),
				ToolCallID:  call.ID,
				ToolName:    BashToolName,
				Action:      "execute",
				Description: fmt.Sprintf("Execute command: %s", params.Command),
				Params: BashPermissionsParams{
					Command: params.Command,
				},
			},
		)
		if !p {
			return ToolResponse{}, permission.ErrorPermissionDenied
		}
	}
	startTime := time.Now()
	if params.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(params.Timeout)*time.Millisecond)
		defer cancel()
	}

	persistentShell := shell.GetPersistentShell(b.workingDir)
	stdout, stderr, err := persistentShell.Exec(ctx, params.Command)

	// Get the current working directory after command execution
	currentWorkingDir := persistentShell.GetWorkingDir()
	interrupted := shell.IsInterrupt(err)
	exitCode := shell.ExitCode(err)
	if exitCode == 0 && !interrupted && err != nil {
		return ToolResponse{}, fmt.Errorf("error executing command: %w", err)
	}

	stdout = truncateOutput(stdout)
	stderr = truncateOutput(stderr)

	errorMessage := stderr
	if errorMessage == "" && err != nil {
		errorMessage = err.Error()
	}

	if interrupted {
		if errorMessage != "" {
			errorMessage += "\n"
		}
		errorMessage += "Command was aborted before completion"
	} else if exitCode != 0 {
		if errorMessage != "" {
			errorMessage += "\n"
		}
		errorMessage += fmt.Sprintf("Exit code %d", exitCode)
	}

	hasBothOutputs := stdout != "" && stderr != ""

	if hasBothOutputs {
		stdout += "\n"
	}

	if errorMessage != "" {
		stdout += "\n" + errorMessage
	}

	metadata := BashResponseMetadata{
		StartTime:        startTime.UnixMilli(),
		EndTime:          time.Now().UnixMilli(),
		Output:           stdout,
		WorkingDirectory: currentWorkingDir,
	}
	if stdout == "" {
		return WithResponseMetadata(NewTextResponse(BashNoOutput), metadata), nil
	}
	stdout += fmt.Sprintf("\n\n<cwd>%s</cwd>", currentWorkingDir)
	return WithResponseMetadata(NewTextResponse(stdout), metadata), nil
}

func truncateOutput(content string) string {
	if len(content) <= MaxOutputLength {
		return content
	}

	halfLength := MaxOutputLength / 2
	start := content[:halfLength]
	end := content[len(content)-halfLength:]

	truncatedLinesCount := countLines(content[halfLength : len(content)-halfLength])
	return fmt.Sprintf("%s\n\n... [%d lines truncated] ...\n\n%s", start, truncatedLinesCount, end)
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	return len(strings.Split(s, "\n"))
}
