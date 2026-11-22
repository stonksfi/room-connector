package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTokenFile_RoundTripAndRejectsMalformedTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	t.Setenv("ROOM_TOKEN_FILE", path)
	prev := useKeychain
	useKeychain = false
	defer func() { useKeychain = prev }()

	_, err := LoadToken()
	assert.ErrorIs(t, err, ErrNoToken)

	require.NoError(t, os.WriteFile(path, []byte("old"), 0o644))
	require.NoError(t, SaveToken("rmd_Jv3abcdefghijklmnop"))
	got, err := LoadToken()
	require.NoError(t, err)
	assert.Equal(t, "rmd_Jv3abcdefghijklmnop", got)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "an existing file is tightened")

	assert.Error(t, SaveToken("rmd_x -w evil\nadd-generic-password"), "nothing that could inject a keychain command")

	require.NoError(t, DeleteToken())
	require.NoError(t, DeleteToken(), "deleting twice is fine")
	_, err = LoadToken()
	assert.ErrorIs(t, err, ErrNoToken)
}
