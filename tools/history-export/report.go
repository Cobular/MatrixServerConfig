package main

import "time"

type inventoryReport struct {
	GeneratedAt             time.Time       `json:"generated_at"`
	BridgeDeviceID          string          `json:"bridge_device_id"`
	RetainedSessions        int             `json:"retained_sessions"`
	BridgeOwnedSessions     int             `json:"bridge_owned_sessions"`
	OtherSenderSessions     int             `json:"other_sender_sessions"`
	RoomsWithOwnedSessions  int             `json:"rooms_with_owned_sessions"`
	EncryptedDiscordPortals int             `json:"encrypted_discord_portals"`
	Rooms                   []inventoryRoom `json:"rooms"`
}

type inventoryRoom struct {
	RoomID         string `json:"room_id"`
	Name           string `json:"name"`
	OwnedSessions  int    `json:"owned_sessions"`
	CurrentHistory string `json:"current_history_visibility"`
	EverRestricted bool   `json:"ever_restrictive_history"`
}

type migrationReport struct {
	GeneratedAt time.Time    `json:"generated_at"`
	Mode        string       `json:"mode"`
	Users       []userReport `json:"users"`
}

type userReport struct {
	UserID          string       `json:"user_id"`
	SharedHistory   bool         `json:"shared_history"`
	Rooms           []roomReport `json:"rooms"`
	SessionCount    int          `json:"session_count"`
	PartialSessions int          `json:"partial_sessions"`
	PackageFile     string       `json:"package_file,omitempty"`
	PassphraseFile  string       `json:"passphrase_file,omitempty"`
	PackageSHA256   string       `json:"package_sha256,omitempty"`
}

type roomReport struct {
	RoomID        string `json:"room_id"`
	Name          string `json:"name"`
	SessionCount  int    `json:"session_count"`
	PartialCount  int    `json:"partial_sessions"`
	EncryptedRefs int    `json:"encrypted_event_references"`
}
