package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/stonksfi/room-connector/internal/store"
)

func TestTokenCache_PicksUpLoginAndSurvivesKeychainHiccups(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	answer, err := "", error(store.ErrNoToken)
	calls := 0
	c := &tokenCache{
		load: func() (string, error) { calls++; return answer, err },
		now:  func() time.Time { return now },
	}
	assert.Equal(t, "", c.get())
	assert.Equal(t, "", c.get())
	assert.Equal(t, 1, calls, "not looked up again within tokenRetry")

	answer, err = "rmd_abc", nil // `roomd login` ran
	now = now.Add(tokenRetry + time.Second)
	assert.Equal(t, "rmd_abc", c.get())

	answer, err = "", errors.New("keychain locked")
	now = now.Add(tokenFresh + time.Second)
	assert.Equal(t, "rmd_abc", c.get(), "a failed read keeps the last good token")

	answer, err = "", store.ErrNoToken // `roomd logout` ran
	now = now.Add(tokenFresh + time.Second)
	assert.Equal(t, "", c.get())
}

func TestTokenCache_ForgetsAtLogout(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	answer, err := "rmd_abc", error(nil)
	c := &tokenCache{load: func() (string, error) { return answer, err }, now: func() time.Time { return now }}
	assert.Equal(t, "rmd_abc", c.get())

	answer, err = "", store.ErrNoToken
	c.forget() // `roomd logout` deleted it and told roomd
	assert.Equal(t, "", c.get(), "not kept for tokenFresh")
	now = now.Add(tokenRetry + time.Second)
	assert.Equal(t, "", c.get())
}
