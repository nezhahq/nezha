package model

import (
	"path/filepath"
	"sync"
	"testing"
)

func TestJWTIPChangeAllowedPersistence(t *testing.T) {
	c := &Config{}
	if c.JWTIPChangeAllowed() {
		t.Fatal("zero-value Config must bind JWT sessions to their IP")
	}
	// An absent agent secret makes Read save the config during initialization.
	path := newTempConfig(t, "jwt_secret_key: test\nallow_jwt_ip_change: true\n")
	if err := c.Read(path, nil); err != nil {
		t.Fatal(err)
	}
	if !c.JWTIPChangeAllowed() {
		t.Fatal("Read must initialize the atomic mirror before saving")
	}
	for _, want := range []bool{true, false} {
		c.SetJWTIPChangeAllowed(want)
		if err := c.Save(); err != nil {
			t.Fatal(err)
		}
		reloaded := &Config{}
		if err := reloaded.Read(path, nil); err != nil {
			t.Fatal(err)
		}
		if c.AllowJWTIPChange != want || reloaded.JWTIPChangeAllowed() != want {
			t.Fatalf("saved and reloaded setting must be %v", want)
		}
	}
}

func TestJWTIPChangeAllowedConcurrentAccess(t *testing.T) {
	c := &Config{filePath: filepath.Join(t.TempDir(), "config.yaml")}
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; i < 10000; i++ {
			c.SetJWTIPChangeAllowed(i%2 == 0)
		}
	})
	wg.Go(func() {
		for i := 0; i < 10000; i++ {
			_ = c.JWTIPChangeAllowed()
		}
	})
	for i := 0; i < 20; i++ {
		if err := c.Save(); err != nil {
			t.Error(err)
		}
	}
	wg.Wait()
}
