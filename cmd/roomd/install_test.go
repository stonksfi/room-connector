package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteMarketplace_LaysOutAnInstallablePlugin(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, writeMarketplace(dir))

	raw, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "marketplace.json"))
	require.NoError(t, err)
	var m struct {
		Name    string `json:"name"`
		Plugins []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"plugins"`
	}
	require.NoError(t, json.Unmarshal(raw, &m))
	assert.Equal(t, marketplaceName, m.Name)
	require.Len(t, m.Plugins, 1)
	assert.Equal(t, "./plugins/"+pluginName, m.Plugins[0].Source)

	plugin := filepath.Join(dir, "plugins", pluginName)
	for _, f := range []string{".claude-plugin/plugin.json", "hooks/hooks.json", "hooks/register.ts", "README.md"} {
		assert.FileExists(t, filepath.Join(plugin, f))
	}
	info, err := os.Stat(filepath.Join(plugin, "bin", "room-emit"))
	require.NoError(t, err)
	assert.NotZero(t, info.Mode().Perm()&0o100, "room-emit must be executable")
	assert.NoDirExists(t, filepath.Join(plugin, ".claude-plugin", "types"), "development types are not shipped")

	// A reinstall replaces the plugin in place.
	require.NoError(t, writeMarketplace(dir))
}

func TestInstallFile_CopiesThroughARename(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "roomd-screen")
	require.NoError(t, os.WriteFile(src, []byte("binary"), 0o755))
	dst := filepath.Join(dir, "bin", "roomd-screen")

	require.NoError(t, installFile(context.Background(), src, dst, "com.stonksfi.roomd-screen", ""))
	raw, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, "binary", string(raw))
	info, err := os.Stat(dst)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
	assert.NoFileExists(t, dst+".new")

	require.NoError(t, installFile(context.Background(), dst, dst, "com.stonksfi.roomd-screen", ""), "installing the installed copy is a no-op")
}

func TestWaitUnloaded_ReportsTheJobGone(t *testing.T) {
	calls := 0
	gone := waitUnloaded(context.Background(), func() bool { calls++; return calls < 3 }, time.Second, time.Millisecond)
	assert.True(t, gone)
	assert.Equal(t, 3, calls)
}

func TestWaitUnloaded_GivesUpAtTheLimit(t *testing.T) {
	start := time.Now()
	gone := waitUnloaded(context.Background(), func() bool { return true }, 20*time.Millisecond, time.Millisecond)
	assert.False(t, gone)
	assert.GreaterOrEqual(t, time.Since(start), 20*time.Millisecond)
}
