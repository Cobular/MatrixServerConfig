package main

import (
	"fmt"
	"net/url"
	"os"

	"gopkg.in/yaml.v3"
)

type bridgeConfig struct {
	AppService struct {
		Database struct {
			URI string `yaml:"uri"`
		} `yaml:"database"`
	} `yaml:"appservice"`
}

type synapseConfig struct {
	Database struct {
		Args struct {
			User     string `yaml:"user"`
			Password string `yaml:"password"`
			Database string `yaml:"database"`
			Host     string `yaml:"host"`
			Port     int    `yaml:"port"`
		} `yaml:"args"`
	} `yaml:"database"`
}

func loadDatabaseURIs(bridgePath, synapsePath string) (string, string, error) {
	var bridge bridgeConfig
	if err := decodeYAMLFile(bridgePath, &bridge); err != nil {
		return "", "", fmt.Errorf("read bridge config: %w", err)
	}
	if bridge.AppService.Database.URI == "" {
		return "", "", fmt.Errorf("bridge config has no appservice.database.uri")
	}

	var synapse synapseConfig
	if err := decodeYAMLFile(synapsePath, &synapse); err != nil {
		return "", "", fmt.Errorf("read Synapse config: %w", err)
	}
	args := synapse.Database.Args
	if args.User == "" || args.Database == "" || args.Host == "" {
		return "", "", fmt.Errorf("Synapse database config is incomplete")
	}

	host := args.Host
	if args.Port != 0 {
		host = fmt.Sprintf("%s:%d", host, args.Port)
	}
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(args.User, args.Password),
		Host:   host,
		Path:   "/" + args.Database,
	}
	query := u.Query()
	query.Set("sslmode", "disable")
	u.RawQuery = query.Encode()

	return bridge.AppService.Database.URI, u.String(), nil
}

func decodeYAMLFile(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	return yaml.NewDecoder(file).Decode(target)
}

func decodeStrictYAMLFile(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	return decoder.Decode(target)
}
