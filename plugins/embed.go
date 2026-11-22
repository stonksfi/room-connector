// Package plugins embeds the agent plugins roomd installs.
package plugins

import "embed"

// ClaudeCode is the Room plugin for Claude Code, rooted at "claude-code". The
// types Claude Code lays in .claude-plugin/types while developing are left out.
//
//go:embed claude-code/.claude-plugin/plugin.json claude-code/hooks claude-code/bin claude-code/README.md
var ClaudeCode embed.FS
