package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
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

func TestLoginHint_UsesRoomdWhenItIsOnPath(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "roomd"), []byte("#!/bin/sh\n"), 0o755))
	t.Setenv("PATH", dir)

	hint := loginHint()
	assert.Contains(t, hint, "run `roomd login`")
	assert.NotContains(t, hint, "export PATH")
}

func TestLoginHint_NamesTheInstalledCopyOffPath(t *testing.T) {
	t.Setenv("ROOM_HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())

	hint := loginHint()
	assert.Contains(t, hint, "run `"+binPath()+" login`")
	assert.Contains(t, hint, `export PATH="`+filepath.Dir(binPath())+`:$PATH"`)
}

func TestAdhocSigned_ReadsUnsignedAndAdhocBinariesAsAdhoc(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("codesign is macOS only")
	}
	plain := filepath.Join(t.TempDir(), "roomd")
	require.NoError(t, os.WriteFile(plain, []byte("not signed"), 0o755))
	assert.True(t, adhocSigned(context.Background(), plain))

	self, err := os.Executable() // go test binaries are ad-hoc signed (or unsigned on amd64)
	require.NoError(t, err)
	assert.True(t, adhocSigned(context.Background(), self))
}
