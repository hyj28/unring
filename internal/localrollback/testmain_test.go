package localrollback

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	_ = os.Setenv("UNRING_TEST_DISABLE_VOLUME_BACKSTOP", "1")
	home, err := os.MkdirTemp("", "unring-localrollback-test-home-*")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("HOME", home)
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
