package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"maunium.net/go/mautrix/crypto/olm"
)

type validationOptions struct {
	AllowPartialSessions          bool
	AllowHistoryVisibilityChanges bool
}

type validatedUser struct {
	Report   userReport
	Sessions []exportedSession
}

func buildInventory(identity bridgeIdentity, sessions []sessionRow, portals map[string]roomInfo) inventoryReport {
	report := inventoryReport{
		GeneratedAt:      nowUTC(),
		BridgeDeviceID:   identity.DeviceID,
		RetainedSessions: len(sessions),
	}
	roomCounts := make(map[string]int)
	for _, session := range sessions {
		if session.SenderKey == identity.SenderKey {
			report.BridgeOwnedSessions++
			roomCounts[session.RoomID]++
		} else {
			report.OtherSenderSessions++
		}
	}
	for _, portal := range portals {
		if portal.BridgeEncrypted && portal.MatrixEncrypted && portal.DiscordState {
			report.EncryptedDiscordPortals++
		}
	}
	for roomID, count := range roomCounts {
		portal := portals[roomID]
		report.Rooms = append(report.Rooms, inventoryRoom{
			RoomID:         roomID,
			Name:           portal.Name,
			OwnedSessions:  count,
			CurrentHistory: portal.CurrentVisibility,
			EverRestricted: portal.EverRestricted,
		})
	}
	report.RoomsWithOwnedSessions = len(report.Rooms)
	sort.Slice(report.Rooms, func(i, j int) bool {
		return report.Rooms[i].RoomID < report.Rooms[j].RoomID
	})
	return report
}

func validateUsers(
	entries []manifestEntry,
	identity bridgeIdentity,
	sessionRows []sessionRow,
	portals map[string]roomInfo,
	references map[string]eventReference,
	memberships map[string]string,
	pickleKey []byte,
	options validationOptions,
) ([]validatedUser, error) {
	sessionsByRoom := make(map[string][]sessionRow)
	for _, session := range sessionRows {
		if session.SenderKey == identity.SenderKey {
			sessionsByRoom[session.RoomID] = append(sessionsByRoom[session.RoomID], session)
		}
	}

	validated := make([]validatedUser, 0, len(entries))
	for _, entry := range entries {
		result := validatedUser{Report: userReport{
			UserID:        entry.UserID,
			SharedHistory: entry.SharedHistory,
		}}
		for _, roomID := range entry.Rooms {
			portal, exists := portals[roomID]
			if !exists {
				return nil, fmt.Errorf("user %s: room %s is not a bridge portal", entry.UserID, roomID)
			}
			if !portal.BridgeEncrypted || !portal.MatrixEncrypted || !portal.DiscordState {
				return nil, fmt.Errorf("user %s: room %s is not an encrypted Discord portal", entry.UserID, roomID)
			}
			if portal.CurrentVisibility != "shared" && portal.CurrentVisibility != "world_readable" {
				return nil, fmt.Errorf("user %s: room %s has non-shareable history visibility %q", entry.UserID, roomID, portal.CurrentVisibility)
			}
			if portal.EverRestricted && !options.AllowHistoryVisibilityChanges {
				return nil, fmt.Errorf("user %s: room %s has previously used restrictive history visibility", entry.UserID, roomID)
			}
			if memberships[membershipMapKey(entry.UserID, roomID)] != "join" {
				return nil, fmt.Errorf("user %s is not currently joined to room %s", entry.UserID, roomID)
			}
			if len(sessionsByRoom[roomID]) == 0 {
				return nil, fmt.Errorf("user %s: room %s has no retained sessions owned by the bridge", entry.UserID, roomID)
			}

			retainedSessionIDs := make(map[string]struct{}, len(sessionsByRoom[roomID]))
			for _, row := range sessionsByRoom[roomID] {
				retainedSessionIDs[row.SessionID] = struct{}{}
			}
			for _, reference := range references {
				if reference.RoomID != roomID || reference.SenderKey != identity.SenderKey {
					continue
				}
				if _, retained := retainedSessionIDs[reference.SessionID]; !retained {
					return nil, fmt.Errorf("room %s references bridge session %s, but its key is not retained", roomID, reference.SessionID)
				}
			}

			roomResult := roomReport{RoomID: roomID, Name: portal.Name}
			for _, row := range sessionsByRoom[roomID] {
				if row.SigningKey != identity.SigningKey {
					return nil, fmt.Errorf("room %s session %s has an unexpected signing key", roomID, row.SessionID)
				}
				session, err := olm.InboundGroupSessionFromPickled(append([]byte(nil), row.Pickle...), pickleKey)
				if err != nil {
					return nil, fmt.Errorf("unpickle room %s session %s: %w", roomID, row.SessionID, err)
				}
				if session.ID().String() != row.SessionID {
					return nil, fmt.Errorf("room %s session ID mismatch: database=%s internal=%s", roomID, row.SessionID, session.ID())
				}
				firstKnownIndex := session.FirstKnownIndex()
				if firstKnownIndex != 0 {
					roomResult.PartialCount++
					result.Report.PartialSessions++
					if !options.AllowPartialSessions {
						return nil, fmt.Errorf("room %s session %s starts at message index %d; rerun with --allow-partial-sessions to export partial history", roomID, row.SessionID, firstKnownIndex)
					}
				}
				reference, exists := references[sessionMapKey(roomID, row.SessionID)]
				if !exists || reference.Count == 0 {
					return nil, fmt.Errorf("room %s session %s has no encrypted event reference", roomID, row.SessionID)
				}
				if reference.SenderKey != identity.SenderKey {
					return nil, fmt.Errorf("room %s session %s event sender key does not match bridge identity", roomID, row.SessionID)
				}
				exportedKey, err := session.Export(firstKnownIndex)
				if err != nil {
					return nil, fmt.Errorf("export room %s session %s: %w", roomID, row.SessionID, err)
				}
				result.Sessions = append(result.Sessions, exportedSession{
					Algorithm:         "m.megolm.v1.aes-sha2",
					ForwardingChains:  []string{},
					RoomID:            roomID,
					SenderKey:         identity.SenderKey,
					SenderClaimedKeys: senderClaimedKeys{Ed25519: identity.SigningKey},
					SessionID:         row.SessionID,
					SessionKey:        string(exportedKey),
					SharedHistory:     entry.SharedHistory,
				})
				roomResult.SessionCount++
				roomResult.EncryptedRefs += reference.Count
				result.Report.SessionCount++
			}
			result.Report.Rooms = append(result.Report.Rooms, roomResult)
		}
		if result.Report.SessionCount == 0 {
			return nil, fmt.Errorf("user %s has no retained bridge sessions in the selected rooms", entry.UserID)
		}
		validated = append(validated, result)
	}
	return validated, nil
}

var filenameUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func packageBaseName(userID string) string {
	base := strings.TrimPrefix(userID, "@")
	base = filenameUnsafe.ReplaceAllString(base, "_")
	digest := sha256.Sum256([]byte(userID))
	return fmt.Sprintf("%s-%x", base, digest[:4])
}

func writePackages(outputDir string, users []validatedUser) (migrationReport, error) {
	report := migrationReport{GeneratedAt: nowUTC(), Mode: "export"}
	packagesDir := filepath.Join(outputDir, "packages")
	passphrasesDir := filepath.Join(outputDir, "passphrases")
	if err := os.MkdirAll(packagesDir, 0o700); err != nil {
		return report, err
	}
	if err := os.MkdirAll(passphrasesDir, 0o700); err != nil {
		return report, err
	}
	if err := os.Chmod(outputDir, 0o700); err != nil {
		return report, err
	}

	for _, user := range users {
		passphrase, err := generatePassphrase()
		if err != nil {
			return report, err
		}
		data, err := encryptKeyExport(passphrase, user.Sessions)
		if err != nil {
			return report, fmt.Errorf("encrypt package for %s: %w", user.Report.UserID, err)
		}
		base := packageBaseName(user.Report.UserID)
		packageName := base + "-element-keys.txt"
		passphraseName := base + "-passphrase.txt"
		packagePath := filepath.Join(packagesDir, packageName)
		passphrasePath := filepath.Join(passphrasesDir, passphraseName)
		if err := writeExclusive(packagePath, data); err != nil {
			return report, err
		}
		if err := writeExclusive(passphrasePath, []byte(passphrase+"\n")); err != nil {
			return report, err
		}
		digest := sha256.Sum256(data)
		item := user.Report
		item.PackageFile = filepath.Join("packages", packageName)
		item.PassphraseFile = filepath.Join("passphrases", passphraseName)
		item.PackageSHA256 = hex.EncodeToString(digest[:])
		report.Users = append(report.Users, item)
	}

	reportPath := filepath.Join(outputDir, "report.json")
	reportData, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return report, err
	}
	reportData = append(reportData, '\n')
	if err := writeExclusive(reportPath, reportData); err != nil {
		return report, err
	}
	return report, nil
}

func writeExclusive(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return file.Sync()
}
