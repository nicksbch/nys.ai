package main

import "testing"

// The config that deploy.sh installs on the server.
func TestDeployedConfig(t *testing.T) {
	c, err := loadConfig("duckpipe.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c.Lake.Catalog == "" || len(c.Pipes) == 0 {
		t.Errorf("config = %+v", c)
	}
}
