//go:build !anchordev

package main

import "testing"

func TestProductionBuildExcludesIdentitySimulation(t *testing.T) {
	if developmentIdentities != nil {
		t.Fatal("production build contains identity simulation")
	}
}
