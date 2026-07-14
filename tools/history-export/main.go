package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"
)

const defaultPickleKey = "maunium.net/go/mautrix-whatsapp"

var nowUTC = func() time.Time { return time.Now().UTC() }

type commandOptions struct {
	BridgeConfig                  string
	SynapseConfig                 string
	Manifest                      string
	OutputDir                     string
	AccountID                     string
	PickleKey                     string
	Timeout                       time.Duration
	AllowPartialSessions          bool
	AllowHistoryVisibilityChanges bool
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: matrix-history-export {inventory|validate|export} [options]")
	}
	mode := args[0]
	if mode != "inventory" && mode != "validate" && mode != "export" {
		return fmt.Errorf("unknown mode %q", mode)
	}

	flags := flag.NewFlagSet(mode, flag.ContinueOnError)
	options := commandOptions{}
	flags.StringVar(&options.BridgeConfig, "bridge-config", "/config/bridge.yaml", "mautrix-discord config path")
	flags.StringVar(&options.SynapseConfig, "synapse-config", "/config/homeserver.yaml", "Synapse config path")
	flags.StringVar(&options.Manifest, "manifest", "", "authorization manifest path")
	flags.StringVar(&options.OutputDir, "output-dir", "/output", "export output directory")
	flags.StringVar(&options.AccountID, "account-id", "", "bridge crypto account ID")
	flags.StringVar(&options.PickleKey, "pickle-key", defaultPickleKey, "bridge crypto pickle key")
	flags.DurationVar(&options.Timeout, "timeout", 10*time.Minute, "overall database operation timeout")
	flags.BoolVar(&options.AllowPartialSessions, "allow-partial-sessions", false, "export sessions whose earliest retained index is greater than zero")
	flags.BoolVar(&options.AllowHistoryVisibilityChanges, "allow-history-visibility-changes", false, "export rooms that previously had restrictive history visibility")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if mode != "inventory" && options.Manifest == "" {
		return fmt.Errorf("--manifest is required for %s mode", mode)
	}

	bridgeURI, synapseURI, err := loadDatabaseURIs(options.BridgeConfig, options.SynapseConfig)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), options.Timeout)
	defer cancel()
	bridgeDB, err := openDatabase(ctx, bridgeURI)
	if err != nil {
		return fmt.Errorf("connect to bridge database: %w", err)
	}
	defer bridgeDB.Close()
	synapseDB, err := openDatabase(ctx, synapseURI)
	if err != nil {
		return fmt.Errorf("connect to Synapse database: %w", err)
	}
	defer synapseDB.Close()

	identity, err := loadBridgeIdentity(ctx, bridgeDB, options.AccountID, []byte(options.PickleKey))
	if err != nil {
		return err
	}
	sessions, err := loadSessions(ctx, bridgeDB, options.AccountID)
	if err != nil {
		return fmt.Errorf("load bridge sessions: %w", err)
	}
	portals, err := loadBridgePortals(ctx, bridgeDB)
	if err != nil {
		return fmt.Errorf("load bridge portals: %w", err)
	}
	if err := enrichMatrixRoomState(ctx, synapseDB, portals); err != nil {
		return fmt.Errorf("load Matrix room state: %w", err)
	}

	if mode == "inventory" {
		return writeJSON(os.Stdout, buildInventory(identity, sessions, portals))
	}

	entries, err := loadManifest(options.Manifest)
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}
	roomIDs, userIDs := manifestIDs(entries)
	references, err := loadEventReferences(ctx, synapseDB, roomIDs)
	if err != nil {
		return fmt.Errorf("load encrypted event references: %w", err)
	}
	memberships, err := loadMemberships(ctx, synapseDB, roomIDs, userIDs)
	if err != nil {
		return fmt.Errorf("load memberships: %w", err)
	}
	validated, err := validateUsers(
		entries,
		identity,
		sessions,
		portals,
		references,
		memberships,
		[]byte(options.PickleKey),
		validationOptions{
			AllowPartialSessions:          options.AllowPartialSessions,
			AllowHistoryVisibilityChanges: options.AllowHistoryVisibilityChanges,
		},
	)
	if err != nil {
		return err
	}

	if mode == "validate" {
		report := migrationReport{GeneratedAt: nowUTC(), Mode: "validate"}
		for _, user := range validated {
			report.Users = append(report.Users, user.Report)
		}
		return writeJSON(os.Stdout, report)
	}
	report, err := writePackages(options.OutputDir, validated)
	if err != nil {
		return err
	}
	return writeJSON(os.Stdout, report)
}

func manifestIDs(entries []manifestEntry) ([]string, []string) {
	roomSet := make(map[string]struct{})
	userIDs := make([]string, 0, len(entries))
	for _, entry := range entries {
		userIDs = append(userIDs, entry.UserID)
		for _, roomID := range entry.Rooms {
			roomSet[roomID] = struct{}{}
		}
	}
	roomIDs := make([]string, 0, len(roomSet))
	for roomID := range roomSet {
		roomIDs = append(roomIDs, roomID)
	}
	sort.Strings(roomIDs)
	sort.Strings(userIDs)
	return roomIDs, userIDs
}

func writeJSON(target *os.File, value any) error {
	encoder := json.NewEncoder(target)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
