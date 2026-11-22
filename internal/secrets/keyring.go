package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const (
	// KeyBytes is the AES-256 key length.
	KeyBytes = 32
	// MaxKeys bounds how many keys one keyring holds at once.
	MaxKeys = 8
	// KeyIDVariable names the environment variable that holds the id of the key used for new writes.
	KeyIDVariable = "SECRET_ENCRYPTION_KEY_ID"
	// KeyIDsVariable lists every key id the process may read. It must include KeyIDVariable's id.
	KeyIDsVariable = "SECRET_ENCRYPTION_KEY_IDS"

	keyVariablePrefix = "ATP_ENCRYPTION_KEY_"
	nonceBytes        = 12
)

var (
	// ErrKeyUnavailable reports a secret sealed under a key id that this process does not hold.
	ErrKeyUnavailable = errors.New("encryption key is not configured")
	// ErrUnreadable reports a stored value that does not authenticate under its key and name.
	ErrUnreadable = errors.New("secret cannot be decrypted with the configured key")
)

var keyIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Keyring seals new values with one current key and opens values sealed under any key it holds.
// Values are AES-256-GCM sealed, and the secret name is bound as additional authenticated data.
type Keyring struct {
	currentID string
	aeads     map[string]cipher.AEAD
}

// NewKeyring builds a keyring from raw keys. currentID must name one of them.
func NewKeyring(currentID string, keys map[string][]byte) (*Keyring, error) {
	if len(keys) == 0 || len(keys) > MaxKeys {
		return nil, fmt.Errorf("keyring must hold 1 to %d keys", MaxKeys)
	}
	ring := &Keyring{currentID: currentID, aeads: make(map[string]cipher.AEAD, len(keys))}
	for id, key := range keys {
		if !keyIDPattern.MatchString(id) {
			return nil, fmt.Errorf("key id %q is invalid", id)
		}
		if len(key) != KeyBytes {
			return nil, fmt.Errorf("key %s must be %d bytes", id, KeyBytes)
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("create AES cipher for key %s: %w", id, err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("create GCM for key %s: %w", id, err)
		}
		ring.aeads[id] = aead
	}
	if _, found := ring.aeads[currentID]; !found {
		return nil, fmt.Errorf("current key %s is not in the keyring", currentID)
	}
	return ring, nil
}

// CurrentID returns the id of the key that seals new values.
func (ring *Keyring) CurrentID() string {
	return ring.currentID
}

// Seal encrypts value for name under the current key with a fresh random nonce.
func (ring *Keyring) Seal(name, value string) (keyID string, nonce, ciphertext []byte, err error) {
	nonce = make([]byte, nonceBytes)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", nil, nil, fmt.Errorf("generate nonce: %w", err)
	}
	ciphertext = ring.aeads[ring.currentID].Seal(nil, nonce, []byte(value), additionalData(name))
	return ring.currentID, nonce, ciphertext, nil
}

// Open decrypts a value stored under name and sealed with keyID. The error never contains the value.
func (ring *Keyring) Open(keyID, name string, nonce, ciphertext []byte) (string, error) {
	aead, found := ring.aeads[keyID]
	if !found {
		return "", fmt.Errorf("%w: key id %s", ErrKeyUnavailable, keyID)
	}
	if len(nonce) != nonceBytes {
		return "", ErrUnreadable
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, additionalData(name))
	if err != nil {
		return "", ErrUnreadable
	}
	return string(plaintext), nil
}

func additionalData(name string) []byte {
	return []byte("atp-secret:v1:" + name)
}

// LoadKeyring reads the keyring from environment variables: KeyIDVariable names the current key,
// KeyIDsVariable lists every readable key, and ATP_ENCRYPTION_KEY_<ID> holds the base64 of each key.
// Errors name variables and key ids but never include key material.
func LoadKeyring(lookup func(string) (string, bool)) (*Keyring, error) {
	currentID, err := requiredVariable(lookup, KeyIDVariable)
	if err != nil {
		return nil, err
	}
	if !keyIDPattern.MatchString(currentID) {
		return nil, fmt.Errorf("%s must be 1-32 lowercase letters, digits, or hyphens", KeyIDVariable)
	}
	listed, err := requiredVariable(lookup, KeyIDsVariable)
	if err != nil {
		return nil, err
	}
	keys := make(map[string][]byte)
	for _, raw := range strings.Split(listed, ",") {
		id := strings.TrimSpace(raw)
		if !keyIDPattern.MatchString(id) {
			return nil, fmt.Errorf("%s contains an invalid key id", KeyIDsVariable)
		}
		if _, repeated := keys[id]; repeated {
			return nil, fmt.Errorf("%s repeats key id %s", KeyIDsVariable, id)
		}
		key, err := loadKey(lookup, id)
		if err != nil {
			return nil, err
		}
		keys[id] = key
	}
	return NewKeyring(currentID, keys)
}

func loadKey(lookup func(string) (string, bool), id string) ([]byte, error) {
	name := keyVariable(id)
	value, found := lookup(name)
	if !found || strings.TrimSpace(value) == "" {
		return nil, fmt.Errorf("encryption key environment variable %s is not set", name)
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil || len(key) != KeyBytes {
		return nil, fmt.Errorf("encryption key in environment variable %s must be base64 of %d bytes", name, KeyBytes)
	}
	return key, nil
}

// keyVariable maps a key id to its environment variable: upper-case, hyphens become underscores.
func keyVariable(id string) string {
	return keyVariablePrefix + strings.ToUpper(strings.ReplaceAll(id, "-", "_"))
}

func requiredVariable(lookup func(string) (string, bool), name string) (string, error) {
	value, found := lookup(name)
	if !found || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return strings.TrimSpace(value), nil
}
