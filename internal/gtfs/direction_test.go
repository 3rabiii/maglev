package gtfs

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"maglev.onebusaway.org/internal/appconf"
	"maglev.onebusaway.org/internal/models"
)

func TestCalculateCompassDirection(t *testing.T) {
	tests := []struct {
		name     string
		latDiff  float64
		lonDiff  float64
		expected string
	}{
		// Cardinal directions
		{name: "North", latDiff: 1.0, lonDiff: 0.0, expected: "N"},
		{name: "South", latDiff: -1.0, lonDiff: 0.0, expected: "S"},
		{name: "East", latDiff: 0.0, lonDiff: 1.0, expected: "E"},
		{name: "West", latDiff: 0.0, lonDiff: -1.0, expected: "W"},

		// Intercardinal directions
		{name: "NorthEast", latDiff: 1.0, lonDiff: 1.0, expected: "NE"},
		{name: "NorthWest", latDiff: 1.0, lonDiff: -1.0, expected: "NW"},
		{name: "SouthEast", latDiff: -1.0, lonDiff: 1.0, expected: "SE"},
		{name: "SouthWest", latDiff: -1.0, lonDiff: -1.0, expected: "SW"},

		// Zero deltas
		{name: "ZeroZero", latDiff: 0.0, lonDiff: 0.0, expected: "unknown"},

		// Near boundary cases
		{name: "AlmostNorth_22deg", latDiff: 0.93, lonDiff: 0.37, expected: "N"},
		{name: "AlmostEast_68deg", latDiff: 0.37, lonDiff: 0.93, expected: "E"},
		{name: "JustIntoNE_25deg", latDiff: 0.9, lonDiff: 0.47, expected: "NE"},

		// Small values
		{name: "SmallNorth", latDiff: 0.0001, lonDiff: 0.0, expected: "N"},
		{name: "SmallSouthWest", latDiff: -0.0001, lonDiff: -0.0001, expected: "SW"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := calculateCompassDirection(tt.latDiff, tt.lonDiff)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetDirectionForStop(t *testing.T) {
	gtfsConfig := Config{
		GtfsURL:      models.GetFixturePath(t, "raba.zip"),
		GTFSDataPath: ":memory:",
		Env:          appconf.Test,
	}
	manager, err := InitGTFSManager(gtfsConfig)
	assert.NoError(t, err)

	ctx := context.Background()

	t.Run("KnownStopReturnsDirection", func(t *testing.T) {
		direction := manager.GetDirectionForStop(ctx, "2000")
		validDirections := map[string]bool{
			"N": true, "NE": true, "E": true, "SE": true,
			"S": true, "SW": true, "W": true, "NW": true,
		}
		assert.True(t, validDirections[direction],
			"Expected a valid compass direction for stop 2000, got: %s", direction)
	})

	t.Run("NonExistentStopReturnsUnknown", func(t *testing.T) {
		direction := manager.GetDirectionForStop(ctx, "nonexistent_stop_xyz")
		assert.Equal(t, "unknown", direction)
	})

	t.Run("LastStopInTripMayReturnUnknown", func(t *testing.T) {
		// A stop that is the last stop on all its trips has no "next stop",
		// so the direction calculation would return unknown.
		// We test that it at least doesn't panic.
		direction := manager.GetDirectionForStop(ctx, "2605")
		validDirections := map[string]bool{
			"N": true, "NE": true, "E": true, "SE": true,
			"S": true, "SW": true, "W": true, "NW": true,
			"unknown": true,
		}
		assert.True(t, validDirections[direction],
			"Expected a valid direction or unknown for terminal stop, got: %s", direction)
	})
}
