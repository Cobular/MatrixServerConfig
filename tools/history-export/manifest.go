package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var matrixUserIDPattern = regexp.MustCompile(`^@[^:[:space:]]+:[^[:space:]]+$`)

type manifest struct {
	Version       int                 `yaml:"version"`
	SharedHistory bool                `yaml:"shared_history"`
	Users         map[string]userSpec `yaml:"users"`
}

type userSpec struct {
	Rooms         []string `yaml:"rooms"`
	SharedHistory *bool    `yaml:"shared_history"`
}

type manifestEntry struct {
	UserID        string
	Rooms         []string
	SharedHistory bool
}

func loadManifest(path string) ([]manifestEntry, error) {
	var source manifest
	if err := decodeStrictYAMLFile(path, &source); err != nil {
		return nil, err
	}
	if source.Version != 1 {
		return nil, fmt.Errorf("unsupported manifest version %d (expected 1)", source.Version)
	}
	if len(source.Users) == 0 {
		return nil, fmt.Errorf("manifest has no users")
	}

	entries := make([]manifestEntry, 0, len(source.Users))
	for userID, spec := range source.Users {
		if !matrixUserIDPattern.MatchString(userID) {
			return nil, fmt.Errorf("invalid Matrix user ID %q", userID)
		}
		if len(spec.Rooms) == 0 {
			return nil, fmt.Errorf("user %s has no rooms", userID)
		}

		seen := make(map[string]struct{}, len(spec.Rooms))
		rooms := make([]string, 0, len(spec.Rooms))
		for _, roomID := range spec.Rooms {
			roomID = strings.TrimSpace(roomID)
			if !strings.HasPrefix(roomID, "!") || !strings.Contains(roomID, ":") {
				return nil, fmt.Errorf("user %s has invalid room ID %q", userID, roomID)
			}
			if _, duplicate := seen[roomID]; duplicate {
				return nil, fmt.Errorf("user %s lists room %s more than once", userID, roomID)
			}
			seen[roomID] = struct{}{}
			rooms = append(rooms, roomID)
		}
		sort.Strings(rooms)

		sharedHistory := source.SharedHistory
		if spec.SharedHistory != nil {
			sharedHistory = *spec.SharedHistory
		}
		entries = append(entries, manifestEntry{
			UserID:        userID,
			Rooms:         rooms,
			SharedHistory: sharedHistory,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].UserID < entries[j].UserID })
	return entries, nil
}
