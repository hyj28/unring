package cli

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

type goCacheEnvironment struct {
	build  string
	module string
}

// Package variables are initialized before TestMain, so cache discovery cannot
// be redirected by the fabricated HOME regardless of TestMain statement order.
var inheritedGoCaches = captureGoCacheEnvironment()

func captureGoCacheEnvironment() goCacheEnvironment {
	return goCacheEnvironment{
		build:  resolveGoCache("GOCACHE"),
		module: resolveGoCache("GOMODCACHE"),
	}
}

func resolveGoCache(name string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	output, err := exec.Command("go", "env", name).Output()
	if err != nil {
		panic("find existing " + name + ": " + err.Error())
	}
	value := strings.TrimSpace(string(output))
	if value == "" {
		panic("find existing " + name + ": go env returned an empty path")
	}
	return value
}

func TestMain(m *testing.M) {
	_ = os.Setenv("UNRING_TEST_DISABLE_VOLUME_BACKSTOP", "1")
	home, err := os.MkdirTemp("", "unring-cli-test-home-*")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("GOCACHE", inheritedGoCaches.build)
	_ = os.Setenv("GOMODCACHE", inheritedGoCaches.module)
	_ = os.Setenv("HOME", home)
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
