package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestLoadAPI(t *testing.T) {
	t.Parallel()

	values := validAPIEnvironment()
	configuration, err := loadAPI(mapLookup(values))
	if err != nil {
		t.Fatal(err)
	}
	if configuration.HTTPAddr != "127.0.0.1:8080" || configuration.PlatformMySQLDSN != values[platformMySQLDSNEnvironment] {
		t.Fatalf("configuration = %#v", configuration)
	}
	if configuration.SecretKeyring == nil || configuration.SecretKeyring.CurrentID() != "k1" {
		t.Fatalf("keyring = %#v", configuration.SecretKeyring)
	}
}

func TestLoadAPIRejectsInvalidEnvironment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		change func(map[string]string)
		want   string
	}{
		{name: "missing DSN", change: func(values map[string]string) { delete(values, platformMySQLDSNEnvironment) }, want: platformMySQLDSNEnvironment},
		{name: "invalid address", change: func(values map[string]string) { values[httpAddrEnvironment] = "8080" }, want: httpAddrEnvironment},
		{name: "weak token", change: func(values map[string]string) { values[apiBearerTokenEnvironment] = "short" }, want: apiBearerTokenEnvironment},
		{name: "invalid digest", change: func(values map[string]string) { values[generatorConfigSHA256Environment] = strings.Repeat("G", 64) }, want: generatorConfigSHA256Environment},
		{name: "missing current key id", change: func(values map[string]string) { delete(values, "SECRET_ENCRYPTION_KEY_ID") }, want: "SECRET_ENCRYPTION_KEY_ID"},
		{name: "missing key list", change: func(values map[string]string) { delete(values, "SECRET_ENCRYPTION_KEY_IDS") }, want: "SECRET_ENCRYPTION_KEY_IDS"},
		{name: "current key not listed", change: func(values map[string]string) { values["SECRET_ENCRYPTION_KEY_ID"] = "k2" }, want: "current key k2"},
		{name: "key variable is unset", change: func(values map[string]string) { delete(values, testKeyVariable) }, want: testKeyVariable},
		{name: "key is not 32 bytes", change: func(values map[string]string) {
			values[testKeyVariable] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 16)))
		}, want: "must be base64 of 32 bytes"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			values := validAPIEnvironment()
			test.change(values)
			_, err := loadAPI(mapLookup(values))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

// testKeyVariable is the environment variable that holds the key with id k1.
const testKeyVariable = "ATP_ENCRYPTION_KEY_K1"

func validAPIEnvironment() map[string]string {
	return map[string]string{
		httpAddrEnvironment:              "127.0.0.1:8080",
		platformMySQLDSNEnvironment:      "platform:secret@tcp(127.0.0.1:3306)/agent_test_platform",
		apiBearerTokenEnvironment:        strings.Repeat("t", 32),
		generatorConfigSHA256Environment: strings.Repeat("a", 64),
		"SECRET_ENCRYPTION_KEY_ID":       "k1",
		"SECRET_ENCRYPTION_KEY_IDS":      "k1",
		testKeyVariable:                  base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))),
	}
}

func mapLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, found := values[name]
		return value, found
	}
}
