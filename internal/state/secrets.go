package state

import (
	"fmt"
	"sort"
	"strings"

	bolt "go.etcd.io/bbolt"
)

func secretKey(project, key string) []byte {
	return []byte(project + "\x00" + key)
}

func (s *Store) SetSecret(project, key, value string) error {
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("secret key cannot be empty")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketSecrets).Put(secretKey(project, key), []byte(value))
	})
}

func (s *Store) UnsetSecret(project, key string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketSecrets).Delete(secretKey(project, key))
	})
}

func (s *Store) ListSecrets(project string) ([]string, error) {
	prefix := []byte(project + "\x00")
	var keys []string
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketSecrets).Cursor()
		for k, _ := c.Seek(prefix); k != nil && len(k) >= len(prefix) && string(k[:len(prefix)]) == string(prefix); k, _ = c.Next() {
			keys = append(keys, string(k[len(prefix):]))
		}
		return nil
	})
	sort.Strings(keys)
	return keys, err
}

// ResolveSecrets returns values for all requested keys or errors listing any missing secrets.
func (s *Store) ResolveSecrets(project string, keys []string) (map[string]string, error) {
	resolved := make(map[string]string, len(keys))
	var missing []string
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketSecrets)
		for _, k := range keys {
			val := b.Get(secretKey(project, k))
			if val == nil {
				missing = append(missing, k)
				continue
			}
			resolved[k] = string(val)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("missing required secrets for project %q: %s (set them via `yak secret set <key>`)", project, strings.Join(missing, ", "))
	}
	return resolved, nil
}
