package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/lib/pq"
	"maunium.net/go/mautrix/crypto/olm"
)

type bridgeIdentity struct {
	DeviceID   string
	SenderKey  string
	SigningKey string
}

type sessionRow struct {
	RoomID          string
	SessionID       string
	SenderKey       string
	SigningKey      string
	Pickle          []byte
	ForwardingChain []string
}

type roomInfo struct {
	RoomID            string
	Name              string
	BridgeEncrypted   bool
	DiscordState      bool
	MatrixEncrypted   bool
	CurrentVisibility string
	EverRestricted    bool
}

type eventReference struct {
	RoomID    string
	SessionID string
	SenderKey string
	Count     int
}

func openDatabase(ctx context.Context, dsn string) (*sql.DB, error) {
	database, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("initialize PostgreSQL client")
	}
	if err := database.PingContext(ctx); err != nil {
		database.Close()
		return nil, fmt.Errorf("PostgreSQL connection failed")
	}
	return database, nil
}

func loadBridgeIdentity(ctx context.Context, database *sql.DB, accountID string, pickleKey []byte) (bridgeIdentity, error) {
	var accountPickle []byte
	var deviceID string
	err := database.QueryRowContext(ctx, `
		SELECT account, device_id
		FROM crypto_account
		WHERE account_id=$1
	`, accountID).Scan(&accountPickle, &deviceID)
	if err != nil {
		return bridgeIdentity{}, fmt.Errorf("load bridge crypto account: %w", err)
	}
	account, err := olm.AccountFromPickled(accountPickle, pickleKey)
	if err != nil {
		return bridgeIdentity{}, fmt.Errorf("unpickle bridge crypto account: %w", err)
	}
	signingKey, senderKey := account.IdentityKeys()
	return bridgeIdentity{
		DeviceID:   deviceID,
		SenderKey:  senderKey.String(),
		SigningKey: signingKey.String(),
	}, nil
}

func loadSessions(ctx context.Context, database *sql.DB, accountID string) ([]sessionRow, error) {
	rows, err := database.QueryContext(ctx, `
		SELECT room_id, session_id, sender_key, signing_key, session,
		       COALESCE(forwarding_chains, '')
		FROM crypto_megolm_inbound_session
		WHERE account_id=$1 AND session IS NOT NULL
		ORDER BY room_id, session_id
	`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []sessionRow
	for rows.Next() {
		var item sessionRow
		var signingKey sql.NullString
		var forwardingChains string
		if err := rows.Scan(
			&item.RoomID,
			&item.SessionID,
			&item.SenderKey,
			&signingKey,
			&item.Pickle,
			&forwardingChains,
		); err != nil {
			return nil, err
		}
		item.SigningKey = signingKey.String
		if forwardingChains != "" {
			item.ForwardingChain = strings.Split(forwardingChains, ",")
		}
		sessions = append(sessions, item)
	}
	return sessions, rows.Err()
}

func loadBridgePortals(ctx context.Context, database *sql.DB) (map[string]roomInfo, error) {
	rows, err := database.QueryContext(ctx, `
		SELECT mxid, COALESCE(name, ''), encrypted
		FROM portal
		WHERE mxid IS NOT NULL AND mxid <> ''
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	portals := make(map[string]roomInfo)
	for rows.Next() {
		var item roomInfo
		if err := rows.Scan(&item.RoomID, &item.Name, &item.BridgeEncrypted); err != nil {
			return nil, err
		}
		portals[item.RoomID] = item
	}
	return portals, rows.Err()
}

func enrichMatrixRoomState(ctx context.Context, database *sql.DB, portals map[string]roomInfo) error {
	rows, err := database.QueryContext(ctx, `
		SELECT DISTINCT ON (bridge.room_id)
		       bridge.room_id,
		       COALESCE(name_json.json::jsonb #>> '{content,name}', ''),
		       COALESCE(history_json.json::jsonb #>> '{content,history_visibility}', 'shared'),
		       encryption.event_id IS NOT NULL,
		       bridge_json.json::jsonb #>> '{content,protocol,id}'
		FROM current_state_events bridge
		JOIN event_json bridge_json USING (event_id)
		LEFT JOIN current_state_events name
		  ON name.room_id=bridge.room_id AND name.type='m.room.name' AND name.state_key=''
		LEFT JOIN event_json name_json ON name_json.event_id=name.event_id
		LEFT JOIN current_state_events history
		  ON history.room_id=bridge.room_id AND history.type='m.room.history_visibility' AND history.state_key=''
		LEFT JOIN event_json history_json ON history_json.event_id=history.event_id
		LEFT JOIN current_state_events encryption
		  ON encryption.room_id=bridge.room_id AND encryption.type='m.room.encryption' AND encryption.state_key=''
		WHERE bridge.type='m.bridge'
		ORDER BY bridge.room_id, bridge.state_key
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var roomID, name, visibility string
		var encrypted bool
		var protocolID sql.NullString
		if err := rows.Scan(&roomID, &name, &visibility, &encrypted, &protocolID); err != nil {
			return err
		}
		item, exists := portals[roomID]
		if !exists {
			continue
		}
		if item.Name == "" {
			item.Name = name
		}
		item.CurrentVisibility = visibility
		item.MatrixEncrypted = encrypted
		item.DiscordState = protocolID.String == "discordgo"
		portals[roomID] = item
	}
	if err := rows.Err(); err != nil {
		return err
	}

	historyRows, err := database.QueryContext(ctx, `
		SELECT DISTINCT events.room_id,
		       event_json.json::jsonb #>> '{content,history_visibility}'
		FROM events
		JOIN event_json USING (event_id)
		WHERE events.type='m.room.history_visibility'
	`)
	if err != nil {
		return err
	}
	defer historyRows.Close()
	for historyRows.Next() {
		var roomID string
		var visibility sql.NullString
		if err := historyRows.Scan(&roomID, &visibility); err != nil {
			return err
		}
		item, exists := portals[roomID]
		if !exists {
			continue
		}
		if visibility.String == "joined" || visibility.String == "invited" || !visibility.Valid {
			item.EverRestricted = true
			portals[roomID] = item
		}
	}
	return historyRows.Err()
}

func loadEventReferences(ctx context.Context, database *sql.DB, roomIDs []string) (map[string]eventReference, error) {
	if len(roomIDs) == 0 {
		return map[string]eventReference{}, nil
	}
	rows, err := database.QueryContext(ctx, `
		SELECT events.room_id,
		       event_json.json::jsonb #>> '{content,session_id}' AS session_id,
		       event_json.json::jsonb #>> '{content,sender_key}' AS sender_key,
		       COUNT(*)
		FROM events
		JOIN event_json USING (event_id)
		WHERE events.type='m.room.encrypted' AND events.room_id=ANY($1)
		GROUP BY events.room_id, session_id, sender_key
	`, pq.Array(roomIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	references := make(map[string]eventReference)
	for rows.Next() {
		var item eventReference
		var sessionID, senderKey sql.NullString
		if err := rows.Scan(&item.RoomID, &sessionID, &senderKey, &item.Count); err != nil {
			return nil, err
		}
		item.SessionID = sessionID.String
		item.SenderKey = senderKey.String
		if item.SessionID != "" {
			references[sessionMapKey(item.RoomID, item.SessionID)] = item
		}
	}
	return references, rows.Err()
}

func loadMemberships(ctx context.Context, database *sql.DB, roomIDs, userIDs []string) (map[string]string, error) {
	rows, err := database.QueryContext(ctx, `
		SELECT state.room_id, state.state_key,
		       event_json.json::jsonb #>> '{content,membership}'
		FROM current_state_events state
		JOIN event_json USING (event_id)
		WHERE state.type='m.room.member'
		  AND state.room_id=ANY($1)
		  AND state.state_key=ANY($2)
	`, pq.Array(roomIDs), pq.Array(userIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	memberships := make(map[string]string)
	for rows.Next() {
		var roomID, userID string
		var membership sql.NullString
		if err := rows.Scan(&roomID, &userID, &membership); err != nil {
			return nil, err
		}
		memberships[membershipMapKey(userID, roomID)] = membership.String
	}
	return memberships, rows.Err()
}

func sessionMapKey(roomID, sessionID string) string {
	return roomID + "\x00" + sessionID
}

func membershipMapKey(userID, roomID string) string {
	return userID + "\x00" + roomID
}
