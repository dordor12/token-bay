package settingsjson

import "errors"

// Sentinel errors for categorical refusals. Callers use errors.Is to classify.
var (
	ErrIncompatibleJSONCComments = errors.New("settingsjson: settings.json contains JSONC comments that Token-Bay v1 cannot preserve")
	ErrPreExistingRedirect       = errors.New("settingsjson: ANTHROPIC_BASE_URL is already set to a non-Token-Bay value")
	ErrBedrockProvider           = errors.New("settingsjson: CLAUDE_CODE_USE_BEDROCK is truthy; ANTHROPIC_BASE_URL would be ignored")
	ErrVertexProvider            = errors.New("settingsjson: CLAUDE_CODE_USE_VERTEX is truthy; ANTHROPIC_BASE_URL would be ignored")
	ErrFoundryProvider           = errors.New("settingsjson: CLAUDE_CODE_USE_FOUNDRY is truthy; ANTHROPIC_BASE_URL would be ignored")
	ErrSymlinkEscapesClaudeDir   = errors.New("settingsjson: settings.json symlink target escapes ~/.claude/")
	ErrSettingsNotWritable       = errors.New("settingsjson: settings.json target is not writable")
)
