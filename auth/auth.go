package auth

import (
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/ProtonMail/go-crypto/openpgp"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/nacl/secretbox"

	"github.com/seamanticus/peroxide/config"
	"github.com/seamanticus/peroxide/protonmail"
)

// authFileMode lets only its owner read and write auth.json.
const authFileMode = 0o600

func authFilePath() (string, error) {
	return config.Path("auth.json")
}

type CachedAuth struct {
	protonmail.Auth
	LoginPassword   string
	MailboxPassword string
	KeySalts        map[string][]byte
	// TODO: add padding
}

func readCachedAuths() (map[string]string, error) {
	p, err := authFilePath()
	if err != nil {
		return nil, fmt.Errorf("failed to get auth file path: %v", err)
	}

	f, err := os.Open(p)
	if os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to open cached auth file: %v", err)
	}
	defer f.Close()

	auths := make(map[string]string)
	if err := json.NewDecoder(f).Decode(&auths); err != nil {
		return nil, fmt.Errorf("failed to read cached auth file: %v", err)
	}

	return auths, nil
}

func saveAuths(auths map[string]string) error {
	p, err := authFilePath()
	if err != nil {
		return fmt.Errorf("failed to get auth file path: %v", err)
	}
	// Usernames are in clear and the records are only as safe as the bridge
	// password: no one else may read them. Chmod too, as OpenFile's mode
	// applies only to a new file and older peroxides created it 0644.
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, authFileMode)
	if err != nil {
		return fmt.Errorf("failed to create cached auth file: %v", err)
	}
	defer f.Close()
	if err := f.Chmod(authFileMode); err != nil {
		return fmt.Errorf("cannot restrict cached auth file to its owner: %w", err)
	}

	if err := json.NewEncoder(f).Encode(auths); err != nil {
		return fmt.Errorf("failed to write cached auth file: %v", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("failed to close cached auth file: %v", err)
	}
	return nil
}

func encrypt(msg []byte, secretKey *[32]byte) (string, error) {
	var nonce [24]byte
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		return "", fmt.Errorf("failed to generate nonce: %v", err)
	}

	encrypted := secretbox.Seal(nonce[:], msg, &nonce, secretKey)
	return base64.StdEncoding.EncodeToString(encrypted), nil
}

func decrypt(encryptedString string, secretKey *[32]byte) ([]byte, error) {
	encrypted, err := base64.StdEncoding.DecodeString(encryptedString)
	if err != nil {
		return nil, fmt.Errorf("invalid encrypted string: %v", err)
	}

	var nonce [24]byte
	copy(nonce[:], encrypted[:24])
	decrypted, ok := secretbox.Open(nil, encrypted[24:], &nonce, secretKey)
	if !ok {
		return nil, errors.New("decryption error")
	}
	return decrypted, nil
}

func EncryptAndSave(auth *CachedAuth, username string, secretKey *[32]byte) error {
	cleartext, err := json.Marshal(auth)
	if err != nil {
		return fmt.Errorf("failed to format cached auth: %v", err)
	}

	encrypted, err := encrypt(cleartext, secretKey)
	if err != nil {
		return err
	}

	auths, err := readCachedAuths()
	if err != nil {
		return err
	}

	if auths == nil {
		auths = make(map[string]string)
	}
	auths[username] = encrypted

	return saveAuths(auths)
}

func authenticate(c *protonmail.Client, cachedAuth *CachedAuth, username string) (openpgp.EntityList, error) {
	auth, err := c.AuthRefresh(&cachedAuth.Auth)
	if apiErr, ok := err.(*protonmail.APIError); ok && apiErr.Code == 10013 {
		// Invalid refresh token, re-authenticate
		authInfo, err := c.AuthInfo(username)
		if err != nil {
			return nil, fmt.Errorf("cannot re-authenticate: failed to get auth info: %v", err)
		}

		auth, err = c.Auth(username, cachedAuth.LoginPassword, authInfo)
		if apiErr, ok := err.(*protonmail.APIError); ok && apiErr.IsHumanVerificationRequired() {
			// Human verification can't be completed from a background
			// session: it needs a browser and a human.
			return nil, fmt.Errorf("cannot re-authenticate: Proton requires human verification, please run `peroxide auth %v` again", username)
		} else if err != nil {
			return nil, fmt.Errorf("cannot re-authenticate: %v", err)
		}

		if auth.TwoFactor.Enabled != 0 {
			return nil, fmt.Errorf("cannot re-authenticate: two factor authentication enabled, please login again manually")
		}
	} else if err != nil {
		return nil, err
	}
	cachedAuth.Auth = *auth

	return c.Unlock(auth, cachedAuth.KeySalts, cachedAuth.MailboxPassword)
}

func ListUsernames() ([]string, error) {
	auths, err := readCachedAuths()
	if err != nil {
		return nil, err
	}

	l := make([]string, 0, len(auths))
	for username, _ := range auths {
		l = append(l, username)
	}
	return l, nil
}

func DeleteUsername(username string) error {
	auths, err := readCachedAuths()
	if err != nil {
		return err
	}

	if _, ok := auths[username]; !ok {
		return fmt.Errorf("no stored credentials for %v", username)
	}

	delete(auths, username)

	if err = saveAuths(auths); err != nil {
		return err
	}
	log.Printf("Deleted credentials for %v", username)
	return nil
}

func GeneratePassword() (secretKey *[32]byte, password string, err error) {
	var key [32]byte
	if _, err = io.ReadFull(rand.Reader, key[:]); err != nil {
		return nil, "", fmt.Errorf("failed to generate key: %v", err)
	}
	password = base64.StdEncoding.EncodeToString(key[:])
	return &key, password, nil
}

// storeKeyInfo labels the store key's derivation, so that it differs from
// the secret key sealing auth.json, which it is derived from.
const storeKeyInfo = "peroxide message store v1"

// StoreKey is the key sealing an account's stored messages.
type StoreKey [32]byte

// deriveStoreKey derives the key of the message store of the account userID
// from the secret key the bridge password encodes: HKDF-SHA256, salted with
// the account's immutable Proton user ID, so one key per account, never
// stored. The secret key is 32 random bytes: no slow KDF is needed.
func deriveStoreKey(secretKey *[32]byte, userID string) (StoreKey, error) {
	if userID == "" {
		return StoreKey{}, errors.New("cannot derive the message store key: no user ID")
	}
	b, err := hkdf.Key(sha256.New, secretKey[:], []byte(userID), storeKeyInfo, len(StoreKey{}))
	if err != nil {
		return StoreKey{}, fmt.Errorf("cannot derive the message store key: %w", err)
	}
	return StoreKey(b), nil
}

type session struct {
	hashedSecretKey []byte
	c               *protonmail.Client
	privateKeys     openpgp.EntityList
	storeKey        StoreKey
}

var ErrUnauthorized = errors.New("Invalid username or password")

type Manager struct {
	newClient func() *protonmail.Client
	sessions  map[string]*session
}

// Auth logs username in with its bridge password, once per process, and
// returns its API client, its unlocked keys and the key sealing its stored
// messages.
func (m *Manager) Auth(username, password string) (*protonmail.Client, openpgp.EntityList, StoreKey, error) {
	var secretKey [32]byte
	passwordBytes, err := base64.StdEncoding.DecodeString(password)
	if err != nil || len(passwordBytes) != len(secretKey) {
		return nil, nil, StoreKey{}, ErrUnauthorized
	}
	copy(secretKey[:], passwordBytes)

	s, ok := m.sessions[username]
	if ok {
		err := bcrypt.CompareHashAndPassword(s.hashedSecretKey, secretKey[:])
		if err != nil {
			return nil, nil, StoreKey{}, ErrUnauthorized
		}
	} else {
		auths, err := readCachedAuths()
		if err != nil && !os.IsNotExist(err) {
			return nil, nil, StoreKey{}, err
		}

		encrypted, ok := auths[username]
		if !ok {
			return nil, nil, StoreKey{}, ErrUnauthorized
		}

		decrypted, err := decrypt(encrypted, &secretKey)
		if err != nil {
			return nil, nil, StoreKey{}, ErrUnauthorized
		}

		var cachedAuth CachedAuth
		if err := json.Unmarshal(decrypted, &cachedAuth); err != nil {
			return nil, nil, StoreKey{}, err
		}

		c := m.newClient()
		c.ReAuth = func() error {
			if _, err := authenticate(c, &cachedAuth, username); err != nil {
				return err
			}
			return EncryptAndSave(&cachedAuth, username, &secretKey)
		}

		// authenticate updates cachedAuth with the new refresh token
		privateKeys, err := authenticate(c, &cachedAuth, username)
		if err != nil {
			return nil, nil, StoreKey{}, err
		}

		if err := EncryptAndSave(&cachedAuth, username, &secretKey); err != nil {
			return nil, nil, StoreKey{}, err
		}

		// The user ID from the account itself: the cached Auth holds what
		// the last /auth/refresh returned, which need not carry it. One
		// request per account and process.
		pu, err := c.GetCurrentUser()
		if err != nil {
			return nil, nil, StoreKey{}, fmt.Errorf("cannot get the account: %w", err)
		}
		storeKey, err := deriveStoreKey(&secretKey, pu.ID)
		if err != nil {
			return nil, nil, StoreKey{}, err
		}

		hashed, err := bcrypt.GenerateFromPassword(secretKey[:], bcrypt.DefaultCost)
		if err != nil {
			return nil, nil, StoreKey{}, err
		}

		s = &session{
			c:               c,
			privateKeys:     privateKeys,
			hashedSecretKey: hashed,
			storeKey:        storeKey,
		}
		m.sessions[username] = s
	}

	return s.c, s.privateKeys, s.storeKey, nil
}

func NewManager(newClient func() *protonmail.Client) *Manager {
	return &Manager{
		newClient: newClient,
		sessions:  make(map[string]*session),
	}
}
