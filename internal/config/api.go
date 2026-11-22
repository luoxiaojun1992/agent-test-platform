package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"agent-test-platform/internal/secrets"
)

const (
	httpAddrEnvironment              = "HTTP_ADDR"
	platformMySQLDSNEnvironment      = "PLATFORM_MYSQL_DSN"
	apiBearerTokenEnvironment        = "API_BEARER_TOKEN"
	generatorConfigSHA256Environment = "GENERATOR_CONFIG_SHA256"
)

type API struct {
	HTTPAddr              string
	PlatformMySQLDSN      string
	BearerToken           string
	GeneratorConfigSHA256 string
	SecretKeyring         *secrets.Keyring
}

func LoadAPI() (API, error) {
	return loadAPI(os.LookupEnv)
}

func loadAPI(lookup func(string) (string, bool)) (API, error) {
	if lookup == nil {
		return API{}, fmt.Errorf("environment lookup is required")
	}
	values := make(map[string]string, 4)
	for _, name := range []string{
		httpAddrEnvironment,
		platformMySQLDSNEnvironment,
		apiBearerTokenEnvironment,
		generatorConfigSHA256Environment,
	} {
		value, found := lookup(name)
		if !found || strings.TrimSpace(value) == "" {
			return API{}, fmt.Errorf("%s is required", name)
		}
		values[name] = value
	}

	address := strings.TrimSpace(values[httpAddrEnvironment])
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return API{}, fmt.Errorf("%s must be a host:port address: %w", httpAddrEnvironment, err)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return API{}, fmt.Errorf("%s contains an invalid port", httpAddrEnvironment)
	}

	token := values[apiBearerTokenEnvironment]
	if token != strings.TrimSpace(token) || len(token) < 32 || len(token) > 512 {
		return API{}, fmt.Errorf("%s must contain 32-512 non-whitespace-padded characters", apiBearerTokenEnvironment)
	}
	digest := values[generatorConfigSHA256Environment]
	if !isLowerSHA256(digest) {
		return API{}, fmt.Errorf("%s must be 64 lowercase hexadecimal characters", generatorConfigSHA256Environment)
	}
	keyring, err := secrets.LoadKeyring(lookup)
	if err != nil {
		return API{}, err
	}

	return API{
		HTTPAddr: address, PlatformMySQLDSN: values[platformMySQLDSNEnvironment],
		BearerToken: token, GeneratorConfigSHA256: digest, SecretKeyring: keyring,
	}, nil
}

func isLowerSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
